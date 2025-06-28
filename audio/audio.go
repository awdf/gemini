package audio

import (
	"encoding/binary"
	"fmt"
	"os"
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

// WriteWAVHeader writes a placeholder WAV header to the file.
// The dataSize and fileSize will need to be updated later.
func WriteWAVHeader(f *os.File) error {
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

// UpdateWAVHeader updates the ChunkSize and Subchunk2Size in the WAV header.
func UpdateWAVHeader(filename string, dataSize int64) error {
	f, err := os.OpenFile(filename, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file %s for header update: %w", filename, err)
	}
	defer f.Close()

	// Update ChunkSize (total file size - 8)
	// Note: AppendUint32 creates a new slice. We need to write the bytes from that slice.
	// The `binary.LittleEndian.AppendUint32` function appends the uint32 to the provided
	// byte slice. If the slice is `nil`, it creates a new one.
	// We then write the resulting 4 bytes at the correct offset.
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
