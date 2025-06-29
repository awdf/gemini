// sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgstreamer-plugins-good1.0-dev
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"

	"capgemini.com/ai"
	"capgemini.com/display"
	"capgemini.com/pipeline"
	"capgemini.com/recorder"
	"capgemini.com/vad"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
)

// vadState represents the state of the Voice Activity Detector.
// newVAD creates a new VAD controller.
// processAudioChunk analyzes an audio chunk's RMS value and updates the recording state.
// It controls the 'valve' element to start or stop the flow of data to the filesink.
// fileWriter is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
// vadController is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
// writeWAVHeader writes a placeholder WAV header to the file.
// The dataSize and fileSize will need to be updated later.
// updateWAVHeader updates the ChunkSize and Subchunk2Size in the WAV header.
// pullAndWriteSamples pulls all available samples from the sink and writes them to the file.
// pullSamples is a dedicated goroutine that only pulls samples from the GStreamer pipeline.
// It calculates the RMS and passes it to other goroutines for processing, but never
// modifies the pipeline state itself. This separation of concerns is key to avoiding deadlocks.
// printBar encapsulates the expensive printing logic.
var voicePtr = flag.Bool("voice", false, "Enable voice responses from the AI")

func main() {
	flag.Parse()
	if *voicePtr {
		log.Print("Voice responses enabled")
	}

	// Initialize GStreamer. This should be called once per application.
	gst.Init(nil)

	// Create buffered channels to decouple the "hot" GStreamer loop from other goroutines.
	rmsDisplayChan := make(chan float64, 10) // For the RMS volume bar
	vadControlChan := make(chan float64, 10) // For the VAD logic
	fileControlChan := make(chan string, 5)  // For WAV files flow
	aiOnDemandChan := make(chan string, 2)   // Pass WAV file name for the AI flow

	// Creation pipeline
	gstPipeline, vadSink, recordingSink := pipeline.CreateVADPipeline()

	// Create the VAD state controller.
	vadState := vad.NewVAD(fileControlChan)

	// Create a GLib Main Loop to handle GStreamer bus messages.
	mainLoop := pipeline.MainLoop(gstPipeline)

	// Handle Ctrl+C signal for graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		log.Println("\nInterrupt received, initiating shutdown...")
		// 1. Signal to end pipline work
		gstPipeline.SendEvent(gst.NewEOSEvent())
		// 2. Schedule MainLoop.Quit() to be called from the main GStreamer thread.
		// This is the safest way to interact with the pipeline from a different
		// thread. This will cause mainLoop.Run() to return, allowing a clean shutdown.
		glib.IdleAdd(func() bool {
			mainLoop.Quit()
			return false // Do not call again
		})
	}()

	// Use a WaitGroup to ensure our goroutines shut down cleanly.
	var wg sync.WaitGroup
	wg.Add(5)
	// Rutine 1. Start a "cold" goroutine for printing the volume bar.
	go display.DisplayRMS(&wg, rmsDisplayChan)
	// Rutine 2. Start the VAD controller goroutine. This is the only goroutine allowed to change
	// the pipeline's state (by controlling the valve).
	go vad.Controller(&wg, vadState, vadControlChan)
	// Rutine 3. Start the puller goroutine.
	go pipeline.PullSamples(&wg, vadSink, rmsDisplayChan, vadControlChan)
	// Rutine 4. Start the file writer goroutine.
	go recorder.FileWriter(&wg, recordingSink, fileControlChan, aiOnDemandChan)
	// Rutine 5. Start the AI Chat goroutine.
	go ai.Chat(&wg, gstPipeline, *voicePtr, aiOnDemandChan)
	// Start the pipeline
	pipeline.Verify(gstPipeline.SetState(gst.StatePlaying))

	log.Println("Listening for audio... Recording will start when sound is detected.")
	log.Println("Each utterance will be saved to a new file (e.g., recording-1.wav). Press Ctrl+C to exit.")

	// Block until the pipeline's bus signals EOS or an error.
	mainLoop.Run()

	log.Println("Stopping pipeline...")
	// Clean up
	close(rmsDisplayChan)
	close(vadControlChan)
	close(fileControlChan)
	close(aiOnDemandChan)

	// Set the pipeline to NULL state. This is a blocking call that will tear down
	// the pipeline and cause sink.PullSample() to unblock and return nil.
	gstPipeline.SetState(gst.StateNull)
	log.Println("Pipeline stopped.")

	// Now that the pipeline is stopped, wait for the processing goroutines to finish their cleanup.
	wg.Wait()
}
