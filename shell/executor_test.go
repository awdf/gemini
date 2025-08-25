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

func TestMain(m *testing.M) {
	// Create a dummy config for tests to avoid dependency on a real file.
	dummyConfigContent := `
[video]
Enabled = false

[ai]
WorkspaceDir = "/tmp/gemini_test_workspace_shell" # Use a unique dir

[shell]
command_end_marker = "GEMINI_CMD_DONE"
`
	tmpfile, err := os.CreateTemp("", "config-*.toml")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(tmpfile.Name()) // clean up

	if _, err := tmpfile.Write([]byte(dummyConfigContent)); err != nil {
		log.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		log.Fatal(err)
	}

	config.Load(tmpfile.Name())
	// Clean up the workspace dir after tests
	code := m.Run()
	os.RemoveAll(config.C.AI.WorkspaceDir)
	os.Exit(code)
}

// newTestExecutor creates a new Executor for testing purposes.
func newTestExecutor(t *testing.T) *Executor {
	bus := EventBus.New()
	executor, err := NewExecutor(&bus)
	require.NoError(t, err)
	return executor
}

func TestExecutor_SendCommand_Echo(t *testing.T) {
	executor := newTestExecutor(t)

	outputChan := make(chan string, 100)
	err := executor.StartInteractive(outputChan)
	require.NoError(t, err)
	t.Cleanup(func() { _ = executor.StopInteractive() })

	var wg sync.WaitGroup
	wg.Add(1)
	var receivedOutput []string
	var outputMutex sync.Mutex
	go func() {
		defer wg.Done()
		for line := range outputChan {
			outputMutex.Lock()
			receivedOutput = append(receivedOutput, line)
			outputMutex.Unlock()
			t.Logf("Shell output: %s", line)
		}
	}()

	command := "echo 'hello world'"
	doneChan, err := executor.SendCommand(command)
	require.NoError(t, err)
	require.NotNil(t, doneChan)

	select {
	case exitCode, ok := <-doneChan:
		require.True(t, ok, "doneChan was closed prematurely")
		assert.Equal(t, 0, exitCode)
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out waiting for 'echo' to complete.")
	}

	// Give a moment for the output to be processed by the goroutine
	time.Sleep(100 * time.Millisecond)

	outputMutex.Lock()
	// The output will contain the command itself, the output, and the prompt.
	// We just check that the expected output is present somewhere.
	var found bool
	for _, line := range receivedOutput {
		if strings.Contains(line, "hello world") {
			found = true
			break
		}
	}
	outputMutex.Unlock()
	assert.True(t, found, "Expected 'hello world' to be in the shell output")

	err = executor.StopInteractive()
	require.NoError(t, err)
	wg.Wait()
}

func TestExecutor_SendCommand_Sleep(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping long running test in short mode")
	}

	executor := newTestExecutor(t)
	outputChan := make(chan string, 100)
	err := executor.StartInteractive(outputChan)
	require.NoError(t, err)
	t.Cleanup(func() { _ = executor.StopInteractive() })

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for line := range outputChan {
			t.Logf("Shell output: %s", line)
		}
	}()

	command := "sleep 5"
	startTime := time.Now()
	doneChan, err := executor.SendCommand(command)
	require.NoError(t, err)
	require.NotNil(t, doneChan)

	select {
	case exitCode, ok := <-doneChan:
		require.True(t, ok, "doneChan was closed prematurely, indicating an issue with the shell or marker detection")
		duration := time.Since(startTime)
		assert.Equal(t, 0, exitCode, "Exit code should be 0 for successful sleep")
		assert.GreaterOrEqual(t, duration, 5*time.Second, "Command should have taken at least 5 seconds")
		assert.Less(t, duration, 6*time.Second, "Command should not take significantly more than 5 seconds")
	case <-time.After(7 * time.Second):
		t.Fatal("Test timed out waiting for 'sleep 5' to complete. The command-end marker was likely not detected.")
	}

	err = executor.StopInteractive()
	require.NoError(t, err)
	wg.Wait()
}
