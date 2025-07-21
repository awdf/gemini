package ai

import (
	"context"
	"fmt"
	"io"
	"log"
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
	ctx         context.Context
	client      *genai.Client
	formatter   *inout.Formatter
	liveSink    *app.Sink
	Element     *gst.Element
	wg          *sync.WaitGroup
	controlChan <-chan string
	textCmdChan <-chan string
	bus         *EventBus.Bus
	session     *genai.Session
	isStreaming bool
	mode        string
	mu          sync.Mutex
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
		wg:          wg,
		ctx:         ctx,
		client:      client,
		formatter:   inout.NewFormatter(),
		bus:         bus,
		controlChan: controlChan,
		textCmdChan: textCmdChan,
		liveSink:    sink,
		Element:     sink.Element,
		isStreaming: false,
		mode:        inout.MixMode,
	}
}

func (l *LiveAI) OpenSession() {
	if l.session != nil {
		return
	}

	// 1. Configure the session based on global settings.
	var modelName string
	liveConfig := &genai.LiveConnectConfig{}
	if config.C.AI.VoiceEnabled {
		// Model: gemini-2.5-flash-preview-native-audio-dialog, gemini-2.5-flash-exp-native-audio-thinking-dialog
		// Model inputs: Audio, videos, and text
		// Model outputs: Text and audio, interleaved
		// For responses with voice. RPD 5 per model
		modelName = "gemini-2.5-flash-preview-native-audio-dialog"
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityAudio}
		liveConfig.SpeechConfig = &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: config.C.AI.Voice,
				},
			},
		}
	} else {
		// Model: gemini-live-2.5-flash-preview
		// Model inputs: Audio, images, videos, and text
		// Model outputs: Text
		// For responses with text. RPD 250
		modelName = config.C.AI.ModelLive
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityText}
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

	l.OpenSession()

	// Use a ticker to poll for new samples without running a 100% CPU busy-loop.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if config.C.Trace {
			log.Println("Live AI accepting PCM data")
		}

		select {
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
				// Signal end of turn and process response in a separate goroutine
				// to avoid blocking the main Run loop.
				go func() {
					l.mu.Lock()
					defer l.mu.Unlock()
					comp := true
					content := genai.LiveClientContentInput{TurnComplete: &comp}
					if err := l.session.SendClientContent(content); err != nil {
						log.Printf("ERROR: failed to send turn complete: %v", err)
						return
					}
					if err := l.processLiveStream(); err != nil {
						log.Printf("ERROR: processing live stream: %v", err)
					}
				}()
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

// processLiveStream handles the incoming messages from an active LiveSession.
func (l *LiveAI) processLiveStream() error {
	// Stop other output
	(*l.bus).Publish("main:topic", "mute:ai.livestream")
	defer (*l.bus).Publish("main:topic", "draw:ai.livestream")

	var fullResponseText string
	var audioData []byte

	for {
		msg, err := l.session.Receive()
		if err != nil {
			if err == io.EOF {
				log.Println("Live stream ended (EOF).")
				break
			}
			return err
		}

		if msg.ServerContent != nil && msg.ServerContent.ModelTurn != nil {
			for _, part := range msg.ServerContent.ModelTurn.Parts {
				if part.Text != "" {
					l.formatter.Print(part.Text)
					fullResponseText += part.Text
				}
				if part.InlineData != nil && len(part.InlineData.Data) > 0 {
					audioData = append(audioData, part.InlineData.Data...)
				}
			}
		}

		if msg.ToolCall != nil {
			log.Printf("Live stream received tool call: %+v", msg.ToolCall)
			// TODO: Implement tool call handling
		}

		// UsageMetadata often signals the end of the model's response for the current turn.
		if msg.UsageMetadata != nil {
			log.Printf("Live stream usage metadata received, ending turn: %+v", msg.UsageMetadata)
			break
		}

		if msg.GoAway != nil {
			log.Printf("Live stream session closing by server: %+v", msg.GoAway)
			break
		}
	}

	if config.C.AI.VoiceEnabled && len(audioData) > 0 {
		log.Println("Playing live audio response...")
		if err := audio.PlayRawPCM(audioData, audio.TTSSampleRate, audio.TTSChannels); err != nil {
			log.Printf("ERROR: Failed to play live audio: %v", err)
		}
	}

	// TODO: The LiveAI component does not currently maintain a conversation history
	// like the standard AI component. To add this, a history slice would need to be
	// added to the LiveAI struct and updated here.
	// modelResponseContent := genai.NewContentFromParts(
	// 	[]*genai.Part{genai.NewPartFromText(fullResponseText)},
	// 	genai.RoleModel,
	// )
	// l.conversationHistory = append(l.conversationHistory, modelResponseContent)

	return nil
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
// It acquires a lock to ensure only one turn is processed at a time.
func (l *LiveAI) sendTextPrompt(prompt string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

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

	// A turn is represented by a Content object. The role for client-sent
	// content is 'user'.
	turn := genai.NewContentFromParts(parts, genai.RoleUser)

	// The input to SendClientContent is a struct that contains the turns.
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}
	if err := l.session.SendClientContent(content); err != nil {
		return fmt.Errorf("failed to send client content: %w", err)
	}

	// After sending, we need to process the response.
	if err := l.processLiveStream(); err != nil {
		return fmt.Errorf("processing live stream after text prompt: %w", err)
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
			// The pipeline is configured for 16-bit, 16kHz mono PCM audio.
			// The correct MIME type for this is audio/l16;rate=16000.
			err := l.session.SendRealtimeInput(genai.LiveRealtimeInput{
				Audio: &genai.Blob{
					MIMEType: "audio/pcm;rate=16000",
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
