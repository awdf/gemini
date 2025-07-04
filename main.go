// sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgstreamer-plugins-good1.0-dev
package main

// DIRECTIVE: Chosen OOP approach for application development
// DIRECTIVE: Each scope of logic should be placed in dedicated module
// DIRECTIVE: Inside module can be only one constructor and name must start from New prefix

import (
	"flag"
	"log"
	"os"
	"sync"

	"capgemini.com/ai"
	"capgemini.com/config"
	"capgemini.com/display"
	"capgemini.com/flow"
	"capgemini.com/input"
	"capgemini.com/pipeline"
	"capgemini.com/recorder"
	"capgemini.com/vad"

	"github.com/go-gst/go-gst/gst"
)

var (
	voicePtr  = flag.Bool("voice", false, "Enable voice responses from the AI")
	configPtr = flag.String("config", "config.toml", "Path to the configuration file")
	aiOffPtr  = flag.Bool("no-ai", false, "Disable AI processing, only record audio")
)

type Runnable interface {
	Run()
}

// App encapsulates the application's state and main components.
type App struct {
	logFile         *os.File
	pipeline        *pipeline.VadPipeline
	recorder        *recorder.Recorder
	vadEngine       *vad.VADEngine
	ai              *ai.AI
	cli             *input.CLI
	display         *display.RMSDisplay
	rmsDisplayChan  chan float64
	vadControlChan  chan float64
	fileControlChan chan string
	aiOnDemandChan  chan string
	textCommandChan chan string
	wg              *sync.WaitGroup
	voiceEnabled    bool
	aiEnabled       bool
	runnables       []Runnable
}

// NewApp creates and initializes a new application instance.
// It sets up all components and channels, making the App ready to run.
func NewApp(voiceEnabled, aiEnabled bool) *App {
	app := &App{
		wg:           &sync.WaitGroup{},
		voiceEnabled: voiceEnabled,
		aiEnabled:    aiEnabled,
	}

	app.initLogging()

	// Create buffered channels to decouple the "hot" GStreamer loop from other goroutines.
	app.rmsDisplayChan = make(chan float64, 10) // For the RMS volume bar
	app.vadControlChan = make(chan float64, 10) // For the VAD logic
	app.fileControlChan = make(chan string, 5)  // For WAV files flow
	app.aiOnDemandChan = make(chan string, 2)   // Pass WAV file name for the AI audio flow
	app.textCommandChan = make(chan string, 5)  // For text commands from CLI

	// Create the main components with Dependency Injection.
	app.recorder = recorder.NewRecorderSink(app.wg, app.fileControlChan, app.aiOnDemandChan)
	app.pipeline = pipeline.NewVADPipeline(app.wg, app.recorder, app.rmsDisplayChan, app.vadControlChan)
	app.vadEngine = vad.NewVAD(app.wg, app.fileControlChan, app.vadControlChan)
	app.ai = ai.NewAI(app.wg, app.pipeline, app.voiceEnabled, app.aiEnabled, app.aiOnDemandChan, app.textCommandChan)
	app.display = display.NewRMSDisplay(app.wg, app.rmsDisplayChan)
	app.cli = input.NewCLI(app.wg, app.textCommandChan)

	//Collect all Runnable for future processing
	app.runnables = []Runnable{
		app.pipeline,
		app.display,
		app.vadEngine,
		app.recorder,
		app.ai,
		app.cli,
	}

	return app
}

func main() {
	// Parse command-line flags
	flag.Parse()
	flow.EnableControl()

	// Load the configuration
	config.Load(*configPtr)

	// Initialize GStreamer. This should be called once per application.
	gst.Init(nil)

	NewApp(*voicePtr, !*aiOffPtr).run()
}

func (app *App) run() {
	// Defer shutdown to ensure it runs when the function exits.
	defer app.shutdown()

	// Launch all runnable components as goroutines.
	for _, r := range app.runnables {
		app.join(r)
	}

	// Process existing files and get the last file index to avoid overwrites.
	lastFileIndex := app.recorder.ProcessExistingRecordings()
	app.vadEngine.SetFileCounter(lastFileIndex)

	// Start the pipeline
	app.pipeline.Play()
	log.Println("Listening for audio... Recording will start when sound is detected. Press Ctrl+C to exit.")

	// Block until the pipeline's bus signals EOS or an error.
	app.pipeline.Loop()
}

func (app *App) join(r Runnable) {
	app.wg.Add(1)
	go r.Run()
}

func (app *App) shutdown() {
	log.Println("Stopping pipeline...")

	// The main event loop has already been stopped when this function is called.
	// We call the pipeline's Stop method, which is designed to handle this state
	// and set the pipeline to NULL safely.
	app.pipeline.Stop()
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
	log.SetPrefix("\x20")
	log.Println("### Application started!!!")

	if config.C.Debug {
		log.Println("!!! DEBUG MODE ENABLED !!!")
	}

	if !app.aiEnabled {
		log.Print("AI processing is disabled. The application will only record audio.")
	}

	// Enable voice responses
	if app.voiceEnabled {
		log.Print("Voice responses enabled")
	}
}
