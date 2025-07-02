package vad

import (
	"fmt"
	"log"
	"sync"
	"time"

	"capgemini.com/config"
)

// VADEngine represents the state of the Voice Activity Detector.
type VADEngine struct {
	mu              sync.Mutex
	isRecording     bool
	silenceEndTime  time.Time
	warmupEndTime   time.Time
	fileCounter     int
	fileControlChan chan<- string
	wg              *sync.WaitGroup
	vadControlChan  <-chan float64
}

// NewVAD creates a new VAD controller.
func NewVAD(wg *sync.WaitGroup, fileControlChan chan<- string, vadControlChan <-chan float64) *VADEngine {
	// Get the warm-up duration from the configuration.
	warmupDuration := config.C.VAD.WarmUpDuration()
	if warmupDuration > 0 {
		log.Printf("VAD initialised with a warm-up period of %s", warmupDuration)
	}
	return &VADEngine{
		wg:              wg,
		fileControlChan: fileControlChan,
		vadControlChan:  vadControlChan,
		// Set the time when the warm-up period will be over.
		warmupEndTime: time.Now().Add(warmupDuration),
	}
}

// SetFileCounter sets the starting number for the file counter to avoid overwriting
// existing recordings.
func (v *VADEngine) SetFileCounter(start int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fileCounter = start
}

// ProcessAudioChunk analyzes an audio chunk's RMS value and updates the recording state.
// It controls the 'valve' element to start or stop the flow of data to the filesink.
func (v *VADEngine) ProcessAudioChunk(rms float64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Check if the warm-up period is active.
	if !v.warmupEndTime.IsZero() {
		if time.Now().Before(v.warmupEndTime) {
			return // Still in warm-up, so ignore this audio chunk.
		}
		// The warm-up period has just ended. Log it and clear the timer
		// so this check doesn't run for every subsequent chunk.
		log.Println("VAD warm-up complete. Now actively listening for speech.")
		v.warmupEndTime = time.Time{}
	}

	isLoud := rms > config.C.VAD.SilenceThreshold

	if isLoud {
		// If we detect sound, and we are not currently recording, we need to start.
		if !v.isRecording {
			v.isRecording = true
			v.fileCounter++
			newFilename := fmt.Sprintf("recording-%d.wav", v.fileCounter)

			log.Printf(">>> Sound detected! RMS: %.2f, Starting recording...\n", rms)
			v.fileControlChan <- "START:" + newFilename
		}
		// If it's loud, we are not in a hangover period, so reset the timer.
		v.silenceEndTime = time.Time{}
	} else if v.isRecording { // is silent, but we were recording
		if v.silenceEndTime.IsZero() {
			// First moment of silence, start the hangover timer.
			v.silenceEndTime = time.Now().Add(config.C.VAD.HangoverDuration())
		} else if time.Now().After(v.silenceEndTime) {
			// Hangover period is over. Stop recording.
			v.isRecording = false

			log.Println("<<< Silence detected. Stopping recording.")
			newFilename := fmt.Sprintf("recording-%d.wav", v.fileCounter)
			v.fileControlChan <- "STOP:" + newFilename
		}
	}
}

// Run is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
func (v *VADEngine) Run() {
	defer close(v.fileControlChan)
	defer v.wg.Done()

	for rms := range v.vadControlChan {
		if config.C.Debug {
			log.Println("VAD received RMS")
		}
		v.ProcessAudioChunk(rms)
	}
	log.Println("VAD work finished")
}
