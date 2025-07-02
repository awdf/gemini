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

// AI encapsulates the state and logic for interacting with the Gemini AI.
type AI struct {
	ctx                 context.Context
	client              *genai.Client
	conversationHistory []*genai.Content
	wg                  *sync.WaitGroup
	pipeline            *pipeline.VadPipeline
	fVoice              bool
	aiEnabled           bool
	fileChan            <-chan string
}

// NewAI creates a new AI instance, initializing the client and conversation history.
func NewAI(wg *sync.WaitGroup, pipeline *pipeline.VadPipeline, fVoice bool, aiEnabled bool, fileChan <-chan string) *AI {
	ctx := context.Background()
	client := helpers.Control(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	}))

	return &AI{
		ctx:       ctx,
		client:    client,
		wg:        wg,
		pipeline:  pipeline,
		fVoice:    fVoice,
		aiEnabled: aiEnabled,
		fileChan:  fileChan,
	}
}

// Run is the main loop for the AI component. It listens for completed audio files,
// sends them for processing, and handles the response.
func (a *AI) Run() {
	defer a.wg.Done()

	if !a.aiEnabled {
		log.Println("AI Chat processor is disabled. Draining file channel to prevent blocking.")
		// We must still consume from the channel to prevent the recorder from blocking.
		for file := range a.fileChan {
			if config.C.Debug {
				log.Printf("AI disabled, discarding file: %s", file)
			}
			// Discard file by doing nothing with it.
		}
		log.Println("AI Chat work finished (disabled).")
		return
	}

	for file := range a.fileChan {
		log.Printf("Chat: Processing %s", file)

		a.withPipelinePausedIfVoice(a.pipeline, a.fVoice, func() {
			err := a.VoiceQuestion(file, config.C.AI.MainPrompt, a.fVoice)
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

// It's call of voice chat with tools enabled according to config
// Will make one call for transcript and chat
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

// It's forced call of Voice chat with tool enabled ignoring config.
// Will make 2 requests: one for transcript and second for text chat with transcript
func (a *AI) VoiceQuestionWithTools(wavPath string, prompt string, fVoice bool) error {
	var save = config.C.AI.EnableTools
	defer func() {
		config.C.AI.EnableTools = save
	}()
	config.C.AI.EnableTools = true

	// Step 1: Transcribe the audio. This call will NOT use tools.
	log.Println("Request 1: Transcribing audio...")
	transcript, err := a.generateTranscript(wavPath)
	if err != nil {
		return fmt.Errorf("transcription failed: %w", err)
	}
	if transcript == "" {
		log.Println("Transcription returned empty, nothing to process.")
		return nil // Not an error, just silence or non-speech audio.
	}

	log.Printf("Transcript: %s\n", transcript)

	// Step 2: Use the transcript to ask the main question. This call CAN use tools.
	log.Println("Request 2: Generating response from transcript...")
	// Combine the main prompt with the transcript.
	fullPrompt := fmt.Sprintf("%s\n\nTranscript: \"%s\"", prompt, transcript)
	parts := []*genai.Part{
		genai.NewPartFromText(fullPrompt),
	}

	// The second step is a pure text-based query, so we can reuse the history logic.
	return a.generateAndProcessContent(parts, fVoice)
}

// generateTranscript performs a dedicated API call to get a transcript from an audio file.
// It does not use tools or conversation history to keep the request clean and focused.
func (a *AI) generateTranscript(wavPath string) (string, error) {
	uploadedFile, err := a.client.Files.UploadFromPath(
		a.ctx,
		wavPath,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("file upload failed: %w", err)
	}

	parts := []*genai.Part{
		genai.NewPartFromText(config.C.AI.TranscriptionPrompt),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	// Use the non-streaming version for a simple transcription task.
	resp, err := a.client.Models.GenerateContent(
		a.ctx,
		config.C.AI.Model,
		contents,
		nil, // No special generation config needed for transcription
	)
	if err != nil {
		return "", err
	}
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return "", fmt.Errorf("invalid response from transcription API")
	}

	// The Text() helper method safely concatenates text from all parts of the response.
	return resp.Text(), nil
}

func (a *AI) Output(resp iter.Seq2[*genai.GenerateContentResponse, error], fVoice bool) (string, error) {
	// State flags to print prefixes only once per block and track formatting.
	var thoughtStarted, answerStarted bool
	var fullResponseText string // To accumulate the full text for history and a single TTS call

	// sources will store unique source URIs and their titles.
	sources := make(map[string]string)

	formatter := display.NewFormatter()

	for chunk, err := range resp {
		if err != nil {
			// On error, ensure we reset color and print a newline.
			formatter.Reset()
			return "", err
		}
		if chunk == nil || len(chunk.Candidates) == 0 {
			continue
		}

		candidate := chunk.Candidates[0]

		// Source attribution is found in the CitationMetadata.
		if candidate.GroundingMetadata != nil {
			for _, chunk := range candidate.GroundingMetadata.GroundingChunks {
				// Perform nil checks for safety
				sources[chunk.Web.URI] = chunk.Web.Title
			}
		}

		if candidate.Content == nil {
			continue
		}

		for _, part := range candidate.Content.Parts {
			if part.Thought && part.Text != "" {
				if !thoughtStarted {
					formatter.Println("Thought:", display.ColorYellow)
					thoughtStarted, answerStarted = true, false // Reset answer flag
				}
				formatter.Print(part.Text)
			} else if part.Text != "" {
				// By checking for non-empty text, we correctly filter out the intermediate
				// tool-use parts and only process the final text answer from the model.
				if !answerStarted {
					if fVoice {
						formatter.Println("Voice answer:", display.ColorCyan)
					} else {
						formatter.Println("Answer:", display.ColorCyan)
					}
					answerStarted, thoughtStarted = true, false
				}

				formatter.Print(part.Text)

				fullResponseText += part.Text
			}
		}
	}
	// After the stream is finished, reset the color and print a final newline.
	formatter.Reset()

	// If any sources were found during the tool-use, print them.
	if len(sources) > 0 {
		formatter.Println("Sources:", display.ColorYellow)
		for uri, title := range sources {
			if title != "" {
				formatter.Println(title, display.ColorCyan)
				formatter.Println(uri, display.ColorBlue)
			} else {
				formatter.Println(uri, display.ColorBlue)
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

	genConfig := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{
			IncludeThoughts: config.C.AI.Thoughts,
			ThinkingBudget:  &config.C.AI.Thinking,
		},
	}

	// Conditionally enable tools based on the configuration.
	// This is only done for the main response generation, not transcription.
	if config.C.AI.EnableTools {
		log.Println("Tool use is enabled for this request.")
		genConfig.Tools = []*genai.Tool{{
			GoogleSearch: &genai.GoogleSearch{},
		}}
	}

	resp := a.client.Models.GenerateContentStream(
		a.ctx,
		config.C.AI.Model,
		contents,
		genConfig,
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
