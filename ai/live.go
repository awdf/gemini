package ai

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
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
	systemPrompt := config.C.AI.SystemPrompt
	var systemInstructionParts []*genai.Part
	if systemPrompt != "" {
		currentTime := time.Now().Format(time.RFC1123)
		systemPrompt = fmt.Sprintf("Current date and time is %s. %s", currentTime, systemPrompt)
		systemInstructionParts = append(systemInstructionParts, genai.NewPartFromText(systemPrompt))
		log.Println("Using system prompt for live session.")
	}

	// Add cache directory files to the system instructions.
	cacheDir := config.C.AI.CacheDir
	filesToInclude, err := findCacheableFiles(cacheDir)
	if err != nil {
		log.Printf("ERROR: could not scan for initial files: %v", err)
	}

	if len(filesToInclude) > 0 {
		log.Printf("Found %d files to include as initial context for live session.", len(filesToInclude))
		if config.C.AI.CacheSystemPrompt != "" {
			systemInstructionParts = append(systemInstructionParts, genai.NewPartFromText(config.C.AI.CacheSystemPrompt))
		}

		for _, file := range filesToInclude {
			localPath := filepath.Join(cacheDir, file.Name())
			// For system instructions in live sessions, all files must be read as raw text. The `createPartFromFile`
			// function handles this logic for text files, but we must read all files as text here to avoid panics.
			data, err := os.ReadFile(localPath)
			if err != nil {
				log.Printf("ERROR: could not read file %s for live session context: %v", localPath, err)
				continue
			}
			systemInstructionParts = append(systemInstructionParts, genai.NewPartFromText(string(data)))
			log.Printf("Cache file uploaded %s", localPath)
		}
	}

	// The role for a system instruction is empty.
	if len(systemInstructionParts) > 0 {
		liveConfig.SystemInstruction = genai.NewContentFromParts(systemInstructionParts, "")
	}

	// Conditionally enable tools based on the configuration.
	// This is only done for the main response generation, not transcription.
	if config.C.AI.EnableTools {
		tool := &genai.Tool{GoogleSearch: &genai.GoogleSearch{}}
		if !urlContextDisabled {
			log.Println("Tools enabled for live session: GoogleSearch, URLContext")
			tool.URLContext = &genai.URLContext{}
		} else {
			log.Println("Tools enabled for live session: GoogleSearch")
		}
		liveConfig.Tools = []*genai.Tool{tool}
	}

	// Add context window compression if enabled.
	if config.C.AI.ContextWindowCompression.Enabled {
		log.Println("Context window compression is enabled for this live session.")
		liveConfig.ContextWindowCompression = &genai.ContextWindowCompressionConfig{
			TriggerTokens: Ptr(config.C.AI.ContextWindowCompression.TriggerTokens),
			SlidingWindow: &genai.SlidingWindow{
				TargetTokens: Ptr(config.C.AI.ContextWindowCompression.TargetTokens),
			},
		}
	}

	// Add session resumption if enabled.
	if config.C.AI.SessionResumption.Enabled {
		log.Println("Session resumption is enabled for this live session.")
		liveConfig.SessionResumption = &genai.SessionResumptionConfig{
			Handle: l.resumptionHandle, // Use the stored handle
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

	helpers.Verify((*l.bus).Subscribe("ai:topic", l.handleEvents))

	l.OpenSession()
	// Start a dedicated goroutine to handle all incoming server messages.
	go l.handleResponses()
	defer l.CloseSession()

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
			l.CloseSession()       // Clean up the old session object.
			l.OpenSession()        // Create a new session.
			go l.handleResponses() // Start a new response handler for the new session.
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
				// This error often happens when the connection is closed, which is expected on shutdown.
				config.DebugPrintf("Live session receive error: %v", err)
			}

			// Signal the main Run loop that the session is dead and needs to be reopened.
			// Use a non-blocking send because the channel is buffered and we only need
			// to signal once. If a signal is already pending, we don't need to send another.
			select {
			case l.sessionClosed <- struct{}{}:
			default:
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

		// Process the content of the message.
		if msg.ServerContent != nil {
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
		} else if msg.ToolCall != nil {
			log.Printf("Live stream received tool call: %+v", msg.ToolCall)
			// TODO: Implement tool call handling
		} else if msg.GoAway != nil {
			log.Printf("Live stream session closing by server: %+v", msg.GoAway.TimeLeft)
			// The loop will terminate in the next iteration due to the connection closing.
		} else if msg.SessionResumptionUpdate != nil {
			log.Printf("Live session resumption handle updated. Resumable: %t", msg.SessionResumptionUpdate.Resumable)
			if msg.SessionResumptionUpdate.Resumable {
				l.resumptionHandle = msg.SessionResumptionUpdate.NewHandle
			}
		} else {
			config.DebugPrintf("Live AI received unhandled message: %+v", msg)
		}

		// UsageMetadata often signals the end of the model's response for the current turn.
		if msg.UsageMetadata != nil {
			log.Printf("Live stream usage metadata received: %+v", msg.UsageMetadata)
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

	parts, _, err := l.parsePromptForMultimedia(prompt)
	if err != nil {
		return fmt.Errorf("failed to parse prompt for multimedia: %w", err)
	}

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

// Ptr returns a pointer to the given value.
func Ptr[T any](v T) *T {
	return &v
}

// parsePromptForMultimedia splits a prompt into text and YouTube URL parts.
// This is a simplified version for the Live API to ensure YouTube videos are
// handled correctly as video parts, which is different from how the URLContext
// tool handles general web pages.
func (l *LiveAI) parsePromptForMultimedia(prompt string) ([]*genai.Part, bool, error) {
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
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			wordAppend(&word)
			continue
		}

		// Specifically check for YouTube URLs to create a video part.
		if strings.Contains(u.Host, youtubeURL1) || strings.Contains(u.Host, youtubeURL2) {
			log.Printf("Detected YouTube URL in prompt: %s", u.String())
			parts = append(parts, genai.NewPartFromURI(u.String(), "video/*"))
			multimediaFound = true
		} else {
			// For all other URLs, keep them as text in the prompt so the URLContext tool can see them.
			wordAppend(&word)
		}
	}

	// Prepend the collected text as the first part, if any.
	if textBuilder.Len() > 0 {
		// Prepend to keep text before media, which is a common pattern.
		parts = append([]*genai.Part{genai.NewPartFromText(textBuilder.String())}, parts...)
	} else if len(parts) == 0 {
		// If there's no text and no multimedia parts, it was an empty or non-URL prompt.
		parts = append(parts, genai.NewPartFromText(prompt))
	}

	return parts, multimediaFound, nil
}
