package inout

import (
	"fmt"
	"time"
)

// DisplayWaiting shows a simple spinner animation in the terminal until the done channel is closed.
// It should be run in a goroutine.
// The `\r` character moves the cursor to the beginning of the line, allowing the next
// print to overwrite the previous one, creating the animation effect.
func DisplayWaiting(advice string, done <-chan struct{}) {
	// Using ANSI escape code to hide the cursor during the animation.
	fmt.Print("\033[?25l")
	// Defer function to show the cursor again when the function exits.
	// This ensures the cursor is restored even if the goroutine panics.
	defer fmt.Print("\033[?25h")

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	spinner := []rune{'|', '/', '-', '\\'}
	i := 0
	for {
		select {
		case <-done:
			// Clear the "Thinking..." message from the line.
			// `\r` moves to the start, `\033[K` clears the line to the end.
			fmt.Print("\r\033[K")
			return
		case <-ticker.C:
			// The space at the end helps clear any previous longer text if needed.
			fmt.Printf("\r%c %s ", spinner[i], advice)
			i = (i + 1) % len(spinner)
		}
	}
}
