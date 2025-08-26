package shell

import (
	"log"
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

const (
	workspaceDir = "/tmp/gemini_test_workspace_shell"
)

func TestMain(m *testing.M) {
	var err error
	logFile, err := os.OpenFile("test.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("error opening log file %s: %v", config.C.LogFile, err)
	}
	log.SetOutput(logFile)
	log.SetPrefix(" ")
	log.Println("### Test: Executor started!!!")

	// Load a default config and then override values needed for this test.
	// This avoids creating and reading a temporary file.
	config.Load("dummy-config.toml") // Creates default config in memory.
	os.Remove("dummy-config.toml")   // Clean up the dummy file created by Load.
	config.C.LogFile = "Test.log"
	config.C.Video.Enabled = false
	config.C.Shell.CommandEndMarkerCore = "GEMINI_CMD_DONE"

	// Clean up the workspace dir after tests
	code := m.Run()
	os.RemoveAll(workspaceDir)
	os.Exit(code)
}

// newTestExecutor creates a new Executor for testing purposes.
func newTestExecutor(t *testing.T) *Executor {
	bus := EventBus.New()
	executor, err := NewExecutor(&bus, workspaceDir)
	require.NoError(t, err)
	return executor
}

func TestExecutor_SendCommand_Fail(t *testing.T) {
	executor := newTestExecutor(t)

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = devNull.Close() })

	outputChan := make(chan string, 100)
	err = executor.StartInteractive(outputChan, devNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = executor.StopInteractive() })

	var receivedOutput []string
	var outputMutex sync.Mutex
	go func() {
		for line := range outputChan {
			outputMutex.Lock()
			receivedOutput = append(receivedOutput, line)
			outputMutex.Unlock()
		}
	}()

	command := "a-command-that-does-not-exist"
	doneChan, err := executor.SendCommand(command)
	require.NoError(t, err)
	require.NotNil(t, doneChan)

	select {
	case exitCode, ok := <-doneChan:
		require.True(t, ok, "doneChan was closed without sending an exit code")
		assert.Equal(t, 127, exitCode, "Expected exit code 127 for 'command not found'")
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out waiting for command to complete.")
	}

	// Check that channel is closed now
	_, ok := <-doneChan
	require.False(t, ok, "doneChan was not closed after receiving the exit code")

	// Give a moment for the output to be processed by the goroutine
	time.Sleep(100 * time.Millisecond)

	outputMutex.Lock()
	defer outputMutex.Unlock()

	// We just check that the expected output is present somewhere.
	var foundErrorOutput bool
	fullOutput := strings.Join(receivedOutput, "\n")
	if strings.Contains(fullOutput, "command not found") {
		foundErrorOutput = true
	}

	assert.True(t, foundErrorOutput, "Expected 'command not found' to be in the shell output. Got: %s", fullOutput)

	err = executor.StopInteractive()
	require.NoError(t, err)
}

func TestExecutor_SendCommand_Sleep(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long running test in short mode")
	}

	executor := newTestExecutor(t)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = devNull.Close() })

	outputChan := make(chan string, 100)
	err = executor.StartInteractive(outputChan, devNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = executor.StopInteractive() })

	go func() {
		for range outputChan {
			// Do not use t.Logf from a background goroutine as it can cause
			// deadlocks. The test will time out if there's an issue, which is
			// sufficient for debugging.
		}
	}()

	// Test a simple long-running command.
	command := "sleep 5"
	startTime := time.Now()
	doneChan, err := executor.SendCommand(command)
	require.NoError(t, err)
	require.NotNil(t, doneChan)

	var exitCode int
	select {
	case code, ok := <-doneChan:
		require.True(t, ok, "doneChan was closed without sending an exit code")
		exitCode = code
	case <-time.After(7 * time.Second):
		t.Fatal("Test timed out waiting for 'sleep 5' to complete. The command-end marker was likely not detected.")
	}

	duration := time.Since(startTime)
	assert.Equal(t, 0, exitCode, "Exit code should be 0 for successful sleep")
	assert.GreaterOrEqual(t, duration, 5*time.Second, "Command should have taken at least 5 seconds")
	assert.Less(t, duration, 6*time.Second, "Command should not take significantly more than 5 seconds")

	err = executor.StopInteractive()
	require.NoError(t, err)
}
