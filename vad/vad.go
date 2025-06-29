package vad

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	DEBUG = false
)

const (
	// SilenceThreshold is the normalized RMS level below which we consider the audio to be silent.
	// You may need to adjust this value based on your microphone and environment.
	SilenceThreshold = 0.02

	// HangoverDuration is the amount of time to continue recording after the audio level
	// drops below the silence threshold. This prevents cutting off recordings prematurely.
	HangoverDuration = 2 * time.Second
)

// State represents the state of the Voice Activity Detector.
type State struct {
	mu              sync.Mutex
	isRecording     bool
	silenceEndTime  time.Time
	fileCounter     int
	fileControlChan chan<- string
}

// NewVAD creates a new VAD controller.
func NewVAD(fileControlChan chan<- string) *State {
	return &State{
		fileControlChan: fileControlChan,
	}
}

// ProcessAudioChunk analyzes an audio chunk's RMS value and updates the recording state.
// It controls the 'valve' element to start or stop the flow of data to the filesink.
func (v *State) ProcessAudioChunk(rms float64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	isLoud := rms > SilenceThreshold

	if isLoud {
		// If we detect sound, and we are not currently recording, we need to start.
		if !v.isRecording {
			v.isRecording = true
			v.fileCounter++
			newFilename := fmt.Sprintf("recording-%d.wav", v.fileCounter)

			log.Println(">>> Sound detected! Starting recording...")
			v.fileControlChan <- "START:" + newFilename
		}
		// If it's loud, we are not in a hangover period, so reset the timer.
		v.silenceEndTime = time.Time{}
	} else if v.isRecording { // is silent, but we were recording
		if v.silenceEndTime.IsZero() {
			// First moment of silence, start the hangover timer.
			v.silenceEndTime = time.Now().Add(HangoverDuration)
		} else if time.Now().After(v.silenceEndTime) {
			// Hangover period is over. Stop recording.
			v.isRecording = false

			log.Println("<<< Silence detected. Stopping recording.")
			newFilename := fmt.Sprintf("recording-%d.wav", v.fileCounter)
			v.fileControlChan <- "STOP:" + newFilename
		}
	}
}

// Controller is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
func Controller(wg *sync.WaitGroup, vad *State, vadControlChan <-chan float64) {
	defer wg.Done()
	for rms := range vadControlChan {
		if DEBUG {
			log.Println("VAD received RMS")
		}
		vad.ProcessAudioChunk(rms)
	}
	log.Println("VAD work finished")
}
