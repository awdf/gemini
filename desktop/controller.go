package desktop

import (
	"image"

	"gemini/images"
)

// Controller defines a standard interface for interacting with a desktop environment.
// This allows for platform-specific implementations (Wayland, X11, Windows, etc.).
type Controller interface {
	// Mouse functions
	MoveMouse(x, y int)
	MouseClick(clicks int)

	// Keyboard functions
	TypeText(text string)
	KeyAction(keyCodes []int, state int)

	// Screen functions
	CaptureScreen() (*images.ScreenshotBuffer, error)
	ScreenSize() (image.Rectangle, error)

	// Cleanup
	Close()
}

// C is a global instance of the desktop controller. It will be initialized in main.go.
var C Controller

// SetController sets the global desktop controller instance.
func SetController(ctrl Controller) {
	C = ctrl
}
