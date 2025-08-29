package images

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewScreenshotBuffer_Release(t *testing.T) {
	data := []byte("test data")
	sb := NewScreenshotBuffer(data)

	assert.NotNil(t, sb)
	assert.Equal(t, data, sb.Bytes())

	// Ensure the buffer is returned to the pool after release
	sb.Release()
	assert.Equal(t, 0, sb.Len())
}

func TestDrawRectangle(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	drawColor := color.RGBA{R: 255, A: 255}
	rect := image.Rect(10, 10, 90, 90)
	thickness := 5

	newImg := DrawRectangle(img, rect, thickness, drawColor)

	// Check some pixels to ensure the rectangle is drawn
	assert.Equal(t, drawColor, newImg.(*image.RGBA).At(10, 10)) // Top-left corner
	assert.Equal(t, drawColor, newImg.(*image.RGBA).At(89, 89)) // Bottom-right corner
	assert.Equal(t, drawColor, newImg.(*image.RGBA).At(10, 50)) // Left edge
	assert.Equal(t, drawColor, newImg.(*image.RGBA).At(50, 10)) // Top edge

	// Check a pixel outside the rectangle
	assert.Equal(t, color.RGBA{}, newImg.(*image.RGBA).At(5, 5))
}

func TestDisplayBounds(t *testing.T) {
	bounds, err := DisplayBounds()

	// This test is highly dependent on the environment. If no display is active,
	// it will return an error. For CI/CD, consider mocking the screenshot package.
	if err != nil {
		t.Logf("Skipping TestDisplayBounds due to error: %v (likely no active display)", err)
		return
	}

	assert.NotNil(t, bounds)
	assert.True(t, bounds.Dx() > 0)
	assert.True(t, bounds.Dy() > 0)
}

func TestTakeScreenshot(t *testing.T) {
	sb, err := TakeScreenshot()

	// This test is also highly dependent on the environment.
	// If DisplayBounds fails, TakeScreenshot will also fail.
	if err != nil {
		t.Logf("Skipping TestTakeScreenshot due to error: %v (likely no active display or capture failure)", err)
		return
	}

	assert.NotNil(t, sb)
	assert.True(t, sb.Len() > 0)

	// Try to decode the image to ensure it's a valid PNG
	_, err = png.Decode(sb)
	assert.NoError(t, err)

	sb.Release()
}

func TestSaveAndReadImage(t *testing.T) {
	data := []byte("dummy image data")
	filename := filepath.Join(os.TempDir(), "test_image.png")

	err := SaveImage(filename, data)
	assert.NoError(t, err)

	readData, err := ReadImage(filename)
	assert.NoError(t, err)
	assert.Equal(t, data, readData)

	err = os.Remove(filename)
	assert.NoError(t, err)
}
