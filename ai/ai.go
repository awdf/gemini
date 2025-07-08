package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"capgemini.com/audio"
	"capgemini.com/config"
	"capgemini.com/helpers"
	"capgemini.com/inout"
	"capgemini.com/pipeline"
	"github.com/asaskevich/EventBus"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
)

// Flags holds the command-line flags that control AI behavior.
type Flags struct {
	Voice      bool // Corresponds to the -voice flag
	Transcript bool // Corresponds to the -ts flag
	Enabled    bool // Corresponds to the -no-ai flag (negated)
}

// AI encapsulates the state and logic for interacting with the Gemini AI.
type AI struct {
	ctx                 context.Context
	client              *genai.Client
	conversationHistory []*genai.Content
	wg                  *sync.WaitGroup
	pipeline            *pipeline.VadPipeline
	flags               *Flags
	fileChan            <-chan string
	textCmdChan         <-chan string
	initialFileParts    []*genai.Part
	cache               *genai.CachedContent
	formatter           *inout.Formatter
	bus                 *EventBus.Bus
}

const (
	youtubeURL1 = "youtube.com"
	youtubeURL2 = "youtu.be"
)

// Flags for Url Context disabled trigger.
const (
	fURLContextDisabled = true
	fURLContextEnabled  = false
)

func init() {
	// Register custom MIME types to ensure correct handling by the AI.
	// This is the ideal place for package-specific, one-time initializations.
	mime.AddExtensionType(".md", "text/markdown")
	mime.AddExtensionType(".wav", "audio/wav")
}

// NewAI creates a new AI instance, initializing the client and conversation history.
func NewAI(wg *sync.WaitGroup, pipeline *pipeline.VadPipeline, flags *Flags, fileChan <-chan string, textCmdChan <-chan string, bus *EventBus.Bus) *AI {
	ctx := context.Background()
	client := helpers.Control(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	}))

	ai := &AI{
		ctx:              ctx,
		client:           client,
		wg:               wg,
		pipeline:         pipeline,
		flags:            flags,
		fileChan:         fileChan,
		textCmdChan:      textCmdChan,
		initialFileParts: nil,
		cache:            nil,
		formatter:        inout.NewFormatter(),
		bus:              bus,
	}

	if config.C.AI.EnableCache {
		ai.uploadCache()
	} else {
		log.Println("AI Caching is disabled. Preparing files to be included in the first message.")
		ai.prepareInitialFiles()
	}

	return ai
}

// Run is the main loop for the AI component. It listens for completed audio files,
// sends them for processing, and handles the response.
func (a *AI) Run() {
	defer a.wg.Done()

	if !a.flags.Enabled {
		log.Println("AI Chat processor is disabled. Draining channels to prevent blocking.")
		// We must still consume from the channels to prevent other goroutines from blocking.
		for a.fileChan != nil || a.textCmdChan != nil {
			select {
			case file, ok := <-a.fileChan:
				if !ok {
					a.fileChan = nil
					continue
				}
				if config.C.Debug {
					log.Printf("AI disabled, discarding file: %s", file)
				}
			case cmd, ok := <-a.textCmdChan:
				if !ok {
					a.textCmdChan = nil
					continue
				}
				if config.C.Debug {
					log.Printf("AI disabled, discarding command: %s", cmd)
				}
				//In case of AI disabled we support CLI and draw it
				(*a.bus).Publish("main:topic", "draw:ai.run")
			}
		}
		log.Println("AI Chat work finished (disabled).")
		return
	}

	for a.fileChan != nil || a.textCmdChan != nil {
		select {
		case file, ok := <-a.fileChan:
			if !ok {
				a.fileChan = nil // Mark as closed
				continue
			}
			log.Printf("Chat: Processing %s", file)
			a.withPipelinePausedIfVoice(a.pipeline, a.flags.Voice, func() {
				action := func() error {
					if a.flags.Transcript {
						return a.VoiceQuestionWithTranscript(file, config.C.AI.MainPrompt, a.flags.Voice)
					}
					return a.VoiceQuestion(file, config.C.AI.MainPrompt, a.flags.Voice)
				}
				err := a.retryWithBackoff(action)
				if err != nil {
					log.Printf("ERROR: AI processing failed for %s after all retries, leaving file for manual processing: %v", file, err)
				} else {
					log.Printf("Chat: Successfully processed %s. Removing file.", file)
					os.Remove(file)
				}
			})
		case cmd, ok := <-a.textCmdChan:
			if !ok {
				a.textCmdChan = nil // Mark as closed
				continue
			}
			log.Println("Chat: Processing text prompt...")
			a.withPipelinePausedIfVoice(a.pipeline, a.flags.Voice, func() {
				action := func() error {
					return a.TextQuestion(cmd, a.flags.Voice)
				}
				err := a.retryWithBackoff(action)
				if err != nil {
					log.Printf("ERROR: AI processing failed for text command after all retries: %v", err)
				}
			})
		}
	}

	log.Println("AI Chat work finished")
}

// retryWithBackoff wraps an action with a retry mechanism, using exponential backoff for specific, transient errors.
func (a *AI) retryWithBackoff(action func() error) error {
	maxRetries := config.C.AI.Retry.MaxRetries
	initialDelay := time.Duration(config.C.AI.Retry.InitialDelayMs) * time.Millisecond
	maxDelay := time.Duration(config.C.AI.Retry.MaxDelayMs) * time.Millisecond

	var lastErr error
	delay := initialDelay

	for i := 0; i <= maxRetries; i++ { // Total attempts = 1 (initial) + maxRetries
		lastErr = action()
		if lastErr == nil {
			return nil // Success
		}

		// Stop if this was the last attempt.
		if i == maxRetries {
			break
		}

		// Check if the error is a googleapi.Error and if it's a retryable status code.
		var gerr *googleapi.Error
		if errors.As(lastErr, &gerr) {
			// Retry on 500 (Internal Server Error), 503 (Service Unavailable), and 429 (Resource Exhausted/Rate Limiting).
			if gerr.Code == 500 || gerr.Code == 503 || gerr.Code == 429 {
				log.Printf("Retryable error detected (code %d). Retrying in %v... (Retry %d of %d)", gerr.Code, delay, i+1, maxRetries)
				time.Sleep(delay)
				// Exponential backoff
				delay *= 2
				if delay > maxDelay {
					delay = maxDelay
				}
				continue // Try again
			}
		}

		// If the error is not nil and not a retryable API error, return it immediately.
		return lastErr
	}

	return fmt.Errorf("failed after %d retries: %w", maxRetries, lastErr)
}

func (a *AI) TextQuestion(prompt string, fVoice bool) error {
	// Check prompt on any type of URLs it can consist.
	// The Gemini API treats special URLs as direct content, not as pages to browse.
	parts, urlContextDisabled, err := a.parsePromptForMultimedia(prompt)
	if err != nil {
		log.Printf("ERROR: could not parse prompt for multimedia: %v", err)
		return err // Stop processing if we can't even parse the prompt.
	}

	// Text models are able to use the URLContext tool to parse and understend web content.
	// In case of exception urlContextDisabled is true, means a special URL was found.
	// special URL: file, YouTube
	if err := a.generateAndProcessContent(parts, fVoice, urlContextDisabled); err != nil {
		log.Printf("ERROR: processing text question: %v", err)
		return err
	}
	return nil
}

// It's call of voice chat with tools enabled according to config
// Will make one call for transcript and chat
func (a *AI) VoiceQuestion(wavPath string, prompt string, fVoice bool) error {
	uploadedFile, err := a.client.Files.UploadFromPath(
		a.ctx,
		wavPath,
		&genai.UploadFileConfig{MIMEType: "audio/wav"},
	)
	if err != nil {
		return fmt.Errorf("file upload failed: %w", err)
	}

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	return a.generateAndProcessContent(parts, fVoice, fURLContextDisabled)
}

// It's forced call of Voice chat with tool enabled ignoring config.
// Will make 2 requests: one for transcript and second for text chat with transcript
func (a *AI) VoiceQuestionWithTranscript(wavPath string, prompt string, fVoice bool) error {
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

	//IMPORTANT: Never participate in the voice answer.
	//Output method is not acceptable here
	formatter := inout.NewFormatter()
	formatter.Println("Transcript:\n", inout.ColorCyan)
	formatter.Print(transcript)
	fmt.Println()

	// Step 2: Use the transcript to ask the main question. This call CAN use tools.
	log.Println("Request 2: Generating response from transcript...")
	// Combine the main prompt with the transcript.
	fullPrompt := fmt.Sprintf("%s\n\nTranscript: \"%s\"", prompt, transcript)
	parts := []*genai.Part{
		genai.NewPartFromText(fullPrompt),
	}

	// The second step is a pure text-based query, so we can reuse the history logic.
	return a.generateAndProcessContent(parts, fVoice, fURLContextEnabled)
}

// generateTranscript performs a dedicated API call to get a transcript from an audio file.
// It does not use tools or conversation history to keep the request clean and focused.
func (a *AI) generateTranscript(wavPath string) (string, error) {
	uploadedFile, err := a.client.Files.UploadFromPath(
		a.ctx,
		wavPath,
		&genai.UploadFileConfig{MIMEType: "audio/wav"},
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

func (a *AI) Output(resp iter.Seq2[*genai.GenerateContentResponse, error], fVoice bool, duration time.Duration) (string, error) {
	// State flags to print prefixes only once per block and track formatting.
	var thoughtStarted, answerStarted bool
	var fullResponseText string // To accumulate the full text for history and a single TTS call

	//Stop other output
	(*a.bus).Publish("main:topic", "mute:ai.output")

	// sources will store unique source URIs and their titles.
	sources := make(map[string]string)

	for chunk, err := range resp {
		if err != nil {
			// On error, ensure we reset color and print a newline.
			a.formatter.Reset()
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
					a.formatter.Println("Thought:", inout.ColorYellow)
					thoughtStarted, answerStarted = true, false // Reset answer flag
				}
				a.formatter.Print(part.Text)
			} else if part.Text != "" {
				// By checking for non-empty text, we correctly filter out the intermediate
				// tool-use parts and only process the final text answer from the model.
				if !answerStarted {
					fmt.Println()
					if fVoice {
						a.formatter.Println("Voice answer:\n", inout.ColorCyan)
					} else {
						a.formatter.Println("Answer:\n", inout.ColorCyan)
					}
					answerStarted, thoughtStarted = true, false
				}

				a.formatter.Print(part.Text)

				fullResponseText += part.Text
			}
		}
	}
	// After the stream is finished, reset the color and print a final newline.
	a.formatter.Reset()
	// If any sources were found during the tool-use, print them.
	if len(sources) > 0 {
		a.formatter.Println("Sources:", inout.ColorYellow)
		for uri, title := range sources {
			if title != "" {
				a.formatter.Println(title, inout.ColorCyan)
				a.formatter.Println(uri, inout.ColorBlue)
			} else {
				a.formatter.Println(uri, inout.ColorBlue)
			}
		}
	}

	// Print execution time metric
	a.formatter.Println(fmt.Sprintf("Request execution time: %.2fs\n", duration.Seconds()), inout.ColorGray)

	//Restore other output
	(*a.bus).Publish("main:topic", "draw:ai.output")

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

// withPipelinePausedIfVoice pauses and resumes the pipeline if voice is enabled,
// executing the provided action in between. It uses a defer to ensure the pipeline
// is always resumed.
func (a *AI) withPipelinePausedIfVoice(p *pipeline.VadPipeline, fVoice bool, action func()) {
	if !fVoice {
		action()
		return
	}

	log.Println("Pausing listening pipeline for AI response...")
	p.Pause()

	defer func() {
		log.Println("Resuming listening pipeline...")
		p.Play()
	}()

	action()
}

// generateAndProcessContent is a universal method to generate content from a set of parts,
// process the streamed response, and update the conversation history.
func (a *AI) generateAndProcessContent(parts []*genai.Part, fVoice bool, urlContextDisabled bool) error {
	startTime := time.Now()

	// Check if we have initial files to prepend
	if len(a.initialFileParts) > 0 {
		log.Println("Prepending initial files to the first message.")
		// Prepend the initial file parts to the current message parts
		parts = append(a.initialFileParts, parts...)
		// Clear the initial parts so they are only sent once
		a.initialFileParts = nil
	}
	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	contents := append(a.conversationHistory, userContent)

	genConfig := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{
			IncludeThoughts: config.C.AI.Thoughts,
			ThinkingBudget:  &config.C.AI.Thinking,
		},
	}

	if a.cache != nil {
		genConfig.CachedContent = a.cache.Name
		log.Println("Using cached content for this request.")
	}

	// Construct the system prompt with the current date and time.
	systemPrompt := config.C.AI.SystemPrompt
	if systemPrompt != "" {
		currentTime := time.Now().Format(time.RFC1123)
		systemPrompt = fmt.Sprintf("Current date and time is %s. %s", currentTime, systemPrompt)
		// The role for a system instruction is empty.
		genConfig.SystemInstruction = genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(systemPrompt)}, "")
	}

	// Conditionally enable tools based on the configuration.
	// This is only done for the main response generation, not transcription.
	if config.C.AI.EnableTools {
		log.Println("Tool use is enabled for this request.")
		if urlContextDisabled {
			log.Println("Google search tool in use")
			genConfig.Tools = []*genai.Tool{{
				GoogleSearch: &genai.GoogleSearch{},
			}}
		} else {
			log.Println("URLContext and Google search tools in use")
			genConfig.Tools = []*genai.Tool{{
				GoogleSearch: &genai.GoogleSearch{},
				URLContext:   &genai.URLContext{},
			}}
		}
	}

	resp := a.client.Models.GenerateContentStream(
		a.ctx,
		config.C.AI.Model,
		contents,
		genConfig,
	)
	duration := time.Since(startTime)

	fullResponse, err := a.Output(resp, fVoice, duration)
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
	log.Println("Generating audio response...")
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

	log.Println("Audio response generated. Play audio...")
	if len(audioData) > 0 {
		// Use the centralized audio playback function.
		return audio.PlayRawPCM(audioData, audio.TTS_SAMPLE_RATE, audio.TTS_CHANNELS)
	}

	return fmt.Errorf("no audio data received from API")
}

// prepareInitialFiles reads files from the cache directory and prepares them to be
// included as context in the first message when caching is disabled.
func (a *AI) prepareInitialFiles() {
	cacheDir := config.C.AI.CacheDir
	if cacheDir == "" {
		return // Silently return if no dir is configured
	}

	files, err := os.ReadDir(cacheDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("ERROR: failed to read cache directory %s: %v", cacheDir, err)
		}
		return
	}

	var filesToInclude []fs.DirEntry
	for _, file := range files {
		if !file.IsDir() && file.Name() != ".gitkeep" {
			filesToInclude = append(filesToInclude, file)
		}
	}

	if len(filesToInclude) == 0 {
		return
	}

	log.Printf("Found %d files to include in the first message.", len(filesToInclude))

	var parts []*genai.Part
	// Add the system prompt for the files first.
	if config.C.AI.CacheSystemPrompt != "" {
		parts = append(parts, genai.NewPartFromText(config.C.AI.CacheSystemPrompt))
	}

	for _, file := range filesToInclude {
		localPath := filepath.Join(cacheDir, file.Name())
		part, err := a.createPartFromFile(localPath)
		if err != nil {
			log.Printf("ERROR: could not prepare file %s for history: %v", localPath, err)
			continue
		}
		parts = append(parts, part)
	}
	a.initialFileParts = parts
}

// uploadCache finds all files in the configured cache directory, uploads them,
// and creates a single cache for the model to use in subsequent conversations.
func (a *AI) uploadCache() {
	cacheDir := config.C.AI.CacheDir
	if cacheDir == "" {
		log.Println("AI.CacheDir is not configured, skipping cache upload.")
		return
	}

	log.Printf("Checking for files in cache directory: %s", cacheDir)

	// Check if cache directory exists.
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		log.Printf("Cache directory '%s' not found, skipping cache upload.", cacheDir)
		return
	}

	files, err := os.ReadDir(cacheDir)
	if err != nil {
		log.Printf("ERROR: failed to read cache directory %s: %v", cacheDir, err)
		return
	}

	var filesToCache []fs.DirEntry
	for _, file := range files {
		if !file.IsDir() && file.Name() != ".gitkeep" {
			filesToCache = append(filesToCache, file)
		}
	}

	if len(filesToCache) == 0 {
		log.Println("No files to cache found in cache directory.")
		return
	}

	log.Printf("Found %d files to upload to model cache.", len(filesToCache))

	var cacheContents []*genai.Content
	for _, file := range filesToCache {
		localPath := filepath.Join(cacheDir, file.Name())
		content, err := a.createContentFromFile(localPath)
		if err != nil {
			log.Printf("ERROR: could not create content for %s: %v", localPath, err)
			continue // Skip this file and try the next
		}
		cacheContents = append(cacheContents, content)
	}

	if len(cacheContents) > 0 {
		log.Println("Creating a single cache for all provided files...")
		if err := a.createAndStoreCache(cacheContents); err != nil {
			log.Printf("ERROR: Failed to create and store the model cache: %v", err)
		}
	}
}

// createPartFromFile uploads a single file and returns a *genai.Part object.
func (a *AI) createPartFromFile(localPath string) (*genai.Part, error) {
	mimeType := mime.TypeByExtension(filepath.Ext(localPath))
	if mimeType == "" {
		mimeType = "application/octet-stream" // Fallback
	}

	// For text-based files, it's often more robust to send them as raw text
	// rather than uploading them and using a file URI. This avoids potential
	// conflicts when mixing file types in a single prompt (e.g., audio + other files).
	if strings.HasPrefix(mimeType, "text/") {
		log.Printf("Reading text-based file %s as a text part...", localPath)
		data, err := os.ReadFile(localPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read text file %s: %w", localPath, err)
		}
		return genai.NewPartFromText(string(data)), nil
	}

	log.Printf("Uploading %s with MIME type %s...", localPath, mimeType)
	document, err := a.client.Files.UploadFromPath(
		a.ctx,
		localPath,
		&genai.UploadFileConfig{MIMEType: mimeType},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to upload file %s: %w", localPath, err)
	}
	return genai.NewPartFromURI(document.URI, document.MIMEType), nil
}

// createContentFromFile uploads a single file and wraps it in a *genai.Content object.
func (a *AI) createContentFromFile(localPath string) (*genai.Content, error) {
	part, err := a.createPartFromFile(localPath)
	if err != nil {
		return nil, err
	}
	parts := []*genai.Part{part}
	// The role for file data is 'user'
	return genai.NewContentFromParts(parts, genai.RoleUser), nil
}

// createAndStoreCache takes a list of contents and creates a single cache object,
// storing it in the AI struct for future use.
func (a *AI) createAndStoreCache(contents []*genai.Content) error {
	createConfig := &genai.CreateCachedContentConfig{
		Contents: contents,
	}

	if config.C.AI.CacheSystemPrompt != "" {
		// The role for a system instruction must be empty.
		createConfig.SystemInstruction = genai.NewContentFromParts(
			[]*genai.Part{genai.NewPartFromText(config.C.AI.CacheSystemPrompt)},
			"",
		)
	}

	cache, err := a.client.Caches.Create(
		a.ctx,
		config.C.AI.Model,
		createConfig,
	)
	if err != nil {
		return fmt.Errorf("cache creation failed: %w", err)
	}

	a.cache = cache
	log.Printf("Successfully created and stored cache: %s", cache.Name)
	return nil
}

// getContentTypeFromURL sends a HEAD request to determine the content type of a URL.
func getContentTypeFromURL(urlStr string) (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second, // Avoid waiting too long
	}
	resp, err := client.Head(urlStr)
	if err != nil {
		return "", fmt.Errorf("HEAD request failed for %s: %w", urlStr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status for %s: %s", urlStr, resp.Status)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		return "", fmt.Errorf("no Content-Type header found for %s", urlStr)
	}

	return contentType, nil
}

// createPartFromURL downloads content from a URL and returns a *genai.Part.
func (a *AI) createPartFromURL(urlStr, mimeType string) (*genai.Part, error) {
	resp, err := http.Get(urlStr)
	if err != nil {
		return nil, fmt.Errorf("failed to GET %s: %w", urlStr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status for %s: %s", urlStr, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body from %s: %w", urlStr, err)
	}

	// The Gemini API can often infer the MIME type from the bytes, but providing it
	// is more explicit and reliable.
	return genai.NewPartFromBytes(data, mimeType), nil
}

// parsePromptForMultimedia splits a prompt into text, YouTube URLs, and local file parts.
// It returns the parts, a boolean indicating if a non-web URL (video, file) was found,
// and an error if file processing fails.
func (a *AI) parsePromptForMultimedia(prompt string) ([]*genai.Part, bool, error) {
	var parts []*genai.Part
	var textBuilder strings.Builder
	multimediaFound := false

	var wordAppend = func(word *string) {
		if textBuilder.Len() > 0 {
			textBuilder.WriteString(" ")
		}
		textBuilder.WriteString(*word)
	}

	// Split prompt into words to find URLs.
	for _, word := range strings.Fields(prompt) {
		// Trim trailing punctuation that might be attached to a URL
		trimmedWord := strings.TrimRight(word, ".,;:!?")
		u, err := url.Parse(trimmedWord)
		// We need a scheme to identify it as a URL we can handle.
		if err != nil || u.Scheme == "" {
			wordAppend(&word)
			continue
		}

		switch u.Scheme {
		case "http", "https":
			// For all web URLs, first perform a HEAD request to ensure they are reachable.
			contentType, err := getContentTypeFromURL(u.String())
			if err != nil {
				// The URL is unreachable or invalid. Inform the user and treat it as plain text.
				// This provides immediate feedback and clarifies that the AI won't be able to browse the link.
				log.Printf("WARNING: URL is unreachable: %s. Treating as plain text. Error: %v", u.String(), err)
				fmt.Printf("Warning: URL is unreachable and will be treated as plain text: %s\n", u.String())
				wordAppend(&word)
				continue
			}

			// Now that we know the URL is reachable, decide how to handle it.
			if strings.Contains(u.Host, youtubeURL1) || strings.Contains(u.Host, youtubeURL2) {
				log.Printf("Detected YouTube URL in prompt: %s", u.String())
				parts = append(parts, genai.NewPartFromURI(u.String(), "video/*"))
				multimediaFound = true
			} else {
				// It's a regular web URL. Check if it's a direct link to a document we can upload.
				baseMimeType, _, _ := mime.ParseMediaType(contentType)
				if strings.HasPrefix(baseMimeType, "image/") || strings.HasPrefix(baseMimeType, "video/") || baseMimeType == "application/pdf" {
					log.Printf("Detected document URL (%s): %s. Downloading content...", contentType, u.String())
					part, err := a.createPartFromURL(u.String(), contentType)
					if err != nil {
						log.Printf("Failed to download content from %s, treating as text. Error: %v", u.String(), err)
						wordAppend(&word)
					} else {
						parts = append(parts, part)
						multimediaFound = true
					}
				} else {
					// Not a direct document link (e.g., text/html), treat as text for the URLContext tool.
					wordAppend(&word)
				}
			}
		case "file":
			log.Printf("Detected local file URL in prompt: %s", u.String())
			part, err := a.createPartFromFile(u.Path)
			if err != nil {
				return nil, false, fmt.Errorf("failed to process file URL %s: %w", u.String(), err)
			}
			parts = append(parts, part)
			multimediaFound = true
		default:
			// Unknown scheme, treat as text.
			wordAppend(&word)
		}
	}

	// Prepend the collected text as the first part, if any.
	if textBuilder.Len() > 0 {
		parts = append([]*genai.Part{genai.NewPartFromText(textBuilder.String())}, parts...)
	} else if len(parts) == 0 {
		// If there's no text and no multimedia parts, it was an empty prompt.
		// Create a single empty text part to maintain behavior.
		parts = append(parts, genai.NewPartFromText(""))
	}

	return parts, multimediaFound, nil
}
