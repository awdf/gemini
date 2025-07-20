package images

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"os"
	"sync"

	"github.com/kbinani/screenshot"
)

// bufferPool is a pool of byte buffers to reduce allocations.
var bufferPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 1024*1024*10)) // 10 MB
	},
}

// ScreenshotBuffer is a wrapper around bytes.Buffer that also holds a reference to the pool.
type ScreenshotBuffer struct {
	*bytes.Buffer
	pool *sync.Pool
}

// Release returns the buffer to the pool.
func (sb *ScreenshotBuffer) Release() {
	sb.Reset()
	sb.pool.Put(sb.Buffer)
}

// TakeScreenshot captures a screenshot and returns it as a ScreenshotBuffer.
func TakeScreenshot() (*ScreenshotBuffer, error) {
	n := screenshot.NumActiveDisplays()
	if n <= 0 {
		return nil, errors.New("no active monitors found")
	}

	bounds := screenshot.GetDisplayBounds(0)
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return nil, fmt.Errorf("invalid display bounds: %+v", bounds)
	}

	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, fmt.Errorf("failed to capture screenshot: %w", err)
	}

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()

	err = png.Encode(buf, img)
	if err != nil {
		bufferPool.Put(buf) // Return the buffer on error
		return nil, fmt.Errorf("failed to encode PNG: %w", err)
	}

	return &ScreenshotBuffer{
		Buffer: buf,
		pool:   &bufferPool,
	}, nil
}

// SaveImage saves the image data to a file.
func SaveImage(name string, data []byte) error {
	if err := os.WriteFile(name, data, 0o644); err != nil {
		return fmt.Errorf("failed to save image to file %s: %w", name, err)
	}
	return nil
}

// ReadImage reads the image data from a file.
func ReadImage(name string) ([]byte, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("failed to read image from file %s: %w", name, err)
	}
	return data, nil
}
