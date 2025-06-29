package pipeline

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"capgemini.com/audio" // Import the audio package for WAV constants

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

const (
	DEBUG = false
)

// CreateVADPipeline sets up a GStreamer pipeline that listens to an audio source,
// analyzes its volume, and records it to a file only when sound is detected.
// It uses a "tee" element to split the audio stream into two branches:
// 1. Analysis Branch: -> queue -> appsink (for calculating RMS in Go)
// 2. Recording Branch: -> queue -> valve -> appsink (for writing to a file in Go)
func CreateVADPipeline() (*gst.Pipeline, *app.Sink, *app.Sink) {
	// Check CLI: gst-launch-1.0 pulsesrc ! audioconvert ! audioresample !  autoaudiosink
	//Devices: pactl list | grep -A2 'Source #' | grep 'Name: ' | cut -d" " -f2

	// Create a new pipeline
	pipeline := Control(gst.NewPipeline("vad-recording-pipeline"))

	// Create elements
	// On modern Linux systems, PipeWire is often the underlying audio server.
	// Reverting to pulsesrc as pipewiresrc is not working in this environment.
	source := Control(gst.NewElementWithName("pulsesrc", "pulse-source"))
	// source.SetProperty("device", "alsa_output.pci-0000_00_1f.3.analog-stereo.monitor")

	// Increase the buffer time to make the source more resilient to startup latency.
	// This helps prevent race conditions in complex pipelines. Value is in microseconds.
	source.SetProperty("buffer-time", int64(500000)) // 500ms

	audioconvert := Control(gst.NewElementWithName("audioconvert", "audio-convert"))

	audioresample := Control(gst.NewElementWithName("audioresample", "audio-resample"))

	// Add a capsfilter to enforce a common, stable format before the tee.
	// This is the most robust position, as it allows the source and converters
	// to negotiate freely, then standardizes the stream before it is split.
	capsfilter := Control(gst.NewElement("capsfilter"))
	capsfilter.SetProperty("caps", gst.NewCapsFromString(
		fmt.Sprintf("audio/x-raw, format=S16LE, layout=interleaved, channels=%d, rate=%d",
			audio.WAV_CHANNELS, audio.WAV_SAMPLE_RATE),
	))

	tee := Control(gst.NewElement("tee"))

	// --- Analysis Branch Elements ---
	analysisQueue := Control(gst.NewElement("queue"))

	vadsink := Control(app.NewAppSink())
	vadsink.SetProperty("sync", false) // Don't synchronize on clock, get data as fast as possible
	vadsink.SetDrop(false)
	// Set the maximum number of buffers that can be queued. This is critical for stability.
	vadsink.SetMaxBuffers(10)

	// --- Recording Branch Elements ---
	recordingQueue := Control(gst.NewElement("queue"))
	// Changed to 0 (no leak) to ensure all data is passed for recording integrity.
	recordingQueue.SetProperty("leaky", 0) // 0 = No leak. We want all data when recording.

	// Use a second appsink for the recording branch. This allows Go to handle
	// file I/O, giving us the flexibility to create new files on the fly.
	recordingSink := Control(app.NewAppSink())
	recordingSink.SetProperty("sync", false)
	recordingSink.SetDrop(false) // Do not drop data; ensure all samples are received for recording.
	// Set a max buffer to prevent runaway memory usage and add stability.
	recordingSink.SetMaxBuffers(10)

	// Add all elements to the pipeline
	Verify(pipeline.AddMany(source, audioconvert, audioresample, capsfilter, tee, analysisQueue, vadsink.Element, recordingQueue, recordingSink.Element))

	// Link the common path
	Verify(gst.ElementLinkMany(source, audioconvert, audioresample, capsfilter, tee))

	// Link the analysis branch
	Verify(gst.ElementLinkMany(tee, analysisQueue, vadsink.Element))

	// Link the recording branch
	Verify(gst.ElementLinkMany(tee, recordingQueue, recordingSink.Element))

	return pipeline, vadsink, recordingSink
}

// MainLoop creates and runs a GLib Main Loop for GStreamer bus messages.
func MainLoop(pipeline *gst.Pipeline) *glib.MainLoop {
	// Create a GLib Main Loop
	mainLoop := glib.NewMainLoop(glib.MainContextDefault(), false)

	// Handle messages from the pipeline bus
	bus := pipeline.GetBus()
	bus.AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			log.Println("End of stream.")
			mainLoop.Quit()
			return false // Stop watching
		case gst.MessageError:
			err := msg.ParseError()
			log.Printf("ERROR: from element %s: %s", msg.Source(), err.Error())
			log.Printf("DEBUG: %s", err.DebugString())
			mainLoop.Quit()
			return false // Stop watching
		}
		return true // Continue watching
	})

	return mainLoop
}

// PullSamples is a dedicated goroutine that only pulls samples from the GStreamer pipeline.
// It calculates the RMS and passes it to other goroutines for processing, but never
// modifies the pipeline state itself. This separation of concerns is key to avoiding deadlocks.
func PullSamples(wg *sync.WaitGroup, sink *app.Sink, rmsDisplayChan chan<- float64, vadControlChan chan<- float64) {
	defer wg.Done()

	for {
		// Check for End-of-Stream first to ensure a clean exit. This is the
		// condition that will terminate this goroutine's loop.
		if sink.IsEOS() {
			log.Println("Pipeline sampler work finished (EOS detected)")
			return
		}

		// Use TryPullSample for a non-blocking pull. The blocking PullSample() was
		// causing a deadlock when the main thread tried to pause the pipeline.
		sample := sink.TryPullSample(0)
		if sample == nil {
			// No sample is available right now. Sleep for a short duration
			// to prevent this loop from consuming 100% CPU.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if DEBUG {
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
			case rmsDisplayChan <- normalizedRms:
			default:
			}

			// Send to VAD controller goroutine (non-blocking)
			select {
			case vadControlChan <- normalizedRms:
			default:
			}
		}
		buffer.Unmap()
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}

// Control is a helper function to check errors during GStreamer element creation.
func Control[T any](object T, err error) T {
	if err != nil {
		panic(err)
	}
	return object
}

// Verify is a helper function to check errors during GStreamer linking.
func Verify(err error) {
	if err != nil {
		panic(err)
	}
}
