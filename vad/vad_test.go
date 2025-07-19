package vad

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gemini/config"
)

// TestMain loads a default configuration for the tests.
func TestMain(m *testing.M) {
	// Load default config to ensure all values are initialized.
	// We can use a dummy path as it will create a default config in memory.
	config.Load("dummy-config.toml")
	os.Remove("dummy-config.toml") // Clean up dummy file
	os.Exit(m.Run())
}

func TestNewVAD(t *testing.T) {
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	fileControlChan := make(chan string, 1)
	vadControlChan := make(chan float64, 1)

	engine := NewVAD(wg, fileControlChan, vadControlChan, &bus)

	require.NotNil(t, engine)
	assert.False(t, engine.IsRecording())
	assert.Zero(t, engine.FileCounter())
}

func TestEngine_SetFileCounter(t *testing.T) {
	engine := &Engine{}
	engine.SetFileCounter(42)
	assert.Equal(t, 42, engine.FileCounter())
}

// TestEngine_Run_StateTransitions tests the core VAD state machine logic.
func TestEngine_Run_StateTransitions(t *testing.T) {
	// Configure VAD parameters for the test
	config.C.VAD.SilenceThreshold = 0.1
	config.C.VAD.HangoverDurationSec = 0.2 // 200ms
	config.C.VAD.WarmupDuration = "0s"     // No warmup for this test

	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	fileControlChan := make(chan string, 10)
	vadControlChan := make(chan float64, 10)

	engine := NewVAD(wg, fileControlChan, vadControlChan, &bus)
	engine.SetFileCounter(9) // Start with a non-zero counter

	wg.Add(1)
	go engine.Run()

	// --- Test Case 1: Silence -> Speaking ---
	t.Run("Silence to Recording", func(t *testing.T) {
		// Send a value below threshold, nothing should happen
		vadControlChan <- 0.05
		select {
		case msg := <-fileControlChan:
			t.Fatalf("should not have received a message, but got: %s", msg)
		case <-time.After(50 * time.Millisecond):
			// Correct, no message
		}

		// Send a value above threshold, should trigger START
		vadControlChan <- 0.2
		select {
		case msg := <-fileControlChan:
			assert.Equal(t, fmt.Sprintf("%srecording-10.wav", MarkerStart), msg)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timed out waiting for START message")
		}
		assert.True(t, engine.IsRecording(), "engine should be in recording state")
		assert.Equal(t, 10, engine.FileCounter(), "file counter should be incremented")
	})

	// --- Test Case 2: Speaking -> Silence (with hangover) -> Speaking ---
	t.Run("Recording with brief silence", func(t *testing.T) {
		// Send a value below threshold, but not for long enough to trigger STOP
		vadControlChan <- 0.05
		time.Sleep(100 * time.Millisecond) // Less than hangover (200ms)
		vadControlChan <- 0.05             // Send another value to trigger check
		select {
		case msg := <-fileControlChan:
			t.Fatalf("should not have received a STOP message, but got: %s", msg)
		default:
			// Correct, no message
		}
		assert.True(t, engine.IsRecording(), "engine should still be in recording state")

		// Go back to loud, should not trigger a new START
		vadControlChan <- 0.3
		select {
		case msg := <-fileControlChan:
			t.Fatalf("should not have received a new START message, but got: %s", msg)
		case <-time.After(50 * time.Millisecond):
			// Correct, no message
		}
		assert.Equal(t, 10, engine.FileCounter(), "file counter should not be incremented again")
	})

	// --- Test Case 3: Speaking -> Silence (long enough) -> Stop ---
	t.Run("Recording to Silence", func(t *testing.T) {
		// Send a value below threshold and wait for hangover to pass
		vadControlChan <- 0.05
		time.Sleep(300 * time.Millisecond) // More than hangover (200ms)

		// Need to send another low value to trigger the check inside the loop
		vadControlChan <- 0.05

		select {
		case msg := <-fileControlChan:
			assert.Equal(t, fmt.Sprintf("%srecording-10.wav", MarkerStop), msg)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timed out waiting for STOP message")
		}
		assert.False(t, engine.IsRecording(), "engine should be back in silent state")
	})

	// --- Cleanup ---
	close(vadControlChan)
	wg.Wait()

	// The control channel should be closed by the Run() goroutine's defer
	_, ok := <-fileControlChan
	assert.False(t, ok, "fileControlChan should be closed")
}

// TestEngine_Run_Warmup tests the VAD warmup logic.
func TestEngine_Run_Warmup(t *testing.T) {
	config.C.VAD.WarmupDuration = "200ms"
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	fileControlChan := make(chan string, 1)
	vadControlChan := make(chan float64, 10) // Buffered to not block sends
	engine := NewVAD(wg, fileControlChan, vadControlChan, &bus)

	// Subscribe to the event bus to catch the ready signal
	eventChan := make(chan string, 1)
	err := bus.Subscribe("main:topic", func(event string) {
		eventChan <- event
	})
	require.NoError(t, err)

	wg.Add(1)
	go engine.Run()

	// Send a loud sample during warmup, it should be ignored.
	vadControlChan <- 0.5
	select {
	case msg := <-fileControlChan:
		t.Fatalf("should not have received START during warmup, but got: %s", msg)
	case <-time.After(50 * time.Millisecond):
		// Correct, no message
	}

	// The Run() loop only checks the warmup timer when a sample is received.
	// We must wait for the warmup period to pass and then send another sample
	// to trigger the check, which will publish the "ready" event.
	time.Sleep(250 * time.Millisecond) // Wait for warmup (200ms) to pass.
	vadControlChan <- 0.01             // Send a dummy sample to wake up the loop.

	// Wait for warmup to finish and check for the event bus message
	select {
	case event := <-eventChan:
		assert.Equal(t, "ready:vad.run", event)
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timed out waiting for 'ready' event on event bus")
	}

	// Send another loud sample, it should now be processed.
	vadControlChan <- 0.5
	assert.Equal(t, fmt.Sprintf("%srecording-1.wav", MarkerStart), <-fileControlChan)

	close(vadControlChan)
	wg.Wait()
}
