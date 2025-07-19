package inout

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisplayWaiting(t *testing.T) {
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	advice := "Thinking..."
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		DisplayWaiting(advice, done)
	}()

	// Let the spinner run for a few cycles to generate output.
	time.Sleep(250 * time.Millisecond)
	close(done) // Signal the function to stop.

	wg.Wait() // Wait for the goroutine to finish.

	w.Close()
	os.Stdout = originalStdout

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	output := buf.String()

	assert.True(t, strings.HasPrefix(output, "\033[?25l"), "output should start with hide cursor ANSI code")
	assert.True(t, strings.HasSuffix(output, "\033[?25h"), "output should end with show cursor ANSI code")
	assert.Contains(t, output, "\r\033[K", "should clear the line on exit")
	assert.Contains(t, output, advice, "output should contain the advice string")
	assert.Regexp(t, `[|/\-\\]`, output, "output should contain at least one spinner character")
}
