package inout

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureOutput captures everything written to os.Stdout during the execution of a function.
func captureOutput(f func()) string {
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = originalStdout

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	if err != nil {
		panic(err)
	}
	return buf.String()
}

func TestNewFormatter(t *testing.T) {
	f := NewFormatter()
	require.NotNil(t, f)
	assert.False(t, f.inBold)
	assert.False(t, f.inCodeBlock)
	assert.False(t, f.inInlineCode)
	assert.False(t, f.inItalic)
}

func TestFormatter_Print(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain text",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "bold text",
			input:    "**bold**",
			expected: ColorDarkMagenta + "bold" + ColorReset,
		},
		{
			name:     "italic text",
			input:    "*italic*",
			expected: ColorWhite + "italic" + ColorReset,
		},
		{
			name:     "inline code",
			input:    "`code`",
			expected: ColorCyan + "code" + ColorReset,
		},
		{
			name:     "code block",
			input:    "```\ncode\n```",
			expected: ColorDarkGreen + "\ncode\n" + ColorReset,
		},
		{
			name:     "mixed formatting",
			input:    "normal **bold** *italic* `code` normal",
			expected: "normal " + ColorDarkMagenta + "bold" + ColorReset + " " + ColorWhite + "italic" + ColorReset + " " + ColorCyan + "code" + ColorReset + " normal",
		},
		{
			name:     "unterminated bold",
			input:    "**bold",
			expected: ColorDarkMagenta + "bold",
		},
		{
			name:     "code block with other markers inside",
			input:    "```**not bold**```",
			expected: ColorDarkGreen + "**not bold**" + ColorReset,
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFormatter()
			output := captureOutput(func() {
				f.Print(tt.input)
			})
			assert.Equal(t, tt.expected, output)
		})
	}
}

func TestFormatter_StatefulPrint(t *testing.T) {
	f := NewFormatter()

	// Start bold
	output := captureOutput(func() {
		f.Print("This is **bold")
	})
	assert.Equal(t, "This is "+ColorDarkMagenta+"bold", output)
	assert.True(t, f.inBold)

	// Continue bold and end it
	output = captureOutput(func() {
		f.Print(" and this is still bold**.")
	})
	assert.Equal(t, " and this is still bold"+ColorReset+".", output)
	assert.False(t, f.inBold)
}

func TestFormatter_Println(t *testing.T) {
	f := NewFormatter()
	output := captureOutput(func() {
		f.Println("Prefix:", ColorDarkCyan)
	})
	// \033[K is for clearing the rest of the line
	assert.Equal(t, ColorDarkCyan+"Prefix: "+ColorReset+"\033[K\n", output)
}

func TestFormatter_Reset(t *testing.T) {
	f := NewFormatter()
	output := captureOutput(func() {
		f.Reset()
	})
	assert.Equal(t, ColorReset+"\n", output)
}

func TestFormatter_Clear(t *testing.T) {
	f := NewFormatter()
	output := captureOutput(func() {
		f.Clear()
	})
	assert.Equal(t, "\033[2J\033[0;0H", output)
}
