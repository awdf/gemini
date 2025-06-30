package audio

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
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

// Constants for Gemini TTS audio format:
// 24000 Hz, 1 channel (mono), 16-bit signed little-endian.
const (
	TTS_CHANNELS    = 1
	TTS_SAMPLE_RATE = 24000
)

// WavFile encapsulates the state and operations for a single WAV audio file.
type WavFile struct {
	file         *os.File
	filename     string
	bytesWritten int64
}

// NewWavFile creates a new WAV file, writes a placeholder header, and returns a WavFile object.
func NewWavFile(filename string) (*WavFile, error) {
	file, err := os.Create(filename)
	if err != nil {
		return nil, fmt.Errorf("creating file %s: %w", filename, err)
	}

	// Write placeholder WAV header
	if err := writeWAVHeader(file); err != nil {
		file.Close()        // Clean up the created file on header write error.
		os.Remove(filename) // Also remove the file.
		return nil, fmt.Errorf("writing WAV header to %s: %w", filename, err)
	}

	return &WavFile{
		file:     file,
		filename: filename,
	}, nil
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

// updateHeader updates the ChunkSize and Subchunk2Size in the WAV header.
// It is an internal method for the WavFile object.
func (w *WavFile) updateHeader() error {
	f, err := os.OpenFile(w.filename, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file %s for header update: %w", w.filename, err)
	}
	defer f.Close()

	// Update ChunkSize (total file size - 8)
	// We write the resulting 4 bytes at the correct offset.
	_, err = f.WriteAt(binary.LittleEndian.AppendUint32(nil, uint32(w.bytesWritten+WAV_HEADER_SIZE-8)), 4) // Write at offset 4
	if err != nil {
		return fmt.Errorf("failed to write RIFF chunk size: %w", err)
	}

	// Update Subchunk2Size (data size)
	_, err = f.WriteAt(binary.LittleEndian.AppendUint32(nil, uint32(w.bytesWritten)), 40) // Write at offset 40
	if err != nil {
		return fmt.Errorf("failed to write data chunk size: %w", err)
	}

	return nil
}

// Write appends raw audio data to the WAV file.
func (w *WavFile) Write(data []byte) error {
	n, err := w.file.Write(data)
	if err != nil {
		return fmt.Errorf("writing to wav file %s: %w", w.filename, err)
	}
	w.bytesWritten += int64(n)
	return nil
}

// Close finalizes the WAV file by updating the header with the correct size and closing the file handle.
func (w *WavFile) Close() error {
	// It's crucial to close the file before updating the header.
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("failed to close file handle for %s: %w", w.filename, err)
	}

	// Update WAV header with actual file sizes.
	return w.updateHeader()
}

// Filename returns the path to the WAV file.
func (w *WavFile) Filename() string {
	return w.filename
}

// Size returns the number of audio data bytes written to the file.
func (w *WavFile) Size() int64 {
	return w.bytesWritten
}

// PlayRawPCM plays a raw PCM audio blob using GStreamer.
// It creates a temporary pipeline to play the provided byte slice.
func PlayRawPCM(data []byte, rate, channels int) error {
	// Create a new pipeline
	pipeline, err := gst.NewPipeline("audio-player")
	if err != nil {
		return fmt.Errorf("failed to create pipeline: %w", err)
	}

	// Create elements
	appsrc, err := app.NewAppSrc()
	if err != nil {
		return fmt.Errorf("failed to create appsrc: %w", err)
	}

	// We must use a capsfilter to describe this format to the pipeline,
	// as there is no WAV header.
	capsfilter, err := gst.NewElement("capsfilter")
	if err != nil {
		return fmt.Errorf("failed to create capsfilter: %w", err)
	}
	capsfilter.SetProperty("caps", gst.NewCapsFromString(
		fmt.Sprintf("audio/x-raw, format=S16LE, layout=interleaved, channels=%d, rate=%d", channels, rate),
	))

	audioconvert, err := gst.NewElement("audioconvert")
	if err != nil {
		return fmt.Errorf("failed to create audioconvert: %w", err)
	}
	audioresample, err := gst.NewElement("audioresample")
	if err != nil {
		return fmt.Errorf("failed to create audioresample: %w", err)
	}
	audiosink, err := gst.NewElement("autoaudiosink")
	if err != nil {
		return fmt.Errorf("failed to create autoaudiosink: %w", err)
	}

	// Add elements to the pipeline
	pipeline.AddMany(appsrc.Element, capsfilter, audioconvert, audioresample, audiosink)

	// Link elements
	if err = gst.ElementLinkMany(appsrc.Element, capsfilter, audioconvert, audioresample, audiosink); err != nil {
		return fmt.Errorf("failed to link elements: %w", err)
	}

	// Push the audio data into appsrc
	buffer := gst.NewBufferFromBytes(data)
	if ret := appsrc.PushBuffer(buffer); ret != gst.FlowOK {
		return fmt.Errorf("failed to push buffer to appsrc: %v", ret)
	}

	// Signal end of stream
	if ret := appsrc.EndStream(); ret != gst.FlowOK {
		return fmt.Errorf("failed to send EOS to appsrc: %v", ret)
	}

	// Start playing
	pipeline.SetState(gst.StatePlaying)

	// Wait for the pipeline to finish by watching the bus for an EOS or Error message.
	bus := pipeline.GetBus()
	msg := bus.TimedPopFiltered(gst.ClockTimeNone, gst.MessageEOS|gst.MessageError)
	if msg != nil && msg.Type() == gst.MessageError {
		return fmt.Errorf("playback error: %s", msg.ParseError().Error())
	}

	// Clean up
	pipeline.SetState(gst.StateNull)
	return nil
}
