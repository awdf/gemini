// This example shows how to use the appsink element.
package main

import (
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

func pulsePipeline(rmsChan chan float64) *gst.Pipeline {
	//Check CLI: gst-launch-1.0 pulsesrc ! audioconvert ! audioresample !  autoaudiosink
	//Devices: pactl list | grep -A2 'Source #' | grep 'Name: ' | cut -d" " -f2

	// Initialize GStreamer
	gst.Init(nil)

	// Create a new pipeline
	pipeline := control(gst.NewPipeline("pulse-capture-pipeline")) // audiotestsrc, pulsesrc, pipewiresrc

	// Create elements for audio capture (e.g., from PipeWire's default audio source)
	// You might need to specify a 'target-object' property for a specific PipeWire node
	// Use 'pw-mon' or 'wpctl status' to find available PipeWire source nodes.
	source := control(gst.NewElementWithName("pulsesrc", "pulse-source"))
	// Optionally set a specific PipeWire source object if you know it
	source.SetProperty("device", "alsa_output.pci-0000_00_1f.3.analog-stereo.monitor")

	// Re-add converters to handle any format differences from the new source.
	audioconvert := control(gst.NewElementWithName("audioconvert", "audio-convert"))
	audioresample := control(gst.NewElementWithName("audioresample", "audio-resample"))

	// AppSink to get raw audio data into Go
	sink := control(app.NewAppSink())

	// Configure AppSink.
	// The 'sync=false' property tells the sink to not sync against the clock. In a
	// live-capture pipeline, we want to process data as soon as it arrives.
	sink.SetProperty("sync", false) // Don't synchronize on clock, get data as fast as possible

	// The 'drop=true' property would tell the sink to drop old buffers if the
	// queue is full, instead of blocking the pipeline. For recording, we want
	// this to be false (the default) to avoid losing data.
	sink.SetDrop(false)

	// Tell the appsink what format we want. It will then be the audiotestsrc's job to
	// provide the format we request.
	// This can be set after linking the two objects, because format negotiation between
	// both elements will happen during pre-rolling of the pipeline.
	sink.SetCaps(
		gst.NewCapsFromString("audio/x-raw, format=S16LE, layout=interleaved, channels=1, rate=16000"))

	// Add all elements to the pipeline
	verify(pipeline.AddMany(source, audioconvert, audioresample, sink.Element))

	// Link elements
	verify(gst.ElementLinkMany(source, audioconvert, audioresample, sink.Element))

	// Getting data out of the appsink is done by setting callbacks on it.
	// The appsink will then call those handlers, as soon as data is available.
	sink.SetCallbacks(&app.SinkCallbacks{
		// Add a "new-sample" callback
		NewSampleFunc: func(sink *app.Sink) gst.FlowReturn {

			// Pull the sample that triggered this callback
			sample := sink.PullSample()
			if sample == nil {
				return gst.FlowEOS
			}

			// Retrieve the buffer from the sample
			buffer := sample.GetBuffer()
			if buffer == nil {
				return gst.FlowError
			}

			// At this point, buffer is only a reference to an existing memory region somewhere.
			// When we want to access its content, we have to map it while requesting the required
			// mode of access (read, read/write).
			//
			// We also know what format to expect because we set it with the caps. So we convert
			// the map directly to signed 16-bit little-endian integers.
			samples := buffer.Map(gst.MapRead).AsInt16LESlice()
			defer buffer.Unmap()

			// Perform the calculation.
			if len(samples) > 0 {
				// To optimize, we calculate the sum of squares on the raw integer
				// values first, avoiding a floating-point division for every sample.
				var sumOfSquares float64

				for _, s := range samples {
					sumOfSquares += float64(int64(s) * int64(s))
				}
				rms := math.Sqrt(sumOfSquares / float64(len(samples)))

				// Normalize the final RMS value to the [0.0, 1.0] range.
				// Max value for S16LE is 32767.
				const normalizationFactor = 32768.0
				normalizedRms := rms / normalizationFactor

				// Send the result to the printing goroutine without blocking.
				select {
				case rmsChan <- normalizedRms: // Attempt to send
				default:
					// Drop the value if the channel is full to prevent this loop from blocking.
				}
			}

			return gst.FlowOK
		},
	})

	return pipeline
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
func printBar(rms float64, lastPick *float64) {
	const size = 100.0
	var barSpace = strings.Repeat("=", int(size))
	var gapSpace = strings.Repeat(" ", int(size))

	// Use a small threshold to avoid printing for near-silent audio.
	if !math.IsNaN(rms) {
		// Since RMS is now normalized to [0.0, 1.0], we can scale it directly to the bar size.
		rait := rms * size

		if rait > size {
			rait = size
		}

		if rait != *lastPick {
			gap := size - rait
			*lastPick = rait
			// This is the expensive call we are throttling.
			fmt.Printf("[%s%s]\n\033[1A", barSpace[:int(rait)], gapSpace[:int(gap)])
		}
	}
}

func displayRMS(rmsChan chan float64) {
	// Refresh the display at a fixed rate (e.g., 20 times per second)
	// This is fast enough for a smooth UI but prevents overwhelming the terminal.
	const updateInterval = 50 * time.Millisecond
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	var lastPrintedRMS float64
	var currentRMS float64

	for {
		select {
		case rms, ok := <-rmsChan:
			if !ok {
				// Channel is closed. Print one last time and exit.
				printBar(currentRMS, &lastPrintedRMS)
				fmt.Println() // Move to a new line for a clean exit
				return
			}
			currentRMS = rms // Keep track of the latest RMS value.
		case <-ticker.C:
			// Ticker fired. Time to update the display.
			printBar(currentRMS, &lastPrintedRMS)
		}
	}
}

func main() {

	var pipeline *gst.Pipeline

	// Handle Ctrl+C signal for graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		fmt.Println("\nInterrupt received, sending EOS to pipeline.")
		pipeline.SendEvent(gst.NewEOSEvent())
	}()

	// Create a buffered channel to decouple the "hot" GStreamer loop from the "cold" I/O loop.
	rmsChan := make(chan float64, 10) //for Root Mean Square(RMS) of 10 stream blocks in row
	// Start a "cold" goroutine for printing. This can block on I/O (like printing to the console)
	// without affecting the real-time GStreamer pipeline.
	go displayRMS(rmsChan)
	// Creation pipeline
	pipeline = pulsePipeline(rmsChan)
	// Start the pipeline
	verify(pipeline.SetState(gst.StatePlaying))
	// Creating main loop and run it
	mainLoop(pipeline).Run()
	// Close channel to ask displayRMS do a finish
	close(rmsChan)
	// Clean up
	fmt.Println("Stopping pipeline...")
	pipeline.SetState(gst.StateNull)
	fmt.Println("Pipeline stopped.")
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
