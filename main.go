// This example shows how to use the appsink element.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

const (
	// SilenceThreshold is the normalized RMS level below which we consider the audio to be silent.
	// You may need to adjust this value based on your microphone and environment.
	SilenceThreshold = 0.02

	// HangoverDuration is the amount of time to continue recording after the audio level
	// drops below the silence threshold. This prevents cutting off recordings prematurely.
	HangoverDuration = 2 * time.Second

	// OutputFilename is the name of the file where the recording will be saved.
	// In this example, it's overwritten on each new recording session.
	OutputFilename = "recording.wav"
)

// createVADPipeline sets up a GStreamer pipeline that listens to an audio source,
// analyzes its volume, and records it to a file only when sound is detected.
// It uses a "tee" element to split the audio stream into two branches:
// 1. Analysis Branch: -> queue -> appsink (for calculating RMS in Go)
// 2. Recording Branch: -> queue -> valve -> wavenc -> filesink (for writing to a .wav file)
func createVADPipeline() (*gst.Pipeline, *app.Sink) {
	// Check CLI: gst-launch-1.0 pulsesrc ! audioconvert ! audioresample !  autoaudiosink
	//Devices: pactl list | grep -A2 'Source #' | grep 'Name: ' | cut -d" " -f2

	// Initialize GStreamer
	gst.Init(nil)

	// Create a new pipeline
	pipeline := control(gst.NewPipeline("vad-recording-pipeline"))

	// Create elements
	// On modern Linux systems, PipeWire is often the underlying audio server.
	// Reverting to pulsesrc as pipewiresrc is not working in this environment.
	source := control(gst.NewElementWithName("pulsesrc", "pulse-source"))
	source.SetProperty("device", "alsa_output.pci-0000_00_1f.3.analog-stereo.monitor")

	audioconvert := control(gst.NewElementWithName("audioconvert", "audio-convert"))

	audioresample := control(gst.NewElementWithName("audioresample", "audio-resample"))

	// Add a capsfilter to enforce a common, stable format before the tee.
	// This is the most robust position, as it allows the source and converters
	// to negotiate freely, then standardizes the stream before it is split.
	capsfilter := control(gst.NewElement("capsfilter"))
	capsfilter.SetProperty("caps", gst.NewCapsFromString(
		"audio/x-raw, format=S16LE, layout=interleaved, channels=2, rate=48000",
	))

	tee := control(gst.NewElement("tee"))

	// --- Analysis Branch Elements ---
	analysisQueue := control(gst.NewElement("queue"))

	sink := control(app.NewAppSink())
	sink.SetProperty("sync", false) // Don't synchronize on clock, get data as fast as possible
	sink.SetDrop(false)
	// Set the maximum number of buffers that can be queued. This is critical for stability.
	sink.SetMaxBuffers(10)

	// --- Recording Branch Elements ---
	// For debugging, we replace the entire recording branch with a simple fakesink.
	// fakesink accepts and discards all data, and never blocks. This allows us to
	// verify if the tee and analysis branch are working correctly without interference
	// from the more complex recording logic (valve, wavenc, etc.).
	recordingQueue := control(gst.NewElement("queue"))
	fakesink := control(gst.NewElement("fakesink"))

	// Add all elements to the pipeline
	verify(pipeline.AddMany(source, audioconvert, audioresample, capsfilter, tee, analysisQueue, sink.Element, recordingQueue, fakesink))

	// Link the common path
	verify(gst.ElementLinkMany(source, audioconvert, audioresample, capsfilter, tee))

	// Link the analysis branch
	verify(gst.ElementLinkMany(tee, analysisQueue, sink.Element))

	// Link the recording branch
	verify(gst.ElementLinkMany(tee, recordingQueue, fakesink))

	return pipeline, sink
}

// vadState represents the state of the Voice Activity Detector.
type vadState struct {
	mu             sync.Mutex
	valve          *gst.Element
	isRecording    bool
	silenceEndTime time.Time
}

// newVAD creates a new VAD controller.
func newVAD(valve *gst.Element) *vadState {
	return &vadState{
		valve: valve,
	}
}

// processAudioChunk analyzes an audio chunk's RMS value and updates the recording state.
// It controls the 'valve' element to start or stop the flow of data to the filesink.
func (v *vadState) processAudioChunk(rms float64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	isLoud := rms > SilenceThreshold

	if isLoud {
		if !v.isRecording {
			// NOTE: The VAD status messages will interrupt the RMS volume bar display.
			// A more sophisticated UI would integrate status messages and the bar into a single display routine.
			fmt.Printf("\n>>> Sound detected! Starting recording to %s...\n", OutputFilename)
			v.valve.SetProperty("drop", false)
			v.isRecording = true
		}
		// If it's loud, we are not in a hangover period, so reset the timer.
		v.silenceEndTime = time.Time{}
	} else if v.isRecording { // is silent, but we were recording
		if v.silenceEndTime.IsZero() {
			// First moment of silence, start the hangover timer.
			v.silenceEndTime = time.Now().Add(HangoverDuration)
		} else if time.Now().After(v.silenceEndTime) {
			// Hangover period is over. Stop recording.
			fmt.Println("\n<<< Silence detected. Stopping recording.")
			v.valve.SetProperty("drop", true)
			v.isRecording = false
			// To save multiple clips, you would update the filesink's 'location' property here
			// with a new filename before the next recording starts.
		}
	}
}

// vadController is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
func vadController(wg *sync.WaitGroup, vad *vadState, vadControlChan <-chan float64, ctx context.Context) {
	defer wg.Done()
	for {
		select {
		case rms, ok := <-vadControlChan:
			if !ok {
				fmt.Println("\nVAD Controller goroutine: channel closed, exiting.")
				return
			}
			// This is the only place (outside main) that modifies pipeline state.
			vad.processAudioChunk(rms)
		case <-ctx.Done():
			fmt.Println("\nVAD Controller goroutine: context cancelled, exiting.")
			return
		}
	}
}

// pullSamples is a dedicated goroutine that only pulls samples from the GStreamer pipeline.
// It calculates the RMS and passes it to other goroutines for processing, but never
// modifies the pipeline state itself. This separation of concerns is key to avoiding deadlocks.
func pullSamples(wg *sync.WaitGroup, sink *app.Sink, rmsChan chan float64, ctx context.Context) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nPuller goroutine: context cancelled, exiting.")
			return
		default:
		}

		sample := sink.PullSample()
		if sample == nil {
			fmt.Println("\nPuller goroutine: received nil sample, exiting.")
			return
		}

		buffer := sample.GetBuffer()
		if buffer != nil {
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
				case rmsChan <- normalizedRms:
				default:
				}
			}
			buffer.Unmap()
		}
		// sample.Unref()
	}
}

func mainLoop(pipeline *gst.Pipeline) *glib.MainLoop {

	// Create a GLib Main Loop
	mainLoop := glib.NewMainLoop(glib.MainContextDefault(), false)

	// Handle messages from the pipeline bus
	bus := pipeline.GetBus()
	bus.AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			fmt.Println("End of stream.")
			mainLoop.Quit()
			return false // Stop watching
		case gst.MessageError:
			err := msg.ParseError()
			fmt.Fprintf(os.Stderr, "Error from element %s: %s\n", msg.Source(), err.Error())
			fmt.Fprintf(os.Stderr, "Debugging info: %s\n", err.DebugString())
			mainLoop.Quit()
			return false // Stop watching
		}
		return true // Continue watching
	})

	return mainLoop
}

// printBar encapsulates the expensive printing logic.
func printBar(rms float64, lastBarLength *int) {
	const barWidth = 100

	// Use a small threshold to avoid printing for near-silent audio.
	if !math.IsNaN(rms) {
		// Since RMS is now normalized to [0.0, 1.0], we can scale it directly to the bar size.
		currentBarLength := int(rms * barWidth)

		if currentBarLength > barWidth {
			currentBarLength = barWidth
		}

		// Only update the display if the integer length of the bar has changed.
		// This is more robust than comparing floats and prevents excessive printing.
		if currentBarLength != *lastBarLength {
			*lastBarLength = currentBarLength
			bar := strings.Repeat("=", currentBarLength)
			gap := strings.Repeat(" ", barWidth-currentBarLength)
			// This is the expensive call we are throttling.
			fmt.Printf("[%s%s]\n\033[1A", bar, gap)
		}
	}
}

func displayRMS(rmsChan chan float64) {
	// Refresh the display at a fixed rate (e.g., 20 times per second)
	// This is fast enough for a smooth UI but prevents overwhelming the terminal.
	const updateInterval = 50 * time.Millisecond
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	var lastPrintedBarLength = -1 // Initialize to -1 to guarantee the first print.
	var currentRMS float64

	for {
		select {
		case rms, ok := <-rmsChan:
			if !ok {
				// Channel is closed.
				fmt.Println() // Move to a new line for a clean exit
				return
			}
			currentRMS = rms // Keep track of the latest RMS value.
		case <-ticker.C:
			// Ticker fired. Time to update the display.
			printBar(currentRMS, &lastPrintedBarLength)
		}
	}
}

func main() {
	// Create a buffered channel to decouple the "hot" GStreamer loop from the "cold" I/O loop.
	rmsChan := make(chan float64, 10)

	// Start a "cold" goroutine for printing the volume bar.
	go displayRMS(rmsChan)

	// Use a WaitGroup to ensure our goroutines shut down cleanly.
	var wg sync.WaitGroup

	// Creation pipeline
	pipeline, sink := createVADPipeline()

	// Create a cancellable context to gracefully shut down the puller goroutine.
	ctx, cancel := context.WithCancel(context.Background())

	// Create a GLib Main Loop to handle GStreamer bus messages.
	mainLoop := mainLoop(pipeline)

	// Handle Ctrl+C signal for graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		fmt.Println("\nInterrupt received, initiating shutdown...")
		// 1. Signal all worker goroutines to stop immediately. This prevents
		// them from accessing the pipeline during shutdown.
		cancel()
		// 2. Schedule MainLoop.Quit() to be called from the main GStreamer thread.
		// This is the safest way to interact with the pipeline from a different
		// thread. This will cause mainLoop.Run() to return, allowing a clean shutdown.
		glib.IdleAdd(func() bool {
			mainLoop.Quit()
			return false // Do not call again
		})
	}()

	// Start the puller goroutine.
	wg.Add(1)
	go pullSamples(&wg, sink, rmsChan, ctx)

	// Start the pipeline
	verify(pipeline.SetState(gst.StatePlaying))

	fmt.Println("Listening for audio... Recording will start when sound is detected.")
	fmt.Printf("Recordings will be saved to %s. Press Ctrl+C to exit.\n", OutputFilename)

	// Block until the pipeline's bus signals EOS or an error.
	mainLoop.Run()

	// Clean up
	close(rmsChan)
	fmt.Println("Stopping pipeline...")
	// Set the pipeline to NULL state. This is a blocking call that will tear down
	// the pipeline and cause sink.PullSample() to unblock and return nil.
	pipeline.SetState(gst.StateNull)
	fmt.Println("Pipeline stopped.")

	// Now that the pipeline is stopped, wait for the processing goroutines to finish their cleanup.
	wg.Wait()
	fmt.Println("All goroutines finished.")
}

func control[T any](object T, err error) T {
	if err != nil {
		panic(err)
	}
	return object
}

func verify(err error) {
	if err != nil {
		panic(err)
	}
}
