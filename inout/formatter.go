package inout

import (
	"fmt"
	"log"
	"strings"
)

// ANSI color codes for terminal output.
const (
	ColorReset       = "\033[0m"
	ColorDarkCyan    = "\033[36m"
	ColorWhite       = "\033[97m"
	ColorCyan        = "\033[96m"
	ColorDarkGreen   = "\033[32m"
	ColorDarkMagenta = "\033[35m"
	ColorDarkYellow  = "\033[33m"
	ColorDarkBlue    = "\033[34m"
	ColorDarkGray    = "\033[90m"
)

// Formatter handles stateful, formatted printing to the console.
type Formatter struct {
	inBold       bool
	inItalic     bool
	inCodeBlock  bool
	inInlineCode bool
}

// NewFormatter creates a new Formatter instance.
func NewFormatter() *Formatter {
	return &Formatter{}
}

// Print processes and prints text with simple markdown-like formatting.
func (f *Formatter) Print(text string, color ...string) {
	if len(color) > 0 {
		fmt.Print(color[0])
	}

	var output strings.Builder
	i := 0
	for i < len(text) {
		if f.inCodeBlock {
			if strings.HasPrefix(text[i:], "```") {
				f.inCodeBlock = false
				output.WriteString(ColorReset)
				i += 3
			} else {
				output.WriteByte(text[i])
				i++
			}
			continue
		}

		if strings.HasPrefix(text[i:], "```") {
			f.inCodeBlock = true
			output.WriteString(ColorDarkGreen)
			i += 3
		} else if strings.HasPrefix(text[i:], "**") {
			f.inBold = !f.inBold
			if f.inBold {
				output.WriteString(ColorDarkMagenta)
			} else {
				output.WriteString(ColorReset)
			}
			i += 2
		} else if strings.HasPrefix(text[i:], "*") {
			f.inItalic = !f.inItalic
			if f.inItalic {
				output.WriteString(ColorWhite)
			} else {
				output.WriteString(ColorReset)
			}
			i++
		} else if strings.HasPrefix(text[i:], "`") {
			f.inInlineCode = !f.inInlineCode
			if f.inInlineCode {
				output.WriteString(ColorCyan)
			} else {
				output.WriteString(ColorReset)
			}
			i++
		} else {
			output.WriteByte(text[i])
			i++
		}
	}
	fmt.Print(output.String())
}

// Println prints a line with an optional prefix color.
func (f *Formatter) Println(text string, color ...string) {
	if len(color) > 0 {
		fmt.Printf("%s%s%s\033[K\n", color[0], text, ColorReset)
	} else {
		fmt.Printf("%s\033[K\n", text)
	}
}

// Reset prints the ANSI reset code and a newline.
func (f *Formatter) Reset() {
	fmt.Print(ColorReset + "\n")
}

// Clear clears the terminal screen.
func (f *Formatter) Clear() {
	fmt.Print("\033[2J\033[0;0H")
}

// LogToolResult formats and logs the result of a tool call to the standard logger.
func LogToolResult(callName string, result any) {
	log.Printf("Tool call '%s' result:", callName)
	if resultMap, ok := result.(map[string]any); ok {
		for key, value := range resultMap {
			// Special handling for file list to make it more readable
			if key == "files" {
				if fileList, ok := value.([]map[string]any); ok {
					log.Printf("  %s: [%d files]", key, len(fileList))
					for _, fileInfo := range fileList {
						log.Printf("    - %v", fileInfo)
					}
					continue // Skip the generic print below
				}
			}
			// Generic print for other keys, with truncation for long values
			valueStr := fmt.Sprintf("%v", value)
			if len(valueStr) > 512 {
				valueStr = fmt.Sprintf("%.512s...", valueStr)
			}
			log.Printf("  %s: %s", key, valueStr)
		}
	} else {
		log.Printf("  Result (not a map): %v", result)
	}
}
