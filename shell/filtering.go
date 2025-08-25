package shell

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"

	"gemini/config"
)

const (
	markerStartByte = 0x01 // SOH (Start of Heading)
	markerEndByte   = 0x02 // STX (Start of Text)
)

// Contains constants above SOH \x01 and STX \x02
func promptCommand() string {
	return fmt.Sprintf(`PROMPT_COMMAND='printf "\x01%s:%%d\x02" $?'`, config.C.Shell.GetCommandEndMarker())
}

// FilteringProvider is an io.Writer that wraps another writer. It scans the
// incoming byte stream for a special marker sequence framed by SOH and STX
// bytes. It filters out this marker sequence and passes all other data to the
// underlying writer. This is more robust than line-based filtering.
type FilteringProvider struct {
	writer   io.Writer
	inMarker bool         // State flag to track if we are currently inside a marker sequence.
	buffer   bytes.Buffer // Reusable buffer to reduce allocations in the Write method.
}

// newFilteringWriter creates a new writer that filters out framed markers.
func NewFilteringProvider(w io.Writer) *FilteringProvider {
	return &FilteringProvider{
		writer:   w,
		inMarker: false,
		// buffer is zero-valued and ready to use.
	}
}

// Write implements the io.Writer interface. It scans for and removes
// marker sequences from the byte stream.
func (fw *FilteringProvider) Write(p []byte) (n int, err error) {
	// Reset the buffer for this write call, but keep the underlying allocated memory.
	fw.buffer.Reset()

	for _, b := range p {
		if fw.inMarker {
			if b == markerEndByte {
				fw.inMarker = false // End of marker sequence.
			}
			// Discard the byte, as it's part of the marker.
		} else {
			if b == markerStartByte {
				fw.inMarker = true // Start of a new marker sequence.
			} else {
				// This byte is not part of a marker, so we should write it.
				fw.buffer.WriteByte(b)
			}
		}
	}

	// Write the collected non-marker bytes to the actual writer.
	if fw.buffer.Len() > 0 {
		if _, err := fw.writer.Write(fw.buffer.Bytes()); err != nil {
			// If the write fails, we can't do much else. We've processed the input bytes.
			return len(p), err
		}
	}

	// We report that we've processed all the input bytes, regardless of filtering.
	return len(p), nil
}

func (fw *FilteringProvider) Send(outputChan chan<- string, e *Executor, pr io.Reader) {
	defer close(outputChan)
	// The scanner is still useful for the internal channel to get line-by-line updates.
	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		line := scanner.Text()
		outputChan <- line
		if exitCode, isMarker := fw.extractExitCodeFromMarker(line); isMarker {
			e.commandMutex.Lock()
			if e.commandInProgress {
				if e.commandDoneChan != nil {
					e.commandDoneChan <- exitCode
					close(e.commandDoneChan)
				}
				e.commandDoneChan = nil
				e.commandInProgress = false
			}
			e.commandMutex.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		config.DebugPrintf("Filtering pipe scanner stopped: %v", err)
	}
}

// extractExitCodeFromMarker checks if a line from the shell output contains the special
// command-end marker and extracts the exit code if it does.
func (fw *FilteringProvider) extractExitCodeFromMarker(line string) (exitCode int, isMarker bool) {
	// The marker is framed by SOH (0x01) and STX (0x02) bytes.
	startIndex := strings.IndexByte(line, markerStartByte)
	if startIndex == -1 {
		return 0, false
	}

	// Search for the end byte *after* the start byte.
	endIndex := strings.IndexByte(line[startIndex:], markerEndByte)
	if endIndex == -1 {
		return 0, false
	}

	// Extract the full marker content, e.g., "__GEMINI_CMD_DONE__:0"
	// The endIndex is relative to the slice starting at startIndex.
	markerContent := line[startIndex+1 : startIndex+endIndex]

	prefix := config.C.Shell.GetCommandEndMarker() + ":"
	// Check if the extracted content starts with the configured core marker string.
	if !strings.HasPrefix(markerContent, prefix) {
		return 0, false
	}

	// Extract the exit code part.
	exitCodeStr := strings.TrimPrefix(markerContent, prefix)
	exitCode, err := strconv.Atoi(exitCodeStr)
	if err != nil {
		log.Printf("WARNING: could not parse exit code from shell marker: '%s'", line)
		return -1, true // It's a marker, but we couldn't parse the code.
	}
	return exitCode, true
}
