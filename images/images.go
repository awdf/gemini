package images

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
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

// ScreenshotBuffer is a wrapper around bytes.Buffer that also holds a reference
// to the pool it came from. This improves encapsulation by making the buffer
// responsible for its own lifecycle management.
type ScreenshotBuffer struct {
	*bytes.Buffer
	pool *sync.Pool
}

// Release returns the buffer to the pool.
func (sb *ScreenshotBuffer) Release() {
	sb.Reset()
	sb.pool.Put(sb.Buffer)
}

// NewScreenshotBuffer creates a new ScreenshotBuffer from a byte slice,
// using a buffer from the pool.
func NewScreenshotBuffer(data []byte) *ScreenshotBuffer {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Write(data) // Write the initial data to the buffer.

	return &ScreenshotBuffer{
		Buffer: buf,
		pool:   &bufferPool,
	}
}

// DrawRectangle draws a rectangle with a specified thickness on the given image.
func DrawRectangle(img image.Image, rect image.Rectangle, thickness int, col color.Color) image.Image {
	// Create a new writable image of the same size and type.
	b := img.Bounds()
	newImg := image.NewRGBA(b)
	draw.Draw(newImg, b, img, image.Point{}, draw.Src)

	// Draw the rectangle by drawing 'thickness' number of rectangles, each one pixel smaller.
	for i := 0; i < thickness; i++ {
		// Create a rectangle for the current thickness layer.
		// Ensure the rectangle doesn't shrink to be invalid.
		r := image.Rect(rect.Min.X+i, rect.Min.Y+i, rect.Max.X-i, rect.Max.Y-i)
		if r.Empty() {
			break
		}

		// Draw horizontal lines
		for x := r.Min.X; x < r.Max.X; x++ {
			newImg.Set(x, r.Min.Y, col)
			newImg.Set(x, r.Max.Y-1, col)
		}
		// Draw vertical lines
		for y := r.Min.Y; y < r.Max.Y; y++ {
			newImg.Set(r.Min.X, y, col)
			newImg.Set(r.Max.X-1, y, col)
		}
	}

	return newImg
}

func DisplayBounds() (*image.Rectangle, error) {
	n := screenshot.NumActiveDisplays()
	if n <= 0 {
		return nil, errors.New("no active monitors found")
	}

	bounds := screenshot.GetDisplayBounds(0)
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return nil, fmt.Errorf("invalid display bounds: %+v", bounds)
	}

	return &bounds, nil
}

// TakeScreenshot captures a screenshot and returns it as a ScreenshotBuffer.
func TakeScreenshot() (*ScreenshotBuffer, error) {
	bounds, err := DisplayBounds()
	if err != nil {
		return nil, err
	}

	img, err := screenshot.CaptureRect(*bounds)
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
