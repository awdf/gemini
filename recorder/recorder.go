package recorder

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"capgemini.com/audio" // Import the audio package for WAV constants

	"github.com/go-gst/go-gst/gst/app"
)

const (
	DEBUG = false
)

// FileWriter is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
func FileWriter(wg *sync.WaitGroup, sink *app.Sink, controlChan <-chan string, fileChan chan<- string) {
	defer wg.Done()

	var f *os.File
	var err error
	var currentFilename string
	var bytesWritten int64 // Track bytes written to the current file
	var isWriting = false

	// Use a ticker to poll for new samples without running a 100% CPU busy-loop.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {

		if DEBUG {
			log.Println("Write to file")
		}

		select {
		case cmd, ok := <-controlChan:
			if !ok { // Channel closed, shutdown.
				if f != nil {
					// This is a final cleanup. If a file was open when the channel
					// closed, finalize it before exiting the goroutine.
					finalizeRecording(f, currentFilename, bytesWritten, fileChan)
					f.Close()
				}
				log.Println("Recording is finished")
				return
			}

			if strings.HasPrefix(cmd, "START:") {
				currentFilename = strings.TrimPrefix(cmd, "START:")
				bytesWritten = 0 // Reset for new recording
				f, err = startNewRecording(f, currentFilename)
				if err != nil {
					log.Printf("ERROR: %v", err)
					f = nil
					isWriting = false
				} else {
					isWriting = true
				}
			} else if strings.HasPrefix(cmd, "STOP:") {
				finalizeRecording(f, currentFilename, bytesWritten, fileChan)
				f = nil
				isWriting = false
				currentFilename = "" // Reset filename
			}

		case <-ticker.C:
			// Always pull samples from the sink. This is crucial to prevent the
			// GStreamer pipeline from blocking if the sink's buffer fills up.
			pullAndWriteSamples(sink, f, &isWriting, &bytesWritten)
		}
	}
}

// startNewRecording handles the creation of a new recording file. It closes any
// previous file, creates the new one, and writes a placeholder WAV header.
func startNewRecording(previousFile *os.File, filename string) (*os.File, error) {
	if previousFile != nil {
		// If a previous file handle exists, it implies a STOP command was missed.
		// We should close it to prevent resource leaks.
		log.Printf("Warning: Starting new recording for %s while previous file was still open. Closing it now.", filename)
		previousFile.Close()
	}

	f, err := os.Create(filename)
	if err != nil {
		// The error is returned to the caller to be handled.
		return nil, fmt.Errorf("creating file %s: %w", filename, err)
	}

	// Write placeholder WAV header
	if err := audio.WriteWAVHeader(f); err != nil {
		f.Close()           // Clean up the created file on header write error.
		os.Remove(filename) // Also remove the file.
		return nil, fmt.Errorf("writing WAV header to %s: %w", filename, err)
	}

	return f, nil
}

// finalizeRecording closes the file, updates its WAV header, and, if the file is large enough,
// sends it to the processing channel. Otherwise, it deletes the file.
func finalizeRecording(f *os.File, filename string, bytesWritten int64, fileChan chan<- string) {
	if f == nil {
		return
	}
	// It's crucial to close the file before updating the header or checking its size.
	f.Close()

	// Update WAV header with actual file sizes.
	if err := audio.UpdateWAVHeader(filename, bytesWritten); err != nil {
		log.Printf("ERROR: updating WAV header for %s: %v", filename, err)
	}

	// Conditionally remove the file if it's too short (likely just noise).
	const minFileSize = 1024 // 1KB threshold.
	if info, err := os.Stat(filename); err == nil && info.Size() < minFileSize {
		log.Printf("Recording %s is very short (%d bytes), deleting.", filename, info.Size())
		os.Remove(filename)
	} else if err == nil {
		log.Printf("Finished recording to %s, sending for processing.", filename)
		fileChan <- filename
	} else {
		log.Printf("ERROR: could not stat file %s to check size: %v", filename, err)
	}
}

// pullAndWriteSamples pulls all available samples from the sink and writes them to the file.
// It MUST be called continuously to drain the sink, even when not recording.
func pullAndWriteSamples(sink *app.Sink, f *os.File, isWriting *bool, bytesWritten *int64) {
	// Pull all available samples from the sink in a loop.
	for {
		sample := sink.TryPullSample(0)
		if sample == nil {
			break // No more samples in queue.
		}

		// Only write to the file if we are in a recording state.
		// Otherwise, we still pull the sample but just discard it.
		if f != nil && *isWriting {
			buffer := sample.GetBuffer()
			if buffer != nil {
				if _, err := f.Write(buffer.Bytes()); err != nil {
					log.Printf("ERROR: writing to file: %v", err)
					*isWriting = false // Stop writing on error.
				}
				*bytesWritten += int64(len(buffer.Bytes()))
				buffer.Unmap()
			}
		}
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}
