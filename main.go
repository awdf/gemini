// sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgstreamer-plugins-good1.0-dev
package main

// DIRECTIVE: Chosen OOP approach for application development
// DIRECTIVE: Each scope of logic should be placed in dedicated module
// DIRECTIVE: Inside module can be only one constructor and name must start from New prefix

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"sync"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"

	"google.golang.org/genai"

	"gemini/ai"
	"gemini/ai/agents"
	"gemini/audio"
	"gemini/config"
	"gemini/flow"
	"gemini/inout"
	"gemini/pipeline"
	"gemini/recorder"
	"gemini/vad"
	"gemini/wayland"
)

// CliFlags holds the parsed command-line flags for the application.
type CliFlags struct {
	Voice      bool
	Live       bool
	Transcript bool
	AIEnabled  bool
	ConfigPath string
}

type Runnable interface {
	Run()
}

// App encapsulates the application's state and main components.
type App struct {
	logFile         *os.File
	pipeline        *pipeline.VadPipeline
	recorder        *recorder.Recorder
	vadEngine       *vad.Engine
	ai              *ai.AI
	cli             *inout.CLI
	display         *inout.RMSDisplay
	rmsDisplayChan  chan float64
	vadControlChan  chan float64
	fileControlChan chan string
	aiOnDemandChan  chan string
	textCommandChan chan string
	wg              *sync.WaitGroup
	flags           *CliFlags
	runnables       []Runnable
	bus             *EventBus.Bus
	live            *ai.LiveAI
}

// NewApp creates and initializes a new application instance.
// It sets up all components and channels, making the App ready to run.
func NewApp(flags *CliFlags) *App {
	app := &App{
		wg:    &sync.WaitGroup{},
		flags: flags,
	}

	app.initLogging()
	bus := EventBus.New()
	app.bus = &bus

	// Create buffered channels to decouple the "hot" GStreamer loop from other goroutines.
	app.rmsDisplayChan = make(chan float64, 10) // For the RMS volume bar
	app.vadControlChan = make(chan float64, 10) // For the VAD logic
	app.fileControlChan = make(chan string, 5)  // For WAV files flow
	app.aiOnDemandChan = make(chan string, 2)   // Pass WAV file name for the AI audio flow
	app.textCommandChan = make(chan string, 5)  // For text commands from CLI

	// Copy of flags for AI component
	aiFlags := &ai.Flags{
		// Voice and Transcript are now managed via the global config.
		Enabled: flags.AIEnabled,
	}

	// Create the main components with Dependency Injection.
	// 2 modes: PostAI and LiveAI
	if flags.Live { // LiveAI init
		app.live = ai.NewLiveSink(app.wg, app.fileControlChan, app.textCommandChan, app.bus)
		// The Live API requires 16kHz mono audio.
		app.pipeline = pipeline.NewVADPipeline(app.wg, app.live.Element, app.rmsDisplayChan, app.vadControlChan, app.bus, audio.LiveChannels, audio.LiveSampleRate)
	} else { // PostAI init
		// Initial message to AI. Build start context.
		app.textCommandChan <- ai.CheckQuestion
		app.recorder = recorder.NewRecorderSink(app.wg, app.fileControlChan, app.aiOnDemandChan, app.bus)
		// For recording, we use the higher quality settings defined in the audio package.
		app.pipeline = pipeline.NewVADPipeline(app.wg, app.recorder.Element, app.rmsDisplayChan, app.vadControlChan, app.bus, audio.WavChannels, audio.WavSampleRate)
		app.ai = ai.NewAI(app.wg, app.pipeline, aiFlags, app.aiOnDemandChan, app.textCommandChan, app.bus)
	}
	app.vadEngine = vad.NewVAD(app.wg, app.fileControlChan, app.vadControlChan, app.bus)
	app.display = inout.NewRMSDisplay(app.wg, app.rmsDisplayChan, app.bus)
	app.cli = inout.NewCLI(app.wg, app.textCommandChan, app.bus, flags.AIEnabled)

	// Collect all runnable components. Some will be nil depending on the mode
	// (e.g., app.live or app.recorder). The join() method safely handles nils.
	app.runnables = []Runnable{
		app.pipeline,
		app.display,
		app.vadEngine,
		app.recorder, // May be nil
		app.ai,       // May be nil
		app.cli,
		app.live, // May be nil
	}

	return app
}

// parseFlags defines and parses the command-line flags, returning them in a struct.
func parseFlags() *CliFlags {
	flags := &CliFlags{}
	// Use a local variable for the negated flag.
	aiOff := flag.Bool("no-ai", false, "Disable AI processing, only record audio")

	flag.BoolVar(&flags.Live, "live", false, "Enable live responses from the AI")
	flag.BoolVar(&flags.Voice, "voice", false, "Enable voice responses from the AI")
	flag.BoolVar(&flags.Transcript, "ts", false, "Enable separate transcription step for voice chat")
	flag.StringVar(&flags.ConfigPath, "config", "config.toml", "Path to the configuration file")

	flag.Parse()

	// The flag is 'no-ai', so we negate it for 'AIEnabled'.
	flags.AIEnabled = !*aiOff
	return flags
}

func testDocxAgent(testFilePath string) {
	log.Println("--- RUNNING DOCX AGENT TEST ---")
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatalf("Failed to create genai client for test: %v", err)
	}

	// The agent factory needs a toolset to register its functions with.
	toolset := agents.NewToolSet()

	// Manually register the agent for this test run.
	agents.Registerate(ctx, client, toolset, agents.AgentDocxReaderName)

	agentCallable, ok := agents.AgentRegistry[agents.AgentDocxReaderName]
	if !ok {
		log.Fatalf("Failed to register or find DOCX reader agent in registry.")
	}

	docxAgent, ok := agentCallable.(interface {
		Handle(*genai.FunctionCall) *genai.FunctionResponse
	})
	if !ok {
		log.Fatalf("Agent does not implement the required Handle method.")
	}

	testCall := &genai.FunctionCall{
		Name: "readDocx",
		Args: map[string]any{
			"path": testFilePath,
		},
	}

	response := docxAgent.Handle(testCall)
	if response == nil {
		log.Fatalf("DOCX agent did not handle the call.")
	}

	if response.Response != nil {
		if errVal, ok := response.Response["error"]; ok {
			log.Fatalf("DOCX agent returned an error: %v", errVal)
		}
		if content, ok := response.Response["html_content"].(string); ok {
			log.Println("--- DOCX Read Successful ---")
			outputFile := "docx_test_output.html"
			err := os.WriteFile(outputFile, []byte(content), 0o644)
			if err != nil {
				log.Fatalf("Failed to write test output to %s: %v", outputFile, err)
			}
			log.Printf("Content from DOCX saved to %s", outputFile)
		} else {
			log.Printf("DOCX agent response did not contain html_content: %+v", response.Response)
		}
	} else {
		log.Fatalf("DOCX agent returned a nil response map.")
	}
	log.Println("--- FINISHED DOCX AGENT TEST ---")
}

func main() {
	flags := parseFlags()
	flow.EnableControl()

	config.Load(flags.ConfigPath)

	// Command-line flags override config file settings for convenience.

	// --- START OF USER REQUESTED TEST BLOCK ---
	// This block is for development purposes to test the DOCX agent.
	// It can be removed once testing is complete.

	testFilePath := "/home/awdf/Workspace/portfolio/CV_ENG.docx"

	if _, err := os.Stat(testFilePath); err == nil {
		// Only run the test if the file exists to avoid crashing on other machines.
		testDocxAgent(testFilePath)
		// Exit after test to prevent running the full application.
		os.Exit(0)
	} else {
		log.Printf("Skipping DOCX agent test: test file not found at %s.", testFilePath)
	}
	// --- END OF USER REQUESTED TEST BLOCK ---

	if flags.Voice {
		config.C.AI.VoiceEnabled = true
	}
	if flags.Transcript {
		config.C.AI.Transcript = true
	}
	gst.Init(nil)
	wayland.DisableJoystick()
	wayland.Init()
	wayland.SetOffset(10)  // Default
	wayland.SetAccuracy(2) // Default
	defer wayland.Done()

	// TODO: Remove after object detection live testing
	// wayland.MoveMouseToPosition(72, 303)
	// wayland.MouseLeftClick(2)
	NewApp(flags).run()
}

func (app *App) run() {
	// Defer shutdown to ensure it runs when the function exits.
	defer app.shutdown()

	// Launch all runnable components as goroutines.
	for _, r := range app.runnables {
		app.join(r)
	}

	// Process existing files and get the last file index to avoid overwrites.
	// This must be guarded as app.recorder is nil in live mode.
	if app.recorder != nil {
		lastFileIndex := app.recorder.ProcessExistingRecordings()
		app.vadEngine.SetFileCounter(lastFileIndex)
	}

	// Start the pipeline
	app.pipeline.Play()
	fmt.Println("Press Ctrl+C to exit.")

	// Block until the pipeline's bus signals EOS or an error.
	app.pipeline.Loop()
}

func (app *App) join(r Runnable) {
	// An interface is only nil if both its type and value are nil.
	// A nil pointer of a concrete type (e.g., (*AI)(nil)) assigned to an
	// interface results in a non-nil interface. We must use reflection
	// to check if the underlying value of the interface is nil.
	if r == nil || reflect.ValueOf(r).IsNil() {
		return
	}
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
	app.logFile, err = os.OpenFile(config.C.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("error opening log file %s: %v", config.C.LogFile, err)
	}
	log.SetOutput(app.logFile)
	log.SetPrefix("\x20")
	log.Println("### Application started!!!")

	config.DebugPrintln("!!! DEBUG MODE ENABLED !!!")

	if !app.flags.AIEnabled {
		log.Print("AI processing is disabled. The application will only record audio.")
	}

	// Enable voice responses
	if app.flags.Voice {
		log.Print("Voice responses enabled")
	}

	// Enable transcript
	if app.flags.Transcript {
		log.Print("Separate transcription step enabled")
	}
}
