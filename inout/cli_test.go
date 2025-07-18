package inout

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gemini/config"
	"gemini/flow"
)

// mockStdin creates a pipe to simulate stdin for tests.
// It returns the read end (to be used as os.Stdin) and the write end (for the test to write to).
func mockStdin(t *testing.T) (*os.File, *os.File) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	originalStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = originalStdin
		r.Close()
		w.Close()
	})
	return r, w
}

func TestCLI_CommandHandling(t *testing.T) {
	// Setup
	bus := EventBus.New()
	cli := NewCLI(&sync.WaitGroup{}, nil, &bus, true)

	t.Run("help command", func(t *testing.T) {
		// Capture output synchronously to avoid race conditions.
		originalStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		cli.command("help")

		w.Close()
		var out bytes.Buffer
		_, err := io.Copy(&out, r)
		require.NoError(t, err)
		os.Stdout = originalStdout

		output := out.String()
		assert.Contains(t, output, "Available commands:")
		assert.Contains(t, output, "/exit")
	})

	t.Run("save command", func(t *testing.T) {
		var eventReceived string
		var wg sync.WaitGroup
		wg.Add(1)
		err := bus.Subscribe("ai:topic", func(event string) {
			eventReceived = event
			wg.Done()
		})
		require.NoError(t, err)

		cli.command("save")
		wg.Wait() // Wait for the async event to be processed

		assert.Equal(t, "save:history.txt", eventReceived)
		require.NoError(t, bus.Unsubscribe("ai:topic", "save:history.txt")) // Clean up
	})

	t.Run("toggle commands", func(t *testing.T) {
		// Test a few toggle commands to ensure the pattern works
		initialDebug := config.C.Debug
		cli.command("debug")
		assert.Equal(t, !initialDebug, config.C.Debug, "Debug flag should be toggled")
		cli.command("debug") // Toggle back
		assert.Equal(t, initialDebug, config.C.Debug, "Debug flag should be toggled back")

		initialVoice := config.C.AI.VoiceEnabled
		cli.command("voice")
		assert.Equal(t, !initialVoice, config.C.AI.VoiceEnabled, "Voice flag should be toggled")
		cli.command("voice") // Toggle back
		assert.Equal(t, initialVoice, config.C.AI.VoiceEnabled, "Voice flag should be toggled back")
	})

	t.Run("thinking command", func(t *testing.T) {
		initialThinking := config.C.AI.Thinking
		defer func() { config.C.AI.Thinking = initialThinking }()

		cli.command("thinking high")
		assert.Equal(t, thinkingLevels["high"], config.C.AI.Thinking)

		// Capture output for the invalid command case
		originalStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		cli.command("thinking invalid")
		w.Close()
		var outInvalid bytes.Buffer
		_, err := io.Copy(&outInvalid, r)
		require.NoError(t, err)
		os.Stdout = originalStdout
		assert.Contains(t, outInvalid.String(), "Unknown thinking level: invalid")

		// Capture output for the usage case
		r, w, _ = os.Pipe()
		os.Stdout = w
		cli.command("thinking")
		w.Close()
		var outUsage bytes.Buffer
		_, err = io.Copy(&outUsage, r)
		require.NoError(t, err)
		os.Stdout = originalStdout
		assert.Contains(t, outUsage.String(), "Usage: /thinking <level>")
	})

	t.Run("exit command", func(t *testing.T) {
		// Mock flow.Quit
		originalQuit := flow.Quit
		var quitCalled bool
		flow.Quit = func() {
			quitCalled = true
		}
		defer func() { flow.Quit = originalQuit }()

		cli.command("exit")
		assert.True(t, quitCalled, "flow.Quit should have been called")
	})
}

func TestCLI_Run_InputProcessing(t *testing.T) {
	_, w := mockStdin(t)
	// Mute log output for this test to keep the test output clean
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	wg := &sync.WaitGroup{}
	cmdChan := make(chan string, 5)
	bus := EventBus.New()
	cli := NewCLI(wg, cmdChan, &bus, true)

	// The CLI's Run loop listens for shutdown signals via the flow package.
	// We must start the flow package's signal processor for the test to terminate correctly.
	flow.EnableControl()

	wg.Add(1)
	go cli.Run()

	t.Run("single line prompt", func(t *testing.T) {
		fmt.Fprintln(w, "hello world")
		select {
		case cmd := <-cmdChan:
			assert.Equal(t, "hello world", cmd)
		case <-time.After(200 * time.Millisecond):
			t.Fatal("timed out waiting for command")
		}
	})

	t.Run("multi-line paste", func(t *testing.T) {
		fmt.Fprintln(w, "line 1")
		time.Sleep(10 * time.Millisecond) // Simulate fast paste
		fmt.Fprintln(w, "line 2")

		select {
		case cmd := <-cmdChan:
			assert.Equal(t, "line 1\nline 2", cmd)
		case <-time.After(200 * time.Millisecond):
			t.Fatal("timed out waiting for command")
		}
	})

	// Send shutdown signal via the flow package to correctly stop the Run loop.
	flow.Interrupt()
	wg.Wait()
}
