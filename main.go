// sudo apt install libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libgstreamer-plugins-good1.0-dev
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-glib/glib"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

const (
	// SilenceThreshold is the normalized RMS level below which we consider the audio to be silent.
	// You may need to adjust this value based on your microphone and environment.
	SilenceThreshold = 0.02

	// HangoverDuration is the amount of time to continue recording after the audio level
	// drops below the silence threshold. This prevents cutting off recordings prematurely.
	HangoverDuration = 2 * time.Second
)

// Constants for WAV header based on the pipeline's capsfilter:
// audio/x-raw, format=S16LE, layout=interleaved, channels=2, rate=48000
const (
	WAV_HEADER_SIZE     = 44 // Standard WAV header size
	WAV_CHANNELS        = 2
	WAV_SAMPLE_RATE     = 48000
	WAV_BITS_PER_SAMPLE = 16
	WAV_BYTE_RATE       = WAV_SAMPLE_RATE * WAV_CHANNELS * (WAV_BITS_PER_SAMPLE / 8) // 192000 bytes/sec
	WAV_BLOCK_ALIGN     = WAV_CHANNELS * (WAV_BITS_PER_SAMPLE / 8)                   // 4 bytes per sample frame
)

// createVADPipeline sets up a GStreamer pipeline that listens to an audio source,
// analyzes its volume, and records it to a file only when sound is detected.
// It uses a "tee" element to split the audio stream into two branches:
// 1. Analysis Branch: -> queue -> appsink (for calculating RMS in Go)
// 2. Recording Branch: -> queue -> valve -> wavenc -> appsink (for writing to a file in Go)
func createVADPipeline() (*gst.Pipeline, *app.Sink, *gst.Element, *app.Sink) {
	// Check CLI: gst-launch-1.0 pulsesrc ! audioconvert ! audioresample !  autoaudiosink
	//Devices: pactl list | grep -A2 'Source #' | grep 'Name: ' | cut -d" " -f2

	// Create a new pipeline
	pipeline := control(gst.NewPipeline("vad-recording-pipeline"))

	// Create elements
	// On modern Linux systems, PipeWire is often the underlying audio server.
	// Reverting to pulsesrc as pipewiresrc is not working in this environment.
	source := control(gst.NewElementWithName("pulsesrc", "pulse-source"))
	source.SetProperty("device", "alsa_output.pci-0000_00_1f.3.analog-stereo.monitor")
	// Increase the buffer time to make the source more resilient to startup latency.
	// This helps prevent race conditions in complex pipelines. Value is in microseconds.
	source.SetProperty("buffer-time", int64(500000)) // 500ms

	audioconvert := control(gst.NewElementWithName("audioconvert", "audio-convert"))

	audioresample := control(gst.NewElementWithName("audioresample", "audio-resample"))

	// Add a capsfilter to enforce a common, stable format before the tee.
	// This is the most robust position, as it allows the source and converters
	// to negotiate freely, then standardizes the stream before it is split.
	capsfilter := control(gst.NewElement("capsfilter"))
	capsfilter.SetProperty("caps", gst.NewCapsFromString(
		"audio/x-raw, format=S16LE, layout=interleaved, channels=2, rate=48000",
	))

	tee := control(gst.NewElement("tee"))

	// --- Analysis Branch Elements ---
	analysisQueue := control(gst.NewElement("queue"))

	vadsink := control(app.NewAppSink())
	vadsink.SetProperty("sync", false) // Don't synchronize on clock, get data as fast as possible
	vadsink.SetDrop(false)
	// Set the maximum number of buffers that can be queued. This is critical for stability.
	vadsink.SetMaxBuffers(10)

	// --- Recording Branch Elements ---
	recordingQueue := control(gst.NewElement("queue"))
	// Set the queue to be "leaky". In "downstream" mode (2), if the downstream
	// elements (valve, wavenc) are not accepting data, the queue will drop
	// old buffers instead of blocking. This is the definitive solution to prevent
	// an idle branch from stalling the entire pipeline at startup.
	// Changed to 0 (no leak) to ensure all data is passed for recording integrity.
	recordingQueue.SetProperty("leaky", 0) // 0 = No leak
	// The 'valve' element acts as a gate. We can open/close it to control the data flow.
	valve := control(gst.NewElement("valve"))
	valve.SetProperty("drop-mode", 1) // Allow pass pipeline events, other drop
	valve.SetProperty("drop", false)  // Start with the valve OPEN to guarantee startup, then close it immediately.

	// Use a second appsink for the recording branch. This allows Go to handle
	// file I/O, giving us the flexibility to create new files on the fly.
	recordingSink := control(app.NewAppSink())
	recordingSink.SetProperty("sync", false)
	recordingSink.SetDrop(false) // Do not drop data; ensure all samples are received for recording.

	// Add all elements to the pipeline
	verify(pipeline.AddMany(source, audioconvert, audioresample, capsfilter, tee, analysisQueue, vadsink.Element, recordingQueue, valve, recordingSink.Element))

	// Link the common path
	verify(gst.ElementLinkMany(source, audioconvert, audioresample, capsfilter, tee))

	// Link the analysis branch
	verify(gst.ElementLinkMany(tee, analysisQueue, vadsink.Element))

	// Link the recording branch
	verify(gst.ElementLinkMany(tee, recordingQueue, valve, recordingSink.Element))

	return pipeline, vadsink, valve, recordingSink
}

// vadState represents the state of the Voice Activity Detector.
type vadState struct {
	mu              sync.Mutex
	valve           *gst.Element
	isRecording     bool
	silenceEndTime  time.Time
	fileCounter     int
	fileControlChan chan<- string
}

// newVAD creates a new VAD controller.
func newVAD(valve *gst.Element, fileControlChan chan<- string) *vadState {
	return &vadState{
		valve:           valve,
		fileControlChan: fileControlChan,
	}
}

// processAudioChunk analyzes an audio chunk's RMS value and updates the recording state.
// It controls the 'valve' element to start or stop the flow of data to the filesink.
func (v *vadState) processAudioChunk(rms float64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	isLoud := rms > SilenceThreshold

	if isLoud {
		// If we detect sound, and we are not currently recording, we need to start.
		if !v.isRecording {
			v.isRecording = true
			v.fileCounter++
			newFilename := fmt.Sprintf("recording-%d.wav", v.fileCounter)

			v.fileControlChan <- "START:" + newFilename
			// Schedule the GStreamer state change to happen on the main GLib thread.
			// This is the safest way to modify a running pipeline from a goroutine.
			glib.IdleAdd(func() bool {
				fmt.Printf("\n>>> Sound detected! Starting recording...\n")
				v.valve.SetProperty("drop", false)
				return false // Do not call again
			})
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

			v.fileControlChan <- "STOP"
			// Schedule the GStreamer state change to happen on the main GLib thread.
			glib.IdleAdd(func() bool {
				fmt.Println("\n<<< Silence detected. Stopping recording.")
				v.valve.SetProperty("drop", true)
				return false // Do not call again
			})
		}
	}
}

// fileWriter is a dedicated goroutine for writing encoded audio data to files.
// It listens for control messages to start new files and finalize (and potentially delete) old ones.
func fileWriter(wg *sync.WaitGroup, sink *app.Sink, controlChan <-chan string) {
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
		select {
		case cmd, ok := <-controlChan:
			if !ok { // Channel closed, shutdown.
				if f != nil {
					f.Close()
				}
				return
			}

			if strings.HasPrefix(cmd, "START:") {
				if f != nil { // Close previous file if it exists
					f.Close()
				}
				currentFilename = strings.TrimPrefix(cmd, "START:")
				f, err = os.Create(currentFilename)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error creating file %s: %v\n", currentFilename, err)
					f = nil
					isWriting = false
				} else {
					// Write placeholder WAV header
					if err := writeWAVHeader(f); err != nil {
						fmt.Fprintf(os.Stderr, "Error writing WAV header to %s: %v\n", currentFilename, err)
						f.Close()
						f = nil
					} else {
						fmt.Printf("Created new recording file: %s\n", currentFilename)
					}
					isWriting = true
				}
			} else if cmd == "STOP" {
				if f != nil {
					f.Close()
					isWriting = false
					// Update WAV header with actual sizes
					if err := updateWAVHeader(currentFilename, bytesWritten); err != nil {
						fmt.Fprintf(os.Stderr, "Error updating WAV header for %s: %v\n", currentFilename, err)
					}
					// Conditionally remove the file. If it's very small, it was likely just noise.
					info, err := os.Stat(currentFilename)
					if err == nil && info.Size() < 1024 {
						fmt.Printf("Recording %s is very short (%d bytes), deleting.\n", currentFilename, info.Size())
						os.Remove(currentFilename)
					} else {
						fmt.Printf("Finished recording to %s\n", currentFilename)
					}
					f = nil
					bytesWritten = 0 // Reset for next recording
				}
			}

		case <-ticker.C:
			pullAndWriteSamples(sink, f, &isWriting, &bytesWritten)
		}
	}
}

// vadController is a dedicated goroutine that listens for RMS values and controls the
// recording valve. Isolating this GStreamer state change into its own goroutine
// is critical for preventing deadlocks.
func vadController(wg *sync.WaitGroup, vad *vadState, vadControlChan <-chan float64) {
	defer wg.Done()
	for rms := range vadControlChan {
		vad.processAudioChunk(rms)
	}
}

// writeWAVHeader writes a placeholder WAV header to the file.
// The dataSize and fileSize will need to be updated later.
func writeWAVHeader(f *os.File) error {
	header := make([]byte, WAV_HEADER_SIZE)

	// RIFF chunk
	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], 0) // Placeholder for ChunkSize (total file size - 8)
	copy(header[8:12], []byte("WAVE"))

	// FMT sub-chunk
	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16) // Subchunk1Size (16 for PCM)
	binary.LittleEndian.PutUint16(header[20:22], 1)  // AudioFormat (1 for PCM)
	binary.LittleEndian.PutUint16(header[22:24], WAV_CHANNELS)
	binary.LittleEndian.PutUint32(header[24:28], WAV_SAMPLE_RATE)
	binary.LittleEndian.PutUint32(header[28:32], WAV_BYTE_RATE)
	binary.LittleEndian.PutUint16(header[32:34], WAV_BLOCK_ALIGN)
	binary.LittleEndian.PutUint16(header[34:36], WAV_BITS_PER_SAMPLE)

	// DATA sub-chunk
	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], 0) // Placeholder for Subchunk2Size (data size)

	_, err := f.Write(header)
	return err
}

// updateWAVHeader updates the ChunkSize and Subchunk2Size in the WAV header.
func updateWAVHeader(filename string, dataSize int64) error {
	f, err := os.OpenFile(filename, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file %s for header update: %w", filename, err)
	}
	defer f.Close()

	// Update ChunkSize (total file size - 8)
	binary.LittleEndian.PutUint32(make([]byte, 4), uint32(dataSize+WAV_HEADER_SIZE-8))
	_, err = f.WriteAt(binary.LittleEndian.AppendUint32(nil, uint32(dataSize+WAV_HEADER_SIZE-8)), 4) // Write at offset 4
	if err != nil {
		return fmt.Errorf("failed to write RIFF chunk size: %w", err)
	}

	// Update Subchunk2Size (data size)
	_, err = f.WriteAt(binary.LittleEndian.AppendUint32(nil, uint32(dataSize)), 40) // Write at offset 40
	if err != nil {
		return fmt.Errorf("failed to write data chunk size: %w", err)
	}

	return nil
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
			break // No more samples in queue.
		}

		buffer := sample.GetBuffer()
		if buffer != nil {
			if _, err := f.Write(buffer.Bytes()); err != nil {
				fmt.Fprintf(os.Stderr, "Error writing to file: %v\n", err)
				*isWriting = false // Stop writing on error.
			}
			*bytesWritten += int64(len(buffer.Bytes()))
		}
		buffer.Unref()
	}
}

// pullSamples is a dedicated goroutine that only pulls samples from the GStreamer pipeline.
// It calculates the RMS and passes it to other goroutines for processing, but never
// modifies the pipeline state itself. This separation of concerns is key to avoiding deadlocks.
func pullSamples(wg *sync.WaitGroup, sink *app.Sink, rmsDisplayChan chan<- float64, vadControlChan chan<- float64) {
	defer wg.Done()

	for {
		sample := sink.PullSample()
		if sample == nil {
			fmt.Println("\nPuller goroutine: received nil sample, exiting.")
			return
		}

		buffer := sample.GetBuffer()
		if buffer != nil {
			samples := buffer.Map(gst.MapRead).AsInt16LESlice()
			if len(samples) > 0 {
				var sumOfSquares float64
				for _, s := range samples {
					sumOfSquares += float64(int64(s) * int64(s))
				}
				rms := math.Sqrt(sumOfSquares / float64(len(samples)))
				const normalizationFactor = 32768.0
				normalizedRms := rms / normalizationFactor

				// Send to display goroutine (non-blocking)
				select {
				case rmsDisplayChan <- normalizedRms:
				default:
				}

				// Send to VAD controller goroutine (non-blocking)
				select {
				case vadControlChan <- normalizedRms:
				default:
				}
			}
			buffer.Unmap()
		}
	}
}

func mainLoop(pipeline *gst.Pipeline) *glib.MainLoop {

	// Create a GLib Main Loop
	mainLoop := glib.NewMainLoop(glib.MainContextDefault(), false)

	// Handle messages from the pipeline bus
	bus := pipeline.GetBus()
	bus.AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			fmt.Println("End of stream.")
			mainLoop.Quit()
			return false // Stop watching
		case gst.MessageError:
			err := msg.ParseError()
			fmt.Fprintf(os.Stderr, "Error from element %s: %s\n", msg.Source(), err.Error())
			fmt.Fprintf(os.Stderr, "Debugging info: %s\n", err.DebugString())
			mainLoop.Quit()
			return false // Stop watching
		}
		return true // Continue watching
	})

	return mainLoop
}

// printBar encapsulates the expensive printing logic.
func printBar(rms float64, lastBarLength *int) {
	const barWidth = 100

	// Use a small threshold to avoid printing for near-silent audio.
	if !math.IsNaN(rms) {
		// Since RMS is now normalized to [0.0, 1.0], we can scale it directly to the bar size.
		currentBarLength := int(rms * barWidth)

		if currentBarLength > barWidth {
			currentBarLength = barWidth
		}

		// Only update the display if the integer length of the bar has changed.
		// This is more robust than comparing floats and prevents excessive printing.
		if currentBarLength != *lastBarLength {
			*lastBarLength = currentBarLength
			bar := strings.Repeat("=", currentBarLength)
			gap := strings.Repeat(" ", barWidth-currentBarLength)
			// This is the expensive call we are throttling.
			fmt.Printf("[%s%s]\n\033[1A", bar, gap)
		}
	}
}

func displayRMS(rmsChan chan float64) {
	// Refresh the display at a fixed rate (e.g., 20 times per second)
	// This is fast enough for a smooth UI but prevents overwhelming the terminal.
	const updateInterval = 50 * time.Millisecond
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	var lastPrintedBarLength = -1 // Initialize to -1 to guarantee the first print.
	var currentRMS float64

	for {
		select {
		case rms, ok := <-rmsChan:
			if !ok {
				break // Channel is closed.
			}
			currentRMS = rms // Keep track of the latest RMS value.
		case <-ticker.C:
			// Ticker fired. Time to update the display.
			printBar(currentRMS, &lastPrintedBarLength)
		}
	}
}

func main() {
	// Initialize GStreamer. This should be called once per application.
	gst.Init(nil)

	// Create buffered channels to decouple the "hot" GStreamer loop from other goroutines.
	rmsDisplayChan := make(chan float64, 10) // For the RMS volume bar
	vadControlChan := make(chan float64, 10) // For the VAD logic

	// Creation pipeline
	pipeline, vadsink, valve, recordingSink := createVADPipeline()

	// Start a "cold" goroutine for printing the volume bar.
	go displayRMS(rmsDisplayChan)

	// Create the VAD state controller.
	fileControlChan := make(chan string, 5)
	vad := newVAD(valve, fileControlChan)

	// Create a GLib Main Loop to handle GStreamer bus messages.
	mainLoop := mainLoop(pipeline)

	// Handle Ctrl+C signal for graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		fmt.Println("\nInterrupt received, initiating shutdown...")
		// 1. Signal to end pipline work
		pipeline.SendEvent(gst.NewEOSEvent())
		// 2. Schedule MainLoop.Quit() to be called from the main GStreamer thread.
		// This is the safest way to interact with the pipeline from a different
		// thread. This will cause mainLoop.Run() to return, allowing a clean shutdown.
		glib.IdleAdd(func() bool {
			mainLoop.Quit()
			return false // Do not call again
		})
	}()

	// Use a WaitGroup to ensure our goroutines shut down cleanly.
	var wg sync.WaitGroup
	// Start the VAD controller goroutine. This is the only goroutine allowed to change
	// the pipeline's state (by controlling the valve).
	wg.Add(1)
	go vadController(&wg, vad, vadControlChan)
	// Start the puller goroutine.
	wg.Add(1)
	go pullSamples(&wg, vadsink, rmsDisplayChan, vadControlChan)
	// Start the file writer goroutine.
	wg.Add(1)
	go fileWriter(&wg, recordingSink, fileControlChan) // Pass bytesWritten

	// Start the pipeline
	verify(pipeline.SetState(gst.StatePlaying))

	fmt.Println("Listening for audio... Recording will start when sound is detected.")
	fmt.Println("Each utterance will be saved to a new file (e.g., recording-1.wav). Press Ctrl+C to exit.")

	// Block until the pipeline's bus signals EOS or an error.
	mainLoop.Run()

	fmt.Println("Stopping pipeline...")
	// Clean up
	close(rmsDisplayChan)
	close(vadControlChan)
	close(fileControlChan)
	// Set the pipeline to NULL state. This is a blocking call that will tear down
	// the pipeline and cause sink.PullSample() to unblock and return nil.
	pipeline.SetState(gst.StateNull)
	fmt.Println("Pipeline stopped.")

	// Now that the pipeline is stopped, wait for the processing goroutines to finish their cleanup.
	wg.Wait()
	fmt.Println("All goroutines finished.")
}

func control[T any](object T, err error) T {
	if err != nil {
		panic(err)
	}
	return object
}

func verify(err error) {
	if err != nil {
		panic(err)
	}
}
