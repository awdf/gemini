package video

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gemini/config"
)

// TestMain initializes GStreamer for the test suite.
func TestMain(m *testing.M) {
	// Load a default config to ensure values are initialized.
	config.Load("dummy-config.toml")
	os.Remove("dummy-config.toml") // Clean up dummy file

	gst.Init(nil)
	code := m.Run()
	os.Exit(code)
}

// TestNewVideoStreamComponent tests the constructor and pipeline creation logic.
// It uses "videotestsrc" to avoid dependency on physical hardware or specific drivers.
func TestNewVideoStreamComponent(t *testing.T) {
	// In a CI environment, even videotestsrc might be missing if base plugins aren't installed.
	_, err := gst.NewElement("videotestsrc")
	if err != nil {
		t.Skipf("Skipping test: could not create videotestsrc element. Error: %v", err)
	}

	// Override config for the test to use a predictable source.
	originalSource := config.C.Video.Source
	config.C.Video.Source = "videotestsrc"
	t.Cleanup(func() {
		config.C.Video.Source = originalSource
	})

	// Mock dependencies
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	frameChan := make(chan []byte, 1)

	// Create the component
	videoStream, err := NewVideoStreamComponent(wg, &bus, frameChan)
	require.NoError(t, err, "NewVideoStreamComponent should not return an error")
	require.NotNil(t, videoStream, "VideoStreamComponent should not be nil")
	require.NotNil(t, videoStream.pipeline, "Pipeline should not be nil")

	// Check initial state
	_, currentState := videoStream.pipeline.GetState(gst.StateNull, gst.ClockTimeNone)
	assert.Equal(t, gst.StateNull, currentState, "Pipeline should be in NULL state initially")

	// Clean up the pipeline resources
	videoStream.pipeline.SetState(gst.StateNull)
}

// TestVideoStreamComponent_Run tests the frame pulling logic of the Run method.
func TestVideoStreamComponent_Run(t *testing.T) {
	_, err := gst.NewElement("videotestsrc")
	if err != nil {
		t.Skipf("Skipping test: could not create videotestsrc element. Error: %v", err)
	}

	// Override config for the test
	originalSource := config.C.Video.Source
	config.C.Video.Source = "videotestsrc"
	// Use a low framerate to make the test more predictable
	originalFrameRate := config.C.Video.FrameRate
	config.C.Video.FrameRate = 2
	t.Cleanup(func() {
		config.C.Video.Source = originalSource
		config.C.Video.FrameRate = originalFrameRate
	})

	// Mock dependencies
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	frameChan := make(chan []byte, 5)

	// Create the component
	videoStream, err := NewVideoStreamComponent(wg, &bus, frameChan)
	require.NoError(t, err)

	// Start the Run method in a goroutine
	wg.Add(1)
	go videoStream.Run()

	// Wait for a frame to be received
	select {
	case frame := <-frameChan:
		assert.NotEmpty(t, frame, "Received frame should not be empty")
		// JPEG files start with FF D8
		assert.True(t, len(frame) > 2 && frame[0] == 0xFF && frame[1] == 0xD8, "Frame should be a JPEG image")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for video frame")
	}

	// Stop the component and wait for the goroutine to finish
	videoStream.Stop()
	wg.Wait()
}
