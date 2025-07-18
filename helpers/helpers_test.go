package helpers

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheck_NoError(t *testing.T) {
	// Test case 1: string value
	expectedStr := "hello"
	resultStr := Check(expectedStr, nil)
	assert.Equal(t, expectedStr, resultStr, "Check should return the value when error is nil")

	// Test case 2: integer value
	expectedInt := 123
	resultInt := Check(expectedInt, nil)
	assert.Equal(t, expectedInt, resultInt, "Check should return the value when error is nil")

	// Test case 3: struct value
	type myStruct struct{ a int }
	expectedStruct := myStruct{a: 42}
	resultStruct := Check(expectedStruct, nil)
	assert.Equal(t, expectedStruct, resultStruct, "Check should return the value when error is nil")
}

func TestCheck_WithError(t *testing.T) {
	testErr := errors.New("this is a test error")

	// We expect a panic when an error is provided.
	assert.Panics(t, func() {
		Check("some value", testErr)
	}, "Check should panic when an error is provided")

	// Also test with a different type to ensure the generic works.
	assert.Panics(t, func() {
		Check(0, testErr)
	}, "Check should panic when an error is provided, regardless of value type")
}

func TestVerify_NoError(t *testing.T) {
	// Verify should not panic when the error is nil.
	assert.NotPanics(t, func() {
		Verify(nil)
	}, "Verify should not panic when error is nil")
}

func TestVerify_WithError(t *testing.T) {
	testErr := errors.New("this is another test error")

	// We expect a panic when an error is provided.
	assert.Panics(t, func() {
		Verify(testErr)
	}, "Verify should panic when an error is provided")
}

func TestSafeSend(t *testing.T) {
	t.Run("send on open channel", func(t *testing.T) {
		ch := make(chan string, 1)
		closed := SafeSend(ch, "hello")
		assert.False(t, closed, "SafeSend should return false for an open channel")

		val := <-ch
		assert.Equal(t, "hello", val, "The value should have been sent to the channel")
	})

	t.Run("send on closed channel", func(t *testing.T) {
		ch := make(chan int)
		close(ch)
		closed := SafeSend(ch, 123)
		assert.True(t, closed, "SafeSend should return true for a closed channel")
	})
}
