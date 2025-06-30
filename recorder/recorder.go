package recorder

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"capgemini.com/audio" // Import the audio package for WAV constants
	"capgemini.com/config"
	"capgemini.com/helpers"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

const (
	DEBUG = false
)

type Recorder struct {
	file            *os.File
	currentFilename string
	bytesWritten    int64 // Track bytes written to the current file
	isWriting       bool
	recordingSink   *app.Sink
	Element         *gst.Element
}

func CreateRecorderSink() *Recorder {
	var r Recorder
	// Use a second appsink for the recording branch. This allows Go to handle
	// file I/O, giving us the flexibility to create new files on the fly.
	r.recordingSink = helpers.Control(app.NewAppSink())
	helpers.Verify(r.recordingSink.SetProperty("sync", false))
	r.recordingSink.SetDrop(false) // Do not drop data; ensure all samples are received for recording.
	// Set a max buffer to prevent runaway memory usage and add stability.
	r.recordingSink.SetMaxBuffers(10)
	r.Element = r.recordingSink.Element
	return &r
}

// FileWriter is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
func (r *Recorder) FileWriter(wg *sync.WaitGroup, controlChan <-chan string, fileChan chan<- string) {
	defer wg.Done()

	var err error

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
				if r.file != nil {
					// This is a final cleanup. If a file was open when the channel
					// closed, finalize it before exiting the goroutine.
					r.finalizeRecording(fileChan)
				}
				log.Println("Recording is finished")
				return
			}

			if strings.HasPrefix(cmd, "START:") {
				r.currentFilename = strings.TrimPrefix(cmd, "START:")
				r.bytesWritten = 0 // Reset for new recording
				err = r.startNewRecording()
				if err != nil {
					log.Printf("ERROR: %v", err)
					r.file = nil
					r.isWriting = false
				} else {
					r.isWriting = true
				}
			} else if strings.HasPrefix(cmd, "STOP:") {
				r.finalizeRecording(fileChan)
				r.file = nil
				r.isWriting = false
				r.currentFilename = "" // Reset filename
			}

		case <-ticker.C:
			// Always pull samples from the sink. This is crucial to prevent the
			// GStreamer pipeline from blocking if the sink's buffer fills up.
			r.pullAndWriteSamples()
		}
	}
}

// startNewRecording handles the creation of a new recording file. It closes any
// previous file, creates the new one, and writes a placeholder WAV header.
func (r *Recorder) startNewRecording() error {
	if r.file != nil {
		// If a previous file handle exists, it implies a STOP command was missed.
		// We should close it to prevent resource leaks.
		log.Printf("Warning: Starting new recording for %s while previous file was still open. Closing it now.", r.currentFilename)
		r.file.Close()
	}

	var err error
	r.file, err = os.Create(r.currentFilename)
	if err != nil {
		// The error is returned to the caller to be handled.
		return fmt.Errorf("creating file %s: %w", r.currentFilename, err)
	}

	// Write placeholder WAV header
	if err := audio.WriteWAVHeader(r.file); err != nil {
		r.file.Close()               // Clean up the created file on header write error.
		os.Remove(r.currentFilename) // Also remove the file.
		return fmt.Errorf("writing WAV header to %s: %w", r.currentFilename, err)
	}

	return nil
}

// finalizeRecording closes the file, updates its WAV header, and, if the file is large enough,
// sends it to the processing channel. Otherwise, it deletes the file.
func (r *Recorder) finalizeRecording(fileChan chan<- string) {
	if r.file == nil {
		return
	}
	// It's crucial to close the file before updating the header or checking its size.
	r.file.Close()

	// Update WAV header with actual file sizes.
	if err := audio.UpdateWAVHeader(r.currentFilename, r.bytesWritten); err != nil {
		log.Printf("ERROR: updating WAV header for %s: %v", r.currentFilename, err)
	}

	// Conditionally remove the file if it's too short (likely just noise).
	if info, err := os.Stat(r.currentFilename); err == nil && info.Size() < config.C.Recorder.MinFileSizeBytes {
		log.Printf("Recording %s is very short (%d bytes), deleting.", r.currentFilename, info.Size())
		os.Remove(r.currentFilename)
	} else if err == nil {
		log.Printf("Finished recording to %s (%d bytes), sending for processing.", r.currentFilename, info.Size())
		fileChan <- r.currentFilename
	} else {
		log.Printf("ERROR: could not stat file %s to check size: %v", r.currentFilename, err)
	}
}

// pullAndWriteSamples pulls all available samples from the sink and writes them to the file.
// It MUST be called continuously to drain the sink, even when not recording.
func (r *Recorder) pullAndWriteSamples() {
	// Pull all available samples from the sink in a loop.
	for {
		sample := r.recordingSink.TryPullSample(0)
		if sample == nil {
			break // No more samples in queue.
		}

		// Only write to the file if we are in a recording state.
		// Otherwise, we still pull the sample but just discard it.
		if r.file != nil && r.isWriting {
			buffer := sample.GetBuffer()
			if buffer != nil {
				if _, err := r.file.Write(buffer.Bytes()); err != nil {
					log.Printf("ERROR: writing to file: %v", err)
					r.isWriting = false // Stop writing on error.
				}
				r.bytesWritten += int64(len(buffer.Bytes()))
				buffer.Unmap()
			}
		}
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}
