//go:build integration

package video

import (
	"gemini/config"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupIntegrationTest reloads the configuration from the actual config.toml file.
// It's called at the beginning of each integration test.
func setupIntegrationTest(t *testing.T) {
	// The default TestMain in video_test.go loads a dummy config.
	// We must reload the real one for integration tests.
	// Assumes tests are run from the project root, so the path is relative from the package dir.
	configPath := "../config.toml"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Skipf("Skipping integration test: config.toml not found at %s", configPath)
	}
	config.Load(configPath)
}

// TestVideoStreamComponent_Integration_RealSource tests the constructor with the real source from config.toml.
// This test requires a correctly configured environment (e.g., running Wayland for pipewiresrc).
// To run: go test -v -tags=integration ./video/...
func TestVideoStreamComponent_Integration_RealSource(t *testing.T) {
	setupIntegrationTest(t)

	// Check if the configured source element can be created.
	_, err := gst.NewElement(config.C.Video.Source)
	if err != nil {
		t.Skipf("Skipping integration test: could not create configured video source '%s'. Error: %v", config.C.Video.Source, err)
	}

	// Mock dependencies
	bus := EventBus.New()
	frameChan := make(chan []byte, 1)

	// Create the component using the real source from config
	videoStream, err := NewVideoStreamComponent(&bus, frameChan)
	require.NoError(t, err, "NewVideoStreamComponent should not return an error with a real source")
	require.NotNil(t, videoStream, "VideoStreamComponent should not be nil")
	require.NotNil(t, videoStream.pipeline, "Pipeline should not be nil")

	// Clean up the pipeline resources
	videoStream.pipeline.SetState(gst.StateNull)
}

// TestVideoStreamComponent_Run_Integration_RealSource tests the frame pulling logic with a real source.
// To run: go test -v -tags=integration ./video/...
func TestVideoStreamComponent_Run_Integration_RealSource(t *testing.T) {
	setupIntegrationTest(t)

	_, err := gst.NewElement(config.C.Video.Source)
	if err != nil {
		t.Skipf("Skipping integration test: could not create configured video source '%s'. Error: %v", config.C.Video.Source, err)
	}

	bus := EventBus.New()
	frameChan := make(chan []byte, 5)

	videoStream, err := NewVideoStreamComponent(&bus, frameChan)
	require.NoError(t, err)

	go videoStream.Run()

	t.Logf("Waiting for video frame from source '%s'. This may require user interaction.", config.C.Video.Source)
	select {
	case frame := <-frameChan:
		assert.NotEmpty(t, frame, "Received frame should not be empty")
		assert.True(t, len(frame) > 2 && frame[0] == 0xFF && frame[1] == 0xD8, "Frame should be a JPEG image")
	case <-time.After(20 * time.Second): // Increase timeout for user interaction
		t.Fatal("timed out waiting for video frame. Did you select a screen to share?")
	}

	videoStream.Stop()
}

// TestForPipewireErrorMessage checks for the specific "error set output format" error.
// This test will FAIL if the error is detected, and PASS if it is not.
func TestForPipewireErrorMessage(t *testing.T) {
	setupIntegrationTest(t)
	if config.C.Video.Source != "pipewiresrc" {
		t.Skip("This test is specific to pipewiresrc")
	}

	// Use a local event bus for this test to avoid interfering with global state
	bus := EventBus.New()
	frameChan := make(chan []byte, 1)

	videoStream, err := NewVideoStreamComponent(&bus, frameChan)
	require.NoError(t, err)

	errorChan := make(chan error, 1)

	// We need to listen on the pipeline's bus, not the one we just created.
	pipelineBus := videoStream.pipeline.GetBus()
	pipelineBus.AddWatch(func(msg *gst.Message) bool {
		if msg.Type() == gst.MessageError {
			pErr := msg.ParseError()
			// We are looking for a specific error.
			if strings.Contains(pErr.Error(), "error set output format") {
				errorChan <- pErr
			}
			return false // Stop watching after the first error
		}
		return true
	})

	videoStream.pipeline.SetState(gst.StatePlaying)

	select {
	case err := <-errorChan:
		// The specific error was received. The test fails.
		t.Fatalf("Caught the specific pipewiresrc error, which means the bug is still present: %v", err)
	case <-time.After(5 * time.Second):
		// No specific error was received after 5 seconds.
		// This suggests the fix is working.
		t.Log("Did not detect the specific pipewiresrc error within the timeout. The fix appears to be working.")
	}

	videoStream.pipeline.SetState(gst.StateNull)
}
