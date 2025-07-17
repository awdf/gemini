package inout

import "fmt"

// ANSI color codes for terminal output.
// https://github.com/ChrisMaunder/How-to-Change-Text-Color-in-a-Linux-Terminal
const (
	ColorReset       = "\033[0m"
	ColorBlack       = "\033[30m" // Reserved
	ColorDarkRed     = "\033[31m" // For errors or warnings
	ColorDarkGreen   = "\033[32m" // For ```code``` blocks
	ColorDarkYellow  = "\033[33m" // For "Thought:" prefix
	ColorDarkBlue    = "\033[34m" // For links highlighting
	ColorDarkMagenta = "\033[35m" // For **bold** text
	ColorDarkCyan    = "\033[36m" // For "Answer:" prefix
	ColorDarkGray    = "\033[90m" // For subtle text like sources
	ColorRed         = "\033[91m" // Reserved
	ColorGreen       = "\033[92m" // Reserved
	ColorYellow      = "\033[93m" // Reserved
	ColorBlue        = "\033[94m" // Reserved
	ColorMagenta     = "\033[95m" // Reserved
	ColorCyan        = "\033[96m" // For `inline code` blocks
	ColorWhite       = "\033[97m" // For *italic* text
)

// Formatter handles stateful, formatted printing to the terminal,
// including markdown-like syntax and ANSI colors.
type Formatter struct {
	inBold       bool
	inCodeBlock  bool
	inInlineCode bool
	inItalic     bool
}

// NewFormatter creates a new terminal text formatter.
func NewFormatter() *Formatter {
	return &Formatter{}
}

// Print processes and prints text, interpreting formatting markers.
func (f *Formatter) Print(text string) {
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		// Check for code block marker first, as it takes precedence.
		if i+2 < len(runes) && runes[i] == '`' && runes[i+1] == '`' && runes[i+2] == '`' {
			f.inCodeBlock = !f.inCodeBlock
			if f.inCodeBlock {
				fmt.Print(ColorDarkGreen)
			} else {
				fmt.Print(ColorReset)
			}
			i += 2 // Advance past the marker.
			continue
		}

		// If we are in a code block, just print the character.
		if f.inCodeBlock {
			fmt.Print(string(runes[i]))
			continue
		}

		// Check for bold marker.
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			f.inBold = !f.inBold
			if f.inBold {
				fmt.Print(ColorDarkMagenta)
			} else {
				fmt.Print(ColorReset)
			}
			i++ // Advance past the marker.
			continue
		}

		// Check for italic marker.
		if runes[i] == '*' {
			f.inItalic = !f.inItalic
			if f.inItalic {
				fmt.Print(ColorWhite)
			} else {
				fmt.Print(ColorReset)
			}
			continue
		}

		// Check for inline code marker.
		if runes[i] == '`' {
			f.inInlineCode = !f.inInlineCode
			if f.inInlineCode {
				fmt.Print(ColorCyan)
			} else {
				fmt.Print(ColorReset)
			}
			continue
		}

		// Print the character.
		fmt.Print(string(runes[i]))
	}
}

// Println prints a formatted line with a specific color prefix.
func (f *Formatter) Println(prefix, color string) {
	fmt.Printf("%s%s %s\033[K\n", color, prefix, ColorReset)
}

// Reset prints a final newline and resets the terminal color.
func (f *Formatter) Reset() {
	fmt.Println(ColorReset)
}

func (f *Formatter) Clear() {
	fmt.Print("\033[2J\033[0;0H")
}
