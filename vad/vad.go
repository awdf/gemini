package vad

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"

	"gemini/config"
)

const (
	// MarkerStart is the prefix for a message indicating recording should start.
	MarkerStart = "START:"
	// MarkerStop is the prefix for a message indicating recording should stop.
	MarkerStop = "STOP:"
)

// Engine implements a simple Voice Activity Detection (VAD) logic.
// It analyzes a stream of RMS audio levels and determines when speech starts and stops.
type Engine struct {
	mu              sync.RWMutex
	wg              *sync.WaitGroup
	fileControlChan chan<- string
	vadControlChan  <-chan float64
	bus             *EventBus.Bus
	isRecording     bool
	fileCounter     int
	silenceEndTime  time.Time
	warmupEndTime   time.Time
}

// NewVAD creates a new VAD engine.
func NewVAD(wg *sync.WaitGroup, fileControlChan chan<- string, vadControlChan <-chan float64, bus *EventBus.Bus) *Engine {
	warmupDuration := config.C.VAD.WarmUpDuration()
	fmt.Println("Listening for audio... Recording will start when sound is detected.")
	if warmupDuration > 0 {
		log.Printf("VAD initialized with a warm-up period of %s", warmupDuration)
		fmt.Printf("VAD initialized with a warm-up period of %s\n", warmupDuration)
	}

	return &Engine{
		wg:              wg,
		fileControlChan: fileControlChan,
		vadControlChan:  vadControlChan,
		bus:             bus,
		warmupEndTime:   time.Now().Add(warmupDuration),
	}
}

// SetFileCounter sets the initial index for recording filenames.
// This should be called before the Run method is started.
func (e *Engine) SetFileCounter(i int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fileCounter = i
}

// IsRecording returns true if the VAD engine currently detects speech.
// This is useful for testing.
func (e *Engine) IsRecording() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isRecording
}

// FileCounter returns the current recording file index.
// This is useful for testing.
func (e *Engine) FileCounter() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.fileCounter
}

// Run is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
func (e *Engine) Run() {
	defer e.wg.Done()
	defer close(e.fileControlChan)
	log.Println("VAD work started")

	warmupOver := e.warmupEndTime.IsZero() || time.Now().After(e.warmupEndTime)

	for rms := range e.vadControlChan {
		if config.C.Trace {
			log.Printf("VAD received RMS: %.2f\n", rms)
		}

		// Handle warmup period
		if !warmupOver {
			if time.Now().Before(e.warmupEndTime) {
				continue // Still in warm-up, ignore audio.
			}
			// Warm-up has just completed.
			log.Println("VAD warm-up complete. Now actively listening for speech.")
			(*e.bus).Publish("main:topic", "ready:vad.run")
			warmupOver = true
		}

		isLoud := rms > config.C.VAD.SilenceThreshold
		hangover := config.C.VAD.HangoverDuration()

		var startCmd, stopCmd string

		e.mu.Lock()
		isRecording := e.isRecording
		if isLoud {
			if !isRecording {
				e.isRecording = true
				e.fileCounter++
				filename := fmt.Sprintf("recording-%d.wav", e.fileCounter)
				config.DebugPrintf(">>> Sound detected! RMS: %.2f, Starting recording to %s\n", rms, filename)
				startCmd = MarkerStart + filename
			}
			e.silenceEndTime = time.Time{} // Reset silence timer
		} else if isRecording { // is silent, but we were recording
			if e.silenceEndTime.IsZero() {
				// First moment of silence, set the end time for the hangover period.
				e.silenceEndTime = time.Now().Add(hangover)
			} else if time.Now().After(e.silenceEndTime) {
				// Hangover period is over. Stop recording.
				e.isRecording = false
				filename := fmt.Sprintf("recording-%d.wav", e.fileCounter)
				config.DebugPrintln("<<< Silence detected. Stopping recording for " + filename)
				stopCmd = MarkerStop + filename
			}
		}
		e.mu.Unlock()

		// Send on channels outside the lock to prevent deadlocks.
		if startCmd != "" {
			e.fileControlChan <- startCmd
		}
		if stopCmd != "" {
			e.fileControlChan <- stopCmd
		}
	}
	log.Println("VAD work finished")
}
