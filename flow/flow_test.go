package flow

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Start the signal processing goroutine once for all tests in this package.
	// This prevents goroutine leaks between tests and race conditions caused
	// by multiple, concurrent signal processors.
	EnableControl()
	os.Exit(m.Run())
}

// setup is a helper function to reset the state of the flow package before each test.
// It's crucial because the package uses global variables.
func setup() func() {
	// Reset listeners before each test to ensure isolation.
	Reset()

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

// syncBuffer is a bytes.Buffer that is safe for concurrent use.
// The standard bytes.Buffer is not safe for concurrent reads and writes, which
// can cause a data race when a test's main goroutine reads from the buffer
// while a background goroutine (like our signal handler) writes to it via the
// global logger.
type syncBuffer struct {
	b bytes.Buffer
	m sync.Mutex
}

// Write appends the contents of p to the buffer, growing the buffer as needed.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()
	return s.b.Write(p)
}

// String returns the contents of the unread portion of the buffer as a string.
func (s *syncBuffer) String() string {
	s.m.Lock()
	defer s.m.Unlock()
	return s.b.String()
}

func TestSigPipeIsIgnored(t *testing.T) {
	// Capture log output to verify the "ignored" message.
	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	originalFlags := log.Flags()
	log.SetFlags(0) // Remove timestamp for consistent output.
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(originalFlags)
	}()

	teardown := setup()
	defer teardown()

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

	// Use require.Eventually to poll for the log message. This is safer than a
	// fixed sleep, as it waits for the concurrent `processSignal` goroutine
	// to write the log message without racing on the buffer access.
	require.Eventually(t, func() bool {
		return strings.Contains(logBuf.String(), "SIGPIPE ignored")
	}, 500*time.Millisecond, 10*time.Millisecond, "Log should indicate that SIGPIPE was ignored")
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
	Reset()

	// 3. Call the function under test directly.
	// We call handle() directly instead of going through the channel
	// to make the test synchronous and avoid race conditions with the goroutine.
	handle(syscall.SIGQUIT)

	// 4. Assert the results.
	assert.True(t, exited, "exitFunc should have been called when no listeners are present")
}
