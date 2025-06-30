package ai

import (
	"context"
	"fmt"
	"iter"
	"log"
	"os"
	"sync"

	"capgemini.com/audio"
	"capgemini.com/config"
	"capgemini.com/display"
	"capgemini.com/helpers"
	"capgemini.com/pipeline"
	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"google.golang.org/genai"
)

const (
	DEBUG = false
)

// AI encapsulates the state and logic for interacting with the Gemini AI.
type AI struct {
	ctx                 context.Context
	client              *genai.Client
	conversationHistory []*genai.Content
}

// NewAI creates a new AI instance, initializing the client and conversation history.
func NewAI() *AI {
	ctx := context.Background()
	client := helpers.Control(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	}))

	return &AI{
		ctx:    ctx,
		client: client,
	}
}

func (a *AI) Chat(wg *sync.WaitGroup, pipeline *pipeline.VadPipeline, fVoice bool, fileChan <-chan string) {
	defer wg.Done()

	for file := range fileChan {
		log.Printf("Chat: Processing %s", file)

		a.withPipelinePausedIfVoice(pipeline, fVoice, func() {
			err := a.VoiceQuestion(file, config.C.AI.MainPrompt, fVoice)
			if err != nil {
				// If there was an error, log it and DO NOT delete the file.
				log.Printf("ERROR: AI processing failed for %s, leaving file for retry: %v", file, err)
			} else {
				// Only remove the file if processing was successful.
				log.Printf("Chat: Successfully processed %s. Removing file.", file)
				os.Remove(file)
			}
		})
	}

	log.Println("AI Chat work finished")
}

func (a *AI) TextQuestion(prompt string, fVoice bool) {
	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
	}
	if err := a.generateAndProcessContent(parts, fVoice); err != nil {
		log.Printf("ERROR: processing text question: %v", err)
	}
}

func (a *AI) VoiceQuestion(wavPath string, prompt string, fVoice bool) error {
	uploadedFile, err := a.client.Files.UploadFromPath(
		a.ctx,
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
	return a.generateAndProcessContent(parts, fVoice)
}

func (a *AI) Output(resp iter.Seq2[*genai.GenerateContentResponse, error], fVoice bool) (string, error) {
	// State flags to print prefixes only once per block and track formatting.
	var thoughtStarted, answerStarted bool
	var fullResponseText string // To accumulate the full text for history and a single TTS call

	formatter := display.NewFormatter()

	for chunk, err := range resp {
		if err != nil {
			// On error, ensure we reset color and print a newline.
			formatter.Reset()
			return "", err
		}
		if chunk == nil || len(chunk.Candidates) == 0 || chunk.Candidates[0].Content == nil {
			continue
		}

		for _, part := range chunk.Candidates[0].Content.Parts {
			if part.Thought {
				if !thoughtStarted {
					formatter.Println("Thought:", display.ColorYellow)
					thoughtStarted, answerStarted = true, false // Reset answer flag
				}
				formatter.Print(part.Text)
			} else {
				if !answerStarted {
					if fVoice {
						formatter.Println("Voice answer:", display.ColorCyan)
					} else {
						formatter.Println("Answer:", display.ColorCyan)
					}
					answerStarted, thoughtStarted = true, false
				}

				// Always print the text chunk as it arrives for immediate feedback.
				formatter.Print(part.Text)

				// Accumulate all non-thought text for history and potential TTS.
				fullResponseText += part.Text
			}
		}
	}
	// After the stream is finished, if voice was enabled and we have text,
	// make a single API call to generate the audio.
	if fVoice && len(fullResponseText) > 0 {
		if err := a.AnswerWithVoice(fullResponseText); err != nil {
			// Log the error but don't fail the whole operation, as the user
			// has already received the text response.
			log.Printf("ERROR: Text-to-speech failed: %v", err)
		}
	}
	// After the stream is finished, reset the color and print a final newline.
	formatter.Reset()
	return fullResponseText, nil
}

// setPipelineState safely sets the pipeline's state by scheduling the call
// on the main GLib context and waiting for it to complete.
func setPipelineState(p *pipeline.VadPipeline, state gst.State) {
	done := make(chan struct{})
	glib.IdleAdd(func() bool {
		p.SetState(state)
		close(done)
		return false // Do not call again
	})
	<-done
}

// withPipelinePausedIfVoice pauses and resumes the pipeline if voice is enabled,
// executing the provided action in between. It uses a defer to ensure the pipeline
// is always resumed.
func (a *AI) withPipelinePausedIfVoice(p *pipeline.VadPipeline, fVoice bool, action func()) {
	if !fVoice {
		action()
		return
	}

	log.Println("Pausing listening pipeline for AI response...")
	setPipelineState(p, gst.StatePaused)

	defer func() {
		log.Println("Resuming listening pipeline...")
		setPipelineState(p, gst.StatePlaying)
	}()

	action()
}

// generateAndProcessContent is a universal method to generate content from a set of parts,
// process the streamed response, and update the conversation history.
func (a *AI) generateAndProcessContent(parts []*genai.Part, fVoice bool) error {
	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	contents := append(a.conversationHistory, userContent)

	resp := a.client.Models.GenerateContentStream(
		a.ctx,
		config.C.AI.Model,
		contents,
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: config.C.AI.Thoughts,
				ThinkingBudget:  &config.C.AI.Thinking,
			},
			// Enable the model to use Google Search to answer questions.
			Tools: []*genai.Tool{{
				GoogleSearch: &genai.GoogleSearch{},
			}},
		},
	)

	fullResponse, err := a.Output(resp, fVoice)
	if err != nil {
		return err // The caller can decide how to log/handle this.
	}

	// If successful, update history.
	a.conversationHistory = append(a.conversationHistory, userContent)
	save := []*genai.Part{
		genai.NewPartFromText(fullResponse),
	}
	modelResponseContent := genai.NewContentFromParts(save, genai.RoleModel)
	a.conversationHistory = append(a.conversationHistory, modelResponseContent)
	return nil
}

// Model RPD 15
func (a *AI) AnswerWithVoice(prompt string) error {
	mode := []string{"AUDIO"}

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	resp := a.client.Models.GenerateContentStream(
		a.ctx,
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
		// Use the centralized audio playback function.
		return audio.PlayRawPCM(audioData, audio.TTS_SAMPLE_RATE, audio.TTS_CHANNELS)
	}

	return fmt.Errorf("no audio data received from API")
}
