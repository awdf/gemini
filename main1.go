package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"google.golang.org/genai"
)

func main1() {
	Recording()
	return

	const prompt = "Explain how AI works in a few words"
	const model = "gemini-2.5-flash"

	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  os.Getenv("GOOGLE_API_KEY"),
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatal(err)
	}

	//Flash	thinking
	// Dynamic:-1
	// None:0
	// low:512,
	// Medium:8,192,
	// High:24,576
	var thinking int32 = -1 // Disables thinking

	resp := client.Models.GenerateContentStream(
		ctx,
		model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: true,
				ThinkingBudget:  &thinking,
			},
		},
	)

	for chunk := range resp {
		for _, part := range chunk.Candidates[0].Content.Parts {
			if len(part.Text) == 0 {
				continue
			}

			if part.Thought {
				fmt.Printf("Thought: %s\n", part.Text)
			} else {
				fmt.Printf("Answer: %s\n", part.Text)
			}
		}
	}

}

// sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgstreamer-plugins-good1.0-dev
func Recording() {
	// Initialize GStreamer
	gst.Init(nil)

	// Create a new pipeline
	pipeline, err := gst.NewPipeline("my-pipewire-capture-pipeline")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create pipeline: %v\n", err)
		return
	}

	// Create elements for audio capture (e.g., from PipeWire's default audio source)
	// You might need to specify a 'target-object' property for a specific PipeWire node
	// Use 'pw-mon' or 'wpctl status' to find available PipeWire source nodes.
	source, err := gst.NewElementWithName("pipewiresrc", "audio-source") // audiotestsrc, pulsesrc, pipewiresrc
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create audio source: %v\n", err)
		return
	}
	// Optionally set a specific PipeWire source object if you know it
	// source.SetProperty("target-object", "alsa_input.pci-0000_00_1f.3.analog-stereo")

	// Re-add converters to handle any format differences from the new source.
	audioconvert, err := gst.NewElementWithName("audioconvert", "audio-convert")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create audioconvert: %v\n", err)
		return
	}
	audioresample, err := gst.NewElementWithName("audioresample", "audio-resample")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create audioresample: %v\n", err)
		return
	}

	// AppSink to get raw audio data into Go
	sink, err := app.NewAppSink()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create appsink: %v\n", err)
		return
	}
	// Configure AppSink.
	// The 'sync=false' property tells the sink to not sync against the clock. In a
	// live-capture pipeline, we want to process data as soon as it arrives.
	sink.SetProperty("sync", false) // Don't synchronize on clock, get data as fast as possible

	// Set the maximum number of buffers that can be queued. The default of 1 is
	// often too small for applications with unpredictable scheduling, like those
	// in Go. A larger queue helps prevent the pipeline from stalling if the app
	// is slightly delayed in processing a buffer.
	sink.SetMaxBuffers(10)

	// The 'drop=true' property would tell the sink to drop old buffers if the
	// queue is full, instead of blocking the pipeline. For recording, we want
	// this to be false (the default) to avoid losing data.
	sink.SetDrop(false)
	sink.SetCaps(gst.NewCapsFromString(
		"audio/x-raw, format=S16LE, layout=interleaved, channels=1, rate=48000",
	))

	// Add all elements to the pipeline
	if err := pipeline.AddMany(source, audioconvert, audioresample, sink.Element); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add elements to pipeline: %v\n", err)
		return
	}

	// Link elements
	if err := gst.ElementLinkMany(source, audioconvert, audioresample, sink.Element); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to link elements: %v\n", err)
		return
	}

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

	// Handle Ctrl+C signal for graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		fmt.Println("\nInterrupt received, sending EOS to pipeline.")
		pipeline.SendEvent(gst.NewEOSEvent())
	}()

	// Create a buffered channel to decouple the "hot" GStreamer loop from the "cold" I/O loop.
	rmsChan := make(chan float64, 100)

	// Start a new goroutine to pull samples from the appsink.
	// This is the "pull" method of using appsink, which is often simpler
	// and more robust than using callbacks in a Go environment.
	go func() {
		for {
			// Pull a sample from the sink. This call will block until a sample is
			// available or the pipeline is stopped.
			sample := sink.PullSample()
			// When the pipeline is shutting down, PullSample will return nil.
			// We can check if it's the specific EOS condition, or just treat any
			// nil return as a signal to stop processing.
			if sample == nil {
				if sink.IsEOS() {
					fmt.Println("Pulling goroutine: received EOS")
				} else {
					fmt.Println("Pulling goroutine: received nil sample, exiting")
				}
				return
			}

			buffer := sample.GetBuffer()
			if buffer == nil {
				// We still need to unreference the sample even if it has no buffer.
				sample.Unref()
				continue
			}

			samples := buffer.Map(gst.MapRead).AsInt16LESlice()

			// Perform the calculation in the hot loop.
			if len(samples) > 0 {
				var square float64
				for _, i := range samples {
					square += float64(i * i)
				}
				rms := math.Sqrt(square / float64(len(samples)))
				// Send the result to the printing goroutine without blocking.
				select {
				case rmsChan <- rms: // Attempt to send
				default:
					// Drop the value if the channel is full to prevent this loop from blocking.
				}
			}

			buffer.Unmap()
			sample.Unref()
		}
	}()

	// Start a "cold" goroutine for printing. This can block on I/O (like printing to the console)
	// without affecting the real-time GStreamer pipeline.
	go func() {
		var lastPick = 0.0
		for rms := range rmsChan {
			const size = 100
			rait := rms / 10.0
			gap := size - rait
			if rait > 0 && gap > 0 && rms != lastPick {
				lastPick = rms
				fmt.Printf("[%s%s]\n\033[1A", strings.Repeat("=", int(rait)), strings.Repeat(" ", int(gap)))
			}
		}
		fmt.Println("Printing goroutine finished.")
	}()

	// Set the pipeline to PLAYING state
	if err := pipeline.SetState(gst.StatePlaying); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to set the pipeline to the playing state: %v\n", err)
		return
	}

	fmt.Println("Pipeline started. Capturing audio... Press Ctrl+C to stop.")
	mainLoop.Run()
	close(rmsChan)

	fmt.Println() // Add a newline for cleaner output.
	// Clean up
	fmt.Println("Stopping pipeline...")
	pipeline.SetState(gst.StateNull)
	fmt.Println("Pipeline stopped.")
}
