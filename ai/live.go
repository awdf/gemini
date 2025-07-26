package ai

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"os"
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
	"gemini/flow"
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
	imageBuffer      *images.ScreenshotBuffer
	isStreaming      bool
	mode             string
	sessionClosed    chan struct{}
	resumptionHandle string
	warmUpDone       bool
	Online           bool
	// mu protects the internal state of the LiveAI struct (e.g., session, Online, resumptionHandle).
	// It allows multiple concurrent readers but only one writer, which is ideal for state
	// that is read often but changed infrequently (like during session setup/teardown).
	mu sync.RWMutex
	// writeMu serializes all write operations to the underlying websocket connection.
	// The Gemini library is not safe for concurrent writes, so this mutex prevents panics
	// by ensuring that only one goroutine can call Send... methods at a time.
	writeMu sync.Mutex
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
		warmUpDone:       false,
		Online:           false,
	}
}

func (l *LiveAI) OpenSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session != nil || l.Online {
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

	// The role for a system instruction is empty.
	if len(systemInstructionParts) > 0 {
		liveConfig.SystemInstruction = genai.NewContentFromParts(systemInstructionParts, "")
	}

	// Conditionally enable tools based on the configuration.
	// This is only done for the main response generation, not transcription.
	if config.C.AI.EnableTools {
		var tools []*genai.Tool
		if config.C.AI.EnableStandardTools {
			searchTool := &genai.Tool{GoogleSearch: &genai.GoogleSearch{}}
			if urlContextDisabled {
				log.Println("Tools enabled for live session: GoogleSearch")
			} else {
				log.Println("Tools enabled for live session: GoogleSearch, URLContext")
				searchTool.URLContext = &genai.URLContext{}
			}
			tools = append(tools, searchTool)
		}

		if config.C.AI.EnableCodeExecution {
			codeExecutionTool := &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}}
			tools = append(tools, codeExecutionTool)
			log.Println("Code execution tool enabled for live session.")
		}

		if config.C.AI.EnableFunctionCalling {
			// Add file system tools
			tools = append(tools, getFileSystemTool())
			log.Println("File system tools enabled for live session.")
		}

		if len(tools) > 0 {
			liveConfig.Tools = tools
		}
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
		if l.resumptionHandle != "" {
			// Log only the first few characters of the handle to avoid leaking sensitive info.
			handleForLog := l.resumptionHandle
			if len(handleForLog) > 10 {
				handleForLog = handleForLog[:10]
			}
			log.Printf("Attempting to resume session with handle: %s...", handleForLog)
		} else {
			log.Println("No resumption handle found, starting a new session.")
		}
		liveConfig.SessionResumption = &genai.SessionResumptionConfig{
			Handle: l.resumptionHandle,
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
	l.Online = true
}

func (l *LiveAI) CloseSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.session == nil {
		return
	}
	l.session.Close()
	l.session = nil
	l.Online = false
}

// Run is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
func (l *LiveAI) Run() {
	defer l.wg.Done()
	defer l.CloseSession()

	// Subscribe to the main event topic to listen for the warm-up completion signal from VAD.
	helpers.Verify((*l.bus).SubscribeAsync("main:topic", func(event string) {
		if strings.HasPrefix(event, "ready:") {
			l.mu.Lock()
			if !l.warmUpDone {
				log.Println("Live AI warm-up complete. Now actively listening for events.")
				l.warmUpDone = true
			}
			l.mu.Unlock()
		}
	}, false))
	helpers.Verify((*l.bus).Subscribe("ai:topic", l.handleEvents))

	l.OpenSession()
	// Start a dedicated goroutine to handle all incoming server messages.
	go l.handleResponses()
	// Send initial files only once at the beginning of the session.
	// This must be done after the response handler is running to catch the server's acknowledgment.
	l.sendInitialFiles()
	// Use a ticker to poll for new samples without running a 100% CPU busy-loop.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	shutdownChan := flow.GetListener()

	for {
		if config.C.Trace {
			log.Println("Live AI accepting PCM data")
		}

		select {
		case <-*shutdownChan:
			log.Println("Live AI shutting down.")
			// The return will trigger the deferred CloseSession() and wg.Done().
			// CloseSession() will unblock the handleResponses goroutine.
			return
		case <-l.sessionClosed:
			// Drain any other pending signals from the channel. This is crucial to
			// prevent a race condition where a signal from a previously failed
			// session handler causes us to immediately kill a brand new session.
			for len(l.sessionClosed) > 0 {
				<-l.sessionClosed
			}

			if !config.C.AI.SessionResumption.Enabled {
				log.Println("Session resumption is disabled. Shutting down on connection loss.")
				flow.Quit()
				return // Exit the Run loop to allow graceful shutdown.
			}

			// The response handler goroutine has exited, meaning the session is dead.
			// We need to re-establish it because resumption is enabled.
			log.Println("Live session connection lost. Re-opening...")
			l.CloseSession() // Clean up the old session object.
			l.OpenSession()  // Re-establish the session.
			log.Println("Live session re-established.")
		case cmd, ok := <-l.controlChan:
			if !ok {
				log.Println("Live AI streaming is finished")
				return
			}
			switch {
			case strings.HasPrefix(cmd, vad.MarkerStart):
				// The native audio models perform their own VAD (automatic activity detection).
				// Sending explicit ActivityStart/ActivityEnd signals conflicts with this,
				// causing a websocket error. We still use our application's VAD to control
				// when we *stream* audio to the API by toggling `isStreaming`, but we don't
				// send the explicit start/end markers.
				log.Println("VAD Start: beginning to stream audio to Live API.")
				l.isStreaming = true

				if l.mode == inout.ImageMode {
					l.mu.Lock()
					if l.imageBuffer != nil {
						log.Println("An image is already being processed, skipping new screenshot for this turn.")
						l.mu.Unlock()
					} else {
						log.Println("Taking live screenshot for AI response...")
						var err error
						l.imageBuffer, err = images.TakeScreenshot()
						l.mu.Unlock() // Unlock before logging and sending to avoid holding lock during I/O
						if err != nil {
							log.Printf("failed to take screenshot: %v", err)
						} else {
							l.sendLiveImage()
						}
					}
				}
			case strings.HasPrefix(cmd, vad.MarkerStop):
				// The response is handled by the handleResponses goroutine.
				// No action needed here to process the stream.
				log.Println("VAD Stop: finishing turn.")
				l.isStreaming = false
				// The image buffer is now released upon GenerationComplete, not here.
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
	var inModelTurn bool // State to track if we are in the middle of a model's turn.
	var turnGroundingChunks []*genai.GroundingChunk
	listener := flow.GetListener()

	fireClose := func() {
		// Signal the main Run loop that the session is dead(dying) and needs to be reopened.
		// Use a non-blocking send because the channel is buffered and we only need
		// to signal once. If a signal is already pending, we don't need to send another.
		l.Online = false
		select {
		case l.sessionClosed <- struct{}{}:
		default:
		}
	}

	// This loop will run for the lifetime of a single session connection.
	// If it exits, the main Run loop will restart it for a new session.
	for {
		// Lock the session for reading. This is a read-lock, so multiple goroutines
		// can read the session pointer concurrently, but it prevents the main Run()
		// loop from closing and replacing the session while we are using it.
		l.mu.RLock()
		session := l.session
		l.mu.RUnlock()

		// Waiting stage, until session in configuration state
		// Online flag early show that session will be closed
		// session = nil when session has been closed by another goroutine.
		if session == nil || !l.Online {
			select {
			case <-*listener: // App closes, work done
				return
			default:
			}
			if config.C.Trace {
				log.Println("Live stream session is temporarily unavailable.")
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// This method blocks until a message is received from the server.
		// The returned message represents a part of or a complete model turn.
		// If the received message is a [LiveServerToolCall],
		// the user must call [SendToolResponse] to provide
		// the function execution result and continue the turn.
		msg, err := session.Receive()
		if err != nil {
			if err == io.EOF {
				log.Println("Live stream ended (EOF).")
			} else {
				// This error often happens when the connection is closed, which is expected on fail.
				// We don't want to spam the log with it during normal shutdown or reconnection.
				log.Printf("Live session receive error: %v", err)
			}

			fireClose()

			// Clean up the player if it exists.
			if streamPlayer != nil {
				if closeErr := streamPlayer.Close(); closeErr != nil {
					log.Printf("ERROR: closing audio stream player on exit: %v", closeErr)
				}
				streamPlayer = nil
			}
			continue
		}

		// Process the content of the message using a switch for clarity.
		switch {
		case msg.SetupComplete != nil:
			log.Println("Live session setup complete.")
		case msg.ServerContent != nil:
			if msg.ServerContent.ModelTurn != nil {
				if !inModelTurn {
					inModelTurn = true
					turnGroundingChunks = nil
					(*l.bus).Publish("main:topic", "mute:ai.handleResponses")
					l.formatter.Clear()
					l.formatter.Println("\nAnswer:", inout.ColorDarkCyan)
				}

				streamPlayer = l.processModelTurnParts(msg.ServerContent.ModelTurn.Parts, streamPlayer)
			}

			if msg.ServerContent.GroundingMetadata != nil {
				turnGroundingChunks = append(turnGroundingChunks, msg.ServerContent.GroundingMetadata.GroundingChunks...)
			}

			if msg.ServerContent.GenerationComplete {
				log.Println("Live stream generation complete, ending turn.")
				l.printGroundingChunks(turnGroundingChunks)
				inModelTurn = false
				if streamPlayer != nil {
					if err := streamPlayer.Close(); err != nil {
						log.Printf("ERROR: closing audio stream player: %v", err)
					}
					streamPlayer = nil
				}
				l.formatter.Reset()
				// Release the image buffer now that the turn is fully complete.
				l.mu.Lock()
				if l.imageBuffer != nil {
					log.Println("Releasing screenshot buffer after completed turn.")
					l.imageBuffer.Release()
					l.imageBuffer = nil
				}
				l.mu.Unlock()
				(*l.bus).Publish("main:topic", "draw:ai.handleResponses")
			}
		case msg.ToolCall != nil:
			go func(request *genai.LiveServerToolCall) {
				responses := l.executeToolCalls(request)

				// Send the results back to the model using the dedicated tool response message.
				toolInput := genai.LiveToolResponseInput{FunctionResponses: responses}

				l.mu.RLock()
				defer l.mu.RUnlock()
				if l.session != nil {
					l.writeMu.Lock()
					if err := l.session.SendToolResponse(toolInput); err != nil {
						log.Printf("ERROR: failed to send tool response: %v", err)
					}
					l.writeMu.Unlock()
				}
			}(msg.ToolCall)
		case msg.GoAway != nil:
			// The loop will terminate in the next iteration due to the connection closing.
			log.Printf("Live stream session GoAway received: %+v", msg.GoAway.TimeLeft)
			fireClose()
		case msg.SessionResumptionUpdate != nil:
			l.mu.Lock()
			if msg.SessionResumptionUpdate.Resumable {
				log.Printf("Live session resumption handle updated. New handle received.")
				l.resumptionHandle = msg.SessionResumptionUpdate.NewHandle
			} else {
				log.Printf("Live session is no longer resumable. Clearing handle.")
				l.resumptionHandle = "" // Clear the handle when the session is not resumable.
			}
			l.mu.Unlock()
		default:
			config.DebugPrintf("Live AI received unhandled message: %+v", msg)
		}

		if msg.UsageMetadata != nil {
			log.Printf("Live stream usage metadata received: InT:%d, OutT:%d, Tot:%d",
				msg.UsageMetadata.PromptTokenCount,
				msg.UsageMetadata.ResponseTokenCount,
				msg.UsageMetadata.TotalTokenCount)
		}
	}
}

// processModelTurnParts handles the processing of text and audio parts from a model's turn.
func (l *LiveAI) processModelTurnParts(parts []*genai.Part, player *audio.PCMStreamPlayer) *audio.PCMStreamPlayer {
	for _, part := range parts {
		if part.Text != "" {
			l.formatter.Print(part.Text)
		}
		if part.InlineData != nil && len(part.InlineData.Data) > 0 {
			var err error
			// Create the player on the first audio chunk received.
			if player == nil && config.C.AI.VoiceEnabled {
				player, err = audio.NewPCMStreamPlayer(audio.TTSSampleRate, audio.TTSChannels)
				if err != nil {
					log.Printf("ERROR: could not create audio stream player: %v", err)
					player = nil // Ensure it's nil on error
				}
			}
			if player != nil {
				if err := player.Write(part.InlineData.Data); err != nil {
					log.Printf("ERROR: writing to audio stream: %v", err)
				}
			}
		}
	}
	return player
}

// printGroundingChunks formats and prints the source attribution information.
func (l *LiveAI) printGroundingChunks(chunks []*genai.GroundingChunk) {
	if len(chunks) == 0 {
		return
	}
	l.formatter.Println("\nSources:", inout.ColorDarkYellow)
	for i, chunk := range chunks {
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
		l.formatter.Reset()
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

	// Send the introductory prompt first.
	if config.C.AI.CacheSystemPrompt != "" {
		if err := l.sendTextPrompt(config.C.AI.CacheSystemPrompt); err != nil {
			log.Printf("ERROR: failed to send initial context prompt: %v", err)
			return // If this fails, don't proceed.
		}
	}

	// Send each file as a separate message.
	for _, file := range filesToInclude {
		localPath := filepath.Join(cacheDir, file.Name())
		data, err := os.ReadFile(localPath)
		if err != nil {
			log.Printf("ERROR: could not read file %s for live session context: %v", localPath, err)
			continue
		}
		// Add a header to each file part to give the model more structure.
		fileContentWithHeader := fmt.Sprintf("\n\n--- Context part start ---\n\n%s\n\n--- End of context part ---", string(data))

		// Re-using sendTextPrompt to send the file content.
		if err := l.sendTextPrompt(fileContentWithHeader); err != nil {
			log.Printf("ERROR: failed to send initial file %s: %v", file.Name(), err)
			// If one file fails, we should probably stop to avoid confusing the model with partial context.
			return
		}
		log.Printf("Cache file sent %s", localPath)
	}

	// Finally, send the check question to prompt the model to acknowledge the files.
	if err := l.sendTextPrompt(CheckQuestion); err != nil {
		log.Printf("ERROR: failed to send final check question: %v", err)
	} else {
		log.Println("Successfully sent all initial files and final prompt.")
	}
}

func (l *LiveAI) sendLiveImage() {
	l.mu.RLock()
	imageBuffer := l.imageBuffer
	online := l.Online
	l.mu.RUnlock()

	if !online || imageBuffer == nil {
		return
	}
	log.Println("Sending live image to session context...")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		Media: &genai.Blob{
			MIMEType: "image/png",
			Data:     imageBuffer.Bytes(), // This is safe because imageBuffer is a local var now
		},
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf("ERROR: failed to send realtime image input: %v", err)
		// Stop streaming on error to prevent flooding with more errors.
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

	l.writeMu.Lock()
	if err := session.SendClientContent(content); err != nil {
		l.writeMu.Unlock()
		return fmt.Errorf("failed to send client content: %w", err)
	}
	l.writeMu.Unlock()

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
			l.writeMu.Lock()
			err := session.SendRealtimeInput(genai.LiveRealtimeInput{
				Audio: &genai.Blob{
					MIMEType: fmt.Sprintf("audio/pcm;rate=%d", audio.LiveSampleRate),
					Data:     buffer.Bytes(),
				},
			})
			l.writeMu.Unlock()
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
		var response *genai.FunctionResponse
		switch call.Name {
		case "uploadImage":
			response = l.handleUploadImageTool(call)
		case "detectObjects":
			response = l.handleDetectObjectsTool(call)
		default:
			response = executeSingleToolCall(call)
		}
		responses = append(responses, response)
	}
	return responses
}

// handleDetectObjectsTool processes the 'detectObjects' tool call.
// It uses a specialized Agent to analyze an image from the current session's
// image buffer and return the findings.
func (l *LiveAI) handleDetectObjectsTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Get the query from the tool call arguments.
	query, ok := call.Args["query"].(string)
	if !ok || query == "" {
		err = fmt.Errorf("'query' argument is required and must be a non-empty string")
	} else {
		// This tool uses the session's image buffer.
		l.mu.RLock()
		imageBuf := l.imageBuffer
		l.mu.RUnlock()

		if imageBuf == nil || imageBuf.Len() == 0 {
			err = fmt.Errorf("no image found in the current session context to detect objects from")
		} else {
			// Create the agent with a specific system prompt for object detection.
			agentConfig := AgentConfig{
				Model:             config.C.AI.Model,
				SystemInstruction: "You are an object detection specialist. For all requested items in the image, provide their label and a bounding box. The bounding box coordinates in the 'box_2d' object should be normalized to 0-1000.",
				EnableTools:       false,
				ResponseSchema:    GetObjectDetectionSchema(),
			}
			agent := NewAgent(l.ctx, agentConfig)

			// Process the image with the agent, using the query from the tool call as the prompt.
			// The image buffer is PNG encoded.
			detectionResult, processErr := agent.Process(query, imageBuf.Bytes(), "image/png")
			if processErr != nil {
				err = fmt.Errorf("object detection failed: %w", processErr)
			} else {
				log.Printf("Object detection successful for query: '%s'", query)
				result = map[string]any{"detected_objects": detectionResult}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}

// handleUploadImageTool processes the 'uploadImage' tool call, which is specific to LiveAI.
// It reads an image from the workspace and sends it to the live session as media input.
func (l *LiveAI) handleUploadImageTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		err = fmt.Errorf("'path' argument is required and must be a non-empty string")
	} else {
		// Use getSafePath to ensure the file is within the configured workspace.
		safePath, pathErr := getSafePath(path)
		if pathErr != nil {
			err = pathErr
		} else {
			// Read the file content.
			data, readErr := os.ReadFile(safePath)
			if readErr != nil {
				err = fmt.Errorf("failed to read image file '%s': %w", path, readErr)
			} else {
				// Determine MIME type from file extension.
				mimeType := mime.TypeByExtension(filepath.Ext(safePath))
				if !strings.HasPrefix(mimeType, "image/") {
					err = fmt.Errorf("file '%s' is not a supported image type (MIME: %s)", path, mimeType)
				} else {
					// Send the image data to the live session.
					l.mu.RLock()
					session := l.session
					l.mu.RUnlock()

					if session == nil {
						err = fmt.Errorf("live session is not active, cannot upload image")
					} else {
						// Use SendClientContent to send the image as a new user turn. This avoids
						// concurrency issues with SendRealtimeInput used for audio streaming and
						// correctly represents the image as a discrete piece of user-provided context.
						parts := []*genai.Part{genai.NewPartFromBytes(data, mimeType)}
						turn := genai.NewContentFromParts(parts, genai.RoleUser)
						content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}
						l.writeMu.Lock()
						sendErr := session.SendClientContent(content)
						l.writeMu.Unlock()
						if sendErr != nil {
							err = fmt.Errorf("failed to send image to session: %w", sendErr)
						} else {
							log.Printf("Successfully sent image '%s' to live session.", path)
							result = map[string]any{"status": fmt.Sprintf("image '%s' uploaded successfully", path)}
						}
					}
				}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}
