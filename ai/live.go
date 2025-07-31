package ai

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
	"gemini/wayland"
)

const (
	objectDetectionAgent = "objectDetection"
	pdfAwareAgent        = "pdfAwareAgent"
	youtubeAgent         = "youtubeAgent"
)

type LiveAI struct {
	ctx              context.Context
	client           *genai.Client
	agents           map[string]Callable
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
	streamPlayer     *audio.PCMStreamPlayer
	mode             string
	sessionClosed    chan struct{}
	resumptionHandle string
	warmUpDone       bool
	Online           bool
	odRoadMap        [3]bool
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

	// --- AppSink Initialization ---
	sink := helpers.Check(app.NewAppSink())
	helpers.Verify(sink.SetProperty("sync", false))
	sink.SetDrop(false)    // Do not drop data; ensure all samples are received for recording.
	sink.SetMaxBuffers(10) // Set a max buffer to prevent runaway memory usage and add stability.

	var streamPlayer *audio.PCMStreamPlayer
	if config.C.AI.VoiceEnabled {
		// In a CI environment without a running audio server, creating an 'autoaudiosink'
		// will fail. We check for its existence to avoid a fatal error.
		_, err := gst.NewElement("autoaudiosink")
		if err != nil {
			log.Printf("WARNING: Could not create autoaudiosink, voice output will be disabled. Error: %v", err)
		} else {
			streamPlayer, err = audio.NewPCMStreamPlayer(audio.TTSSampleRate, audio.TTSChannels)
			if err != nil {
				// This is a non-fatal error if the player can't be created, but we should log it.
				log.Printf("ERROR: Could not create PCM stream player, voice output will be disabled. Error: %v", err)
				streamPlayer = nil // Ensure it's nil on error
			}
		}
	}
	agents := make(map[string]Callable)
	// --- Agent Initialization ---
	{
		// Object Detection Agent
		bounds := helpers.Check(images.DisplayBounds())
		width := bounds.Dx()
		height := bounds.Dy()
		grid := ObjectDetectionNormalizationGrid
		halfGrid := grid / 2
		boundingBoxSystemInstructions := fmt.Sprintf(`You are an object detection specialist. 
The user will provide a query describing an object to find in the provided image. 
Your task is to locate that object and return its 2D bounding box.
The image dimensions are %d x %d (width x height). The origin (0,0) is at the top-left corner.
You MUST return the bounding box coordinates normalized to a %dx%d grid.
For example, for a 200x400 image, a point at (x=100, y=200) should be returned as (x=%d, y=%d).
Return the response as a JSON array with labels. Never return masks or code fencing. Limit to 25 objects.
If an object is present multiple times, name them according to their unique characteristic (colors, size, position, unique characteristics, etc..).`,
			width, height, grid, grid, halfGrid, halfGrid)
		agentConfig := AgentConfig{
			Name:              objectDetectionAgent,
			Model:             config.C.AI.ModelObjectDetection,
			SystemInstruction: boundingBoxSystemInstructions,
			Temperature:       helpers.Ptr(float32(0.0)),
			EnableTools:       false,
			ResponseSchema:    GetObjectDetectionSchema(),
		}
		agent := NewAgent(ctx, client, agentConfig)
		agents[objectDetectionAgent] = agent
	}

	{
		// PDF reader agent
		agentSystemInstructions := `You are an PDF document reader specialist. 
The user will provide a query with pdf document, read document please and provide concise and accurate response.`
		agentConfig := AgentConfig{
			Name:              pdfAwareAgent,
			Model:             config.C.AI.Model,
			SystemInstruction: agentSystemInstructions,
			Temperature:       helpers.Ptr(float32(0.0)),
			EnableTools:       false,
			ResponseSchema:    GetPdfReaderSchema(),
		}
		agent := NewAgent(ctx, client, agentConfig)
		agents[pdfAwareAgent] = agent
	}

	{
		// YouTube analysis agent
		agentSystemInstructions := `You are a comprehensive YouTube video analysis expert. Your goal is to extract as much meaningful information as possible from the provided video. Analyze both the audio and visual components to generate a detailed report.

Your response MUST be a single block of text and should be structured using Markdown headings for the following sections:

### Summary
Provide a concise, high-level summary of the video's main topic and purpose.

### Key Topics
Identify the main topics or chapters discussed in the video.

### Detailed Transcript with Visual Context
Provide a full and accurate transcript of the video's audio. Where relevant, interleave descriptions of important visual elements, on-screen text, or actions that provide context to the speech. For example: "[Visual: A diagram of a neural network is shown on screen]".

### Key Takeaways
List the most important points, conclusions, or actionable advice presented in the video.

Analyze the video thoroughly to provide a rich and informative response.`
		agentConfig := AgentConfig{
			Name:              youtubeAgent,
			Model:             config.C.AI.Model,
			SystemInstruction: agentSystemInstructions,
			Temperature:       helpers.Ptr(float32(0.2)),
			EnableTools:       false,
			ResponseSchema:    GetYoutubeAgentSchema(),
		}
		agent := NewAgent(ctx, client, agentConfig)
		agents[youtubeAgent] = agent
	}

	for _, agent := range agents {
		agent.WarmUp()
	}

	return &LiveAI{
		wg:               wg,
		ctx:              ctx,
		client:           client,
		agents:           agents,
		formatter:        inout.NewFormatter(),
		bus:              bus,
		controlChan:      controlChan,
		textCmdChan:      textCmdChan,
		liveSink:         sink,
		Element:          sink.Element,
		streamPlayer:     streamPlayer,
		isStreaming:      false,
		mode:             config.C.Mode,
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

	// 1. Configure the session based on global settings.
	var modelName string
	liveConfig := &genai.LiveConnectConfig{}
	// Models documentation https://ai.google.dev/gemini-api/docs/live
	//
	// Native audio models:
	// Model: gemini-2.5-flash-preview-native-audio-dialog, gemini-2.5-flash-exp-native-audio-thinking-dialog
	// Model inputs: Audio, videos, and text
	// Model outputs: Text and audio, interleaved
	// For responses with voice. Free RPD 5 per model
	// gemini-2.5-flash-preview-native-audio-dialog tools: Search, Function calling
	// gemini-2.5-flash-exp-native-audio-thinking-dialog
	// Tools: Search, Function calling
	//
	// Half-cascade audio models:
	// Model: gemini-live-2.5-flash-preview, gemini-2.0-flash-live-001
	// Model inputs: Audio, images, videos, and text
	// Model outputs: Text, Audio
	// For responses with text. Free RPD 250 per model
	// Tools: Search, Url context, Structured outputs, Function calling, Code execution
	if config.C.AI.VoiceEnabled {
		modelName = config.C.AI.ModelLiveTTS
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityAudio}
		liveConfig.SpeechConfig = &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: config.C.AI.Voice,
				},
			},
		}
	} else {
		modelName = config.C.AI.ModelLive
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityText}
	}

	// The role for a system instruction is empty.
	systemPrompt := config.C.AI.GetSystemInstruction()
	liveConfig.SystemInstruction = genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromText(systemPrompt),
	}, "")
	config.DebugPrintf("Using system prompt for live session: %s", systemPrompt)

	// Conditionally enable tools based on the configuration.
	// This is only done for the main response generation, not transcription.
	if config.C.AI.EnableTools {
		var tools []*genai.Tool
		if config.C.AI.EnableStandardTools {
			searchTool := &genai.Tool{GoogleSearch: &genai.GoogleSearch{}}
			if config.C.AI.URLContextDisabled {
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
			tools = append(tools, getFunctionTools())
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
	defer func() {
		if l.streamPlayer != nil {
			log.Println("Closing LiveAI stream player...")
			if err := l.streamPlayer.Close(); err != nil {
				log.Printf("ERROR: closing LiveAI stream player: %v", err)
			}
		}
	}()

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
					// If an old image buffer exists from a previous turn, release it.
					if l.imageBuffer != nil {
						log.Println("Releasing previous screenshot buffer for new turn.")
						l.imageBuffer.Release()
						l.imageBuffer = nil
					}
					log.Println("Taking live screenshot for AI response...")
					var err error
					l.imageBuffer, err = images.TakeScreenshot()
					// TODO: Remove after live testing
					go helpers.Verify(images.SaveImage("Screenshot.png", l.imageBuffer.Bytes()))
					l.mu.Unlock() // Unlock before logging and sending to avoid holding lock during I/O
					if err != nil {
						log.Printf("failed to take screenshot: %v", err)
					} else {
						l.sendLiveImage()
					}
				}
			case strings.HasPrefix(cmd, vad.MarkerStop):
				// The response is handled by the handleResponses goroutine.
				// No action needed here to process the stream.
				l.notifyStreamDone()
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

			continue
		}

		// Process the content of the message using a switch for clarity.
		switch {
		case msg.SetupComplete != nil:
			log.Println("Live session setup complete.")
		case msg.ServerContent != nil:
			if msg.ServerContent.ModelTurn != nil {
				if !inModelTurn {
					// Do once per content block
					log.Println("Live stream generation started.")
					inModelTurn = true
					turnGroundingChunks = nil
					(*l.bus).Publish("main:topic", "mute:ai.handleResponses")
					l.formatter.Clear()
					l.formatter.Println("\nAnswer:", inout.ColorDarkCyan)
				}
				// Do on each turn
				l.processModelTurnParts(msg.ServerContent.ModelTurn.Parts)
			}

			if msg.ServerContent.GroundingMetadata != nil {
				// Turn has metadata
				turnGroundingChunks = append(turnGroundingChunks, msg.ServerContent.GroundingMetadata.GroundingChunks...)
			}

			if msg.ServerContent.GenerationComplete {
				// Do once per content block
				log.Println("Live stream generation complete.")
				l.printGroundingChunks(turnGroundingChunks)
				inModelTurn = false
				l.formatter.Reset()
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
func (l *LiveAI) processModelTurnParts(parts []*genai.Part) {
	for _, part := range parts {
		config.DebugPrintf("Live stream received part: %+v", part)
		if part.Text != "" {
			l.formatter.Print(part.Text)
		}
		if part.InlineData != nil && len(part.InlineData.Data) > 0 {
			config.DebugPrintf("Live stream received data blob: %s, size: %d", part.InlineData.MIMEType, len(part.InlineData.Data))
			if l.streamPlayer != nil {
				if err := l.streamPlayer.Write(part.InlineData.Data); err != nil {
					log.Printf("ERROR: writing to audio stream: %v", err)
				}
			}
		}
	}
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

	var parts []*genai.Part

	// Send the introductory prompt first.
	if config.C.AI.CacheSystemPrompt != "" {
		parts = append(parts, genai.NewPartFromText(config.C.AI.CacheSystemPrompt))
	}

	// Send each file as a separate turn.
	for _, file := range filesToInclude {
		localPath := filepath.Join(cacheDir, file.Name())
		document, err := l.client.Files.UploadFromPath(l.ctx, localPath, &genai.UploadFileConfig{
			MIMEType: "text/plain",
		})
		if err != nil {
			log.Printf("ERROR: could not read file %s for live session context: %v", localPath, err)
			continue
		}
		log.Printf("Cache file %s succefully uploaded", localPath)
		part := genai.NewPartFromURI(document.URI, document.MIMEType)
		parts = append(parts, part)
	}
	parts = append(parts, genai.NewPartFromText(CheckQuestion))

	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	l.writeMu.Lock()
	if err := l.session.SendClientContent(content); err != nil {
		log.Printf("failed to send client content: %v", err)
	}
	l.writeMu.Unlock()
}

func (l *LiveAI) sendLiveMessage(text string) {
	online := l.Online

	if !online {
		return
	}
	log.Println("Sending live message to session context...")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		Text: text,
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf("ERROR: failed to send realtime image input: %v", err)
		// Stop streaming on error to prevent flooding with more errors.
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
	parts := []*genai.Part{genai.NewPartFromText(prompt)}

	if l.mode == inout.ImageMode {
		l.mu.Lock()
		// Release any old buffer and take a new screenshot for this turn.
		if l.imageBuffer != nil {
			log.Println("Releasing previous screenshot buffer for new text prompt turn.")
			l.imageBuffer.Release()
		}
		log.Println("Taking screenshot for AI response...")
		var err error
		l.imageBuffer, err = images.TakeScreenshot()
		l.mu.Unlock()
		if err != nil {
			return fmt.Errorf("failed to take screenshot: %w", err)
		}
		parts = append(parts, genai.NewPartFromBytes(l.imageBuffer.Bytes(), "image/png"))
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

func (l *LiveAI) notifyStreamDone() {
	online := l.Online

	if !online {
		return
	}
	log.Println("Live stream voice activity complete, ending stream")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		AudioStreamEnd: true,
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf("ERROR: failed to send realtime image input: %v", err)
		// Stop streaming on error to prevent flooding with more errors.
	}
}

// Explicit activity control is not supported when automatic activity detection is enabled.
func (l *LiveAI) notifyActivityStart() {
	online := l.Online

	if !online {
		return
	}
	log.Println("Live stream voice activity started")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		ActivityStart: &genai.ActivityStart{},
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf("ERROR: failed to send realtime image input: %v", err)
		// Stop streaming on error to prevent flooding with more errors.
	}
}

// Explicit activity control is not supported when automatic activity detection is enabled.
func (l *LiveAI) notifyActivityEnd() {
	online := l.Online

	if !online {
		return
	}
	log.Println("Live stream activity ended")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		ActivityEnd: &genai.ActivityEnd{},
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf("ERROR: failed to send realtime image input: %v", err)
		// Stop streaming on error to prevent flooding with more errors.
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
		case "verifyObjectDetection":
			response = l.handleVerifyObjectDetectionTool(call)
		case "mouseClick":
			response = l.handleMouseClickTool(call)
		case "readPdf":
			response = l.handleReadPdfTool(call)
		case "analyzeYoutubeVideo":
			response = l.handleYoutubeAnalysisTool(call)
		default:
			response = executeSingleToolCall(call)
		}
		responses = append(responses, response)
	}
	return responses
}

func (l *LiveAI) handleReadPdfTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// 1. Parse arguments
	path, pathOK := call.Args["path"].(string)
	query, queryOK := call.Args["query"].(string)

	if !pathOK || path == "" || !queryOK || query == "" {
		err = fmt.Errorf("'path' and 'query' arguments are required and must be non-empty strings")
	} else {
		// 2. Get safe path and read file
		safePath, pathErr := getSafePath(path)
		if pathErr != nil {
			err = pathErr
		} else {
			pdfBytes, readErr := os.ReadFile(safePath)
			if readErr != nil {
				err = fmt.Errorf("failed to read PDF file '%s': %w", path, readErr)
			} else {
				// 3. Get and use the agent
				agent, ok := l.agents[pdfAwareAgent]
				if !ok {
					err = fmt.Errorf("PDF reader agent not initialized")
				} else {
					// 4. Process with the agent
					summary, processErr := agent.Process(query, genai.NewPartFromBytes(pdfBytes, "application/pdf"))
					if processErr != nil {
						err = fmt.Errorf("PDF processing failed: %w", processErr)
					} else {
						log.Printf("PDF processing successful for query: '%s'", query)
						result = map[string]any{"summary": summary}
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

func (l *LiveAI) handleYoutubeAnalysisTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// 1. Parse arguments
	url, urlOK := call.Args["url"].(string)
	query := "Please analyze the provided video and generate a comprehensive report based on your instructions."

	if !urlOK || url == "" {
		err = fmt.Errorf("'url' argument is required and must be a non-empty string")
	} else {
		// 2. Get and use the agent
		agent, ok := l.agents[youtubeAgent]
		if !ok {
			err = fmt.Errorf("YouTube analysis agent not initialized")
		} else {
			// 3. Process with the agent. The Gemini API accepts various video MIME types,
			// but "video/mp4" is recommended in documentation for YouTube URLs.
			resultText, processErr := agent.Process(query, genai.NewPartFromURI(url, "video/mp4"))
			if processErr != nil {
				err = fmt.Errorf("YouTube video processing failed: %w", processErr)
			} else {
				log.Printf("YouTube video analysis successful for url: '%s'", url)
				result = map[string]any{"result": resultText}
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

func (l *LiveAI) handleMouseClickTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Enforce the correct tool-use sequence.
	if !l.odRoadMap[0] || !l.odRoadMap[1] {
		err = fmt.Errorf("you must get positive approve from 'verifyObjectDetection' tool before apply mouse actions")
	} else {
		// 1. Parse arguments
		xNorm, xOK := call.Args["x"].(float64)
		yNorm, yOK := call.Args["y"].(float64)
		clicksFloat, _ := call.Args["clicks"].(float64)

		if !xOK || !yOK {
			err = fmt.Errorf("arguments 'x' and 'y' are required and must be numbers")
		} else {
			// 2. Get image dimensions from session buffer
			l.mu.RLock()
			imageBuf := l.imageBuffer
			l.mu.RUnlock()

			if imageBuf == nil || imageBuf.Len() == 0 {
				err = fmt.Errorf("no image found in the current session context to calculate click coordinates")
			} else {
				// 3. Decode image config to get bounds efficiently
				imgConfig, _, decodeErr := image.DecodeConfig(bytes.NewReader(imageBuf.Bytes()))
				if decodeErr != nil {
					err = fmt.Errorf("failed to decode screenshot config for click: %w", decodeErr)
				} else {
					// 4. Denormalize coordinates
					imgWidth := float64(imgConfig.Width)
					imgHeight := float64(imgConfig.Height)

					absX := int((xNorm / float64(ObjectDetectionNormalizationGrid)) * imgWidth)
					absY := int((yNorm / float64(ObjectDetectionNormalizationGrid)) * imgHeight)

					clicks := int(clicksFloat)
					if clicks < 1 {
						clicks = 1
					}

					log.Printf("Performing %d mouse click(s) at absolute pixel coordinates (%d, %d)", clicks, absX, absY)

					// 5. Execute the desktop automation.
					wayland.MoveMouseToPosition(absX, absY)
					time.Sleep(100 * time.Millisecond)
					wayland.MouseLeftClick(clicks)

					result = map[string]any{"status": fmt.Sprintf("%d mouse click(s) performed at (%d, %d)", clicks, absX, absY)}
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

	l.odRoadMap = [3]bool{false, false, false}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}

func (l *LiveAI) handleVerifyObjectDetectionTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Enforce the correct tool-use sequence.
	if !l.odRoadMap[0] {
		err = fmt.Errorf("you must call 'detectObjects' successfully before you can verify the result")
	} else {
		// 1. Parse arguments from the tool call
		xminNorm, xminOK := call.Args["xmin"].(float64)
		yminNorm, yminOK := call.Args["ymin"].(float64)
		xmaxNorm, xmaxOK := call.Args["xmax"].(float64)
		ymaxNorm, ymaxOK := call.Args["ymax"].(float64)

		if !xminOK || !yminOK || !xmaxOK || !ymaxOK {
			err = fmt.Errorf("invalid or missing normalized bounding box arguments (xmin, ymin, xmax, ymax)")
		} else {
			// 2. Get the original screenshot from the session buffer
			l.mu.RLock()
			imageBuf := l.imageBuffer
			l.mu.RUnlock()

			if imageBuf == nil || imageBuf.Len() == 0 {
				err = fmt.Errorf("no image found in the current session context to verify")
			} else {
				// 3. Decode the image
				originalImg, decodeErr := png.Decode(bytes.NewReader(imageBuf.Bytes()))
				if decodeErr != nil {
					err = fmt.Errorf("failed to decode screenshot for verification: %w", decodeErr)
				} else {
					// 4. Get image dimensions and denormalize coordinates
					bounds := originalImg.Bounds()
					imgWidth := float64(bounds.Dx())
					imgHeight := float64(bounds.Dy())

					xmin := int((xminNorm / float64(ObjectDetectionNormalizationGrid)) * imgWidth)
					ymin := int((yminNorm / float64(ObjectDetectionNormalizationGrid)) * imgHeight)
					xmax := int((xmaxNorm / float64(ObjectDetectionNormalizationGrid)) * imgWidth)
					ymax := int((ymaxNorm / float64(ObjectDetectionNormalizationGrid)) * imgHeight)

					// 5. Draw the rectangle
					rect := image.Rect(xmin, ymin, xmax, ymax)
					imgWithBox := images.DrawRectangle(originalImg, rect, 3, color.RGBA{R: 255, A: 255}) // Red box, 3px thick

					// 6. Encode the new image back to a PNG buffer
					newImageBuf := new(bytes.Buffer)
					if encodeErr := png.Encode(newImageBuf, imgWithBox); encodeErr != nil {
						err = fmt.Errorf("failed to encode verification image: %w", encodeErr)
					} else {
						// TODO: remove after object detection live testing
						go helpers.Verify(images.SaveImage("Detect.png", newImageBuf.Bytes()))
						// 7. Send the new image and a verification prompt to the session
						parts := []*genai.Part{
							genai.NewPartFromText("Tool have drawn the red box according to provided coordinates. Is the user requested object to detect inside the red box correctly identified? If not, try resolve this issue without user confirmation"),
							genai.NewPartFromBytes(newImageBuf.Bytes(), "image/png"),
						}
						turn := genai.NewContentFromParts(parts, genai.RoleUser)
						content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

						l.mu.RLock()
						session := l.session
						l.mu.RUnlock()

						if session == nil {
							err = fmt.Errorf("live session is not active, cannot send verification image")
						} else {
							l.writeMu.Lock()
							sendErr := session.SendClientContent(content)
							l.writeMu.Unlock()
							if sendErr != nil {
								err = fmt.Errorf("failed to send verification image to session: %w", sendErr)
							} else {
								log.Println("Successfully sent verification image to live session.")
								result = map[string]any{"status": "Verification image sent. Awaiting confirmation."}
							}
						}
					}
				}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	} else {
		// Mark this step as complete on the roadmap only on success.
		l.odRoadMap[1] = true
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

// handleDetectObjectsTool processes the 'detectObjects' tool call.
// It uses a specialized Agent to analyze an image from the current session's
// image buffer and return the findings.
// Cookbook: https://github.com/google-gemini/cookbook/blob/d1eed253584683b1a435783cf5f319bb235aea97/quickstarts/Spatial_understanding.ipynb
func (l *LiveAI) handleDetectObjectsTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing LiveAI tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Get the query from the tool call arguments.
	query, ok := call.Args["query"].(string)
	if !ok || query == "" {
		err = fmt.Errorf("'query' argument is required and must be a non-empty string")
	} else {
		// Add a guardrail to prevent the model from sending overly simplistic queries by
		// checking for a minimum number of words. This is more robust than checking
		// character length, as the model can't bypass it with extra spaces.
		const minWordCount = 5
		if len(strings.Fields(query)) < minWordCount {
			err = fmt.Errorf("query '%s' is not descriptive enough (must be at least %d words). Please provide a more descriptive query, for example: 'the blue \"Submit\" button in the center of the form'", query, minWordCount)
		} else {
			// This tool uses the session's image buffer.
			l.mu.RLock()
			imageBuf := l.imageBuffer
			l.mu.RUnlock()

			if imageBuf == nil || imageBuf.Len() == 0 {
				err = fmt.Errorf("no image found in the current session context to detect objects from")
			} else {
				// Get screen dimensions to provide context to the model.
				bounds, boundsErr := images.DisplayBounds()
				if boundsErr != nil {
					err = fmt.Errorf("failed to get display bounds for object detection context: %w", boundsErr)
				} else {
					log.Printf("Object detection image size %d x %d (width x height).", bounds.Dx(), bounds.Dy())
					// Create the agent with a specific system prompt for object detection.
					agent, ok := l.agents[objectDetectionAgent]
					if !ok {
						err = fmt.Errorf("object detection agent not initialized")
					} else {
						// Process the image with the agent, using the query from the tool call as the prompt.
						// The image buffer is PNG encoded.
						detectionResult, processErr := agent.Process(query, genai.NewPartFromBytes(imageBuf.Bytes(), "image/png"))
						if processErr != nil {
							err = fmt.Errorf("object detection failed: %w", processErr)
						} else {
							log.Printf("Object detection successful for query: '%s'", query)
							result = map[string]any{"detected_objects": detectionResult}
						}
					}
				}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	} else {
		// Mark this step as complete on the roadmap only on success.
		l.odRoadMap[0] = true
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
