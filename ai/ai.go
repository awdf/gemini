package ai

import (
	"bufio"
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

	"github.com/asaskevich/EventBus"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"

	"gemini/ai/agents"
	"gemini/audio"
	"gemini/config"
	"gemini/desktop"
	"gemini/helpers"
	"gemini/images"
	"gemini/inout"
	"gemini/pipeline"
	"gemini/shell"
)

// Flags holds the command-line flags that control AI behavior.
type Flags struct {
	// Enabled is the master switch for the AI component. Other toggles are in config.
	Enabled bool // Corresponds to the -no-ai flag (negated)
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
	cache               *genai.CachedContent
	formatter           *inout.Formatter
	bus                 *EventBus.Bus
	toolset             *genai.Tool
	agents              map[string]agents.Callable
	initialContextAdded bool
	shellExecutor       *shell.Executor
	cli                 *inout.CLI
	mode                string
}

const (
	youtubeURL1 = "youtube.com"
	youtubeURL2 = "youtu.be"
)

// Flags for Url Context disabled trigger.
const (
	fURLContextDisabled = true
	fURLContextEnabled  = false
	fNoVoicePrompt      = false
	fVoicePrompt        = true
)

const CheckQuestion = "What is time and date now?"

func init() {
	// Register custom MIME types to ensure correct handling by the AI.
	// This is the ideal place for package-specific, one-time initializations.
	helpers.Verify(mime.AddExtensionType(".md", "text/markdown"))
	helpers.Verify(mime.AddExtensionType(".wav", config.MIMEAudio))
}

// NewAI creates a new AI instance, initializing the client and conversation history.
func NewAI(
	wg *sync.WaitGroup,
	pipeline *pipeline.VadPipeline,
	flags *Flags,
	fileChan <-chan string,
	textCmdChan <-chan string,
	bus *EventBus.Bus,
	shellExecutor *shell.Executor,
	cli *inout.CLI,
) *AI {
	ctx := context.Background()
	client := helpers.Check(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	}))

	// --- Agent Initialization ---
	toolset := agents.NewToolSet()
	if config.C.AI.EnableTools && config.C.AI.EnableFunctionCalling {
		agents.BuildAgentNetwork(ctx, client, toolset, bus)
	}

	ai := &AI{
		ctx:                 ctx,
		client:              client,
		wg:                  wg,
		pipeline:            pipeline,
		flags:               flags,
		fileChan:            fileChan,
		textCmdChan:         textCmdChan,
		cache:               nil,
		formatter:           inout.NewFormatter(),
		bus:                 bus,
		initialContextAdded: false,
		mode:                config.C.Mode,
		toolset:             toolset,
		agents:              agents.AgentRegistry,
		shellExecutor:       shellExecutor,
		cli:                 cli,
	}

	if config.C.AI.EnableFunctionCalling && config.C.AI.WorkspaceDir != "" {
		// This log confirms that PostAI mode is aware of the workspace for file system tools.
		log.Printf("AI function calling is enabled. Workspace is set to: %s", config.C.AI.WorkspaceDir)
	}

	if config.C.AI.EnableCache {
		ai.uploadCache()
	} else {
		log.Println("AI Caching is disabled. Preparing files to be included as initial context.")
		ai.prepareInitialFiles()
	}

	return ai
}

// Run is the main loop for the AI component. It listens for completed audio files,
// sends them for processing, and handles the response.
func (a *AI) Run() {
	defer a.wg.Done()

	// helpers.Verify(a.Livestream("Hi! It's my first message"))

	if !a.flags.Enabled {
		// Blocking call
		a.passiveRun()
		return
	}

	helpers.Verify((*a.bus).Subscribe(config.AITopic, a.handleEvents))

	for a.fileChan != nil || a.textCmdChan != nil {
		select {
		case file, ok := <-a.fileChan:
			if !ok {
				a.fileChan = nil // Mark as closed
				continue
			}
			log.Printf("Chat: Processing %s", file)
			a.withPipelinePausedIfVoice(a.pipeline, func() {
				a.withScreenshotIfImageMode(func(buffer *images.ScreenshotBuffer) {
					// The buffer will be non-nil only in ImageMode, otherwise it's nil.
					// This simplifies the logic by removing the need for an inner switch.
					action := func() error {
						if config.C.AI.Transcript {
							return a.VoiceQuestionWithTranscript(file, buffer, config.C.AI.VoicePrompt)
						}
						return a.VoiceQuestion(file, buffer, config.C.AI.VoicePrompt)
					}
					err := a.retryWithBackoff(action)
					if err != nil {
						log.Printf("ERROR: AI processing failed for %s after all retries, leaving file for manual processing: %v", file, err)
					} else {
						log.Printf("Chat: Successfully processed %s. Removing file.", file)
						os.Remove(file)
					}
				})
			})
		case cmd, ok := <-a.textCmdChan:
			if !ok {
				a.textCmdChan = nil // Mark as closed
				continue
			}
			log.Printf("Chat: Processing text prompt in %s mode...\n", a.mode)
			a.withPipelinePausedIfVoice(a.pipeline, func() {
				a.withScreenshotIfImageMode(func(buffer *images.ScreenshotBuffer) {
					action := func() error {
						return a.TextQuestion(cmd, buffer)
					}

					err := a.retryWithBackoff(action)
					if err != nil {
						log.Printf("ERROR: AI processing failed for text command after all retries: %v", err)
					}
				})
			})
		}
	}

	log.Println("AI Chat work finished")
}

func (a *AI) passiveRun() {
	log.Println("AI Chat processor is disabled. Draining channels to prevent blocking.")
	// We must still consume from the channels to prevent other goroutines from blocking.
	for a.fileChan != nil || a.textCmdChan != nil {
		select {
		case file, ok := <-a.fileChan:
			if !ok {
				a.fileChan = nil
				continue
			}
			config.DebugPrintf("AI disabled, discarding file: %s", file)
		case cmd, ok := <-a.textCmdChan:
			if !ok {
				a.textCmdChan = nil
				continue
			}

			config.DebugPrintf("AI disabled, discarding command: %s", cmd)
		}
	}
	log.Println("AI Chat work finished (disabled).")
}

// handleEvents processes commands sent to the AI component via the event bus.
func (a *AI) handleEvents(event string) {
	config.DebugPrintf("AI component received event: %s\n", event)
	parts := strings.SplitN(event, ":", 2)
	if len(parts) < 2 {
		log.Printf("WARNING: received malformed AI event: %s", event)
		return
	}
	command, payload := parts[0], parts[1]

	switch command {
	case "save":
		if err := a.saveConversationHistory(payload); err != nil {
			log.Printf("ERROR: failed to save conversation history to %s: %v", payload, err)
		}
	case "mode":
		a.mode = payload
	default:
		log.Printf("WARNING: received unknown AI event: %s", event)
	}
}

// saveConversationHistory writes the current conversation to a text file.
func (a *AI) saveConversationHistory(filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create history file: %w", err)
	}
	defer f.Close()

	writer := bufio.NewWriter(f)

	historyToSave := a.conversationHistory
	// If initial context was added, the first two entries are the file context
	// and the canned response. We skip them for a cleaner history file.
	if a.initialContextAdded && len(historyToSave) >= 2 {
		historyToSave = historyToSave[2:]
	}

	for _, content := range historyToSave {
		role := content.Role
		if role == "" {
			role = "system" // System instructions have an empty role.
		}

		// Concatenate text from all parts of the content.
		var textBuilder strings.Builder
		for _, part := range content.Parts {
			// In older library versions, Part is a struct. Access the Text field directly.
			if part != nil && part.Text != "" {
				textBuilder.WriteString(part.Text)
			}
		}
		if _, err := writer.WriteString(fmt.Sprintf("--- %s ---\n%s\n\n", strings.ToUpper(role), textBuilder.String())); err != nil {
			return err
		}
	}

	if err := writer.Flush(); err != nil {
		return err
	}
	log.Printf("Conversation history successfully saved to %s", filename)
	return nil
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

func (a *AI) TextQuestion(prompt string, imageBuffer *images.ScreenshotBuffer) error { // Check prompt on any type of URLs it can consist.
	// The Gemini API treats special URLs as direct content, not as pages to browse.
	// Check prompt on any type of URLs it can consist.
	// The Gemini API treats special URLs as direct content, not as pages to browse.
	parts, urlContextDisabled, err := a.parsePromptForMultimedia(prompt)
	if err != nil {
		log.Printf("ERROR: could not parse prompt for multimedia: %v", err)
		return err // Stop processing if we can't even parse the prompt.
	}

	if imageBuffer != nil {
		parts = append(parts, genai.NewPartFromBytes(imageBuffer.Bytes(), config.MIMEImage))
	}

	// Text models are able to use the URLContext tool to parse and understend web content.
	// In case of exception urlContextDisabled is true, means a special URL was found.
	// special URL: file, YouTube
	if err := a.generateAndProcessContent(parts, urlContextDisabled, fNoVoicePrompt); err != nil {
		log.Printf("ERROR: processing text question: %v", err) // The Gemini API treats special URLs as direct content, not as pages to browse.
		return err
	}
	return nil
}

// It's call of voice chat with tools enabled according to config
// Will make one call for transcript and chat
func (a *AI) VoiceQuestion(
	wavPath string,
	imageBuffer *images.ScreenshotBuffer,
	prompt string,
) error {
	uploadedFile, err := a.client.Files.UploadFromPath(
		a.ctx,
		wavPath,
		&genai.UploadFileConfig{MIMEType: config.MIMEAudio},
	)
	if err != nil {
		return fmt.Errorf("file upload failed: %w", err)
	}

	parts := []*genai.Part{
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
		genai.NewPartFromText(prompt),
	}

	if imageBuffer != nil { // Fix: Use append correctly to modify the slice
		parts = append(parts, genai.NewPartFromBytes(imageBuffer.Bytes(), config.MIMEImage))
	}
	return a.generateAndProcessContent(parts, fURLContextDisabled, fVoicePrompt)
}

// It's forced call of Voice chat with tool enabled ignoring config.
// Will make 2 requests: one for transcript and second for text chat with transcript
func (a *AI) VoiceQuestionWithTranscript(
	wavPath string,
	imageBuffer *images.ScreenshotBuffer,
	prompt string,
) error {
	save := config.C.AI.EnableTools
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

	// IMPORTANT: Never participate in the voice answer.
	// Output method is not acceptable here
	formatter := inout.NewFormatter()
	formatter.Println("Transcript:\n", inout.ColorDarkCyan)
	formatter.Print(transcript)
	fmt.Println()

	// Step 2: Use the transcript to ask the main question. This call CAN use tools.
	log.Println("Request 2: Generating response from transcript...")
	// Combine the main prompt with the transcript.
	fullPrompt := fmt.Sprintf("%s\n\nTranscript: \"%s\"", prompt, transcript)
	parts := []*genai.Part{
		genai.NewPartFromText(fullPrompt),
	}
	if imageBuffer != nil { // Fix: Use append correctly to modify the slice
		parts = append(parts, genai.NewPartFromBytes(imageBuffer.Bytes(), config.MIMEImage))
	}
	// The second step is a pure text-based query, so we can reuse the history logic.
	return a.generateAndProcessContent(parts, fURLContextEnabled, fNoVoicePrompt)
}

// generateTranscript performs a dedicated API call to get a transcript from an audio file.
// It does not use tools or conversation history to keep the request clean and focused.
func (a *AI) generateTranscript(wavPath string) (string, error) {
	uploadedFile, err := a.client.Files.UploadFromPath(
		a.ctx,
		wavPath,
		&genai.UploadFileConfig{MIMEType: config.MIMEAudio},
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

func (a *AI) Output(resp iter.Seq2[*genai.GenerateContentResponse, error], duration time.Duration) (string, []*genai.FunctionCall, error) {
	// State flags to print prefixes only once per block and track formatting.
	var thoughtStarted, answerStarted bool
	var fullResponseText string // To accumulate the full text for history and a single TTS call
	var functionCalls []*genai.FunctionCall

	// Stop other output
	(*a.bus).Publish(config.MainTopic, "mute:ai.output")

	// sources will store unique source URIs and their titles.
	sources := make(map[string]string)

	for chunk, err := range resp {
		if err != nil {
			// On error, ensure we reset color and print a newline.
			a.formatter.Reset() // On error, ensure we reset color and print a newline.
			return "", nil, err
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
			if part == nil {
				continue
			}

			// A part represents a "thought" if it's a function call.
			// This logic is based on older, struct-based genai.Part.
			if fc := part.FunctionCall; fc != nil {
				if !thoughtStarted {
					a.formatter.Println("Thought:", inout.ColorDarkYellow)
					thoughtStarted, answerStarted = true, false // Reset answer flag
				}
				// We can format the function call to be readable.
				thoughtText := fmt.Sprintf("TOOL CALL: %s(%v)\n", fc.Name, fc.Args)
				a.formatter.Print(thoughtText)
				functionCalls = append(functionCalls, fc)
			} else if part.Text != "" { // A part is part of the answer if it has text.
				if !answerStarted {
					a.formatter.Clear()
					if config.C.AI.VoiceEnabled {
						a.formatter.Println("Voice answer:", inout.ColorDarkCyan)
					} else {
						a.formatter.Println("Answer:", inout.ColorDarkCyan)
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

	// If there are function calls, we are in an intermediate step.
	// Don't print sources, execution time, or play audio yet.
	// The final response will trigger that.
	if len(functionCalls) > 0 {
		return "", functionCalls, nil
	}

	// If any sources were found during the tool-use, print them.
	if len(sources) > 0 {
		a.formatter.Println("Sources:", inout.ColorDarkYellow)
		for uri, title := range sources {
			if title != "" {
				a.formatter.Println(title, inout.ColorDarkCyan)
				a.formatter.Println(uri, inout.ColorDarkBlue)
			} else {
				a.formatter.Println(uri, inout.ColorDarkBlue)
			}
		}
	}

	// Print execution time metric
	a.formatter.Println(fmt.Sprintf("Request execution time: %.2fs\n", duration.Seconds()), inout.ColorDarkGray)

	// Restore other output
	(*a.bus).Publish(config.MainTopic, "draw:ai.output")

	// After the stream is finished, if voice was enabled and we have text,
	// make a single API call to generate the audio.
	if config.C.AI.VoiceEnabled && len(fullResponseText) > 0 {
		if err := a.AnswerWithVoice(fullResponseText); err != nil {
			// Log the error but don't fail the whole operation, as the user
			// has already received the text response.
			log.Printf("ERROR: Text-to-speech failed: %v", err)
		}
	}

	return fullResponseText, nil, nil
}

// withPipelinePausedIfVoice pauses and resumes the pipeline if voice is enabled,
// executing the provided action in between. It uses a defer to ensure the pipeline
// is always resumed.
func (a *AI) withPipelinePausedIfVoice(p *pipeline.VadPipeline, action func()) {
	if !config.C.AI.VoiceEnabled {
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

func (a *AI) withScreenshotIfImageMode(action func(imageBuffer *images.ScreenshotBuffer)) {
	if a.mode != inout.ImageMode {
		action(nil)
		return
	}

	log.Println("Taking screenshot for AI response...")
	screenshotBuffer, err := desktop.C.CaptureScreen()
	if err != nil {
		log.Printf("ERROR: AI processing failed for screenshot capture: %v", err)
		return
	}
	defer screenshotBuffer.Release()

	action(screenshotBuffer)
}

// generateAndProcessContent is a universal method to generate content from a set of parts,
// process the streamed response, and update the conversation history.
func (a *AI) generateAndProcessContent(
	parts []*genai.Part,
	urlContextDisabled bool,
	isVoicePrompt bool,
) error {
	startTime := time.Now()

	userContent := genai.NewContentFromParts(parts, genai.RoleUser) // The conversation starts with the history plus the new user message.
	conversation := append(a.conversationHistory, userContent)

	// Start the waiting animation in a separate goroutine.
	done := make(chan struct{})
	// go inout.DisplayWaiting("Thinking...", done)

	defer close(done) // Signal the waiting display to stop.

	for { // Loop for tool calling
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

		systemPrompt := config.C.AI.GetSystemInstruction()
		if systemPrompt != "" {
			genConfig.SystemInstruction = genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(systemPrompt)}, "")
		}

		if config.C.AI.EnableTools {
			log.Println("Tool use is enabled for this request.")
			var tools []*genai.Tool

			// Standart tools for chat requests
			if config.C.AI.EnableStandardTools {
				searchTool := &genai.Tool{GoogleSearch: &genai.GoogleSearch{}}
				if !(config.C.AI.URLContextDisabled || urlContextDisabled) {
					log.Println("URLContext tool in use")
					searchTool.URLContext = &genai.URLContext{}
				}
				tools = append(tools, searchTool)
			}

			if config.C.AI.EnableCodeExecution {
				codeExecutionTool := &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}}
				tools = append(tools, codeExecutionTool)
				log.Println("Code execution tool enabled for this request.")
			}

			// Function calling tools, don't works togather with sandart tools.
			if config.C.AI.EnableFunctionCalling {
				// The GenerateContentStream method used by PostAI does not support the 'Behavior'
				// field in FunctionDeclarations, which is used by LiveAI. We must create a
				// "clean" copy of the toolset with the Behavior field unset to avoid an API error.
				cleanToolset := &genai.Tool{
					FunctionDeclarations: make([]*genai.FunctionDeclaration, len(a.toolset.FunctionDeclarations)),
				}
				for i, fd := range a.toolset.FunctionDeclarations {
					cleanFd := *fd                               // Make a shallow copy of the declaration.
					cleanFd.Behavior = genai.BehaviorUnspecified // Unset the behavior.
					cleanToolset.FunctionDeclarations[i] = &cleanFd
				}
				tools = append(tools, cleanToolset)
				log.Println("Function calling tool enabled for this request (PostAI mode).")
			}

			if len(tools) > 0 {
				genConfig.Tools = tools
			}
		}

		resp := a.client.Models.GenerateContentStream(
			a.ctx,
			config.C.AI.Model,
			conversation,
			genConfig,
		)

		// Output now returns function calls
		fullResponse, functionCalls, err := a.Output(resp, time.Since(startTime))
		if err != nil {
			return err
		}

		// If there are no function calls, we have our final answer.
		if len(functionCalls) == 0 {
			// If successful, update history for the next turn.
			modelResponseContent := genai.NewContentFromParts(
				[]*genai.Part{genai.NewPartFromText(fullResponse)},
				genai.RoleModel,
			)

			// Conditionally add user's prompt to history.
			if !isVoicePrompt || config.C.AI.VoiceHistory {
				a.conversationHistory = append(a.conversationHistory, userContent, modelResponseContent)
			} else {
				log.Println("Skipping voice prompt in conversation history as per configuration.")
				a.conversationHistory = append(a.conversationHistory, modelResponseContent)
			}
			return nil // Exit the loop and the function
		}

		// We have tool calls to execute.
		modelParts, toolResponseParts := a.executeToolCalls(functionCalls)
		modelContentWithCalls := genai.NewContentFromParts(modelParts, genai.RoleModel)
		toolContent := genai.NewContentFromParts(toolResponseParts, genai.RoleUser)
		conversation = append(conversation, modelContentWithCalls, toolContent)
	}
}

// executeToolCalls handles a request from the model to execute one or more tool calls.
func (a *AI) executeToolCalls(calls []*genai.FunctionCall) (modelParts, toolResponseParts []*genai.Part) {
	// In PostAI mode, we inject the necessary dependencies (CLI, ShellExecutor)
	// into the arguments of each tool call. This is particularly important for
	// the SystemAgent, which needs these components to perform its tasks like
	// user confirmation and command execution. This approach keeps the agent's
	// Handle method as the single point of entry for its dependencies, aligning
	// with the pattern used in LiveAI mode and promoting consistency.

	for _, call := range calls {
		modelParts = append(modelParts, &genai.Part{FunctionCall: call})
		var response *genai.FunctionResponse

		// Iterate through all registered agents, following a chain of responsibility pattern.
		// The first agent that recognizes the tool call will handle it.
		for _, agent := range a.agents {
			// Inject dependencies required by agents like SystemAgent.
			if call.Args == nil {
				call.Args = make(map[string]any)
			}
			call.Args["cli_component"] = a.cli
			call.Args["executor_component"] = a.shellExecutor
			response = agent.Handle(call)
			if response != nil {
				break // An agent handled the call, so we can stop searching.
			}
		}

		// If no agent handled the call (response is still nil), fall back to the general-purpose tool dispatcher.
		if response == nil {
			response = executeSingleToolCall(call)
		}

		// Tools like 'uploadImage' can return a 'send_content' key intended for live mode.
		// In non-live mode, this content cannot be sent, so we must remove it to avoid
		// sending a complex, unhandled object back to the model.
		if response.Response != nil {
			delete(response.Response, "send_content")
		}
		toolResponseParts = append(toolResponseParts, genai.NewPartFromFunctionResponse(response.Name, response.Response))
	}
	return modelParts, toolResponseParts
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
		return audio.PlayRawPCM(audioData, audio.TTSSampleRate, audio.TTSChannels)
	}

	return fmt.Errorf("no audio data received from API")
}

// findCacheableFiles scans a directory for files that can be cached or used as initial context.
// It filters out directories and special files like .gitkeep.
func findCacheableFiles(cacheDir string) ([]fs.DirEntry, error) {
	if cacheDir == "" {
		return nil, nil // Not an error, just no directory configured.
	}

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		log.Printf("Cache directory '%s' not found, skipping.", cacheDir)
		return nil, nil
	}

	files, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache directory %s: %w", cacheDir, err)
	}

	var filesToProcess []fs.DirEntry
	for _, file := range files {
		if !file.IsDir() && file.Name() != ".gitkeep" {
			filesToProcess = append(filesToProcess, file)
		}
	}

	if len(filesToProcess) == 0 {
		return nil, nil
	}

	return filesToProcess, nil
}

// addInitialContextTurn adds a user message and a canned model response to the
// conversation history and prints the response to the user.
func (a *AI) addInitialContextTurn(
	userContent *genai.Content,
	modelResponseText string,
	logMessage string,
) {
	modelContent := genai.NewContentFromParts(
		[]*genai.Part{genai.NewPartFromText(modelResponseText)},
		genai.RoleModel,
	)

	// Add both to the conversation history.
	a.conversationHistory = append(a.conversationHistory, userContent, modelContent)

	a.initialContextAdded = true
	log.Println(logMessage)
}

func (a *AI) prepareInitialFiles() {
	cacheDir := config.C.AI.CacheDir

	filesToInclude, err := findCacheableFiles(cacheDir)
	if err != nil {
		log.Printf("ERROR: could not scan for initial files: %v", err)
		return
	}

	if len(filesToInclude) == 0 {
		return
	}

	log.Printf("Found %d files to include as initial context.", len(filesToInclude))

	var parts []*genai.Part
	// Add the system prompt for the files first.
	if config.C.AI.CacheSystemPrompt != "" {
		parts = append(parts, genai.NewPartFromText(config.C.AI.CacheSystemPrompt))
	}
	initialPartsLen := len(parts)

	for _, file := range filesToInclude {
		localPath := filepath.Join(cacheDir, file.Name())
		part, err := a.createPartFromFile(localPath)
		if err != nil {
			log.Printf("ERROR: could not prepare file %s for history: %v", localPath, err)
			continue
		}
		parts = append(parts, part)
	}

	// Only add the context turn if at least one file was successfully processed.
	if len(parts) > initialPartsLen {
		userContent := genai.NewContentFromParts(parts, genai.RoleUser)
		modelResponseText := "OK. I have received the files and will use them as context. How can I help you?"
		logMessage := "Initial file context and canned response have been added to the conversation history."
		a.addInitialContextTurn(userContent, modelResponseText, logMessage)
	}
}

// uploadCache finds all files in the configured cache directory, uploads them,
// and creates a single cache for the model to use in subsequent conversations.
func (a *AI) uploadCache() {
	cacheDir := config.C.AI.CacheDir

	filesToCache, err := findCacheableFiles(cacheDir)
	if err != nil {
		log.Printf("ERROR: could not scan for cacheable files: %v", err)
		return
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
		} else {
			// This is a placeholder user message to create a valid conversation turn.
			// The actual context (files and system prompt) is in the cache itself.
			placeholderUserPrompt := "I have provided some files for context."
			userContent := genai.NewContentFromParts(
				[]*genai.Part{genai.NewPartFromText(placeholderUserPrompt)},
				genai.RoleUser,
			)
			modelResponseText := "OK. I have received the files in a cache for context. How can I help you?"
			logMessage := "Initial file cache context and canned response have been added to the conversation history."
			a.addInitialContextTurn(userContent, modelResponseText, logMessage)
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

	wordAppend := func(word *string) {
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
