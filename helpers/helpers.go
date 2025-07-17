package helpers

// Check is a helper function to check errors during GStreamer element creation.
func Check[T any](object T, err ...interface{}) T {
	if len(err) > 0 {
		if e, ok := err[0].(error); ok {
			if e != nil {
				panic(e)
			}
		} else if err[0] != nil {
			panic(err[0])
		}
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
