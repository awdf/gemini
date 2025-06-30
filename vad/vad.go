package vad

import (
	"fmt"
	"log"
	"sync"
	"time"

	"capgemini.com/config"
)

const (
	DEBUG = false
)

// VADEngine represents the state of the Voice Activity Detector.
type VADEngine struct {
	mu              sync.Mutex
	isRecording     bool
	silenceEndTime  time.Time
	fileCounter     int
	fileControlChan chan<- string
}

// CreateVAD creates a new VAD controller.
func CreateVAD(fileControlChan chan<- string) *VADEngine {
	return &VADEngine{
		fileControlChan: fileControlChan,
	}
}

// ProcessAudioChunk analyzes an audio chunk's RMS value and updates the recording state.
// It controls the 'valve' element to start or stop the flow of data to the filesink.
func (v *VADEngine) ProcessAudioChunk(rms float64) {
	v.mu.Lock()
	defer v.mu.Unlock()

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

// Controller is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
func (v *VADEngine) Controller(wg *sync.WaitGroup, vadControlChan <-chan float64) {
	defer wg.Done()
	for rms := range vadControlChan {
		if DEBUG {
			log.Println("VAD received RMS")
		}
		v.ProcessAudioChunk(rms)
	}
	log.Println("VAD work finished")
}
