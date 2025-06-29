package ai

import (
	"context"
	"fmt"
	"iter"
	"log"
	"os"
	"sync"

	"capgemini.com/config"
	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"google.golang.org/genai"
)

const (
	DEBUG = false
)

// ANSI color codes for terminal output.
const (
	colorReset  = "\033[0m"
	colorYellow = "\033[33m" // For "Thought:" prefix
	colorCyan   = "\033[36m" // For "Answer:" prefix
	colorWhite  = "\033[97m" // For **bold** text
	colorGreen  = "\033[32m" // For ```code``` blocks
)

var ctx context.Context
var client *genai.Client

// Conversation history
var conversationHistory []*genai.Content

var isInit = false

func Init() {

	ctx = context.Background()
	client = Control(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  os.Getenv("GOOGLE_API_KEY"),
		Backend: genai.BackendGeminiAPI,
	}))

	isInit = true
}

func Chat(wg *sync.WaitGroup, pipeline *gst.Pipeline, fVoice bool, fileChan <-chan string) {
	defer wg.Done()

	if !isInit {
		Init()
	}

	for file := range fileChan {
		log.Printf("Chat: Processing %s", file)

		// If voice responses are enabled, pause the listening pipeline
		// to prevent it from recording its own speech.
		if fVoice {
			log.Println("Pausing listening pipeline for AI response...")
			// Schedule the state change on the main GLib context to avoid deadlocks.
			// This is a blocking operation, so we wait for it to complete.
			done := make(chan struct{})
			glib.IdleAdd(func() bool {
				pipeline.SetState(gst.StatePaused)
				close(done)
				return false // Do not call again
			})
			<-done
		}

		err := VoiceQuestion(file, config.C.AI.MainPrompt, fVoice)
		if err != nil {
			// If there was an error, log it and DO NOT delete the file.
			log.Printf("ERROR: AI processing failed for %s, leaving file for retry: %v", file, err)
		} else {
			// Only remove the file if processing was successful.
			log.Printf("Chat: Successfully processed %s. Removing file.", file)
			os.Remove(file)
		}
		// Resume the listening pipeline after the AI has finished speaking.
		if fVoice {
			log.Println("Resuming listening pipeline...")
			// Schedule the state change on the main GLib context.
			done := make(chan struct{})
			glib.IdleAdd(func() bool {
				pipeline.SetState(gst.StatePlaying)
				close(done)
				return false // Do not call again
			})
			<-done
		}
	}

	log.Println("AI Chat work finished")
}

func TextQuestion(prompt string, fVoice bool) {

	if !isInit {
		Init()
	}

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
	}

	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	contents := append(conversationHistory, userContent)

	resp := client.Models.GenerateContentStream(
		ctx,
		config.C.AI.Model,
		contents,
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: config.C.AI.Thoughts,
				ThinkingBudget:  &config.C.AI.Thinking,
			},
		},
	)

	fullResponse, err := Output(resp, fVoice)
	if err != nil {
		log.Printf("ERROR: processing text question: %v", err)
	} else {
		conversationHistory = append(conversationHistory, userContent)
		save := []*genai.Part{
			genai.NewPartFromText(fullResponse),
		}
		modelResponseContent := genai.NewContentFromParts(save, genai.RoleModel)
		conversationHistory = append(conversationHistory, modelResponseContent)
	}
}

func VoiceQuestion(wavPath string, prompt string, fVoice bool) error {

	if !isInit {
		Init()
	}

	uploadedFile, err := client.Files.UploadFromPath(
		ctx,
		wavPath,
		nil,
	)
	if err != nil {
		return fmt.Errorf("file upload failed: %w", err)
	}

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	contents := append(conversationHistory, userContent)

	resp := client.Models.GenerateContentStream(
		ctx,
		config.C.AI.Model,
		contents,
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: config.C.AI.Thoughts,
				ThinkingBudget:  &config.C.AI.Thinking,
			},
		},
	)

	fullResponse, err := Output(resp, fVoice)
	if err != nil {
		return err
	}

	conversationHistory = append(conversationHistory, userContent)
	save := []*genai.Part{
		genai.NewPartFromText(fullResponse),
	}
	modelResponseContent := genai.NewContentFromParts(save, genai.RoleModel)
	conversationHistory = append(conversationHistory, modelResponseContent)
	return nil
}

func printFormatted(text string, inBold, inCodeBlock *bool) {
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		// Check for code block marker first, as it takes precedence.
		if i+2 < len(runes) && runes[i] == '`' && runes[i+1] == '`' && runes[i+2] == '`' {
			*inCodeBlock = !*inCodeBlock
			if *inCodeBlock {
				fmt.Print(colorGreen)
			} else {
				fmt.Print(colorReset)
			}
			i += 2 // Advance past the marker.
			continue
		}

		// If we are in a code block, just print the character.
		if *inCodeBlock {
			fmt.Print(string(runes[i]))
			continue
		}

		// Check for bold marker.
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			*inBold = !*inBold
			if *inBold {
				fmt.Print(colorWhite)
			} else {
				fmt.Print(colorReset)
			}
			i++ // Advance past the marker.
			continue
		}

		// Print the character.
		fmt.Print(string(runes[i]))
	}
}

func Output(resp iter.Seq2[*genai.GenerateContentResponse, error], fVoice bool) (string, error) {
	// State flags to print prefixes only once per block and track formatting.
	var thoughtStarted, answerStarted, inBold, inCodeBlock bool
	var fullResponseText string // To accumulate the full text for history and a single TTS call

	for chunk, err := range resp {
		if err != nil {
			// On error, ensure we reset color and print a newline.
			fmt.Println(colorReset)
			return "", err
		}
		if chunk == nil || len(chunk.Candidates) == 0 || chunk.Candidates[0].Content == nil {
			continue
		}

		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.Thought {
				if !thoughtStarted {
					fmt.Printf("\n%sThought: %s\n", colorYellow, colorReset)
					thoughtStarted, answerStarted = true, false // Reset answer flag
				}
				printFormatted(part.Text, &inBold, &inCodeBlock)
			} else {
				if !answerStarted {
					if fVoice {
						fmt.Printf("\n%sVoice answer: %s\n", colorCyan, colorReset)
					} else {
						fmt.Printf("\n%sAnswer: %s\n", colorCyan, colorReset)
					}
					answerStarted, thoughtStarted = true, false
				}

				// Always print the text chunk as it arrives for immediate feedback.
				printFormatted(part.Text, &inBold, &inCodeBlock)

				// Accumulate all non-thought text for history and potential TTS.
				fullResponseText += part.Text
			}
		}
	}
	// After the stream is finished, if voice was enabled and we have text,
	// make a single API call to generate the audio.
	if fVoice && len(fullResponseText) > 0 {
		if err := AnswerWithVoice(fullResponseText); err != nil {
			// Log the error but don't fail the whole operation, as the user
			// has already received the text response.
			log.Printf("ERROR: Text-to-speech failed: %v", err)
		}
	}
	// After the stream is finished, reset the color and print a final newline.
	fmt.Println(colorReset)
	return fullResponseText, nil
}

// Model RPD 15
func AnswerWithVoice(prompt string) error {
	if !isInit {
		Init()
	}
	var mode = []string{"AUDIO"}

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	resp := client.Models.GenerateContentStream(
		ctx,
		config.C.AI.ModelTTS,
		contents,
		&genai.GenerateContentConfig{
			ResponseModalities: mode, // Corrected: mode is now a []string
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
						VoiceName: config.C.AI.Voice,
					},
				},
			},
		},
	)

	var audioData []byte
	for chunk, err := range resp {
		if err != nil {
			return err
		}
		// Add nil checks for robustness
		if chunk == nil || len(chunk.Candidates) == 0 || chunk.Candidates[0].Content == nil {
			continue
		}

		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.InlineData != nil && len(part.InlineData.Data) > 0 {
				audioData = append(audioData, part.InlineData.Data...)
			}
		}
	}

	if len(audioData) > 0 {
		return playWavBlob(audioData)
	}

	return fmt.Errorf("no audio data received from API")
}

func playWavBlob(data []byte) error {
	// Create a new pipeline
	pipeline, err := gst.NewPipeline("audio-player")
	if err != nil {
		return fmt.Errorf("failed to create pipeline: %w", err)
	}

	// Create elements
	appsrc, err := app.NewAppSrc()
	if err != nil {
		return fmt.Errorf("failed to create appsrc: %w", err)
	}

	// The Gemini TTS API returns raw 16-bit linear PCM audio.
	// We must use a capsfilter to describe this format to the pipeline,
	// as there is no WAV header. 'wavparse' is not needed.
	capsfilter, err := gst.NewElement("capsfilter")
	if err != nil {
		return fmt.Errorf("failed to create capsfilter: %w", err)
	}
	// Default Gemini TTS format: 24000 Hz, 1 channel (mono), 16-bit signed little-endian.
	capsfilter.SetProperty("caps", gst.NewCapsFromString(
		"audio/x-raw, format=S16LE, layout=interleaved, channels=1, rate=24000",
	))

	audioconvert, err := gst.NewElement("audioconvert")
	if err != nil {
		return fmt.Errorf("failed to create audioconvert: %w", err)
	}
	audioresample, err := gst.NewElement("audioresample")
	if err != nil {
		return fmt.Errorf("failed to create audioresample: %w", err)
	}
	audiosink, err := gst.NewElement("autoaudiosink")
	if err != nil {
		return fmt.Errorf("failed to create autoaudiosink: %w", err)
	}

	// Add elements to the pipeline
	pipeline.AddMany(appsrc.Element, capsfilter, audioconvert, audioresample, audiosink)

	// Link elements
	if err = gst.ElementLinkMany(appsrc.Element, capsfilter, audioconvert, audioresample, audiosink); err != nil {
		return fmt.Errorf("failed to link elements: %w", err)
	}

	// Push the WAV data into appsrc
	buffer := gst.NewBufferFromBytes(data)
	if ret := appsrc.PushBuffer(buffer); ret != gst.FlowOK {
		return fmt.Errorf("failed to push buffer to appsrc: %v", ret)
	}

	// Signal end of stream
	if ret := appsrc.EndStream(); ret != gst.FlowOK {
		return fmt.Errorf("failed to send EOS to appsrc: %v", ret)
	}

	// Start playing
	pipeline.SetState(gst.StatePlaying)

	// Wait for the pipeline to finish by watching the bus for an EOS or Error message.
	bus := pipeline.GetBus()
	msg := bus.TimedPopFiltered(gst.ClockTimeNone, gst.MessageEOS|gst.MessageError)
	if msg != nil && msg.Type() == gst.MessageError {
		return fmt.Errorf("playback error: %s", msg.ParseError().Error())
	}

	// Clean up
	pipeline.SetState(gst.StateNull)
	return nil
}

// Control is a helper function to check errors during GStreamer element creation.
func Control[T any](object T, err error) T {
	if err != nil {
		panic(err)
	}
	return object
}

// Verify is a helper function to check errors during GStreamer linking.
func Verify(err error) {
	if err != nil {
		panic(err)
	}
}
