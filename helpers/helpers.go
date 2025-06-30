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
