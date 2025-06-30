package display

import "fmt"

// ANSI color codes for terminal output.
const (
	ColorReset  = "\033[0m"
	ColorYellow = "\033[33m" // For "Thought:" prefix
	ColorCyan   = "\033[36m" // For "Answer:" prefix
	ColorWhite  = "\033[97m" // For **bold** text
	ColorGreen  = "\033[32m" // For ```code``` blocks
)

// Formatter handles stateful, formatted printing to the terminal,
// including markdown-like syntax and ANSI colors.
type Formatter struct {
	inBold      bool
	inCodeBlock bool
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
				fmt.Print(ColorGreen)
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
				fmt.Print(ColorWhite)
			} else {
				fmt.Print(ColorReset)
			}
			i++ // Advance past the marker.
			continue
		}

		// Print the character.
		fmt.Print(string(runes[i]))
	}
}

// Println prints a formatted line with a specific color prefix.
func (f *Formatter) Println(prefix, color string) {
	fmt.Printf("\n%s%s %s\n", color, prefix, ColorReset)
}

// Reset prints a final newline and resets the terminal color.
func (f *Formatter) Reset() {
	fmt.Println(ColorReset)
}
