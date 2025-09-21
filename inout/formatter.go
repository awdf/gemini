package inout

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

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
	ColorYellow      = "\033[93m" // For *italic* text
	ColorBlue        = "\033[94m" // Reserved
	ColorMagenta     = "\033[95m" // Reserved
	ColorCyan        = "\033[96m" // For `inline code` blocks
	ColorWhite       = "\033[97m" // For special emphasis
	// ANSI style code for strikethrough.
	StyleStrikethrough = "\033[9m"
)

// Formatter handles stateful, formatted printing to the console.
type Formatter struct {
	inBold          bool
	inItalic        bool
	inCodeBlock     bool
	inInlineCode    bool
	inStrikethrough bool
	atLineStart     bool
	codeBlockLang   string
	codeBlockBuffer strings.Builder
	// numberedListRegex is a compiled regular expression for detecting numbered lists.
	numberedListRegex *regexp.Regexp
}

// NewFormatter creates a new Formatter instance.
func NewFormatter() *Formatter {
	return &Formatter{
		atLineStart:       true,
		numberedListRegex: regexp.MustCompile(`^(\d+)\. `),
	}
}

// Print processes and prints text with simple markdown-like formatting.
// This version is more robust, handles more markdown features, and correctly
// manages nested styles by re-evaluating the active styles after every change.
func (f *Formatter) Print(text string, color ...string) {
	if len(color) > 0 {
		fmt.Print(color[0])
	}

	var output strings.Builder
	i := 0

	// applyCurrentStyle determines and writes the correct ANSI codes based on the current state.
	applyCurrentStyle := func(out *strings.Builder) {
		// Reset is important to not stack styles incorrectly.
		out.WriteString(ColorReset)
		if f.inCodeBlock {
			out.WriteString(ColorDarkGreen)
			return
		}
		if f.inInlineCode {
			out.WriteString(ColorCyan)
			return
		}

		// Apply styles. For simplicity in terminal rendering, bold color takes precedence over italic.
		if f.inBold {
			out.WriteString(ColorDarkMagenta)
		} else if f.inItalic {
			out.WriteString(ColorWhite)
		}

		// Strikethrough can be combined with colors.
		if f.inStrikethrough {
			out.WriteString(StyleStrikethrough)
		}
	}

	for i < len(text) {
		// If we are inside a code block, we buffer the content until we find the closing tag.
		if f.inCodeBlock {
			if strings.HasPrefix(text[i:], "```") {
				f.inCodeBlock = false
				// Once the block is closed, highlight the buffered content.
				if err := f.highlightCodeBlock(&output); err != nil {
					log.Printf("ERROR: code highlighting failed: %v. Falling back to plain text.", err)
					// Fallback to simple green color for the code block.
					output.WriteString(ColorReset)
					output.WriteString(ColorDarkGreen)
					output.WriteString(f.codeBlockBuffer.String())
				}
				f.codeBlockBuffer.Reset()
				f.codeBlockLang = ""

				applyCurrentStyle(&output)
				i += 3
				f.atLineStart = true
			} else {
				// Still inside code block, buffer the character.
				f.codeBlockBuffer.WriteByte(text[i])
				i++
			}
			continue
		}

		// Handle block-level formatting at the start of a line.
		if f.atLineStart {
			// Skip leading spaces, but keep track of them for indentation.
			leadingSpaces := 0
			for i+leadingSpaces < len(text) && text[i+leadingSpaces] == ' ' {
				leadingSpaces++
			}

			// If the rest of the line is empty or just a newline, print it and continue.
			if i+leadingSpaces >= len(text) || text[i+leadingSpaces] == '\n' {
				// No block marker found, proceed to normal character handling.
			} else {
				remainingText := text[i+leadingSpaces:]

				// Unordered list
				if strings.HasPrefix(remainingText, "* ") || strings.HasPrefix(remainingText, "- ") {
					output.WriteString(strings.Repeat(" ", leadingSpaces))
					output.WriteString(ColorDarkYellow + "• " + ColorReset)
					i += leadingSpaces + 2
					f.atLineStart = false
					continue // Restart loop to process text after the marker
				}
				// Blockquote
				if strings.HasPrefix(remainingText, "> ") {
					output.WriteString(strings.Repeat(" ", leadingSpaces))
					output.WriteString(ColorDarkGray + "| " + ColorReset)
					i += leadingSpaces + 2
					f.atLineStart = false
					continue
				}
				// Numbered list
				if match := f.numberedListRegex.FindStringSubmatch(remainingText); match != nil {
					output.WriteString(strings.Repeat(" ", leadingSpaces))
					output.WriteString(ColorDarkYellow + match[1] + ". " + ColorReset)
					i += leadingSpaces + len(match[0])
					f.atLineStart = false
					continue
				}
			}
		}

		// Check for ``` first, as it can contain a language hint and must be handled before other markers.
		if strings.HasPrefix(text[i:], "```") {
			f.inCodeBlock = true
			i += 3
			// Look for a language hint on the same line, terminated by a newline.
			lineContent := text[i:]
			endOfLine := strings.Index(lineContent, "\n")

			if endOfLine != -1 {
				// Found a newline. The part before it is the language.
				lang := strings.TrimSpace(lineContent[:endOfLine])
				// Simple validation: language hints shouldn't contain spaces or markdown.
				if !strings.ContainsAny(lang, " `*") {
					f.codeBlockLang = lang
					i += endOfLine + 1 // Consume language and newline.
				}
			}
			// Don't print anything, just start buffering.
			continue
		} else if strings.HasPrefix(text[i:], "**") {
			f.inBold = !f.inBold
			applyCurrentStyle(&output)
			i += 2
		} else if strings.HasPrefix(text[i:], "~~") {
			f.inStrikethrough = !f.inStrikethrough
			applyCurrentStyle(&output)
			i += 2
		} else if strings.HasPrefix(text[i:], "*") {
			f.inItalic = !f.inItalic
			applyCurrentStyle(&output)
			i++
		} else if strings.HasPrefix(text[i:], "`") {
			f.inInlineCode = !f.inInlineCode
			applyCurrentStyle(&output)
			i++
		} else {
			// Regular character.
			if text[i] == '\n' {
				f.atLineStart = true
			} else {
				f.atLineStart = false
			}
			output.WriteByte(text[i])
			i++
		}
	}
	// In raw terminal mode (like 'system' mode), a line feed (\n) alone only moves the
	// cursor down, not to the beginning of the line. We must send a carriage return
	// as well (\r\n) to get the correct behavior. This replaces all newlines.
	fmt.Print(strings.ReplaceAll(output.String(), "\n", "\r\n"))
}

// highlightCodeBlock formats the buffered code with syntax highlighting using the chroma library.
func (f *Formatter) highlightCodeBlock(out *strings.Builder) error {
	code := f.codeBlockBuffer.String()
	lang := f.codeBlockLang

	// If no language is specified, use the simple green color for a plain code block.
	if lang == "" {
		out.WriteString(ColorReset)
		out.WriteString(ColorDarkGreen)
		out.WriteString(code)
		return nil
	}

	// Get a lexer for the language. Fallback to a plain text lexer if not found.
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	// Get a style/theme. 'monokai' is a popular theme that looks good on dark backgrounds.
	style := styles.Get("monokai")
	if style == nil {
		style = styles.Fallback
	}

	// Get a formatter for 256-color terminal output.
	formatter := formatters.Get("terminal256")
	if formatter == nil {
		formatter = formatters.Fallback
	}

	// Get an iterator over the tokens.
	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return err
	}

	// Format the code and write the highlighted output to the buffer.
	return formatter.Format(out, style, iterator)
}

// Println prints a line with an optional prefix color.
func (f *Formatter) Println(text string, color ...string) {
	if len(color) > 0 {
		fmt.Printf("%s%s%s\033[K\r\n", color[0], text, ColorReset)
	} else {
		fmt.Printf("%s\033[K\r\n", text)
	}
}

// Printlnf prints a formatted line.
func (f *Formatter) Printlnf(format string, a ...any) {
	fmt.Printf(format+"\033[K\r\n", a...)
}

// PrintNl prints a newline, then a line with an optional prefix color.
func (f *Formatter) PrintNl(text string, color ...string) {
	if len(color) > 0 {
		fmt.Printf("\r\n%s%s%s\033[K\r\n", color[0], text, ColorReset)
	} else {
		fmt.Printf("\r\n%s\033[K\r\n", text)
	}
}

// Reset prints the ANSI reset code and a newline.
func (f *Formatter) Reset() {
	fmt.Print(ColorReset + "\r\n")
}

// Clear clears the terminal screen.
func (f *Formatter) Clear() {
	fmt.Print("\033[2J\033[0;0H")
}

// PrintRaw prints text directly to the console, ensuring newlines are correctly formatted as \r\n.
// It does not perform any markdown parsing or affect the formatter's state.
func (f *Formatter) PrintRaw(text string) {
	fmt.Print(strings.ReplaceAll(text, "\n", "\r\n"))
}

// LogToolResult formats and logs the result of a tool call to the standard logger.
func LogToolResult(callName string, result any) {
	log.Printf("Tool call '%s' result:", callName)
	if resultMap, ok := result.(map[string]any); ok {
		for key, value := range resultMap {
			// Special handling for file list to make it more readable and less verbose.
			if key == "files" {
				if fileList, ok := value.([]map[string]any); ok {
					log.Printf("  %s: [%d files]", key, len(fileList))
					for _, fileInfo := range fileList {
						log.Printf("    - %v", fileInfo)
					}
					continue
				}
				if fileList, ok := value.([]string); ok {
					log.Printf("  %s: [%d files]", key, len(fileList))
					for _, fileName := range fileList {
						log.Printf("    - %s", fileName)
					}
					continue
				}
			}
			// Other map keys with values
			log.Printf("  %s: %v", key, value)
		}
	} else {
		// If the result is not a map, pretty-print the whole thing as JSON.
		jsonData, err := json.MarshalIndent(result, "  ", "  ")
		if err != nil {
			log.Printf("  Result (could not marshal to JSON, falling back to default format): %v", result)
		} else {
			log.Printf("  Result:\n%s", string(jsonData))
		}
	}
}
