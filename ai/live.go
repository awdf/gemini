package ai

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"google.golang.org/genai"

	"gemini/audio"
	"gemini/config"
	"gemini/helpers"
	"gemini/images"
	"gemini/inout"
	"gemini/vad"
)

type LiveAI struct {
	ctx              context.Context
	client           *genai.Client
	formatter        *inout.Formatter
	liveSink         *app.Sink
	Element          *gst.Element
	wg               *sync.WaitGroup
	controlChan      <-chan string
	textCmdChan      <-chan string
	bus              *EventBus.Bus
	session          *genai.Session
	isStreaming      bool
	mode             string
	sessionClosed    chan struct{}
	resumptionHandle string
	mu               sync.RWMutex
}

func NewLiveSink(
	wg *sync.WaitGroup,
	controlChan <-chan string,
	textCmdChan <-chan string,
	bus *EventBus.Bus,
) *LiveAI {
	ctx := context.Background()
	client := helpers.Check(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	}))
	sink := helpers.Check(app.NewAppSink())
	helpers.Verify(sink.SetProperty("sync", false))
	sink.SetDrop(false)    // Do not drop data; ensure all samples are received for recording.
	sink.SetMaxBuffers(10) // Set a max buffer to prevent runaway memory usage and add stability.

	return &LiveAI{
		wg:               wg,
		ctx:              ctx,
		client:           client,
		formatter:        inout.NewFormatter(),
		bus:              bus,
		controlChan:      controlChan,
		textCmdChan:      textCmdChan,
		liveSink:         sink,
		Element:          sink.Element,
		isStreaming:      false,
		mode:             inout.MixMode,
		sessionClosed:    make(chan struct{}, 1), // Buffered channel to prevent blocking
		resumptionHandle: "",
	}
}

func (l *LiveAI) OpenSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session != nil {
		return
	}

	var urlContextDisabled bool

	// 1. Configure the session based on global settings.
	var modelName string
	liveConfig := &genai.LiveConnectConfig{}
	// Models documentation https://ai.google.dev/gemini-api/docs/live
	if config.C.AI.VoiceEnabled {
		// Native audio models
		// Model: gemini-2.5-flash-preview-native-audio-dialog, gemini-2.5-flash-exp-native-audio-thinking-dialog
		// Model inputs: Audio, videos, and text
		// Model outputs: Text and audio, interleaved
		// For responses with voice. RPD 5 per model
		// gemini-2.5-flash-preview-native-audio-dialog tools: Search, Function calling
		// gemini-2.5-flash-exp-native-audio-thinking-dialog tools: Search
		modelName = config.C.AI.ModelLiveTTS // This model does not support URLContext.
		urlContextDisabled = true
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityAudio}
		liveConfig.SpeechConfig = &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: config.C.AI.Voice,
				},
			},
		}
	} else {
		// Half-cascade audio models
		// Model: gemini-live-2.5-flash-preview, gemini-2.0-flash-live-001
		// Model inputs: Audio, images, videos, and text
		// Model outputs: Text
		// For responses with text. RPD 250 per model
		// Tools: Search, Function calling, Code execution, Url context
		urlContextDisabled = false // This model supports URLContext.
		modelName = config.C.AI.ModelLive
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityText}
	}

	// Add system prompt if configured.
	systemPrompt := config.C.AI.GetSystemInstruction()
	var systemInstructionParts []*genai.Part
	if systemPrompt != "" {
		currentTime := time.Now().Format(time.RFC1123)
		systemPrompt = fmt.Sprintf("Current date and time is %s. %s", currentTime, systemPrompt)
		systemInstructionParts = append(systemInstructionParts, genai.NewPartFromText(systemPrompt))
		log.Println("Using system prompt for live session.")
	}

	// voicePrompt := config.C.AI.VoicePrompt
	// textResponse := !config.C.AI.VoiceEnabled
	// if textResponse && voicePrompt != "" {
	// 	systemInstructionParts = append(systemInstructionParts, genai.NewPartFromText(voicePrompt))
	// 	log.Println("Using voice prompt for live session.")
	// }

	// The role for a system instruction is empty.
	if len(systemInstructionParts) > 0 {
		liveConfig.SystemInstruction = genai.NewContentFromParts(systemInstructionParts, "")
	}

	// Conditionally enable tools based on the configuration.
	// This is only done for the main response generation, not transcription.
	if config.C.AI.EnableTools {
		liveConfig.Tools = []*genai.Tool{}
		searchTool := &genai.Tool{GoogleSearch: &genai.GoogleSearch{}}
		if urlContextDisabled {
			log.Println("Tools enabled for live session: GoogleSearch")
		} else {
			log.Println("Tools enabled for live session: GoogleSearch, URLContext")
			searchTool.URLContext = &genai.URLContext{}
		}
		liveConfig.Tools = append(liveConfig.Tools, searchTool)
		// Add file system tools
		liveConfig.Tools = append(liveConfig.Tools, getFileSystemTool())
		log.Println("File system tools enabled for live session.")
	}

	// Add context window compression if enabled.
	if config.C.AI.ContextWindowCompression.Enabled {
		log.Println("Context window compression is enabled for this live session.")
		compressionConfig := &genai.ContextWindowCompressionConfig{
			SlidingWindow: &genai.SlidingWindow{},
		}
		if config.C.AI.ContextWindowCompression.TriggerTokens > 0 {
			log.Printf("Using custom TriggerTokens: %d", config.C.AI.ContextWindowCompression.TriggerTokens)
			compressionConfig.TriggerTokens = helpers.Ptr(config.C.AI.ContextWindowCompression.TriggerTokens)
		} else {
			log.Println("Using default TriggerTokens.")
		}
		if config.C.AI.ContextWindowCompression.TargetTokens > 0 {
			log.Printf("Using custom TargetTokens: %d", config.C.AI.ContextWindowCompression.TargetTokens)
			compressionConfig.SlidingWindow.TargetTokens = helpers.Ptr(config.C.AI.ContextWindowCompression.TargetTokens)
		} else {
			log.Println("Using default TargetTokens.")
		}
		liveConfig.ContextWindowCompression = compressionConfig
	}

	// Add session resumption if enabled.
	if config.C.AI.SessionResumption.Enabled {
		log.Println("Session resumption is enabled for this live session.")
		liveConfig.SessionResumption = &genai.SessionResumptionConfig{
			Handle: l.resumptionHandle, // Use the stored handle
		}
	}

	// Add proactivity config if enabled.
	if config.C.AI.Proactivity.Enabled {
		log.Println("Proactivity is enabled for this live session.")
		liveConfig.Proactivity = &genai.ProactivityConfig{
			ProactiveAudio: helpers.Ptr(config.C.AI.Proactivity.ProactiveAudio),
		}
	}

	// 2. Connect to the live session.
	// Use the model specified in the config, which is suitable for streaming.
	log.Println("Connecting to live session with model:", modelName)
	l.session = helpers.Check(l.client.Live.Connect(l.ctx, modelName, liveConfig))

	// After opening, we must wait for the server's initial setup message.
	msg := helpers.Check(l.session.Receive())
	if msg.SetupComplete == nil {
		// This is a protocol violation from the server, which is a fatal error.
		log.Fatalf("ERROR: expected setup complete message, got: %+v", msg)
	}
	log.Printf("Live session connected. %v", msg)
}

func (l *LiveAI) CloseSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session == nil {
		return
	}
	l.session.Close()
	l.session = nil
}

// Run is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
func (l *LiveAI) Run() {
	defer l.wg.Done()
	defer l.CloseSession()

	helpers.Verify((*l.bus).Subscribe("ai:topic", l.handleEvents))

	// DRY
	initNewSession := func() {
		l.OpenSession()
		l.sendInitialFiles()
		// Start a dedicated goroutine to handle all incoming server messages.
		go l.handleResponses()
	}

	initNewSession()
	// Use a ticker to poll for new samples without running a 100% CPU busy-loop.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if config.C.Trace {
			log.Println("Live AI accepting PCM data")
		}

		select {
		case <-l.sessionClosed:
			// The response handler goroutine has exited, meaning the session is dead.
			// We need to re-establish it.
			log.Println("Live session connection lost. Re-opening...")
			l.CloseSession() // Clean up the old session object.
			initNewSession()
			log.Println("Live session re-established.")
		case cmd, ok := <-l.controlChan:
			if !ok {
				log.Println("Live AI streaming is finished")
				return
			}
			switch {
			case strings.HasPrefix(cmd, vad.MarkerStart):
				log.Println("VAD Start: beginning to stream audio to Live API.")
				l.isStreaming = true
			case strings.HasPrefix(cmd, vad.MarkerStop):
				log.Println("VAD Stop: finishing turn.")
				l.isStreaming = false
				// The response is handled by the handleResponses goroutine.
				// No action needed here to process the stream.
			default:
				log.Printf("WARNING: received unknown control command: %s", cmd)
			}

		case textCmd, ok := <-l.textCmdChan:
			if !ok {
				l.textCmdChan = nil // Mark as closed
				continue
			}
			log.Printf("Live AI: Processing text prompt in %s mode...\n", l.mode)
			// Process in a separate goroutine to avoid blocking the main Run loop.
			go func(prompt string) {
				if err := l.sendTextPrompt(prompt); err != nil {
					// The error is already logged inside sendTextPrompt,
					// but we can log it again here for more context.
					log.Printf("ERROR: failed to process text prompt: %v", err)
				}
			}(textCmd)

		case <-ticker.C:
			l.pullAndSendSamples()
		}
	}
}

// handleResponses runs in a dedicated goroutine, processing all messages from the server.
func (l *LiveAI) handleResponses() {
	var streamPlayer *audio.PCMStreamPlayer
	var playerErr error
	var inModelTurn bool // State to track if we are in the middle of a model's turn.
	var turnGroundingChunks []*genai.GroundingChunk

	for {
		// Lock the session for reading. This is a read-lock, so multiple goroutines
		// can read the session pointer concurrently, but it prevents the main Run()
		// loop from closing and replacing the session while we are using it.
		l.mu.RLock()
		session := l.session
		l.mu.RUnlock()

		if session == nil {
			// The session has been closed by another goroutine. This handler's job is done.
			// The main Run() loop will start a new handler for the new session.
			return
		}
		msg, err := session.Receive()
		if err != nil {
			if err == io.EOF {
				log.Println("Live stream ended (EOF).")
			} else {
				// This error often happens when the connection is closed, which is expected on fail.
				log.Printf("Live session receive error: %v", err)
			}

			// TODO: Remove check when fully pass live testing. Voice session limit 5 per day.
			if !config.C.AI.VoiceEnabled {
				// Signal the main Run loop that the session is dead and needs to be reopened.
				// Use a non-blocking send because the channel is buffered and we only need
				// to signal once. If a signal is already pending, we don't need to send another.
				select {
				case l.sessionClosed <- struct{}{}:
				default:
				}
			}

			// Clean up the player if it exists.
			if streamPlayer != nil {
				if closeErr := streamPlayer.Close(); closeErr != nil {
					log.Printf("ERROR: closing audio stream player on exit: %v", closeErr)
				}
				streamPlayer = nil
			}
			// Ensure UI is un-muted on error/exit
			l.formatter.Reset()
			(*l.bus).Publish("main:topic", "draw:ai.handleResponses.error")
			return // Exit the goroutine.
		}

		// Process the content of the message using a switch for clarity.
		switch {
		case msg.ServerContent != nil:
			if msg.ServerContent.ModelTurn != nil {
				// If this is the first chunk of a new model turn, print the header.
				if !inModelTurn {
					inModelTurn = true
					// Reset the accumulator for the new turn.
					turnGroundingChunks = nil
					(*l.bus).Publish("main:topic", "mute:ai.handleResponses")
					l.formatter.Clear()
					l.formatter.Println("\nAnswer:", inout.ColorDarkCyan)
				}

				for _, part := range msg.ServerContent.ModelTurn.Parts {
					if part.Text != "" {
						l.formatter.Print(part.Text)
					}
					if part.InlineData != nil && len(part.InlineData.Data) > 0 {
						// Create the player on the first audio chunk received.
						if streamPlayer == nil && config.C.AI.VoiceEnabled {
							streamPlayer, playerErr = audio.NewPCMStreamPlayer(audio.TTSSampleRate, audio.TTSChannels)
							if playerErr != nil {
								log.Printf("ERROR: could not create audio stream player: %v", playerErr)
								streamPlayer = nil // Ensure it's nil on error
							}
						}
						if streamPlayer != nil {
							if err := streamPlayer.Write(part.InlineData.Data); err != nil {
								log.Printf("ERROR: writing to audio stream: %v", err)
							}
						}
					}
				}
			}

			// Accumulate any grounding chunks received in this message.
			if msg.ServerContent.GroundingMetadata != nil {
				turnGroundingChunks = append(turnGroundingChunks, msg.ServerContent.GroundingMetadata.GroundingChunks...)
			}

			// When generation is complete, it signals the end of the model's turn.
			if msg.ServerContent.GenerationComplete {
				log.Println("Live stream generation complete, ending turn.")

				// If any sources were accumulated during the turn, print them now.
				// The API guarantees that the chunks are ordered to correspond to the [1], [2]...
				// markers in the response text.
				if len(turnGroundingChunks) > 0 {
					l.formatter.Println("\nSources:", inout.ColorDarkYellow)
					for i, chunk := range turnGroundingChunks {
						// Perform nil checks for safety
						if chunk == nil || chunk.Web == nil {
							continue
						}
						uri, title := chunk.Web.URI, chunk.Web.Title
						// Prepend the citation number, e.g., "[1] Title"
						sourcePrefix := fmt.Sprintf("[%d]", i+1)
						if title != "" {
							l.formatter.Println(fmt.Sprintf("%s %s", sourcePrefix, title), inout.ColorDarkCyan)
							l.formatter.Println(uri, inout.ColorDarkBlue)
						} else {
							l.formatter.Println(fmt.Sprintf("%s %s", sourcePrefix, uri), inout.ColorDarkBlue)
						}
					}
				}

				inModelTurn = false // The turn is over, reset the state.
				// The turn is over. Close the player and reset for the next turn.
				if streamPlayer != nil {
					if err := streamPlayer.Close(); err != nil {
						log.Printf("ERROR: closing audio stream player: %v", err)
					}
					streamPlayer = nil
				}
				l.formatter.Reset()
				(*l.bus).Publish("main:topic", "draw:ai.handleResponses")
			}
		case msg.ToolCall != nil:
			// It's crucial to handle tool calls in a separate goroutine to avoid
			// blocking the message receiving loop. This allows the app to remain
			// responsive and continue processing other messages (like audio)
			// while a tool is executing.
			go func(request *genai.LiveServerToolCall) {
				responses := l.executeToolCalls(request)

				// Send the results back to the model using the dedicated tool response message.
				toolInput := genai.LiveToolResponseInput{FunctionResponses: responses}

				l.mu.RLock()
				defer l.mu.RUnlock()
				if l.session != nil {
					// Use SendToolResponse, which is the correct method for sending back
					// the results of function calls in a live session.
					if err := l.session.SendToolResponse(toolInput); err != nil {
						log.Printf("ERROR: failed to send tool response: %v", err)
					}
				}
			}(msg.ToolCall)
		case msg.GoAway != nil:
			// The loop will terminate in the next iteration due to the connection closing.
			log.Printf("Live stream session closing by server: %+v", msg.GoAway.TimeLeft)
		case msg.SessionResumptionUpdate != nil:
			log.Printf("Live session resumption handle updated. Resumable: %t", msg.SessionResumptionUpdate.Resumable)
			if msg.SessionResumptionUpdate.Resumable {
				l.resumptionHandle = msg.SessionResumptionUpdate.NewHandle
			}
		default:
			config.DebugPrintf("Live AI received unhandled message: %+v", msg)
		}

		// UsageMetadata often signals the end of the model's response for the current turn.
		if msg.UsageMetadata != nil {
			log.Printf("Live stream usage metadata received: InT:%d, OutT:%d, Tot:%d",
				msg.UsageMetadata.PromptTokenCount,
				msg.UsageMetadata.ResponseTokenCount,
				msg.UsageMetadata.TotalTokenCount)
		}
	}
}

// handleEvents processes commands sent to the AI component via the event bus.
func (l *LiveAI) handleEvents(event string) {
	config.DebugPrintf("LiveAI component received event: %s\n", event)
	parts := strings.SplitN(event, ":", 2)
	if len(parts) < 2 {
		log.Printf("WARNING: received malformed LiveAI event: %s", event)
		return
	}
	command, payload := parts[0], parts[1]

	switch command {
	case "mode":
		l.mode = payload
		log.Printf("LiveAI mode set to: %s", payload)
	default:
		// The "save" event is not handled here as LiveAI does not maintain history.
		config.DebugPrintf("LiveAI component ignoring event: %s", event)
	}
}

// sendInitialFiles reads files from the cache directory and sends them as the first
// user turn in the live session.
func (l *LiveAI) sendInitialFiles() {
	cacheDir := config.C.AI.CacheDir
	filesToInclude, err := findCacheableFiles(cacheDir)
	if err != nil {
		log.Printf("ERROR: could not scan for initial files: %v", err)
		return
	}
	if len(filesToInclude) == 0 {
		return // Nothing to send
	}

	log.Printf("Found %d files to send as initial context for live session.", len(filesToInclude))
	var parts []*genai.Part
	if config.C.AI.CacheSystemPrompt != "" {
		parts = append(parts, genai.NewPartFromText(config.C.AI.CacheSystemPrompt))
	}

	for _, file := range filesToInclude {
		localPath := filepath.Join(cacheDir, file.Name())
		data, err := os.ReadFile(localPath)
		if err != nil {
			log.Printf("ERROR: could not read file %s for live session context: %v", localPath, err)
			continue
		}
		// Add a header to each file part to give the model more structure.
		fileContentWithHeader := fmt.Sprintf("\n\n--- Start of file: %s ---\n\n%s\n\n--- End of file: %s ---", file.Name(), string(data), file.Name())
		parts = append(parts, genai.NewPartFromText(fileContentWithHeader))
		log.Printf("Cache file prepared %s", localPath)
	}

	if len(parts) == 0 {
		log.Println("No files were successfully prepared to be sent.")
		return
	}

	parts = append(parts, genai.NewPartFromText(CheckQuestion))

	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	// Lock the mutex only for the duration of accessing the shared session object.
	l.mu.RLock()
	session := l.session
	l.mu.RUnlock()

	if session == nil {
		log.Println("ERROR: cannot send initial files, session is nil.")
		return
	}

	if err := session.SendClientContent(content); err != nil {
		log.Printf("ERROR: failed to send initial files as client content: %v", err)
	} else {
		log.Println("Successfully sent initial files as the first user turn.")
	}
}

// sendTextPrompt sends a text prompt (and potentially a screenshot) to the live session.
// The response is handled by the separate handleResponses goroutine.
func (l *LiveAI) sendTextPrompt(prompt string) error {
	var imageBuffer *images.ScreenshotBuffer
	var err error
	if l.mode == inout.ImageMode {
		log.Println("Taking screenshot for AI response...")
		imageBuffer, err = images.TakeScreenshot()
		if err != nil {
			return fmt.Errorf("failed to take screenshot: %w", err)
		}
		defer imageBuffer.Release()
	}

	parts := []*genai.Part{genai.NewPartFromText(prompt)}

	if imageBuffer != nil {
		parts = append(parts, genai.NewPartFromBytes(imageBuffer.Bytes(), "image/png"))
	}

	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	// Lock the mutex only for the duration of accessing the shared session object.
	// This prevents holding the lock during long-running network calls.
	l.mu.RLock()
	session := l.session
	l.mu.RUnlock()

	// The session might be nil if it has just been closed and is waiting to be
	// reopened by the main Run loop.
	if session == nil {
		return fmt.Errorf("session is temporarily unavailable, please try again")
	}

	if err := session.SendClientContent(content); err != nil {
		return fmt.Errorf("failed to send client content: %w", err)
	}

	return nil
}

// pullAndSendSamples pulls all available samples from the sink and sends them to the Live API.
// It MUST be called continuously to drain the sink, even when not actively streaming to the API.
func (l *LiveAI) pullAndSendSamples() {
	// Pull all available samples from the sink in a loop.
	for {
		sample := l.liveSink.TryPullSample(0)
		if sample == nil {
			break // No more samples in queue.
		}

		// Only send audio to the API if we are in a streaming state (between VAD start/stop).
		if !l.isStreaming {
			continue // Discard the sample.
		}

		buffer := sample.GetBuffer()
		if buffer != nil {
			// The session might be nil if it has just been closed and is waiting to be
			// reopened by the main Run loop. We need to lock to safely access it.
			l.mu.RLock()
			session := l.session
			l.mu.RUnlock()

			if session == nil {
				log.Printf("WARNING: live session is unavailable, dropping audio sample.")
				buffer.Unmap()
				continue
			}

			// The pipeline is configured for 16-bit, 16kHz mono PCM audio.
			// The correct MIME type for this is audio/l16;rate=16000.
			err := session.SendRealtimeInput(genai.LiveRealtimeInput{
				Audio: &genai.Blob{
					MIMEType: fmt.Sprintf("audio/pcm;rate=%d", audio.LiveSampleRate),
					Data:     buffer.Bytes(),
				},
			})
			if err != nil {
				log.Printf("ERROR: failed to send realtime audio input: %v", err)
				// Stop streaming on error to prevent flooding with more errors.
				l.isStreaming = false
			}
			buffer.Unmap()
		}
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}

// executeToolCalls handles a request from the model to execute one or more tool calls.
// It executes them concurrently and returns a slice of their responses.
// It executes them sequentially, in the order they are received, and returns a slice of their responses.
func (l *LiveAI) executeToolCalls(request *genai.LiveServerToolCall) []*genai.FunctionResponse {
	var responses []*genai.FunctionResponse

	// Execute tool calls sequentially, in the order they are received.
	// This is crucial because one tool call might depend on the result of a previous one
	// (e.g., creating a file, then reading it).
	for _, call := range request.FunctionCalls {
		response := l.executeSingleToolCall(call)
		responses = append(responses, response)
	}
	return responses
}

// executeSingleToolCall dispatches a single tool call to the appropriate Go function
// and returns a structured FunctionResponse.
func (l *LiveAI) executeSingleToolCall(call *genai.FunctionCall) *genai.FunctionResponse {
	var result any
	var err error

	// For safety, we print the arguments. In a real application, you might want more structured logging.
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	switch call.Name {
	case "listFiles":
		// The model might not provide a path if it wants the root, so we default to ".".
		path, _ := call.Args["path"].(string)
		if path == "" {
			path = "."
		}
		result, err = listFiles(path)
	case "readFile":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			result, err = readFile(path)
		}
	case "createFile":
		path, pathOK := call.Args["path"].(string)
		content, contentOK := call.Args["content"].(string)
		if !pathOK || path == "" || !contentOK {
			// The model can sometimes forget to provide content.
			err = fmt.Errorf("'path' (string) and 'content' (string) arguments are required")
		} else {
			result, err = createFile(path, content)
		}
	case "deleteFile":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			result, err = deleteFile(path)
		}
	default:
		err = fmt.Errorf("unknown tool call: %s", call.Name)
	}

	// The model expects a JSON object as a response. If we have an error,
	// we'll return it in a structured way.
	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	// The response from a tool must be a map[string]any.
	responseMap, ok := result.(map[string]any)
	if !ok {
		// This should not happen with the current tool implementations, but it's a good safeguard.
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:       call.ID,
		Name:     call.Name,
		Response: responseMap,
	}
}

// --- File System Tool Implementations ---

// expandPath handles tilde expansion for file paths (e.g., "~/Documents").
func expandPath(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}

	// Path is "~" or "~/..."
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	homeDir := usr.HomeDir

	if path == "~" {
		return homeDir, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(homeDir, path[2:]), nil
	}

	return path, fmt.Errorf("unsupported tilde expansion: only '~' and '~/' are supported")
}

// getSafePath joins the base directory with a user-provided path and ensures
// it doesn't escape the base directory.
func getSafePath(userPath string) (string, error) {
	// For security, all file operations are restricted to the configured workspace directory.
	baseDir := config.C.AI.WorkspaceDir
	if baseDir == "" {
		return "", fmt.Errorf("workspace directory is not configured")
	}

	expandedBaseDir, err := expandPath(baseDir)
	if err != nil {
		return "", fmt.Errorf("could not expand workspace directory path '%s': %w", baseDir, err)
	}

	// Create the directory if it doesn't exist.
	if err := os.MkdirAll(expandedBaseDir, 0o755); err != nil {
		return "", fmt.Errorf("could not create workspace directory: %w", err)
	}

	absBase, err := filepath.Abs(expandedBaseDir)
	if err != nil {
		return "", fmt.Errorf("could not get absolute path for workspace: %w", err)
	}

	// Join the base directory with the user-provided path.
	// If userPath is absolute, Join returns userPath.
	// If userPath is relative, it's joined with absBase.
	finalPath := ""
	if strings.HasPrefix(userPath, absBase) {
		// Clean the path to resolve any ".." or "." components.
		finalPath = filepath.Clean(userPath)
	} else {
		// Join joins any number of path elements into a single path, separating them with an OS specific [Separator].
		// Empty elements are ignored. The result is Cleaned. However, if the argument list is empty or all its elements are empty,
		// Join returns an empty string. On Windows, the result will only be a UNC path if the first non-empty element is a UNC path.
		finalPath = filepath.Join(absBase, userPath)
	}

	// Security check: ensure the final, absolute path is still within the workspace.
	// This prevents path traversal attacks (e.g., path: "../../../etc/passwd").
	if !strings.HasPrefix(finalPath, absBase) {
		return "", fmt.Errorf("path traversal detected: access to '%s' is not allowed as it is outside the workspace", userPath)
	}

	return finalPath, nil
}

func listFiles(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(safePath)
	if err != nil {
		return nil, err
	}

	var files []map[string]any
	for _, entry := range entries {
		info, err := entry.Info()
		fileInfo := map[string]any{
			"name":  entry.Name(),
			"isDir": entry.IsDir(),
		}
		if err == nil {
			fileInfo["size"] = info.Size()
			fileInfo["modTime"] = info.ModTime().Format(time.RFC3339)
		}
		files = append(files, fileInfo)
	}
	// Return as a map for consistency, making it clear to the model what it's receiving.
	return map[string]any{"files": files}, nil
}

func readFile(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(safePath)
	if err != nil {
		return nil, err
	}
	// Return as a map for consistency, making it clear to the model what it's receiving.
	return map[string]any{"content": string(content)}, nil
}

func createFile(path string, content string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	err = os.WriteFile(safePath, []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("file '%s' created successfully", path)}, nil
}

func deleteFile(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	err = os.Remove(safePath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("file '%s' deleted successfully", path)}, nil
}

func getFileSystemTool() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        "listFiles",
				Description: "List files and directories in a given path relative to the workspace. Use '.' for the current directory.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"path": {Type: genai.TypeString, Description: "The directory path to list. Defaults to the workspace root if empty."},
					},
				},
				Response: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"files": {
							Type:        genai.TypeArray,
							Description: "A list of files and directories.",
							Items: &genai.Schema{
								Type: genai.TypeObject,
								Properties: map[string]*genai.Schema{
									"name":    {Type: genai.TypeString, Description: "The name of the file or directory."},
									"isDir":   {Type: genai.TypeBoolean, Description: "True if the entry is a directory."},
									"size":    {Type: genai.TypeInteger, Description: "The size of the file in bytes."},
									"modTime": {Type: genai.TypeString, Description: "The modification time in RFC3339 format."},
								},
							},
						},
					},
				},
			},
			{
				Name:        "readFile",
				Description: "Read the entire content of a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to read."}}, Required: []string{"path"}},
				Response: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"content": {Type: genai.TypeString, Description: "The content of the file."},
						"error":   {Type: genai.TypeString, Description: "An error message if the operation failed."},
					},
				},
			},
			{
				Name:        "createFile",
				Description: "Create or overwrite a file in the workspace with specified content.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to create."}, "content": {Type: genai.TypeString, Description: "The content to write to the file."}}, Required: []string{"path", "content"}},
				Response: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"status": {Type: genai.TypeString, Description: "The result of the file creation operation."},
						"error":  {Type: genai.TypeString, Description: "An error message if the operation failed."},
					},
				},
			},
			{
				Name:        "deleteFile",
				Description: "Delete a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to delete."}}, Required: []string{"path"}},
				Response: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"status": {Type: genai.TypeString, Description: "The result of the file deletion operation."},
						"error":  {Type: genai.TypeString, Description: "An error message if the operation failed."},
					},
				},
			},
		},
	}
}

// for test function call /home/awdf/Workspace/IT_Kombinat.txt
