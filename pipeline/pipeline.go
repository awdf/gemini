package pipeline

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"capgemini.com/audio" // Import the audio package for WAV constants
	"capgemini.com/config"
	"capgemini.com/flow"
	"capgemini.com/helpers"
	"capgemini.com/recorder"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

type VadPipeline struct {
	pipeline       *gst.Pipeline
	vadSink        *app.Sink
	recorder       *recorder.Recorder
	loop           *glib.MainLoop
	wg             *sync.WaitGroup
	rmsDisplayChan chan<- float64
	vadControlChan chan<- float64
	bus            *EventBus.Bus
}

// NewVADPipeline sets up a GStreamer pipeline that listens to an audio source,
// analyzes its volume, and records it to a file only when sound is detected.
// It uses a "tee" element to split the audio stream into two branches:
// 1. Analysis Branch: -> queue -> appsink (for calculating RMS in Go)
// 2. Recording Branch: -> queue -> valve -> appsink (for writing to a file in Go)
func NewVADPipeline(wg *sync.WaitGroup, recorder *recorder.Recorder, rmsDisplayChan chan<- float64, vadControlChan chan<- float64, bus *EventBus.Bus) *VadPipeline {
	// Check CLI: gst-launch-1.0 pulsesrc ! audioconvert ! audioresample !  autoaudiosink
	//Devices: pactl list | grep -A2 'Source #' | grep 'Name: ' | cut -d" " -f2

	var p VadPipeline
	p.recorder = recorder
	p.wg = wg

	p.rmsDisplayChan = rmsDisplayChan
	p.vadControlChan = vadControlChan
	p.bus = bus
	// Create a new pipeline
	p.pipeline = helpers.Control(gst.NewPipeline("vad-recording-pipeline"))

	// Create elements
	// On modern Linux systems, PipeWire is often the underlying audio server.
	// Reverting to pulsesrc as pipewiresrc is not working in this environment.
	source := helpers.Control(gst.NewElementWithName("pulsesrc", "pulse-source"))
	if config.C.Pipeline.Device != "" {
		helpers.Verify(source.SetProperty("device", config.C.Pipeline.Device))
	}

	// Increase the buffer time to make the source more resilient to startup latency.
	// This helps prevent race conditions in complex pipelines. Value is in microseconds.
	helpers.Verify(source.SetProperty("buffer-time", config.C.Pipeline.BufferTimeUs))

	audioconvert := helpers.Control(gst.NewElementWithName("audioconvert", "audio-convert"))

	audioresample := helpers.Control(gst.NewElementWithName("audioresample", "audio-resample"))

	// Add a capsfilter to enforce a common, stable format before the tee.
	// This is the most robust position, as it allows the source and converters
	// to negotiate freely, then standardizes the stream before it is split.
	capsfilter := helpers.Control(gst.NewElement("capsfilter"))
	helpers.Verify(capsfilter.SetProperty("caps", gst.NewCapsFromString(
		fmt.Sprintf("audio/x-raw, format=S16LE, layout=interleaved, channels=%d, rate=%d",
			audio.WAV_CHANNELS, audio.WAV_SAMPLE_RATE),
	)))

	tee := helpers.Control(gst.NewElement("tee"))

	// --- Analysis Branch Elements ---
	analysisQueue := helpers.Control(gst.NewElement("queue"))

	p.vadSink = helpers.Control(app.NewAppSink())
	helpers.Verify(p.vadSink.SetProperty("sync", false)) // Don't synchronize on clock, get data as fast as possible
	p.vadSink.SetDrop(false)
	// Set the maximum number of buffers that can be queued. This is critical for stability.
	p.vadSink.SetMaxBuffers(10)

	// --- Recording Branch Elements ---
	recordingQueue := helpers.Control(gst.NewElement("queue"))

	// Add all elements to the pipeline
	helpers.Verify(p.pipeline.AddMany(source, audioconvert, audioresample, capsfilter, tee, analysisQueue, p.vadSink.Element, recordingQueue, recorder.Element))

	// Link the common path
	helpers.Verify(gst.ElementLinkMany(source, audioconvert, audioresample, capsfilter, tee))

	// Link the analysis branch
	helpers.Verify(gst.ElementLinkMany(tee, analysisQueue, p.vadSink.Element))

	// Link the recording branch
	helpers.Verify(gst.ElementLinkMany(tee, recordingQueue, recorder.Element))

	p.mainLoop()
	return &p
}

// MainLoop creates and runs a GLib Main Loop for GStreamer bus messages.
func (p *VadPipeline) mainLoop() {
	// Create a GLib Main Loop
	p.loop = glib.NewMainLoop(glib.MainContextDefault(), false)

	// Handle messages from the pipeline bus
	bus := p.pipeline.GetBus()
	bus.AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			log.Println("End of stream.")
			p.loop.Quit()
			return false // Stop watching
		case gst.MessageError:
			err := msg.ParseError()
			log.Printf("ERROR: from element %s: %s", msg.Source(), err.Error())
			log.Printf("DEBUG: %s", err.DebugString())
			p.loop.Quit()
			return false // Stop watching
		}
		return true // Continue watching
	})
}

func (p *VadPipeline) SendEvent(event *gst.Event) {
	p.pipeline.SendEvent(event)
}

// setPipelineStateAndWait schedules a state change on the main GLib context
// and blocks until it is complete. This is for making state changes from
// goroutines other than the main GStreamer/GLib thread.
func (p *VadPipeline) setPipelineStateAndWait(state gst.State) {
	done := make(chan struct{})
	glib.IdleAdd(func() bool {
		helpers.Verify(p.pipeline.SetState(state))
		close(done)
		return false // Do not call again
	})
	<-done
}

func (p *VadPipeline) Play() {
	// This method is called from a glib.IdleAdd context, so it's
	// executing on the main GStreamer thread. It's safe to set the state directly
	// without scheduling another task, which would cause a deadlock.
	glib.IdleAdd(func() bool {
		helpers.Verify(p.pipeline.SetState(gst.StatePlaying))
		return false // Do not call again
	})
}

func (p *VadPipeline) Pause() {
	// Pause is called from the AI goroutine, so it must use the wait helper.
	p.setPipelineStateAndWait(gst.StatePaused)
}

func (p *VadPipeline) Stop() {
	// The main loop is stopped when this is called from app.shutdown(), so using
	// glib.IdleAdd in setPipelineState would deadlock. We can call SetState
	// directly as it's safe to do from the main thread after the loop has exited.
	helpers.Verify(p.pipeline.SetState(gst.StateNull))
}

func (p *VadPipeline) Loop() {
	go func() {
		<-*flow.GetListener()
		p.Abort("Interrupt received, pipline will be initiating shutdown...")
	}()

	p.loop.Run()
}

func (p *VadPipeline) Abort(reason string) {
	log.Println(reason)
	// 1. Signal to end pipeline work
	p.SendEvent(gst.NewEOSEvent())
	// 2. Schedule MainLoop.Quit() to be called from the main GStreamer thread.
	// This is the safest way to interact with the pipeline from a different
	// thread. This will cause mainLoop.Run() to return, allowing a clean shutdown.
	p.Quit()
}

func (p *VadPipeline) Quit() {
	glib.IdleAdd(func() bool {
		p.loop.Quit()
		return false // Do not call again
	})
}

// Run is a dedicated goroutine that only pulls samples from the GStreamer pipeline.
// It calculates the RMS and passes it to other goroutines for processing, but never
// modifies the pipeline state itself. This separation of concerns is key to avoiding deadlocks.
func (p *VadPipeline) Run() {
	// When this goroutine exits for any reason (e.g., EOS, error), we must
	// ensure the main GStreamer loop is also terminated to prevent a hang.
	defer p.Quit()

	// This goroutine is the producer for the display and VAD channels.
	// By Go convention, the producer is responsible for closing the channel
	// to signal to consumers that no more data will be sent.
	defer close(p.rmsDisplayChan)
	defer close(p.vadControlChan)
	defer p.wg.Done()

	p.vadSink.GetState(gst.StatePlaying, gst.ClockTimeNone)

	// Give the pipeline a moment to settle. If the source failed to start,
	// this delay gives the GStreamer bus time to deliver an error message
	// before this goroutine detects the EOS and triggers a shutdown.
	time.Sleep(200 * time.Millisecond)
	for {

		// Check for End-of-Stream first to ensure a clean exit. This is the
		// condition that will terminate this goroutine's loop.
		if p.vadSink.IsEOS() {
			log.Println("Pipeline sampler work finished (EOS detected)")
			return
		}

		// Use TryPullSample for a non-blocking pull. The blocking PullSample() was
		// causing a deadlock when the main thread tried to pause the pipeline.
		sample := p.vadSink.TryPullSample(0)
		if sample == nil {
			// No sample is available right now. Sleep for a short duration
			// to prevent this loop from consuming 100% CPU.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if config.C.Trace {
			log.Println("Puller goroutine: received sample")
		}

		buffer := sample.GetBuffer()
		if buffer == nil {
			continue
		}

		samples := buffer.Map(gst.MapRead).AsInt16LESlice()
		if len(samples) > 0 {
			var sumOfSquares float64
			for _, s := range samples {
				sumOfSquares += float64(int64(s) * int64(s))
			}
			rms := math.Sqrt(sumOfSquares / float64(len(samples)))
			const normalizationFactor = 32768.0
			normalizedRms := rms / normalizationFactor

			// Send to display goroutine (non-blocking)
			select {
			case p.rmsDisplayChan <- normalizedRms:
			default:
			}

			// Send to VAD controller goroutine (non-blocking)
			select {
			case p.vadControlChan <- normalizedRms:
			default:
			}
		}
		buffer.Unmap()
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}
