package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-gst/go-gst/gst"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWavFile_Lifecycle covers the creation, writing, and closing of a WavFile.
func TestWavFile_Lifecycle(t *testing.T) {
	tempDir := t.TempDir()
	filename := filepath.Join(tempDir, "test.wav")

	// 1. Create a new WAV file
	wavFile, err := NewWavFile(filename)
	require.NoError(t, err, "NewWavFile should not return an error")
	require.NotNil(t, wavFile, "NewWavFile should return a valid WavFile object")

	// 2. Check initial state
	assert.Equal(t, filename, wavFile.Filename(), "Filename should match")
	assert.Equal(t, int64(0), wavFile.Size(), "Initial size should be 0")

	// 3. Check that the physical file was created with a placeholder header
	fileInfo, err := os.Stat(filename)
	require.NoError(t, err, "The WAV file should exist on disk")
	assert.Equal(t, int64(WavHeaderSize), fileInfo.Size(), "Initial file size should be the header size")

	// 4. Write some data
	dataToWrite := make([]byte, 1024)
	for i := range dataToWrite {
		dataToWrite[i] = byte(i % 256)
	}
	err = wavFile.Write(dataToWrite)
	require.NoError(t, err, "Write should not return an error")
	assert.Equal(t, int64(1024), wavFile.Size(), "Size should be updated after write")

	// 5. Close the file, which should update the header
	err = wavFile.Close()
	require.NoError(t, err, "Close should not return an error")

	// 6. Verify the final file content and header values
	finalData, err := os.ReadFile(filename)
	require.NoError(t, err, "Should be able to read the final WAV file")

	expectedTotalSize := int64(WavHeaderSize + len(dataToWrite))
	assert.Equal(t, expectedTotalSize, int64(len(finalData)), "Final file size should be header + data")

	// Check RIFF ChunkSize (Total file size - 8)
	expectedChunkSize := uint32(expectedTotalSize - 8)
	actualChunkSize := binary.LittleEndian.Uint32(finalData[4:8])
	assert.Equal(t, expectedChunkSize, actualChunkSize, "RIFF ChunkSize in header is incorrect")

	// Check data Subchunk2Size (just the data size)
	expectedDataSize := uint32(len(dataToWrite))
	actualDataSize := binary.LittleEndian.Uint32(finalData[40:44])
	assert.Equal(t, expectedDataSize, actualDataSize, "Data Subchunk2Size in header is incorrect")

	// Verify the data itself was written correctly
	assert.Equal(t, dataToWrite, finalData[WavHeaderSize:], "The written audio data does not match")
}

// TestNewWavFile_ErrorHandling checks if NewWavFile handles errors correctly,
// for example, when trying to create a file in a non-existent directory.
func TestNewWavFile_ErrorHandling(t *testing.T) {
	// Attempt to create a file in a path that cannot exist.
	invalidPath := filepath.Join(t.TempDir(), "nonexistent_dir", "test.wav")
	_, err := NewWavFile(invalidPath)
	require.Error(t, err, "NewWavFile should return an error for an invalid path")
}

// TestPlayRawPCM verifies that the GStreamer pipeline for playing audio can be
// constructed and run without immediate errors. This test requires a working
// GStreamer installation but not necessarily an active audio device.
func TestPlayRawPCM(t *testing.T) {
	gst.Init(nil)

	// Create a small, silent audio buffer (10ms of mono audio at TTS rate).
	data := make([]byte, TTSSampleRate/100*1*(16/8))

	err := PlayRawPCM(data, TTSSampleRate, TTSChannels)
	assert.NoError(t, err, "PlayRawPCM should execute without pipeline errors on a configured system")
}
