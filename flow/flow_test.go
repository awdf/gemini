package flow

import (
	"bytes"
	"log"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setup is a helper function to reset the state of the flow package before each test.
// It's crucial because the package uses global variables.
func setup() func() {
	// Reset listeners before each test to ensure isolation.
	listeners = make([]*chan os.Signal, 0)

	// The signal processing goroutine is started by EnableControl.
	// In a real application, this is called once. For tests, we can call it
	// but acknowledge that stopping the background goroutine is complex.
	// The tests are designed to work with this running goroutine.

	// Return a teardown function.
	return func() {
		// In a more complex scenario, we might add a 'done' channel to the
		// processSignal goroutine to stop it gracefully. For these tests,
		// letting it run for the duration of the test suite is sufficient.
	}
}

func TestGetListenerAndSignalDispatch(t *testing.T) {
	teardown := setup()
	defer teardown()

	// Start the signal processor.
	EnableControl()

	listenerChan := GetListener()
	require.NotNil(t, listenerChan, "GetListener should return a non-nil channel pointer")

	// Send a signal using the package's public function, which puts it on the internal channel.
	go Interrupt()

	select {
	case sig := <-*listenerChan:
		assert.Equal(t, syscall.SIGINT, sig, "Listener should receive the SIGINT signal")
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func TestMultipleListeners(t *testing.T) {
	teardown := setup()
	defer teardown()

	EnableControl()

	listener1 := GetListener()
	listener2 := GetListener()

	require.NotSame(t, listener1, listener2, "GetListener should return new channels for each call")

	// Use a wait group to ensure both listeners receive the signal before the test ends.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		select {
		case sig := <-*listener1:
			assert.Equal(t, syscall.SIGTERM, sig, "Listener 1 should receive SIGTERM")
		case <-time.After(1 * time.Second):
			t.Error("listener 1 timed out")
		}
	}()

	go func() {
		defer wg.Done()
		select {
		case sig := <-*listener2:
			assert.Equal(t, syscall.SIGTERM, sig, "Listener 2 should receive SIGTERM")
		case <-time.After(1 * time.Second):
			t.Error("listener 2 timed out")
		}
	}()

	// Send the signal that both listeners should receive.
	go Terminate()

	wg.Wait()
}

func TestSigPipeIsIgnored(t *testing.T) {
	// Capture log output to verify the "ignored" message.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	originalFlags := log.Flags()
	log.SetFlags(0) // Remove timestamp for consistent output.
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(originalFlags)
	}()

	teardown := setup()
	defer teardown()

	EnableControl()
	listener := GetListener()

	// Send SIGPIPE directly to the internal channel.
	signalChan <- syscall.SIGPIPE

	// The listener should NOT receive the signal. We'll check for a timeout.
	select {
	case sig := <-*listener:
		t.Fatalf("Listener should not have received SIGPIPE, but got %v", sig)
	case <-time.After(100 * time.Millisecond):
		// This is the expected outcome.
	}

	// Check that the correct log message was printed.
	assert.Contains(t, logBuf.String(), "SIGPIPE ignored", "Log should indicate that SIGPIPE was ignored")
}

func TestHandle_FatalOnNoListeners(t *testing.T) {
	// This test verifies that the application exits when a signal is received
	// and there are no listeners. It does this by mocking the exit function.

	// 1. Setup the mock for the exit function.
	originalExitFunc := exitFunc
	var exited bool
	exitFunc = func(_ string, _ ...interface{}) {
		exited = true
		// Don't actually exit, just record that we were called.
	}
	defer func() { exitFunc = originalExitFunc }()

	// 2. Ensure there are no listeners.
	listeners = make([]*chan os.Signal, 0)

	// 3. Call the function under test directly.
	// We call handle() directly instead of going through the channel
	// to make the test synchronous and avoid race conditions with the goroutine.
	handle(syscall.SIGQUIT)

	// 4. Assert the results.
	assert.True(t, exited, "exitFunc should have been called when no listeners are present")
}
