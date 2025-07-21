package ai

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"google.golang.org/genai"

	"gemini/audio"
	"gemini/config"
	"gemini/helpers"
	"gemini/inout"
)

type LiveAI struct {
	ctx         context.Context
	client      *genai.Client
	formatter   *inout.Formatter
	liveSink    *app.Sink
	Element     *gst.Element
	wg          *sync.WaitGroup
	controlChan <-chan string
	bus         *EventBus.Bus
}

func NewLiveSink(wg *sync.WaitGroup, controlChan <-chan string, bus *EventBus.Bus) *LiveAI {
	var l LiveAI
	l.liveSink = helpers.Check(app.NewAppSink())
	helpers.Verify(l.liveSink.SetProperty("sync", false))
	l.liveSink.SetDrop(false) // Do not drop data; ensure all samples are received for recording.
	// Set a max buffer to prevent runaway memory usage and add stability.
	l.liveSink.SetMaxBuffers(10)
	l.Element = l.liveSink.Element
	l.wg = wg
	l.controlChan = controlChan
	l.bus = bus
	return &l
}

func (l *LiveAI) Run() {
	defer l.wg.Done()
}

// Livestream establishes a bidirectional connection to the Gemini API for real-time interaction.
// It sends an initial prompt and then processes the stream of responses from the model.
// This function is designed for a single prompt-response cycle.
func (l *LiveAI) Livestream(prompt string) error {
	// 1. Configure the session based on global settings.
	liveConfig := &genai.LiveConnectConfig{}
	if config.C.AI.VoiceEnabled {
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityAudio}
		liveConfig.SpeechConfig = &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: config.C.AI.Voice,
				},
			},
		}
	} else {
		liveConfig.ResponseModalities = []genai.Modality{genai.ModalityText}
	}

	// 2. Connect to the live session.
	// Use the model specified in the config, which is suitable for streaming.
	modelName := config.C.AI.ModelLive
	log.Println("Connecting to live session with model:", modelName)
	session, err := l.client.Live.Connect(l.ctx, modelName, liveConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to live session: %w", err)
	}
	defer session.Close()

	// 3. Wait for the initial "setup complete" message from the server.
	msg, err := session.Receive()
	if err != nil {
		return fmt.Errorf("error receiving setup message: %w", err)
	}
	if msg.SetupComplete == nil {
		return fmt.Errorf("expected setup complete message, got: %+v", msg)
	}
	log.Printf("Live session connected. %v", msg)

	// 4. Send the initial prompt to the model.
	comp := true
	log.Println("Sending prompt:", prompt)
	content := genai.LiveClientContentInput{
		Turns: []*genai.Content{
			genai.NewContentFromParts(
				[]*genai.Part{
					genai.NewPartFromText(prompt),
				},
				genai.RoleUser,
			),
		},
		TurnComplete: &comp,
	}

	if err := session.SendClientContent(content); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}

	// helpers.Verify(session.SendRealtimeInput(genai.LiveRealtimeInput{
	// 	Text: prompt,
	// }))

	// 5. Process the stream of responses.
	return l.processLiveStream(session)
}

// processLiveStream handles the incoming messages from an active LiveSession.
func (l *LiveAI) processLiveStream(session *genai.Session) error {
	// Stop other output
	(*l.bus).Publish("main:topic", "mute:ai.livestream")
	defer (*l.bus).Publish("main:topic", "draw:ai.livestream")

	var fullResponseText string
	var audioData []byte

	for {
		msg, err := session.Receive()
		if err != nil {
			if err == io.EOF {
				log.Println("Live stream ended (EOF).")
				break
			}
			return fmt.Errorf("error receiving from live stream: %w", err)
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

	// TODO: Add response to conversation history if needed.
	// modelResponseContent := genai.NewContentFromParts(
	// 	[]*genai.Part{genai.NewPartFromText(fullResponseText)},
	// 	genai.RoleModel,
	// )
	// a.conversationHistory = append(a.conversationHistory, modelResponseContent)

	return nil
}
