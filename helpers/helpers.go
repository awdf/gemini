package helpers

import "log"

// SafeSend attempts to send a value on a channel.
// It returns true if the send fails because the channel is closed, preventing a panic.
func SafeSend[T any](ch chan<- T, val T) (closed bool) {
	defer func() {
		if recover() != nil {
			// A panic during a channel send indicates that the channel is closed.
			closed = true
		}
	}()

	// This will panic if the channel is closed. The defer will recover from it.
	ch <- val

	return false // Send was successful
}

// Check is a generic helper function that panics if the error is not nil.
// It returns the value if the error is nil, which allows for chaining.
// This is intended for use during initialization where an error is considered
// a fatal, unrecoverable condition (e.g., failing to create a GStreamer element).
// Example: element := helpers.Check(gst.NewElement("my-element"))
func Check[T any](val T, err error) T {
	if err != nil {
		log.Panicf("unrecoverable error: %v", err)
	}
	return val
}

// Verify is a helper function that panics if the error is not nil.
// It's used for function calls that return only an error and where an
// error indicates a critical, unrecoverable failure.
// Example: helpers.Verify(element.SetProperty("prop", value))
func Verify(err error) {
	if err != nil {
		log.Panicf("unrecoverable error: %v", err)
	}
}
