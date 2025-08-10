package desktop

import (
	"fmt"
	"image"
	"time"

	"gemini/images"
	"gemini/wayland"
)

// WaylandController implements the Controller interface for the Wayland display server.
type WaylandController struct {
	screenBounds image.Rectangle
}

// NewWaylandController initializes the Wayland C library and returns a controller.
func NewWaylandController() (*WaylandController, error) {
	wayland.DisableJoystick()
	if err := wayland.Init(); err != nil {
		return nil, fmt.Errorf("failed to initialize wayland controller: %w", err)
	}
	wayland.SetOffset(10)
	wayland.SetAccuracy(2)

	// Grab the screen once to cache its dimensions. This is much more efficient
	// than grabbing a full frame every time ScreenSize() is called.
	img, err := images.DisplayBounds()
	if err != nil {
		wayland.Done() // Clean up on failure
		return nil, fmt.Errorf("failed to get screen size on init: %w", err)
	}

	return &WaylandController{screenBounds: img.Bounds()}, nil
}

// MoveMouse moves the mouse cursor to an absolute position.
func (wc *WaylandController) MoveMouse(x, y int) {
	wayland.MoveMouseToPosition(x, y)
}

// MouseClick performs a left mouse click.
func (wc *WaylandController) MouseClick(clicks int) {
	time.Sleep(100 * time.Millisecond) // Short delay before click
	wayland.MouseLeftClick(clicks)
}

// TypeText simulates typing a string.
func (wc *WaylandController) TypeText(text string) {
	wayland.Type(text)
}

// KeyAction simulates pressing and releasing keys.
func (wc *WaylandController) KeyAction(keyCodes []int, state int) {
	wayland.KeyAction(keyCodes, state)
}

// CaptureScreen grabs the current screen content and returns it as a PNG-encoded buffer.
func (wc *WaylandController) CaptureScreen() (*images.ScreenshotBuffer, error) {
	return images.TakeScreenshot()
}

// ScreenSize returns the cached dimensions of the screen.
func (wc *WaylandController) ScreenSize() (image.Rectangle, error) {
	return wc.screenBounds, nil
}

// Close cleans up the Wayland connection.
func (wc *WaylandController) Close() {
	wayland.Done()
}
