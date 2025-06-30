// sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgstreamer-plugins-good1.0-dev
package main

import (
	"flag"
	"log"
	"os"
	"sync"

	"capgemini.com/ai"
	"capgemini.com/config"
	"capgemini.com/display"
	"capgemini.com/flow"
	"capgemini.com/helpers"
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
var (
	voicePtr  = flag.Bool("voice", false, "Enable voice responses from the AI")
	configPtr = flag.String("config", "config.toml", "Path to the configuration file")
)

// App encapsulates the application's state and main components.
type App struct {
	logFile         *os.File
	pipeline        *pipeline.VadPipeline
	recorder        *recorder.Recorder
	rmsDisplayChan  chan float64
	vadControlChan  chan float64
	fileControlChan chan string
	aiOnDemandChan  chan string
	wg              *sync.WaitGroup
	voiceEnabled    bool
}

func main() {
	// Parse command-line flags
	flag.Parse()
	flow.Controls()

	// Load the configuration
	config.Load(*configPtr)

	app := &App{
		wg:           &sync.WaitGroup{},
		voiceEnabled: *voicePtr,
	}

	app.initLogging()
	app.run()
	app.shutdown()
}

func (app *App) run() {

	// Initialize GStreamer. This should be called once per application.
	gst.Init(nil)

	// Create buffered channels to decouple the "hot" GStreamer loop from other goroutines.
	app.rmsDisplayChan = make(chan float64, 10) // For the RMS volume bar
	app.vadControlChan = make(chan float64, 10) // For the VAD logic
	app.fileControlChan = make(chan string, 5)  // For WAV files flow
	app.aiOnDemandChan = make(chan string, 2)   // Pass WAV file name for the AI flow

	// Creation vadPipline
	app.recorder = recorder.CreateRecorderSink()
	app.pipeline = pipeline.CreateVADPipeline(app.recorder)

	// Create the VAD state controller.
	vadEngine := vad.CreateVAD(app.fileControlChan)

	go func() {
		<-*flow.GetListener()
		log.Println("Interrupt received, initiating shutdown...")
		// 1. Signal to end pipeline work
		app.pipeline.SendEvent(gst.NewEOSEvent())
		// 2. Schedule MainLoop.Quit() to be called from the main GStreamer thread.
		// This is the safest way to interact with the pipeline from a different
		// thread. This will cause mainLoop.Run() to return, allowing a clean shutdown.
		glib.IdleAdd(func() bool {
			app.pipeline.Quit()
			return false // Do not call again
		})
	}()

	// Use a WaitGroup to ensure our goroutines shut down cleanly.
	app.wg.Add(5)
	// Routine 1. Start a "cold" goroutine for printing the volume bar.
	go display.DisplayRMS(app.wg, app.rmsDisplayChan)
	// Routine 2. Start the VAD controller goroutine. This is the only goroutine allowed to change
	// the pipeline's state (by controlling the valve).
	go vadEngine.Controller(app.wg, app.vadControlChan)
	// Routine 3. Start the puller goroutine.
	go app.pipeline.PullSamples(app.wg, app.rmsDisplayChan, app.vadControlChan)
	// Routine 4. Start the file writer goroutine.
	go app.recorder.FileWriter(app.wg, app.fileControlChan, app.aiOnDemandChan)
	// Routine 5. Start the AI Chat goroutine.
	go ai.Chat(app.wg, app.pipeline, app.voiceEnabled, app.aiOnDemandChan)
	// Start the pipeline
	helpers.Verify(app.pipeline.SetState(gst.StatePlaying))

	log.Println("Listening for audio... Recording will start when sound is detected.")
	log.Println("Each utterance will be saved to a new file (e.g., recording-n.wav). Press Ctrl+C to exit.")

	// Block until the pipeline's bus signals EOS or an error.
	app.pipeline.Run()
}

func (app *App) shutdown() {
	log.Println("Stopping pipeline...")
	// Clean up
	close(app.rmsDisplayChan)
	close(app.vadControlChan)
	close(app.fileControlChan)
	close(app.aiOnDemandChan)

	// Set the pipeline to NULL state. This is a blocking call that will tear down
	// the pipeline and cause sink.PullSample() to unblock and return nil.
	app.pipeline.SetState(gst.StateNull)
	log.Println("Pipeline stopped.")

	// Now that the pipeline is stopped, wait for the processing goroutines to finish their cleanup.
	app.wg.Wait()
	log.Println("All goroutines finished.")
	app.logFile.Close()
}

func (app *App) initLogging() {
	// Set up logging
	var err error
	app.logFile, err = os.OpenFile(config.C.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("error opening log file %s: %v", config.C.LogFile, err)
	}
	log.SetOutput(app.logFile)
	log.Println("### Application started!!!")

	// Enable voice responses
	if app.voiceEnabled {
		log.Print("Voice responses enabled")
	}
}
