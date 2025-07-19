package recorder

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gemini/config"
	"gemini/vad"
)

// TestMain sets up the environment for the recorder tests. It initializes
// GStreamer and creates a temporary directory for test recordings. This
// directory is cleaned up after all tests in the package have run.
func TestMain(m *testing.M) {
	// Load default config to ensure all values are initialized.
	// We can use a dummy path as it will create a default config in memory.
	config.Load("dummy-config.toml")
	os.Remove("dummy-config.toml") // Clean up dummy file

	// Set a dedicated, temporary path for recordings created during tests.
	// The implementation reads from ".", so we don't set config.C.Recorder.Path.
	// Instead, tests will manage the working directory.
	testDir := "test_recordings"
	os.MkdirAll(testDir, 0o755)
	// Ensure the directory is clean before starting.
	// Initialize GStreamer for this test package.
	gst.Init(nil)
	code := m.Run()

	// Cleanup the test directory after all tests in the package have run.
	os.RemoveAll(testDir)
	os.Exit(code)
}

func TestNewRecorderSink(t *testing.T) {
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	controlChan := make(chan string, 1)
	outputFileChan := make(chan string, 1)

	rec := NewRecorderSink(wg, controlChan, outputFileChan, &bus)
	require.NotNil(t, rec)
	require.NotNil(t, rec.Element)
	// The default name for an appsink element is "appsink0", "appsink1", etc.
	// We check the factory name to be sure.
	factory := rec.Element.GetFactory()
	require.NotNil(t, factory)
	assert.Equal(t, "appsink", factory.GetName())
}

func TestProcessExistingRecordings(t *testing.T) {
	// The implementation reads from the current working directory.
	// We create a temporary directory and change into it for this test.
	testDir := t.TempDir()
	originalWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(testDir))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(originalWD))
	})

	// The recorder needs a file channel to send processed files to.
	fileChan := make(chan string, 10)
	// We can pass nil for dependencies not used by ProcessExistingRecordings.
	rec := NewRecorderSink(nil, nil, fileChan, nil)

	t.Run("with existing files", func(t *testing.T) {
		// Create a subdirectory for this specific test run to keep it clean.
		subDir := filepath.Join(testDir, "existing_files")
		require.NoError(t, os.Mkdir(subDir, 0o755), "should create sub-directory for test")

		// Change into the subdir for the test and ensure we change back.
		currentWD, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(subDir), "should change into sub-directory")
		t.Cleanup(func() { require.NoError(t, os.Chdir(currentWD)) })

		// Create dummy files
		// The implementation expects "recording-N.wav".
		require.NoError(t, os.WriteFile("recording-1.wav", []byte("dummy"), 0o644))
		require.NoError(t, os.WriteFile("recording-5.wav", []byte("dummy"), 0o644))
		require.NoError(t, os.WriteFile("recording-3.wav", []byte("dummy"), 0o644))
		require.NoError(t, os.WriteFile("not-a-recording.txt", []byte("dummy"), 0o644))
		require.NoError(t, os.WriteFile("recording-final.wav", []byte("dummy"), 0o644))

		lastIndex := rec.ProcessExistingRecordings()
		assert.Equal(t, 5, lastIndex, "should return the highest index from existing files")

		// Check that the files were queued. The implementation sends just the filename.
		require.Len(t, fileChan, 3, "should have queued 3 valid recording files")
		assert.Equal(t, "recording-1.wav", <-fileChan)
		assert.Equal(t, "recording-3.wav", <-fileChan)
		assert.Equal(t, "recording-5.wav", <-fileChan)
	})

	t.Run("with no files", func(t *testing.T) {
		// Create a new empty subdir for this subtest
		subDir := filepath.Join(testDir, "no_files")
		require.NoError(t, os.Mkdir(subDir, 0o755))

		currentWD, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(subDir), "should change into sub-directory")
		t.Cleanup(func() { require.NoError(t, os.Chdir(currentWD)) })

		lastIndex := rec.ProcessExistingRecordings()
		assert.Equal(t, 0, lastIndex, "should return 0 when no files exist")
	})
}

// TestRecorder_Run_RecordingCycle is an integration test for the full recording lifecycle.
func TestRecorder_Run_RecordingCycle(t *testing.T) {
	// Create a temporary directory for this test's recordings.
	testRecDir := t.TempDir()

	// 1. Setup dependencies
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	controlChan := make(chan string, 2)
	outputFileChan := make(chan string, 1)

	// Set a small MinFileSizeBytes for the test, so our small test file is not deleted.
	config.C.Recorder.MinFileSizeBytes = 10

	rec := NewRecorderSink(wg, controlChan, outputFileChan, &bus)
	require.NotNil(t, rec)

	// 2. Start the recorder's main loop in a goroutine
	wg.Add(1)
	go rec.Run()

	// 3. Create a test GStreamer pipeline to feed audio into the recorder's appsink
	pipeline, err := gst.NewPipeline("test-rec-pipeline")
	require.NoError(t, err)

	src, _ := gst.NewElement("audiotestsrc")
	src.Set("num-buffers", 50) // Generate a limited number of buffers
	conv, _ := gst.NewElement("audioconvert")
	resample, _ := gst.NewElement("audioresample")

	require.NoError(t, pipeline.AddMany(src, conv, resample, rec.Element))
	require.NoError(t, gst.ElementLinkMany(src, conv, resample, rec.Element))

	// 4. Run the test cycle
	require.NoError(t, pipeline.SetState(gst.StatePlaying))

	// The VAD engine would create the filename and send start/stop commands.
	// We simulate that here by sending commands directly to the recorder's control channel.
	expectedFile := filepath.Join(testRecDir, "test_recording_cycle.wav")
	startCommand := fmt.Sprintf("%s%s", vad.MarkerStart, expectedFile)
	stopCommand := fmt.Sprintf("%s%s", vad.MarkerStop, expectedFile)

	controlChan <- startCommand
	time.Sleep(200 * time.Millisecond) // Let it record for a bit
	controlChan <- stopCommand

	// 5. Assertions
	// Check that the file path was sent on the channels
	select {
	case filePath := <-outputFileChan:
		assert.Equal(t, expectedFile, filePath)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for file path on outputFileChan")
	}

	// 6. Cleanup
	// Wait for the pipeline to finish naturally by reaching EOS
	pBus := pipeline.GetBus()
	pBus.TimedPopFiltered(gst.ClockTime(5*time.Second), gst.MessageEOS)
	pipeline.SetState(gst.StateNull)

	// The recorder's Run loop exits when its control channel is closed.
	// The pipeline EOS does not close the recorder's control channel.
	close(controlChan)

	wg.Wait()
}
