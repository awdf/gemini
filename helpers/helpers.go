package helpers

// Control is a helper function to check errors during GStreamer element creation.
func Control[T any](object T, err error) T {
	if err != nil {
		panic(err)
	}
	return object
}

// Verify is a helper function to check errors during GStreamer linking.
func Verify(err error) {
	if err != nil {
		panic(err)
	}
}

// SafeSend attempts to send a value to a channel. It returns true if the send
// fails because the channel is closed. This prevents a panic.
func SafeSend[T any](ch chan<- T, value T) (closed bool) {
	defer func() {
		if recover() != nil {
			// The panic indicates that the channel is closed.
			closed = true
		}
	}()
	ch <- value  // This will panic if ch is closed.
	return false // Send was successful.
}
