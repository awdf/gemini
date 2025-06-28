package ai

import (
	"context"
	"fmt"
	"iter"
	"log"
	"os"
	"sync"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"google.golang.org/genai"
)

const (
	DEBUG = false
)

// Model
const model = "gemini-2.5-flash"
const modelTTS = "gemini-2.5-flash-preview-tts"
const voice = "Kore"

// Context
var ctx context.Context

// Client
var client *genai.Client

// Flash model thinking
// Dynamic:-1
// None:0
// low:512,
// Medium:8,192,
// High:24,576
var thinking int32 = -1
var thoughts = false

var isInit = false

func Init() {

	ctx = context.Background()
	client = Control(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  os.Getenv("GOOGLE_API_KEY"),
		Backend: genai.BackendGeminiAPI,
	}))

	isInit = true
}

func Chat(wg *sync.WaitGroup, fileChan <-chan string) {
	defer wg.Done()

	if !isInit {
		Init()
	}

	for file := range fileChan {
		log.Printf("Chat: Processing %s", file)
		err := VoiceQuestion(file, "Step 1: Generate a transcript of the speech. Step 2: Respond to the question from the speech.")
		if err != nil {
			// If there was an error, log it and DO NOT delete the file.
			log.Printf("ERROR: AI processing failed for %s, leaving file for retry: %v", file, err)
		} else {
			// Only remove the file if processing was successful.
			log.Printf("Chat: Successfully processed %s. Removing file.", file)
			os.Remove(file)
		}
	}
	log.Println("AI Chat work finished")
}

func TextQuestion(prompt string) {

	if !isInit {
		Init()
	}

	resp := client.Models.GenerateContentStream(
		ctx,
		model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: thoughts,
				ThinkingBudget:  &thinking,
			},
		},
	)

	if err := Output(resp); err != nil {
		log.Printf("ERROR: processing text question: %v", err)
	}
}

func VoiceQuestion(wavPath string, prompt string) error {

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
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	resp := client.Models.GenerateContentStream(
		ctx,
		model,
		contents,
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: thoughts,
				ThinkingBudget:  &thinking,
			},
		},
	)

	return Output(resp)
}

func Output(resp iter.Seq2[*genai.GenerateContentResponse, error]) error {
	for chunk, err := range resp {
		if err != nil {
			return err
		}
		// Add nil checks for robustness
		if chunk == nil || len(chunk.Candidates) == 0 || chunk.Candidates[0].Content == nil {
			continue
		}

		for _, part := range chunk.Candidates[0].Content.Parts {
			if len(part.Text) == 0 {
				continue
			}

			if part.Thought {
				log.Printf("Thought: %s", part.Text)
			} else {
				log.Printf("Answer: %s", part.Text)
			}
		}
	}
	return nil
}

func AnwerWithVoice(prompt string) error {
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
		modelTTS,
		contents,
		&genai.GenerateContentConfig{
			ResponseModalities: mode, // Corrected: mode is now a []string
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
						VoiceName: voice,
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
	log.Println("Playing AI voice response...")
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
