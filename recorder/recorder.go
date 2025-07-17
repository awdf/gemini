package recorder

import (
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gemini/audio" // Import the audio package for WAV constants
	"gemini/config"
	"gemini/helpers"

	"github.com/asaskevich/EventBus"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

type Recorder struct {
	currentWav    *audio.WavFile // The active WAV file being written to.
	isWriting     bool
	recordingSink *app.Sink
	Element       *gst.Element
	wg            *sync.WaitGroup
	controlChan   <-chan string
	fileChan      chan<- string
	bus           *EventBus.Bus
}

func NewRecorderSink(wg *sync.WaitGroup, controlChan <-chan string, fileChan chan<- string, bus *EventBus.Bus) *Recorder {
	var r Recorder
	// Use a second appsink for the recording branch. This allows Go to handle
	// file I/O, giving us the flexibility to create new files on the fly.
	r.recordingSink = helpers.Control(app.NewAppSink())
	helpers.Verify(r.recordingSink.SetProperty("sync", false))
	r.recordingSink.SetDrop(false) // Do not drop data; ensure all samples are received for recording.
	// Set a max buffer to prevent runaway memory usage and add stability.
	r.recordingSink.SetMaxBuffers(10)
	r.Element = r.recordingSink.Element
	r.wg = wg
	r.controlChan = controlChan
	r.fileChan = fileChan
	r.bus = bus
	return &r
}

// Run is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
func (r *Recorder) Run() {
	defer close(r.fileChan)
	defer r.wg.Done()

	// Use a ticker to poll for new samples without running a 100% CPU busy-loop.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {

		if config.C.Trace {
			log.Println("Write to file")
		}

		select {
		case cmd, ok := <-r.controlChan:
			if !ok { // Channel closed, shutdown.
				if r.currentWav != nil {
					// Final cleanup if a file was open.
					r.finalizeAndSend()
				}
				log.Println("Recording is finished")
				return
			}

			if strings.HasPrefix(cmd, "START:") {
				filename := strings.TrimPrefix(cmd, "START:")
				// If a previous file was being written, finalize it first.
				if r.currentWav != nil {
					log.Printf("Warning: START received while a recording was in progress. Finalizing previous file.")
					r.finalizeAndSend()
				}
				var err error
				r.currentWav, err = audio.NewWavFile(filename)
				if err != nil {
					log.Printf("ERROR: failed to start new recording: %v", err)
					r.currentWav = nil
					r.isWriting = false
				} else {
					r.isWriting = true
				}
			} else if strings.HasPrefix(cmd, "STOP:") {
				filename := strings.TrimPrefix(cmd, "STOP:")
				// Check that the STOP command is for the file we think we are writing.
				if r.currentWav != nil && r.currentWav.Filename() != filename {
					log.Printf("Warning: STOP command for '%s' received, but current recording is '%s'. Finalizing current recording.", filename, r.currentWav.Filename())
				}
				r.finalizeAndSend() // Finalize the current recording.
				r.isWriting = false
			}

		case <-ticker.C:
			// Always pull samples from the sink. This is crucial to prevent the
			// GStreamer pipeline from blocking if the sink's buffer fills up.
			r.pullAndWriteSamples()
		}
	}
}

// finalizeAndSend closes the current WAV file, decides whether to keep it,
// and sends it for processing if valid.
func (r *Recorder) finalizeAndSend() {
	if r.currentWav == nil {
		return
	}

	filename := r.currentWav.Filename()
	size := r.currentWav.Size()

	if err := r.currentWav.Close(); err != nil {
		log.Printf("ERROR: failed to finalize recording for %s: %v", filename, err)
		// Even with an error, we nullify the wav object to prevent reuse.
		r.currentWav = nil
		return
	}

	// Conditionally remove the file if it's too short (likely just noise).
	if size < config.C.Recorder.MinFileSizeBytes {
		log.Printf("Recording %s is too short (%d bytes), deleting.", filename, size)
		if err := os.Remove(filename); err != nil {
			log.Printf("WARNING: failed to remove short recording %s: %v", filename, err)
		}
	} else {
		log.Printf("Finished recording to %s (%d bytes), sending for processing.", filename, size)
		r.fileChan <- filename
		// if helpers.SafeSend(r.fileChan, filename) {
		// 	log.Printf("Could not send %s for processing, AI channel is closed.", filename)
		// }
	}

	// Reset for the next recording.
	r.currentWav = nil
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
		if r.currentWav != nil && r.isWriting {
			buffer := sample.GetBuffer()
			if buffer != nil {
				if err := r.currentWav.Write(buffer.Bytes()); err != nil {
					log.Printf("ERROR: %v", err)
					r.isWriting = false // Stop writing on error.
				}
				buffer.Unmap()
			}
		}
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}

// ProcessExistingRecordings scans the current directory for unprocessed recordings,
// queues them for AI processing, and returns the highest recording number found.
func (r *Recorder) ProcessExistingRecordings() int {
	log.Println("Checking for existing recordings to process...")

	files, err := os.ReadDir(".")
	if err != nil {
		log.Printf("ERROR: could not read current directory: %v", err)
		return 0
	}

	re := regexp.MustCompile(`^recording-(\d+)\.wav$`)
	type recordingInfo struct {
		name string
		num  int
	}
	var recordings []recordingInfo

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		matches := re.FindStringSubmatch(file.Name())
		if len(matches) > 1 {
			if num, err := strconv.Atoi(matches[1]); err == nil {
				recordings = append(recordings, recordingInfo{name: file.Name(), num: num})
			}
		}
	}

	if len(recordings) == 0 {
		log.Println("No existing recordings found.")
		return 0
	}

	// Sort files numerically to process them in order.
	sort.Slice(recordings, func(i, j int) bool {
		return recordings[i].num < recordings[j].num
	})

	log.Printf("Found %d existing recordings. Queueing for processing.", len(recordings))
	for _, rec := range recordings {
		log.Printf("Queueing existing recording: %s", rec.name)
		if helpers.SafeSend(r.fileChan, rec.name) {
			log.Println("Could not queue existing recordings, AI channel is closed. Aborting.")
			break
		}
	}

	// The highest number is in the last element of the sorted slice.
	return recordings[len(recordings)-1].num
}
