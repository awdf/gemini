package desktop

import (
	"fmt"
	"image"
	"os"
	"time"

	"gemini/images"
	"gemini/shell"
	"gemini/wayland"
)

// WaylandController implements the Controller interface for the Wayland display server.
type WaylandController struct {
	screenBounds image.Rectangle
	shellExec    *shell.Executor
}

// NewWaylandController initializes the Wayland C library and returns a controller.
func NewWaylandController(shellExec *shell.Executor) (*WaylandController, error) {
	wayland.DisableJoystick()
	if err := wayland.Init(); err != nil {
		return nil, fmt.Errorf("failed to initialize wayland controller: %w", err)
	}
	wayland.SetOffset(10)
	wayland.SetAccuracy(2)

	// This is much more efficient than grabbing a full frame every time ScreenSize() is called.
	rect, err := images.DisplayBounds()
	if err != nil {
		wayland.Done() // Clean up on failure
		return nil, fmt.Errorf("failed to get screen size on init: %w", err)
	}

	return &WaylandController{
		screenBounds: rect.Bounds(),
		shellExec:    shellExec,
	}, nil
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

// StartInteractiveShell starts a persistent shell session.
func (wc *WaylandController) StartInteractiveShell(outputChan chan<- string) error {
	userTerminal := os.Stdout
	return wc.shellExec.StartInteractive(outputChan, userTerminal)
}

// SendToShell sends input to the active shell session.
func (wc *WaylandController) SendToShell(input string) error {
	return wc.shellExec.SendInput(input)
}

// SendCommandToShell sends a command to the shell and returns a channel that
// is closed when the command finishes.
func (wc *WaylandController) SendCommandToShell(command string) (<-chan int, error) {
	return wc.shellExec.SendCommand(command)
}

// In your desktop controller implementation file (e.g., desktop/wayland.go)
func (wс *WaylandController) IsShellCommandRunning() bool {
	if wс.shellExec == nil {
		return false
	}
	return wс.shellExec.IsCommandRunning()
}

// StopInteractiveShell stops the active shell session.
func (wc *WaylandController) StopInteractiveShell() error {
	return wc.shellExec.StopInteractive()
}

// Close cleans up the Wayland connection.
func (wc *WaylandController) Close() {
	// Attempt to gracefully stop the interactive shell if it's running.
	// We can ignore the error as we are shutting down anyway.
	_ = wc.shellExec.StopInteractive()
	wayland.Done()
}
