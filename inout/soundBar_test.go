package inout

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gemini/config"
)

func TestRMSDisplay_PrintBar(t *testing.T) {
	// Setup
	bus := EventBus.New()
	display := NewRMSDisplay(&sync.WaitGroup{}, nil, &bus)
	display.barWidth = 10 // Use a small width for easier testing

	t.Run("does not print when not warmed up", func(t *testing.T) {
		originalStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		display.warmUpDone = false
		display.muted = false
		display.currentRMS = 0.5
		display.printBar()

		w.Close()
		var out bytes.Buffer
		_, err := io.Copy(&out, r)
		require.NoError(t, err)
		os.Stdout = originalStdout
		assert.Empty(t, out.String())
	})

	t.Run("does not print when muted", func(t *testing.T) {
		originalStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		display.warmUpDone = true
		display.muted = true
		display.currentRMS = 0.5
		display.printBar()

		w.Close()
		var out bytes.Buffer
		_, err := io.Copy(&out, r)
		require.NoError(t, err)
		os.Stdout = originalStdout
		assert.Empty(t, out.String())
	})

	t.Run("prints correctly when active", func(t *testing.T) {
		display.warmUpDone = true
		display.muted = false

		// Test case: 50% volume
		originalStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		display.currentRMS = 0.5
		display.printBar()

		w.Close()
		var out50 bytes.Buffer
		_, err := io.Copy(&out50, r)
		require.NoError(t, err)
		os.Stdout = originalStdout

		// Expected: 5 '=' and 5 ' '
		expected := strings.Repeat("=", 5) + strings.Repeat(" ", 5)
		assert.Contains(t, out50.String(), expected)

		// Test case: 0% volume
		originalStdout = os.Stdout
		r, w, _ = os.Pipe()
		os.Stdout = w

		display.lastPrintedBarLength = -1 // Reset to force a reprint
		display.currentRMS = 0.0
		display.printBar()

		w.Close()
		var out0 bytes.Buffer
		_, err = io.Copy(&out0, r)
		require.NoError(t, err)
		os.Stdout = originalStdout

		expected = strings.Repeat(" ", 10)
		assert.Contains(t, out0.String(), expected)
	})

	t.Run("does not reprint for same bar length", func(t *testing.T) {
		display.warmUpDone = true
		display.muted = false
		display.lastPrintedBarLength = -1 // Reset

		// First print
		originalStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		display.currentRMS = 0.53 // bar length = 5
		display.printBar()

		w.Close()
		var outFirst bytes.Buffer
		_, err := io.Copy(&outFirst, r)
		require.NoError(t, err)
		os.Stdout = originalStdout

		assert.NotEmpty(t, outFirst.String())
		assert.Equal(t, 5, display.lastPrintedBarLength)

		// Second print with slightly different RMS but same bar length
		originalStdout = os.Stdout
		r, w, _ = os.Pipe()
		os.Stdout = w

		display.currentRMS = 0.58 // bar length = 5
		display.printBar()

		w.Close()
		var outSecond bytes.Buffer
		_, err = io.Copy(&outSecond, r)
		require.NoError(t, err)
		os.Stdout = originalStdout

		assert.Empty(t, outSecond.String(), "Should not reprint if bar length is the same")
	})
}

func TestRMSDisplay_Run_EventHandling(t *testing.T) {
	// Manually set the required config value to avoid panic and dependency on file.
	config.C.Display.UpdateIntervalMs = 50

	bus := EventBus.New()
	rmsChan := make(chan float64, 1)
	wg := &sync.WaitGroup{}
	display := NewRMSDisplay(wg, rmsChan, &bus)

	wg.Add(1)
	go display.Run()

	// Use require.Eventually to poll for the state change.
	// This test has a potential race condition: the bus.Publish call might execute
	// before the display.Run() goroutine has had time to call bus.Subscribe.
	// To solve this without adding test-only synchronization code to the main
	// component, we publish the event inside the 'Eventually' loop. The first
	// few publishes might be lost, but once the subscription is active, the
	// event will be received, the state will change, and the condition will pass.
	// This is safe because the 'ready' event handler is idempotent.
	require.Eventually(t, func() bool {
		bus.Publish("main:topic", "ready:app.run")
		display.Mu.RLock()
		defer display.Mu.RUnlock()
		return !display.muted && display.warmUpDone
	}, 1*time.Second, 10*time.Millisecond, "The display should become un-muted and warmed-up after the 'ready' event")

	// Shutdown
	close(rmsChan)
	wg.Wait()
}
