package recorder

import (
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
					f.Close()
				}
				log.Println("Recording is finished")
				return
			}

			if strings.HasPrefix(cmd, "START:") {
				if f != nil { // Close previous file if it exists
					f.Close()
				}
				currentFilename = strings.TrimPrefix(cmd, "START:")
				f, err = os.Create(currentFilename)
				if err != nil {
					log.Printf("ERROR: creating file %s: %v", currentFilename, err)
					f = nil
					isWriting = false
				} else {
					// Write placeholder WAV header
					if err := audio.WriteWAVHeader(f); err != nil {
						log.Printf("ERROR: writing WAV header to %s: %v", currentFilename, err)
						f.Close()
						f = nil
					} else {
						log.Printf("Created new recording file: %s", currentFilename)
					}
					isWriting = true
				}
			} else if strings.HasPrefix(cmd, "STOP:") {
				if f != nil {
					f.Close()
					isWriting = false
					// Update WAV header with actual sizes
					if err := audio.UpdateWAVHeader(currentFilename, bytesWritten); err != nil {
						log.Printf("ERROR: updating WAV header for %s: %v", currentFilename, err)
					}
					// Conditionally remove the file. If it's very small, it was likely just noise.
					info, err := os.Stat(currentFilename)
					if err == nil && info.Size() < 1024 {
						log.Printf("Recording %s is very short (%d bytes), deleting.", currentFilename, info.Size())
						os.Remove(currentFilename)
					} else {
						log.Printf("Finished recording to %s", currentFilename)
					}
					f = nil
					bytesWritten = 0 // Reset for next recording

					currentFilename = strings.TrimPrefix(cmd, "STOP:")
					fileChan <- currentFilename
				}
			}

		case <-ticker.C:
			pullAndWriteSamples(sink, f, &isWriting, &bytesWritten)
		}
	}
}

// pullAndWriteSamples pulls all available samples from the sink and writes them to the file.
func pullAndWriteSamples(sink *app.Sink, f *os.File, isWriting *bool, bytesWritten *int64) {
	if f == nil || !*isWriting {
		return
	}

	// Pull all available samples from the sink.
	for {
		sample := sink.TryPullSample(0)
		if sample == nil {
			if DEBUG {
				log.Println("Writer goroutine: no samples in queue, exiting.")
			}
			break // No more samples in queue.
		}

		if DEBUG {
			log.Println("Writer goroutine: received sample")
		}

		buffer := sample.GetBuffer()
		if buffer != nil {
			if _, err := f.Write(buffer.Bytes()); err != nil {
				log.Printf("ERROR: writing to file: %v", err)
				*isWriting = false // Stop writing on error.
			}
			*bytesWritten += int64(len(buffer.Bytes()))
		}
		buffer.Unmap()
		//IMPORTANT: Golang GStreamer unref sample automatically
		// sample.Unref()
	}
}
