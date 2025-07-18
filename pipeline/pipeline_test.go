package pipeline

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gemini/audio"
	"gemini/config"
	"gemini/recorder"
)

// TestMain is the entry point for all tests in this package. It handles the
// global initialization and de-initialization of GStreamer, which is required
// to prevent race conditions and crashes when running tests in parallel.
func TestMain(m *testing.M) {
	gst.Init(nil)
	code := m.Run()
	// gst.Deinit() is commented out as it was causing SIGTRAP errors in the test
	// environment, likely due to complex interactions with Go's test runner cleanup.
	os.Exit(code)
}

// TestNewVADPipeline tests the constructor.
// This is more of an integration test as it tries to create a real pipeline.
// It might be flaky in environments without a pulse audio server.
func TestNewVADPipeline(t *testing.T) {
	// In a CI environment without a running audio server (like PulseAudio),
	// creating a 'pulsesrc' element can fail or hang. We skip this test
	// if we can't create the source element, making the test suite more robust.
	_, err := gst.NewElement("pulsesrc")
	if err != nil {
		t.Skipf("Skipping test: could not create pulsesrc element, likely no audio server. Error: %v", err)
	}

	// Set a default config value to prevent a GStreamer critical warning.
	// The main application loads this from a file, but tests do not.
	config.C.Pipeline.BufferTimeUs = 200000

	// Mock dependencies
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	rmsChan := make(chan float64, 1)
	vadChan := make(chan float64, 1)

	// The recorder needs a file channel, which we can ignore for this test.
	fileChan := make(chan string, 1)
	aiChan := make(chan string, 1)
	rec := recorder.NewRecorderSink(wg, fileChan, aiChan, &bus)

	p := NewVADPipeline(wg, rec, rmsChan, vadChan, &bus)
	require.NotNil(t, p)
	require.NotNil(t, p.pipeline)
	assert.Equal(t, "vad-recording-pipeline", p.pipeline.GetName())

	// Check pipeline state after creation (should be NULL)
	_, currentState := p.pipeline.GetState(gst.StateNull, gst.ClockTimeNone)
	assert.Equal(t, gst.StateNull, currentState)

	// In a test context without a running GLib main loop, explicitly setting the
	// complex pipeline (with pulsesrc) back to the NULL state can be problematic
	// and was causing a CGo-related error where the return value was nil.
	// The primary goal of this test is to verify construction. Resource cleanup
	// is handled by Go's garbage collector, which will call the necessary
	// unref functions on the underlying GStreamer objects.
}

// TestVadPipeline_Run_SampleProcessing tests the core sample processing logic in the Run method.
// It uses a `fakesrc` to generate predictable audio data, making the test self-contained
// and independent of the host's audio hardware.
func TestVadPipeline_Run_SampleProcessing(t *testing.T) {
	// 1. Create a test pipeline with fakesrc
	// The pipeline must include audioconvert and a capsfilter to ensure the audio format
	// is S16LE, which is what the Run() method's RMS calculation expects.
	// We use audiotestsrc as it's more reliable for generating audio signals than fakesrc.
	pipelineString := fmt.Sprintf(
		"audiotestsrc num-buffers=10 ! audioconvert ! audio/x-raw,format=S16LE,channels=%d,rate=%d ! appsink name=testsink",
		audio.WavChannels, audio.WavSampleRate,
	)
	pipeline, err := gst.NewPipelineFromString(pipelineString)
	require.NoError(t, err)
	sink, err := pipeline.GetElementByName("testsink")
	require.NoError(t, err)
	appSink := app.SinkFromElement(sink)

	// 2. Create a VadPipeline instance with our test components
	wg := &sync.WaitGroup{}
	rmsChan := make(chan float64, 10)
	vadChan := make(chan float64, 10)
	p := &VadPipeline{
		pipeline:       pipeline,
		vadSink:        appSink,
		wg:             wg,
		rmsDisplayChan: rmsChan,
		vadControlChan: vadChan,
		// The Run method's defer p.Quit() requires a running loop.
		loop: glib.NewMainLoop(glib.MainContextDefault(), false),
	}

	// 3. Start the main loop and the Run goroutine.
	loopExited := make(chan struct{})
	go func() {
		p.loop.Run()
		close(loopExited)
	}()

	wg.Add(1)
	go p.Run()

	// 4. Start the pipeline
	err = pipeline.SetState(gst.StatePlaying)
	require.NoError(t, err)

	// 5. Read from channels and assert
	// audiotestsrc produces a sine wave. The RMS should be constant and non-zero.
	// We'll just check that we receive values.
	select {
	case rms := <-rmsChan:
		assert.Greater(t, rms, 0.0, "RMS value should be positive")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RMS value")
	}

	select {
	case vad := <-vadChan:
		assert.Greater(t, vad, 0.0, "VAD value should be positive")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for VAD value")
	}

	// 6. Wait for the pipeline to finish and the goroutine to exit
	bus := pipeline.GetBus()
	defer bus.Unref()
	msg := bus.TimedPopFiltered(gst.ClockTime(5*time.Second), gst.MessageEOS|gst.MessageError)
	// The pipeline should finish cleanly with an EOS message.
	// If msg is nil, it means we timed out, which is a test failure.
	if !assert.NotNil(t, msg, "should have received EOS or Error message") {
		t.FailNow()
	}
	assert.Equal(t, gst.MessageEOS, msg.Type())

	wg.Wait() // Wait for p.Run() to finish

	// Drain any remaining values from the channels. The for-range loop will
	// automatically exit when the channel is closed and empty.
	for range rmsChan {
		// Added NOP for pass golangci-lint check on empty loop
		time.Sleep(0)
	}
	for range vadChan {
		// Added NOP for pass golangci-lint check on empty loop
		time.Sleep(0)
	}

	// Check that channels are closed
	_, ok := <-rmsChan
	assert.False(t, ok, "rmsDisplayChan should be closed")
	_, ok = <-vadChan
	assert.False(t, ok, "vadControlChan should be closed")

	// Wait for the glib loop to exit, which is triggered by p.Quit() in p.Run()'s defer.
	<-loopExited

	// Cleanup
	// The return value of SetState can be ignored here as we are at the end of the test.
	// The primary goal is to release GStreamer resources.
	_ = pipeline.SetState(gst.StateNull)
}

// TestVadPipeline_StateChange tests the state change methods like Play, Pause, Stop.
func TestVadPipeline_StateChange(t *testing.T) {
	pipeline, err := gst.NewPipelineFromString("fakesrc ! fakesink")
	require.NoError(t, err)

	p := &VadPipeline{
		pipeline: pipeline,
		loop:     glib.NewMainLoop(glib.MainContextDefault(), false),
	}

	// Run the main loop in a goroutine so it doesn't block
	go p.loop.Run()
	t.Cleanup(p.loop.Quit)

	// Test Play() from NULL state
	p.Play()
	stateChangeReturn, _ := p.pipeline.GetState(gst.StatePlaying, gst.ClockTime(5*time.Second))
	require.Equal(t, gst.StateChangeSuccess, stateChangeReturn, "Pipeline should successfully transition to PLAYING")

	// Test Pause() from PLAYING state
	p.Pause()
	_, currentState := p.pipeline.GetState(gst.StatePaused, gst.ClockTimeNone)
	assert.Equal(t, gst.StatePaused, currentState)

	// Test Stop()
	p.loop.Quit()
	time.Sleep(100 * time.Millisecond) // Give a moment for the loop to exit.
	p.Stop()
	_, currentState = p.pipeline.GetState(gst.StateNull, gst.ClockTimeNone)
	assert.Equal(t, gst.StateNull, currentState)
}

// TestVadPipeline_Abort tests the graceful shutdown via the Abort method.
func TestVadPipeline_Abort(t *testing.T) {
	// 1. Create a test pipeline that can run for a bit
	pipeline, err := gst.NewPipelineFromString("fakesrc ! fakesink")
	require.NoError(t, err)

	p := &VadPipeline{
		pipeline: pipeline,
		loop:     glib.NewMainLoop(glib.MainContextDefault(), false),
	}

	// 2. Start the pipeline's main loop in a goroutine
	loopExited := make(chan struct{})
	go func() {
		p.loop.Run()
		close(loopExited)
	}()

	// 3. Set the pipeline to PLAYING state and wait for it to start
	p.Play()
	ret, _ := p.pipeline.GetState(gst.StatePlaying, gst.ClockTime(5*time.Second))
	require.Equal(t, gst.StateChangeSuccess, ret)

	// 4. Call Abort from another goroutine to trigger shutdown
	go p.Abort("test abort")

	// 5. Assert that the main loop exits gracefully
	<-loopExited
}
