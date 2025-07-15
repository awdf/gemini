package vad

import (
	"fmt"
	"log"
	"sync"
	"time"

	"capgemini.com/config"
	"github.com/asaskevich/EventBus"
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
	bus             *EventBus.Bus
}

// NewVAD creates a new VAD controller.
func NewVAD(wg *sync.WaitGroup, fileControlChan chan<- string, vadControlChan <-chan float64, bus *EventBus.Bus) *VADEngine {
	// Get the warm-up duration from the configuration.
	warmupDuration := config.C.VAD.WarmUpDuration()
	fmt.Println("Listening for audio... Recording will start when sound is detected. ")
	if warmupDuration > 0 {
		log.Printf("VAD initialised with a warm-up period of %s", warmupDuration)
		fmt.Printf("VAD initialised with a warm-up period of %s\n", warmupDuration)
	}
	return &VADEngine{
		wg:              wg,
		fileControlChan: fileControlChan,
		vadControlChan:  vadControlChan,
		// Set the time when the warm-up period will be over.
		warmupEndTime: time.Now().Add(warmupDuration),
		bus:           bus,
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
		go func() {
			//It is blocking call, and we can't wait here,
			// otherwise vadControlChan channel get overhead
			(*v.bus).Publish("main:topic", "ready:vad.processAudioChunk")
		}()
		v.warmupEndTime = time.Time{}
	}

	isLoud := rms > config.C.VAD.SilenceThreshold

	if isLoud {
		// If we detect sound, and we are not currently recording, we need to start.
		if !v.isRecording {
			v.isRecording = true
			v.fileCounter++
			newFilename := fmt.Sprintf("recording-%d.wav", v.fileCounter)
			config.DebugPrintf(">>> Sound detected! RMS: %.2f, Starting recording...\n", rms)
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
			config.DebugPrintln("<<< Silence detected. Stopping recording.")
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
	log.Println("VAD work started")

	for rms := range v.vadControlChan {
		if config.C.Trace {
			log.Printf("VAD received RMS: %.2f\n", rms)
		}
		v.ProcessAudioChunk(rms)
	}
	log.Println("VAD work finished")
}
