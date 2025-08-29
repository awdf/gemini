package shell

import (
	"bufio"
	"bytes"
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

// FilteringProvider is an io.Writer that wraps another writer. It scans the
// incoming byte stream for a special marker sequence framed by SOH and STX
// bytes. It filters out this marker sequence and passes all other data to the
// underlying writer. This is more robust than line-based filtering.
type FilteringProvider struct {
	ex       *Executor
	writer   io.Writer
	inMarker bool         // State flag to track if we are currently inside a marker sequence.
	buffer   bytes.Buffer // Reusable buffer to reduce allocations in the Write method.
}

// newFilteringWriter creates a new writer that filters out framed markers.
func NewFilteringProvider(e *Executor, w io.Writer) *FilteringProvider {
	return &FilteringProvider{
		ex:       e,
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

func (fw *FilteringProvider) Send(outputChan chan<- string, pr io.Reader) {
	defer close(outputChan)
	// The scanner is still useful for the internal channel to get line-by-line updates.
	scanner := bufio.NewScanner(pr)
	scanner.Split(fw.geminiSplitFunc)

	for scanner.Scan() {
		line := scanner.Text()
		outputChan <- line
		if exitCode, isMarker := fw.extractExitCodeFromMarker(line); isMarker {
			// Check if this is the first marker for the current session.
			// This synchronizes startup and prevents the initial prompt's marker
			// from being mistaken for a command result.
			if !fw.ex.isShellReady {
				fw.ex.isShellReady = true
				log.Println("First shell marker consumed for synchronization.")
				close(fw.ex.shellReadyChan)
				continue // Skip further processing for this first marker.
			}
			fw.ex.commandMutex.Lock()
			if fw.ex.commandDoneChan != nil {
				// Send the marker's exit code.
				fw.ex.commandDoneChan <- exitCode
				fw.ex.commandMarkerCount++

				// The channel is buffered to hold two markers. The first is the
				// one from the prompt before the command, and the second is
				// the one after the command completes. Once we've sent two,
				// the command is done.
				if fw.ex.commandMarkerCount == cap(fw.ex.commandDoneChan) {
					close(fw.ex.commandDoneChan)
					fw.ex.commandDoneChan = nil
				}
			}
			fw.ex.commandMutex.Unlock()
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

// geminiSplitFunc is a bufio.SplitFunc that splits the input stream by newlines
// or by the command-end marker sequence (\x01...\x02). This ensures that both
// regular shell output and the special markers are tokenized correctly, even if
// a marker does not end with a newline.
func (fw *FilteringProvider) geminiSplitFunc(data []byte, atEOF bool) (advance int, token []byte, err error) {
	// 1. Handle EOF with no more data.
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// 2. Search for the start of a marker.
	if i := bytes.IndexByte(data, markerStartByte); i >= 0 {
		// A marker start is present.
		// 2a. If there is text before the marker, return that text as the first token.
		if i > 0 {
			return i, data[0:i], nil
		}

		// 2b. The marker starts at the beginning of the data. Find its end.
		if j := bytes.IndexByte(data, markerEndByte); j >= 0 {
			// We found the end. The token is the complete marker.
			return j + 1, data[0 : j+1], nil
		}

		// 2c. We have a start but no end. If at EOF, it's a corrupt final token.
		if atEOF {
			return len(data), data, nil
		}

		// 2d. Incomplete marker, need more data.
		return 0, nil, nil
	}

	// 3. No marker start found. Look for a newline.
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// Found a newline. Return the line as a token.
		return i + 1, data[0:i], nil
	}

	// 4. No delimiters found, but we are at EOF. Return the remaining data.
	if atEOF {
		return len(data), data, nil
	}

	// 5. No delimiters found and not at EOF. Request more data.
	return 0, nil, nil
}
