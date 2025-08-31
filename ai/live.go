package ai

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"google.golang.org/genai"

	"gemini/agents"
	"gemini/audio"
	"gemini/config"
	"gemini/desktop"
	"gemini/flow"
	"gemini/helpers"
	"gemini/images"
	"gemini/inout"
	"gemini/vad"
	"gemini/video"
)

type StreamType int

const (
	None        StreamType = 0
	AudioStream StreamType = 1 << iota // 1
	ShellStream                        // 2
	VideoStream                        // 4
	All         = AudioStream | ShellStream | VideoStream
)

// String provides a human-readable representation of the StreamType,
// handling single and combined flags.
func (s StreamType) String() string {
	if s == None {
		return "None"
	}

	var parts []string
	if s&AudioStream != 0 {
		parts = append(parts, "Audio")
	}
	if s&ShellStream != 0 {
		parts = append(parts, "Shell")
	}
	if s&VideoStream != 0 {
		parts = append(parts, "Video")
	}

	return strings.Join(parts, "|")
}

// Package-level constant for sendLiveInputErrorPrefix, without the %v placeholder.
const sendLiveInputErrorPrefix = "ERROR: failed to send realtime input: "

type LiveAI struct {
	ctx              context.Context
	client           *genai.Client
	agents           map[string]agents.Callable
	formatter        *inout.Formatter
	liveSink         *app.Sink
	Element          *gst.Element
	wg               *sync.WaitGroup
	flags            *Flags
	controlChan      <-chan string
	textCmdChan      <-chan string
	videoFrameChan   chan []byte // Channel for incoming video frames
	bus              *EventBus.Bus
	session          *genai.Session
	imageBuffer      *images.ScreenshotBuffer
	cli              *inout.CLI
	toolset          *genai.Tool
	streamPlayer     *audio.PCMStreamPlayer
	activities       StreamType
	mode             string
	videoStream      *video.VideoStreamComponent
	sessionClosed    chan struct{}
	resumptionHandle string
	warmUpDone       bool
	vadDisabled      bool
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
	flags *Flags,
	cli *inout.CLI,
	videoFrameChan chan []byte, // Accept video frame channel
) *LiveAI {
	ctx := context.Background()
	client := helpers.Check(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	}))
	// --- AppSink Initialization ---
	sink := helpers.Check(app.NewAppSink())
	sink.SetSync(false)
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

	// --- Agent Initialization ---
	// Create an empty toolset that will be populated by the agents.
	toolset := agents.NewToolSet()

	// Initialize each agent, passing the toolset to them.
	// Each agent's constructor will add its functions to the toolset and
	// register its handler in the AgentRegistry via the Registerate function.
	agents.BuildAgentNetwork(ctx, client, toolset, bus)

	return &LiveAI{
		wg:               wg,
		ctx:              ctx,
		flags:            flags,
		client:           client,
		agents:           agents.AgentRegistry,
		formatter:        inout.NewFormatter(),
		bus:              bus,
		controlChan:      controlChan,
		textCmdChan:      textCmdChan,
		videoFrameChan:   videoFrameChan, // Store video frame channel
		liveSink:         sink,
		Element:          sink.Element,
		streamPlayer:     streamPlayer,
		toolset:          toolset,
		cli:              cli,
		videoStream:      nil, // Will be created on demand
		activities:       None,
		mode:             config.C.Mode,
		sessionClosed:    make(chan struct{}, 1), // Buffered channel to prevent blocking
		resumptionHandle: "",
		warmUpDone:       false,
		vadDisabled:      config.C.VAD.DisableNativeVAD,
		Online:           false,
	}
}

// startActivity adds an activity flag and notifies the model if it's the first one.
func (l *LiveAI) startActivity(activity StreamType) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// If this is the first activity starting, notify the model.
	if l.activities == None {
		l.notifyActivityStart(activity)
	}
	l.activities |= activity
}

// stopActivity removes an activity flag and notifies the model if it's the last one.
func (l *LiveAI) stopActivity(activity StreamType) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.activities &= ^activity

	// If this was the last activity, signal the end of the entire turn.
	if l.activities == None {
		l.notifyActivityEnd(activity)
	} else {
		log.Printf("Live can not stop activity. Exists another activity: %v", l.activities)
	}
}

func (l *LiveAI) isActive(activity StreamType) bool {
	locked := l.mu.TryLock()
	defer func() {
		if locked {
			l.mu.Unlock()
		}
	}()
	return l.activities&activity != None
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
	// We can use model native or application provided VAD control
	liveConfig.RealtimeInputConfig = &genai.RealtimeInputConfig{
		TurnCoverage:     genai.TurnCoverageTurnIncludesOnlyActivity,
		ActivityHandling: genai.ActivityHandlingStartOfActivityInterrupts,
		AutomaticActivityDetection: &genai.AutomaticActivityDetection{
			Disabled:                 l.vadDisabled,
			StartOfSpeechSensitivity: genai.StartSensitivityLow,
			EndOfSpeechSensitivity:   genai.EndSensitivityLow,
			PrefixPaddingMs:          helpers.Ptr(int32(config.C.VAD.SilenceThreshold * 1000)),
			SilenceDurationMs:        helpers.Ptr(int32(config.C.VAD.HangoverDurationSec * 1000)),
		},
	}

	// Input audio transcript
	if config.C.AI.Transcript {
		liveConfig.InputAudioTranscription = &genai.AudioTranscriptionConfig{}
	}

	// Video input will be handled by sending Video blobs directly via SendRealtimeInput().
	if config.C.Video.Enabled {
		log.Println("Live session: Video input enabled (configured by sending Video blobs).")
	}

	// You can only set one response modality (TEXT or AUDIO) per session in the session configuration.
	// Setting both results in a config error message. This means that you can configure the model to
	// respond with either text or audio, but not both in the same session.
	if config.C.AI.VoiceEnabled {
		modelName = config.C.AI.ModelLiveTTS
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityAudio}
		liveConfig.OutputAudioTranscription = &genai.AudioTranscriptionConfig{}
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
			tools = append(tools, l.toolset)
			log.Println("Function calling tools enabled for live session.")
		}

		if len(tools) > 0 {
			liveConfig.Tools = tools
			if config.IsDebug() {
				var toolNames []string
				for _, tool := range tools {
					if tool.GoogleSearch != nil {
						toolNames = append(toolNames, "GoogleSearch")
					}
					if tool.URLContext != nil {
						toolNames = append(toolNames, "URLContext")
					}
					if tool.CodeExecution != nil {
						toolNames = append(toolNames, "CodeExecution")
					}
					if len(tool.FunctionDeclarations) > 0 {
						for _, fd := range tool.FunctionDeclarations {
							toolNames = append(toolNames, "Function:"+fd.Name)
						}
					}
				}
				// Corrected: Ensure log.Printf format string is correct for dynamic toolNames
				config.DebugPrintf("Opening live session with tools: [%s]", strings.Join(toolNames, ", "))
			}
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
			// Corrected: cast to int64 before passing to Ptr
			compressionConfig.TriggerTokens = helpers.Ptr(int64(config.C.AI.ContextWindowCompression.TriggerTokens))
		} else {
			log.Println("Using default TriggerTokens.")
		}
		if config.C.AI.ContextWindowCompression.TargetTokens > 0 {
			log.Printf("Using custom TargetTokens: %d", config.C.AI.ContextWindowCompression.TargetTokens)
			// Corrected: cast to int64 before passing to Ptr
			compressionConfig.SlidingWindow.TargetTokens = helpers.Ptr(int64(config.C.AI.ContextWindowCompression.TargetTokens))
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
	helpers.Verify((*l.bus).SubscribeAsync(config.MainTopic, func(event string) {
		if strings.HasPrefix(event, "ready:") {
			l.mu.Lock()
			if !l.warmUpDone {
				log.Println("Live AI warm-up complete. Now actively listening for events.")
				l.warmUpDone = true
			}
			l.mu.Unlock()
		}
	}, false))
	helpers.Verify((*l.bus).Subscribe(config.AITopic, l.handleEvents))
	helpers.Verify((*l.bus).SubscribeAsync(config.AgentTopic, l.handleAgentToolResponse, false))

	// Block system mode output for case when in config set as default mode
	l.cli.ReceiveShellPause(inout.ShellPauseStart)
	// Initialize first session with Model
	l.OpenSession()
	// Start a dedicated goroutine to handle all incoming Model messages.
	go l.handleResponses()
	// Send initial files only once at the beginning of the session.
	// This must be done after the response handler is running to catch the server's acknowledgment.
	l.sendInitialFiles()
	// Use a ticker to poll for new samples without running a 100% CPU busy-loop.
	audioTicker := time.NewTicker(20 * time.Millisecond) // Renamed from 'ticker'
	defer audioTicker.Stop()
	// Use a separate ticker to poll for shell output from the CLI.
	shellPollTicker := time.NewTicker(250 * time.Millisecond)
	defer shellPollTicker.Stop()
	// Application flow control channel
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
			// The only one place where we accumulate user voice control interactions
			if !ok {
				log.Println("Live AI streaming is finished")
				return
			}
			switch {
			case strings.HasPrefix(cmd, vad.MarkerStart):
				if l.isActive(AudioStream) {
					l.mu.Unlock()
					log.Println("WARNING: VAD start detected while an audio stream is already active. Ignoring.")
					continue
				}

				// If an activity is not already in progress, this voice input will start a new audio activity.
				// If a text activity is in progress, this voice input will be streamed as part of it,
				// and the subsequent VAD Stop will terminate the entire turn.
				l.startActivity(AudioStream)

				// The native audio models perform their own VAD (automatic activity detection).
				// Sending explicit ActivityStart/ActivityEnd signals conflicts with this,
				// causing a websocket error. We still use our application's VAD to control when we
				// *stream* audio to the API, but we must provide fresh time context for each turn
				// because the initial system prompt's time becomes stale.
				log.Println("VAD Start: beginning to stream audio to Live API.")
				currentTime := config.FormatTimeWithTimezone(config.C.AI.Timezone)
				l.sendLiveMessage(fmt.Sprintf("The current time is %s.", currentTime))

				if l.mode == inout.ImageMode {
					l.mu.Lock()
					// If an old image buffer exists from a previous turn, release it.
					if l.imageBuffer != nil {
						l.imageBuffer.Release()
						l.imageBuffer = nil
					}
					var err error
					l.imageBuffer, err = desktop.C.CaptureScreen()
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
				log.Println("VAD Stop: finishing turn.")

				// Drain any remaining audio that's already in the sink's queue
				// before we mark the stream and activity as inactive.
				l.pullAndSendSamples()

				// The user's turn is over, whether it was a pure audio turn or a text turn
				// concluded by voice. Reset the state for the next turn.
				// If an activity was in progress, end it. We always send AudioStream type because
				// a VAD stop means audio was just sent, which needs to be terminated correctly.
				if l.cli.GetShellState() {
					l.stopActivity(AudioStream)
				} else {
					l.stopActivity(AudioStream | ShellStream)
				}

				// The image buffer is now released upon GenerationComplete, not here.
			default:
				log.Printf("WARNING: received unknown control command: %s", cmd)
			}
		case textCmd, ok := <-l.textCmdChan:
			// The only one place where we accumulate interactions: user text prompt to model
			if !ok {
				l.textCmdChan = nil // Mark as closed
				continue
			}

			// If we are in system mode and there's an open shell activity,
			// this text prompt is the user's question about that activity.
			// We send it and then end the activity to trigger a model response.
			switch l.mode {
			case inout.SystemMode:
				// CLI provide a non-empty user prompt, send it as the final question.
				log.Println("Live AI: Sending user system mode prompt...")
				l.sendLiveMessage(textCmd)

				// In case of voice activity we will wait until it ends and finalize turn.
				// Otherwise, just end the activity to get a response to both the shell output and user prompt.
				l.stopActivity(ShellStream)
			case inout.VideoMode:
				log.Println("Live AI: Sending user video mode prompt...")
				l.sendLiveMessage(textCmd)

				// We need stop activity for model turn started.
				// New activiti will be open automatically on first video frame arrived
				l.stopActivity(VideoStream)
			default:
				// This is a regular, self-contained text prompt. Ignore if empty.
				if textCmd != "" {
					log.Printf("Live AI: Processing text prompt in %s mode...\n", l.mode)
					go func(prompt string) {
						if err := l.sendTextPrompt(prompt); err != nil {
							log.Printf("ERROR: failed to process text prompt: %v", err)
						}
					}(textCmd)
				}
			}
		case frame, ok := <-l.videoFrameChan: // Handle incoming video frames
			// In video mode we got new continuous activity type: user video to model
			if !ok {
				l.videoFrameChan = nil // Mark as closed
				continue
			}
			if config.C.Video.Enabled {
				l.sendLiveVideoFrame(frame)
			}
		case <-audioTicker.C: // Use the renamed audioTicker
			// App synk voice chunk processing, interaction: user voice to model
			l.pullAndSendSamples()
		case <-shellPollTicker.C:
			// In system mode there is third interaction type: shell output to model.
			// Shell have never finalize turn, only stream output data
			if l.mode == inout.SystemMode {
				// genai API has error and panic on big ammounts of streamed data(ex: journalctl).
				// This "pull and batch" pattern is crucial for stability. Instead of
				// sending every line of shell output as it occurs (which can overwhelm
				// the API and cause errors or panics), we collect output in the CLI's
				// buffer and fetch it in 250ms intervals. This coalesces rapid-fire
				// output into a single, manageable chunk for the model.
				if output := l.cli.ReceiveShellOutput(); output != "" {
					l.handleShellOutput(output)
				}
			}
		}
	}
}

// handleAgentToolResponse handles delayed/asynchronous tool responses published by agents.
func (l *LiveAI) handleAgentToolResponse(response *genai.FunctionResponse) {
	log.Printf("Received delayed tool response for call ID %s", response.ID)

	// Lock the session for reading.
	l.mu.RLock()
	session := l.session
	l.mu.RUnlock()

	if session == nil {
		log.Printf("WARNING: Session is nil, cannot send delayed tool response for call ID %s", response.ID)
		return
	}

	l.handleSendContent(response)

	toolInput := genai.LiveToolResponseInput{FunctionResponses: []*genai.FunctionResponse{response}}

	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if err := session.SendToolResponse(toolInput); err != nil {
		log.Printf("ERROR: failed to send delayed tool response: %v", err)
	}
}

// handleSendContent checks for and processes the special "send_content" key in a tool response.
// It sends the content to the live session and removes the key from the response map.
func (l *LiveAI) handleSendContent(response *genai.FunctionResponse) {
	if response == nil || response.Response == nil {
		return
	}

	content, ok := response.Response["send_content"].(genai.LiveClientContentInput)
	if !ok {
		return
	}

	// Lock for reading the session pointer.
	l.mu.RLock()
	session := l.session
	l.mu.RUnlock()

	if session != nil {
		l.writeMu.Lock()
		if err := session.SendClientContent(content); err != nil {
			// Use a generic log message that fits both sync and async contexts.
			log.Printf("ERROR: failed to send agent content to session for tool call '%s': %v", response.Name, err)
		}
		l.writeMu.Unlock()
	}

	delete(response.Response, "send_content")
}

// handleResponses runs in a dedicated goroutine, processing all messages from the server.
// Must be run only once to avoid double processing of responses
func (l *LiveAI) handleResponses() {
	var needToGo bool
	var generation bool
	var inModelTurn bool   // State to track if we are in the middle of a model's turn.
	var inTranscript bool  // State to track if we are in the middle of a model's turn.
	var outTranscript bool // State to track if we are in the middle of a model's turn.
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

			// On a receive error, signal the main Run() loop to re-establish the
			// connection. This goroutine will then 'continue' and wait in its
			// 'session is temporarily unavailable' block until the new session is ready.
			// This ensures this goroutine is a long-lived singleton.
			continue
		}

		// Process the content of the message using a switch for clarity.
		switch {
		case msg.SetupComplete != nil:
			// Resume shell output polling now that the new session is ready.
			l.cli.ReceiveShellPause(inout.ShellPauseStop)
			log.Println("Live session setup complete.")
		case msg.ServerContent != nil:
			l.processTranscript(msg.ServerContent.InputTranscription, &inTranscript, "Transcript:")
			l.processTranscript(msg.ServerContent.OutputTranscription, &outTranscript, "")

			if msg.ServerContent.ModelTurn != nil {
				if !inModelTurn {
					// Do once per content block
					log.Println("Live model stream generation started.")
					inModelTurn = true
					turnGroundingChunks = nil
					(*l.bus).Publish(config.MainTopic, "mute:ai.handleResponses")

					// Usuely transcript clear screen.
					if !config.C.AI.Transcript {
						l.formatter.Clear()
					}

					// System mode sensetive to new lines, avoid this
					if l.mode != inout.SystemMode {
						l.formatter.PrintNl("Answer:", inout.ColorDarkCyan)
					}
				}
				// Do on each turn for text or voice data
				l.processModelTurnParts(msg.ServerContent.ModelTurn.Parts)
			}

			if msg.ServerContent.GroundingMetadata != nil {
				// Turn has metadata
				turnGroundingChunks = append(turnGroundingChunks, msg.ServerContent.GroundingMetadata.GroundingChunks...)
			}

			if msg.ServerContent.GenerationComplete {
				// Do once per content block
				log.Println("Live model stream generation complete.")
				l.printGroundingChunks(turnGroundingChunks)
				inTranscript = false
				outTranscript = false
				inModelTurn = false
				l.formatter.Reset()
				(*l.bus).Publish(config.MainTopic, "draw:ai.handleResponses")
			}
		case msg.ToolCall != nil:
			go func(request *genai.LiveServerToolCall) {
				log.Printf("Live stream received %d tool call(s) request.", len(request.FunctionCalls))
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
			// Pause shell output polling to prevent sending data to a closed session.
			l.cli.ReceiveShellPause(inout.ShellPauseStart)
			// If a shell activity is in progress, end it gracefully before closing the session.
			l.stopActivity(ShellStream)

			if generation {
				needToGo = true
			} else {
				fireClose()
			}
		case msg.SessionResumptionUpdate != nil:
			if msg.SessionResumptionUpdate.Resumable {
				generation = false
				log.Printf("Live session resumption handle updated. New handle received.")
				l.resumptionHandle = msg.SessionResumptionUpdate.NewHandle
				// When a GoAway signal is received during a generation, we set 'needToGo'
				// to true. This ensures that we don't close the session immediately.
				// Instead, we wait for this final 'Resumable' update, which contains
				// the handle needed to resume the session later. Once we have the handle,
				// we can safely close the connection.
				if needToGo {
					needToGo = false
					fireClose()
				}
			} else {
				generation = true
				log.Printf("Live session resumption handle generates. Wait please.")
			}
		default:
			config.DebugPrintf("Live AI received unhandled message: %+v", msg)
		}

		if msg.UsageMetadata != nil {
			// The log.Printf format string was corrected in the previous iteration.
			log.Printf("Live stream usage metadata received: InT:%d, OutT:%d, Tot:%d",
				msg.UsageMetadata.PromptTokenCount,
				msg.UsageMetadata.ResponseTokenCount,
				msg.UsageMetadata.TotalTokenCount)
		}
	}
}

// processTranscript handles the printing of both input and output transcriptions
// from the server, managing the state to correctly format the output.
func (l *LiveAI) processTranscript(transcript *genai.Transcription, inProgress *bool, label string) {
	if transcript == nil {
		return
	}

	if !*inProgress {
		config.DebugPrintln("Transcript generetion started")
		*inProgress = true
		if label != "" {
			l.formatter.Clear()
			l.formatter.PrintNl(fmt.Sprintf("\n%s", label), inout.ColorDarkCyan)
		}
	}

	// Always print the text part of the transcript chunk.
	l.formatter.Print(transcript.Text)
}

// processModelTurnParts handles the processing of text and audio parts from a model's turn.
func (l *LiveAI) processModelTurnParts(parts []*genai.Part) {
	for _, part := range parts {
		config.DebugPrintf("Live stream received part: %+v", part)
		if part.Text != "" {
			l.formatter.Print(part.Text)
		}

		if part.ExecutableCode != nil {
			config.DebugPrintf("Live stream received executable code part: %+v", part.ExecutableCode)
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
	l.formatter.PrintNl("Sources:", inout.ColorDarkYellow)
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
	config.DebugPrintf("LiveAI component received event: %s", event)
	parts := strings.SplitN(event, ":", 2)
	if len(parts) < 2 {
		log.Printf("WARNING: received malformed LiveAI event: %s", event)
		return
	}
	command, payload := parts[0], parts[1]

	switch command {
	case "mode":
		previousMode := l.mode
		if previousMode != payload {
			l.mode = payload
			log.Printf("LiveAI mode set to: %s", payload)

			// Stop video if switching away from video mode
			if previousMode == inout.VideoMode && l.videoStream != nil {
				log.Println("Switching away from video mode, stopping video stream.")
				l.videoStream.Stop()
				l.videoStream = nil
				l.stopActivity(VideoStream) // Stop the activity on error
			}

			// Handle System mode transitions
			if payload == inout.SystemMode {
				l.startActivity(ShellStream)
				l.sendLiveMessage("System Notification: You have entered system mode. You can now use shell commands via the 'submit_shell_command' tool.")
			} else if previousMode == inout.SystemMode {
				l.sendLiveMessage("System Notification: You have left system mode. Shell commands are no longer available.")
				l.stopActivity(ShellStream)
			}

			// Start video if switching to video mode
			if payload == inout.VideoMode {
				if !config.C.Video.Enabled {
					log.Println("Video mode selected, but video is disabled in config.")
				} else if l.videoStream == nil {
					l.startVideoStream()
					// Video activity starts automatically on first frame arrived.
				}
			}
		}
	case "restart_session":
		log.Printf("Restarting live session due to configuration change: %s", payload)
		// The main Run loop will detect the closed session and reopen it with the new config.
		l.CloseSession()
	default:
		// The "save" event is not handled here as LiveAI does not maintain history.
		config.DebugPrintf("LiveAI component ignoring event: %s", event)
	}
}

// startVideoStream initializes and runs the video component in a new goroutine.
func (l *LiveAI) startVideoStream() {
	log.Println("Initializing and starting video stream for video mode.")
	var err error
	l.videoStream, err = video.NewVideoStreamComponent(l.wg, l.bus, l.videoFrameChan)
	if err != nil {
		log.Printf("ERROR: Failed to initialize video stream component: %v", err)
		l.videoStream = nil // ensure it's nil on error
		return
	}

	l.wg.Add(1)
	go l.videoStream.Run()
}

// handleShellOutput processes a batch of shell output received from the CLI.
func (l *LiveAI) handleShellOutput(output string) {
	trimmedContent := strings.TrimSpace(output)
	if trimmedContent == "" {
		return
	}

	// Check if the session is online before attempting to send.
	l.mu.RLock()
	online := l.Online
	l.mu.RUnlock()

	if !online {
		// This case should be rare because the polling loop should only
		// run when the session is online, but it's a good safeguard.
		// The CLI will continue buffering, so no data is lost.
		log.Println("LiveAI is offline, shell output will be polled on next cycle.")
		return
	}

	// If this is the first piece of shell output in a sequence, start a new user activity turn.
	l.startActivity(ShellStream)
	// Send the buffered shell output as part of the ongoing user turn.
	l.sendLiveMessage(fmt.Sprintf("User shell output: [%s]", trimmedContent))
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

	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	l.writeMu.Lock()
	if err := l.session.SendClientContent(content); err != nil {
		log.Printf("failed to send client content: %v", err)
	}
	l.writeMu.Unlock()
}

// sendLiveMessage sends a simple text message to the active live session.
// It is used for sending contextual information or user prompts that are not
// part of a larger content turn. It is safe for concurrent use.
func (l *LiveAI) sendLiveMessage(text string) {
	online := l.Online

	if !online {
		return
	}
	log.Println("Streaming live message to model...")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		Text: text,
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
		// Stop streaming on error to prevent flooding with more errors.
	}
}

// sendLiveImage sends the currently held screenshot buffer to the active live session.
// It checks if a session is online and an image buffer exists before sending.
// It is safe for concurrent use.
func (l *LiveAI) sendLiveImage() {
	l.mu.RLock()
	imageBuffer := l.imageBuffer
	online := l.Online
	l.mu.RUnlock()

	if !online || imageBuffer == nil {
		return
	}
	log.Println("Streaming live image to model...")

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		Media: &genai.Blob{
			MIMEType: config.MIMEImage,
			Data:     imageBuffer.Bytes(),
		},
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
		// Stop streaming on error to prevent flooding with more errors.
	}
}

// sendLiveVideoFrame sends a single video frame to the active live session.
// It checks if a session is online and starts a VideoStream activity if not already active.
func (l *LiveAI) sendLiveVideoFrame(frame []byte) {
	l.mu.RLock()
	online := l.Online
	l.mu.RUnlock()

	if !online || len(frame) == 0 {
		return
	}

	// Start video activity if it's not already active.
	if !l.isActive(VideoStream) {
		l.startActivity(VideoStream)
		log.Println("Started VideoStream activity.")
		// We might want to send a message to the model here like "User is now sharing video."
	}

	if config.C.Trace {
		log.Printf("Streaming video frame (%d bytes) to model...", len(frame))
	}

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
		Video: &genai.Blob{
			MIMEType: config.MIMEVideo, // GStreamer pipeline is configured to produce image/jpeg frames
			Data:     frame,
		},
	})
	l.writeMu.Unlock()
	if err != nil {
		log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
		// Stop streaming on error to prevent flooding with more errors.
	}
}

// sendTextPrompt sends a text prompt (and potentially a screenshot) to the live session.
// The response is handled by the separate handleResponses goroutine.
func (l *LiveAI) sendTextPrompt(prompt string) error {
	// To address the issue of a stale system prompt in a long-running live session,
	// we prepend the current time to each text prompt. This ensures the model
	// always has up-to-date time context, similar to how PostAI works.
	currentTime := config.FormatTimeWithTimezone(config.C.AI.Timezone)
	fullPrompt := fmt.Sprintf("The current time is %s. The user's request is: %s", currentTime, prompt)

	parts := []*genai.Part{genai.NewPartFromText(fullPrompt)}

	if l.mode == inout.ImageMode {
		l.mu.Lock()
		// Release any old buffer and take a new screenshot for this turn.
		if l.imageBuffer != nil {
			log.Println("Releasing previous screenshot buffer for new text prompt turn.")
			l.imageBuffer.Release()
		}
		log.Println("Taking screenshot for AI response...")
		screenshotBuf, err := desktop.C.CaptureScreen()
		l.mu.Unlock()
		if err == nil {
			l.imageBuffer = screenshotBuf
		} else {
			log.Printf("failed to take screenshot: %v", err)
		}
		parts = append(parts, genai.NewPartFromBytes(l.imageBuffer.Bytes(), config.MIMEImage))
	}

	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	l.mu.RLock()
	session := l.session
	l.mu.RUnlock()

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
		if !l.isActive(AudioStream) {
			continue // Discard the sample if we're not in an audio activity.
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
				log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
				// Stop streaming on error to prevent flooding with more errors.
				l.stopActivity(AudioStream)
			}
			buffer.Unmap()
		}
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}

// notifyActivityStart signals the start of user activity to the model.
// Explicit activity control is not supported when automatic activity detection is enabled.
func (l *LiveAI) notifyActivityStart(streamType StreamType) {
	online := l.Online

	if !online {
		return
	}

	// These notifications should only be sent when the application is managing VAD,
	// which is when native VAD is disabled (l.vadDisabled == true).
	if !l.vadDisabled {
		return
	}

	// Use a guard clause to handle unsupported stream types.
	if streamType != AudioStream && streamType != ShellStream && streamType != VideoStream {
		log.Printf("WARNING: unhandled stream type in notifyActivityStart: %s", streamType)
		return
	}

	// For all streams (audio, text, video), when manual activity detection is used, we explicitly
	// signal the start of a user's turn.
	log.Printf("Live stream activity started for %s stream", streamType)
	input := genai.LiveRealtimeInput{ActivityStart: &genai.ActivityStart{}}

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(input)
	l.writeMu.Unlock()
	if err != nil {
		log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
		// Stop streaming on error to prevent flooding with more errors.
	}
}

// notifyActivityEnd signals the end of user activity to the model.
// It may also signal the end of the audio stream for audio-based turns.
// Explicit activity control is not supported when automatic activity detection is enabled.
func (l *LiveAI) notifyActivityEnd(streamType StreamType) {
	online := l.Online

	if !online {
		return
	}

	// These notifications should only be sent when the application is managing VAD,
	// which is when native VAD is disabled (l.vadDisabled == true).
	if !l.vadDisabled {
		// This should only be sent when automatic activity detection is enabled (which is the default).
		// Indicates that the audio stream has ended, e.g. because the microphone was turned off.
		// The client can reopen the stream by sending an audio message.
		if l.isActive(AudioStream) {
			log.Println("Live stream audio stream ended.")
			l.writeMu.Lock()
			err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
				AudioStreamEnd: true,
			})
			l.writeMu.Unlock()
			if err != nil {
				log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
				// Don't proceed if this fails, as the activity end might be invalid.
				return
			}
		}
		return
	}

	// After handling stream-specific endings, we signal the end of the user's overall activity.
	log.Printf("Live stream activity complete for %s stream.", streamType)
	input := genai.LiveRealtimeInput{
		ActivityEnd: &genai.ActivityEnd{},
	}

	l.writeMu.Lock()
	err := l.session.SendRealtimeInput(input)
	l.writeMu.Unlock()
	if err != nil {
		log.Printf(sendLiveInputErrorPrefix+"%v", err) // Corrected usage
	}
}

// executeToolCalls handles a request from the model to execute one or more tool calls.
// It executes them sequentially, in the order they are received, and returns a slice of their responses.
func (l *LiveAI) executeToolCalls(request *genai.LiveServerToolCall) []*genai.FunctionResponse {
	var responses []*genai.FunctionResponse
	// Execute tool calls sequentially, in the order they are received.
	// This is crucial because one tool call might depend on the result of a previous one
	// (e.g., creating a file, then reading it).
	for _, call := range request.FunctionCalls {
		log.Printf("Executing tool call: '%s' with Args: %+v", call.Name, call.Args)
		// Add the current image buffer to any tool call that might need it.
		// The tool itself is responsible for using or ignoring this argument.
		l.mu.RLock()
		if call.Args == nil {
			call.Args = make(map[string]any)
		}

		if l.imageBuffer != nil {
			call.Args["image_buffer"] = l.imageBuffer
		}

		call.Args["cli_component"] = l.cli

		l.mu.RUnlock()

		var response *genai.FunctionResponse

		// Iterate through all registered agents, following a chain of responsibility pattern.
		// The first agent that recognizes the tool call will handle it.
		for _, agent := range l.agents {
			response = agent.Handle(call)
			if response != nil {
				break // An agent handled the call, so we can stop searching.
			}
		}

		// If no agent handled the call (response is still nil), fall back to the general-purpose tool dispatcher.
		if response == nil {
			response = executeSingleToolCall(call)
		}

		responses = append(responses, response)

		l.handleSendContent(response)
	}
	return responses
}
