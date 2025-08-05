package agents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aiq/go-rtf"
	"google.golang.org/genai"

	"gemini/config"
)

func init() {
	RegisterFactory(AgentRtfReaderName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		return NewRtfReaderAgent(ctx, client, toolset)
	})
}

type RtfReaderAgent struct {
	*Agent
}

// NewRtfReaderAgent creates a specialized agent for converting RTF documents to HTML.
func NewRtfReaderAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *RtfReaderAgent {
	functions := genai.FunctionDeclaration{
		Name:        "readRtf",
		Description: "RTF Reader: Read an RTF file and return its contents as HTML. You MUST use this tool to read the content of any RTF file before you can analyze or summarize it.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the RTF file.",
				},
			},
			Required: []string{"path"},
		},
		Behavior: genai.BehaviorBlocking,
	}
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &functions)

	agentConfig := AgentConfig{
		Name: AgentRtfReaderName,
	}

	baseAgent := NewAgent(ctx, client, agentConfig)

	rtfAgent := &RtfReaderAgent{Agent: baseAgent}

	return rtfAgent
}

func (a *RtfReaderAgent) WarmUp() time.Duration {
	// This agent only performs local operations, no warm-up needed.
	return 0
}

func (a *RtfReaderAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "readRtf":
		return a.handleConvertRtfToHtmlTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *RtfReaderAgent) handleConvertRtfToHtmlTool(call *genai.FunctionCall) *genai.FunctionResponse {
	var result any
	var err error

	path, pathOK := call.Args["path"].(string)
	if !pathOK || path == "" {
		err = fmt.Errorf("'path' argument is required and must be a non-empty string")
	} else {
		safePath, pathErr := config.GetSafePath(path)
		if pathErr != nil {
			err = pathErr
		} else {
			rtfBytes, readErr := os.ReadFile(safePath)
			if readErr != nil {
				err = fmt.Errorf("failed to read RTF file '%s': %w", path, readErr)
			} else {
				// Get a fresh, stateful ruleset and its corresponding finalizer for this conversion.
				// This is critical to prevent state from leaking between different tool calls.
				rules, postRules, finalizer := extendedHTMLRules(a)

				// Use our extended rules, custom ignore list, and new post-rules.
				html, convertErr := rtf.Convert(string(rtfBytes), rules, rtfIgnoreList(), postRules, finalizer)
				if convertErr != nil {
					err = fmt.Errorf("RTF read failed: %w", convertErr)
				} else {
					// The new state-based styling should prevent empty/invalid tags.
					a.Printf("RTF read successful for file: '%s'", path)
					result = map[string]any{"html_content": html}
				}
			}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}

// rtfIgnoreList creates a custom ignore list that allows processing of field results.
// By default, the library ignores `field`, `fldinst`, and `fldrslt`. We remove them
// so our custom rules can process them.
func rtfIgnoreList() []string {
	defaultList := rtf.IgnoreList()
	var newList []string
	// wordsToKeep is a set of control words that are often on the default ignore
	// list but are crucial for our conversion. By removing them from the final
	// ignore list, we ensure the parser doesn't set the "ignorable" flag
	// when it encounters them, which would cause subsequent data (like image
	// hex data) to be skipped.
	wordsToKeep := map[string]bool{
		"field":    true,
		"fldinst":  true,
		"fldrslt":  true,
		"listtext": true,
		"pict":     true,
		"pngblip":  true,
		"jpegblip": true,
		"header":   true, // To handle header groups
		"footer":   true, // To handle footer groups
		// Common picture metadata words that must not be ignored.
		"picw":          true,
		"pich":          true,
		"picwgoal":      true,
		"pichgoal":      true,
		"picscalex":     true,
		"picscaley":     true,
		"picscaled":     true,
		"piccropt":      true,
		"piccropb":      true,
		"piccropl":      true,
		"piccropr":      true,
		"wmetafile":     true,
		"emfblip":       true,
		"macpict":       true,
		"pmmetafile":    true,
		"dibitmap":      true,
		"wbitmap":       true,
		"wbmbitspixel":  true,
		"wbmplanes":     true,
		"wbmwidthbytes": true,
		"blipuid":       true,
		"bliptag":       true,
		"bin":           true, // For binary data length, often precedes image data.
	}

	for _, item := range defaultList {
		if !wordsToKeep[item] {
			newList = append(newList, item)
		}
	}
	return newList
}

// borderProps holds the CSS-like properties for a border.
type borderProps struct {
	style      string
	width      int // in twips
	colorIndex int
	space      int // in twips
}

// reset clears the border properties to their default state.
func (b *borderProps) reset() {
	b.style = ""
	b.width, b.colorIndex, b.space = 0, 0, 0
}

// cellDef holds the full set of border properties for a single table cell.
type cellDef struct {
	borderTop, borderBottom, borderLeft, borderRight borderProps
	foregroundColorIndex                             int
	backgroundColorIndex                             int
}

// styleState holds all character-level formatting that can be scoped by RTF groups.
type styleState struct {
	isBold, isItalic, isUnderline bool
	fontSize                      int
	fontFamily                    string
	currentColorIndex             int
	// Caching the last applied style string at each stack level prevents redundant
	// <span> tags from being generated.
	lastAppliedStyle string
	isStyleSpanOpen  bool
}

// extendedHTMLRules creates a new, stateful ruleset and a finalizer for a single RTF conversion.
// It returns both so they can share the same state via a closure, ensuring that each
// conversion is independent and does not suffer from stale state.
// It now also returns a set of post-processing rules for handling complex fields like hyperlinks.
// Specification: https://www.biblioscape.com/rtf15_spec.htm
func extendedHTMLRules(a *RtfReaderAgent) (rtf.RuleSet, rtf.PostRuleSet, rtf.Finalizer) {
	// --- State variables for a single conversion run ---
	var (
		isParagraphOpen                                      bool
		bodyStyleApplied                                     bool
		bodyStyleCheckDone                                   bool
		isListActive, isItemActive                           bool
		isInTable, isInRow, isInCell                         bool
		isParagraphInTable                                   bool // Flag to mark a paragraph as part of a table.
		isInPicture                                          bool
		pictureWidthPixels                                   int
		pictureHeightPixels                                  int
		pictureWidthTwips                                    int
		pictureHeightTwips                                   int
		pictureScaleX                                        int = 100 // Default scale is 100%
		pictureScaleY                                        int = 100 // Default scale is 100%
		pictureType                                          string
		firstLineIndent                                      int
		pBorderTop, pBorderBottom, pBorderLeft, pBorderRight borderProps
		currentBorders                                       []*borderProps
		footerActions                                        *rtf.Actions
		// New state for robust table parsing
		rowCellDefs      []cellDef
		currentCellDef   *cellDef
		currentCellIndex int
		rowCellPositions []int
		tableLeftIndent  int

		// Character formatting is now managed by a stack to handle RTF's group scoping.
		styleStack []styleState
	)
	var leftIndent, rightIndent, paperWidth, marginLeft, marginRight int

	// Initialize the style stack with the default RTF state.
	initialState := styleState{fontSize: 24} // Default is 12pt (24 half-points)
	styleStack = []styleState{initialState}
	currentStyle := func() *styleState {
		return &styleStack[len(styleStack)-1]
	}
	// hexRegex is used to extract the hexadecimal image data from a pict block.
	hexRegex := regexp.MustCompile(`(?i)([0-9a-f][0-9a-f\s]*)$`)
	var colorTable []string // stores hex colors like "#RRGGBB"
	var currentR, currentG, currentB int
	var firstSemicolonInColorTbl bool
	var textAlign string // Can be "left", "right", "center", "justify"

	// getStyle generates the CSS for paragraph indentation.
	getStyle := func() string {
		var styles []string
		// RTF indents are in "twips". 1 point = 20 twips.
		if leftIndent > 0 {
			styles = append(styles, fmt.Sprintf("padding-left: %.2fpt", float64(leftIndent)/20.0))
		}
		if rightIndent > 0 {
			styles = append(styles, fmt.Sprintf("padding-right: %.2fpt", float64(rightIndent)/20.0))
		}
		if firstLineIndent != 0 {
			styles = append(styles, fmt.Sprintf("text-indent: %.2fpt", float64(firstLineIndent)/20.0))
		}
		if textAlign != "" {
			styles = append(styles, fmt.Sprintf("text-align: %s", textAlign))
		}

		// Add border styles
		addBorder := func(side string, props borderProps) {
			if props.style == "none" {
				styles = append(styles, fmt.Sprintf("border-%s: none", side))
				return
			}
			// Render a border if a style has been explicitly set. This is more robust
			// than checking for width, as some RTF writers imply a default width.
			if props.style != "" {
				width := props.width
				if width == 0 {
					width = 15 // Default to 0.75pt if no width is specified.
				}
				widthPt := float64(width) / 20.0
				bStyle := props.style
				bColor := "#000000" // Default
				if props.colorIndex > 0 && props.colorIndex < len(colorTable) {
					bColor = colorTable[props.colorIndex]
				}
				styles = append(styles, fmt.Sprintf("border-%s: %.2fpt %s %s", side, widthPt, bStyle, bColor))
				if props.space > 0 {
					styles = append(styles, fmt.Sprintf("padding-%s: %.2fpt", side, float64(props.space)/20.0))
				}
			}
		}
		addBorder("top", pBorderTop)
		addBorder("bottom", pBorderBottom)
		addBorder("left", pBorderLeft)
		addBorder("right", pBorderRight)
		return strings.Join(styles, "; ")
	}

	// getInlineStyle generates the CSS for inline elements like <span>.
	getInlineStyle := func() string {
		cs := currentStyle()
		var styles []string
		// RTF default font size is 12pt (24 half-points).
		if cs.fontSize != 24 {
			styles = append(styles, fmt.Sprintf("font-size:%.0fpt", float64(cs.fontSize)/2.0))
		}
		if cs.fontFamily != "" {
			styles = append(styles, fmt.Sprintf("font-family:'%s'", cs.fontFamily))
		}
		var finalColorIndex int
		// An explicit \cf inside a cell's content takes highest precedence.
		if cs.currentColorIndex > 0 {
			finalColorIndex = cs.currentColorIndex
		} else if isInCell && currentCellIndex < len(rowCellDefs) && rowCellDefs[currentCellIndex].foregroundColorIndex > 0 {
			// Otherwise, use the cell's default color if defined.
			finalColorIndex = rowCellDefs[currentCellIndex].foregroundColorIndex
		}
		if finalColorIndex >= 0 && finalColorIndex < len(colorTable) {
			styles = append(styles, fmt.Sprintf("color:%s", colorTable[finalColorIndex]))
		}
		if cs.isBold {
			styles = append(styles, "font-weight:bold")
		}
		if cs.isItalic {
			styles = append(styles, "font-style:italic")
		}
		if cs.isUnderline {
			styles = append(styles, "text-decoration:underline")
		}
		return strings.Join(styles, "; ")
	}

	// closeStyleSpan closes the generic style span if it's open.
	closeStyleSpan := func(stack rtf.StackType) {
		cs := currentStyle()
		if cs.isStyleSpanOpen {
			stack.Actions().AppendString("</span>")
			cs.isStyleSpanOpen = false
		}
	}

	// openStyleSpan closes any existing style span and opens a new one if needed.
	openStyleSpan := func(stack rtf.StackType) {
		cs := currentStyle()
		// Do not attempt to open a style span if we are not inside a paragraph
		// or a list item. This prevents invalid nesting like <span><p>...</p></span>
		// by ensuring block-level tags are opened before inline ones.
		if !isParagraphOpen && !isItemActive {
			return
		}
		currentStyleStr := getInlineStyle()
		// Only change the span if the style has actually changed.
		if currentStyleStr != cs.lastAppliedStyle {
			closeStyleSpan(stack) // Close the old span first.
			if currentStyleStr != "" {
				stack.Actions().AppendString(fmt.Sprintf(`<span style="%s">`, currentStyleStr))
				cs.isStyleSpanOpen = true
			}
			cs.lastAppliedStyle = currentStyleStr
		}
	}

	// openParagraph closes any existing paragraph and opens a new one with the current style.
	openParagraph := func(stack rtf.StackType) {
		style := getStyle()
		if style != "" {
			stack.Actions().AppendString(fmt.Sprintf(`<p style="%s;">`, style))
		} else {
			stack.Actions().AppendString("<p>")
		}
		isParagraphOpen = true
	}

	// ensureTableCell lazily creates table structure (table, row, cell) if needed.
	// It's called before writing any content that belongs inside a cell.
	ensureTableCell := func(stack rtf.StackType) {
		if !isParagraphInTable {
			return // Not in a table context, do nothing.
		}

		// This is the "lazy" part. Create table structure only when content is imminent.
		if !isInTable {
			var styles []string
			styles = append(styles, "border-collapse: collapse")
			// Use "table-layout: fixed" to ensure browsers respect the <col> widths.
			styles = append(styles, "table-layout: fixed")

			var colgroup string
			var tableWidthTwips int

			if len(rowCellPositions) > 0 {
				var colWidths []string
				// The first column's width is its right edge minus the table's left indent.
				lastPos := tableLeftIndent
				for _, pos := range rowCellPositions {
					width := pos - lastPos
					if width > 0 {
						widthPt := float64(width) / 20.0
						colWidths = append(colWidths, fmt.Sprintf(`<col style="width: %.2fpt;">`, widthPt))
					}
					lastPos = pos
				}
				// The total table width is the right edge of the last cell minus the table's left indent.
				tableWidthTwips = lastPos - tableLeftIndent

				if len(colWidths) > 0 {
					colgroup = fmt.Sprintf("<colgroup>%s</colgroup>", strings.Join(colWidths, ""))
				}
			}

			if tableWidthTwips > 0 {
				widthPt := float64(tableWidthTwips) / 20.0
				styles = append(styles, fmt.Sprintf("width: %.2fpt", widthPt))
			}

			styleAttr := ""
			if len(styles) > 0 {
				styleAttr = fmt.Sprintf(` style="%s"`, strings.Join(styles, "; "))
			}

			stack.Actions().AppendString(fmt.Sprintf("<table%s>\n%s<tbody>\n", styleAttr, colgroup))
			isInTable = true
		}
		if !isInRow {
			stack.Actions().AppendString("<tr>\n")
			isInRow = true
		}
		if !isInCell {
			// Get the border properties for the current cell from our parsed definitions.
			var props cellDef
			if currentCellIndex < len(rowCellDefs) {
				props = rowCellDefs[currentCellIndex]
			}

			var styles []string
			addBorder := func(side string, bProps borderProps) {
				if bProps.style == "none" {
					styles = append(styles, fmt.Sprintf("border-%s: none", side))
					return
				}
				if bProps.style != "" {
					width := bProps.width
					if width == 0 {
						width = 15 // Default to 0.75pt if no width is specified.
					}
					widthPt := float64(width) / 20.0
					bStyle := bProps.style
					bColor := "#000000" // Default
					if bProps.colorIndex > 0 && bProps.colorIndex < len(colorTable) {
						bColor = colorTable[bProps.colorIndex]
					}
					styles = append(styles, fmt.Sprintf("border-%s: %.2fpt %s %s", side, widthPt, bStyle, bColor))
					if bProps.space > 0 {
						styles = append(styles, fmt.Sprintf("padding-%s: %.2fpt", side, float64(bProps.space)/20.0))
					}
				}
			}
			addBorder("top", props.borderTop)
			addBorder("bottom", props.borderBottom)
			addBorder("left", props.borderLeft)
			addBorder("right", props.borderRight)

			// Add background color style.
			if props.backgroundColorIndex > 0 && props.backgroundColorIndex < len(colorTable) {
				bgColor := colorTable[props.backgroundColorIndex]
				// RTF often uses white for "no color", so we only apply non-white backgrounds
				// to avoid adding unnecessary `background-color: #ffffff`.
				if bgColor != "#ffffff" {
					styles = append(styles, fmt.Sprintf("background-color: %s", bgColor))
				}
			}

			styles = append(styles, "padding: 5px") // Add a default padding for all cells.

			stack.Actions().AppendString(fmt.Sprintf(`<td style="%s">`, strings.Join(styles, "; ")))
			isInCell = true
		}
	}

	// postRules defines the post-processing rules for handling `\field` groups
	// that contain hyperlinks. It's defined as a closure to get access to the state
	// variables of extendedHTMLRules, which is necessary for resetting style state.
	postRules := func() rtf.PostRuleSet {
		var url string

		postRulesMap := rtf.PostRuleSet{
			"trowd": func(actions *rtf.Actions) error {
				// After the table row definition group is fully parsed, we must
				// reset currentCellDef to nil. This prevents the \cf rule from
				// incorrectly modifying cell properties when it should be modifying
				// the global text color.
				currentCellDef = nil
				// The definition group itself produces no output.
				*actions = rtf.Actions{}
				return nil
			},
			"fldinst": func(actions *rtf.Actions) error {
				// This rule runs after the \fldinst group is parsed.
				// We execute its actions to get the raw text content (e.g., "HYPERLINK ...").
				var buf bytes.Buffer
				actions.Execute(&buf)
				text := strings.TrimSpace(buf.String())

				// Use regex to extract the URL from the instruction text.
				re := regexp.MustCompile(`(?i)HYPERLINK\s+"([^"]+)"`)
				if subs := re.FindStringSubmatch(text); len(subs) > 1 {
					url = subs[1]
				}
				// Clear the actions for this group so the raw instruction text isn't rendered.
				*actions = rtf.Actions{}
				return nil
			},
			"fldrslt": func(actions *rtf.Actions) error {
				// This rule runs after the \fldrslt group is parsed. We have the URL from
				// fldinst and the display text is in 'actions'. We replace the display
				// text actions with the full <a> tag.
				linkTextActions := actions.Clone()

				// Replace the group's actions with a new action that creates the hyperlink.
				*actions = *createHyperlinkAction(url, linkTextActions)

				url = "" // Reset url for the next link
				return nil
			},
			"field": func(actions *rtf.Actions) error {
				// The field group's actions now correctly contain the link and any following text.
				// No special processing is needed here, but the rule must exist to be in the PostRuleSet.
				return nil
			},
		}

		postRulesMap["pict"] = func(actions *rtf.Actions) error {
			// The picture itself is a block-level element. We'll wrap it in a paragraph.
			// The paragraph was already closed by the `\pict` rule.
			var finalTag string
			if pictureType != "" {
				// The actions can contain a mix of injected HTML (like a <td> tag from
				// separate them before decoding the hex.
				var contentBuf bytes.Buffer
				actions.Execute(&contentBuf)
				content := contentBuf.String()

				// The hex data is a long string of hex characters, possibly with whitespace,
				// at the end of the content buffer. We use a regex to reliably extract it,
				// even if the prefix (e.g., a <td> tag) contains hex-like characters.
				match := hexRegex.FindStringSubmatch(content)

				var prefix, hexContent string
				if len(match) > 1 {
					hexContent = match[1]
					// The prefix is everything before the hex content.
					prefix = content[:len(content)-len(hexContent)]
				} else {
					// No hex data found, just keep whatever was there.
					hexContent = ""
					prefix = content
				}

				cleanedHex := strings.Join(strings.Fields(hexContent), "")
				binData, err := hex.DecodeString(cleanedHex)
				if err != nil {
					a.Printf("RTF: failed to decode image hex data: %v", err)
					// Keep the prefix but show an error for the image part.
					finalTag = prefix + `<p>[Error: Could not decode image]</p>`
				} else {
					a.Printf("RTF: decoded %s image data, size: %d bytes", pictureType, len(binData))
					b64Data := base64.StdEncoding.EncodeToString(binData)

					// Start with the goal size in twips, if available. This is the unscaled size.
					widthTwips := pictureWidthTwips
					heightTwips := pictureHeightTwips

					// If goal size (\picwgoal) is not available, calculate it from the source pixel dimensions (\picw).
					if widthTwips == 0 && pictureWidthPixels > 0 {
						// Convert source pixels to twips (1px = 15 twips, a common assumption).
						widthTwips = pictureWidthPixels * 15
					}
					if heightTwips == 0 && pictureHeightPixels > 0 {
						heightTwips = pictureHeightPixels * 15
					}

					// Now, apply scaling to the determined base twips dimensions.
					// The scaling factor is a percentage.
					if widthTwips > 0 {
						widthTwips = (widthTwips * pictureScaleX) / 100
					}
					if heightTwips > 0 {
						heightTwips = (heightTwips * pictureScaleY) / 100
					}

					var styleAttr string
					if widthTwips > 0 && heightTwips > 0 {
						// Convert twips to points (20 twips = 1 point) for CSS.
						widthPt := float64(widthTwips) / 20.0
						heightPt := float64(heightTwips) / 20.0
						styleAttr = fmt.Sprintf(`style="width:%.2fpt; height:%.2fpt;"`, widthPt, heightPt)
					} else {
						// Fallback style if dimensions are not specified.
						styleAttr = `style="max-width:100%; height:auto;"`
					}
					imgTag := fmt.Sprintf(`<img src="data:%s;base64,%s" alt="embedded image" %s />`, pictureType, b64Data, styleAttr)
					// Wrap in a paragraph for block layout, applying paragraph styles.
					pStyle := getStyle()
					if pStyle != "" {
						finalTag = prefix + fmt.Sprintf(`<p style="%s">%s</p>`, pStyle, imgTag)
					} else {
						finalTag = prefix + fmt.Sprintf("<p>%s</p>", imgTag)
					}
				}
			} else {
				// Not a known picture type, render nothing or a placeholder.
				finalTag = ""
			}

			// Clear the original actions and add the img tag.
			*actions = rtf.Actions{}
			actions.AppendString(finalTag)

			// Reset picture state after processing is complete.
			isInPicture = false
			pictureType = ""
			pictureWidthTwips = 0
			pictureHeightTwips = 0
			pictureWidthPixels = 0
			pictureHeightPixels = 0
			pictureScaleX = 100
			pictureScaleY = 100
			return nil
		}

		postRulesMap["footer"] = func(actions *rtf.Actions) error {
			// The footer group has been parsed. Store its actions to be appended
			// at the very end of the document by the finalizer.
			footerActions = actions.Clone()

			// After parsing the footer, which is a self-contained unit, we must
			// reset any state that might have been modified during its parse,
			// to prevent it from "leaking" into the parsing of the main document body
			// that follows the footer group in the RTF stream.
			isInTable, isInRow, isInCell, isParagraphInTable = false, false, false, false
			styleStack = []styleState{initialState} // Reset character style stack.

			// Clear the original actions so the footer isn't rendered in place.
			*actions = rtf.Actions{}
			return nil
		}

		return postRulesMap
	}

	// Start with a clean ruleset for full control over paragraph structure.
	rules := rtf.RuleSet{
		"line": rtf.As("<br>\n"),
		// Add rules for group delimiters to manage the style stack.
		"{": func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
			// A new group is starting. Push a copy of the current style state onto the stack.
			// The RTF parser will handle its own group stack; we just mirror it for styles.
			styleStack = append(styleStack, *currentStyle())
			return nil
		},
		"}": func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
			// A group is ending. The style is about to revert to the parent's state.
			// First, close any span that was opened within the closing group.
			closeStyleSpan(stack)
			// Now, pop our style stack to revert to the parent's style state.
			if len(styleStack) > 1 {
				styleStack = styleStack[:len(styleStack)-1]
			}
			return nil
		},
	}

	rules["header"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// Ignore the content of headers.
		stack.SetIgnorable(true)
		return nil
	}
	rules["footer"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// This rule ensures the \footer group is not ignored, allowing the post-rule
		// to capture its content.
		// We must explicitly set ignorable to false in case it's inside a `\*` group.
		stack.SetIgnorable(false)

		// Reset all table-related state before parsing the footer content to prevent
		// styles from the main document body from leaking into the footer. This ensures
		// the footer is rendered with a clean slate.
		isInTable, isInRow, isInCell = false, false, false
		isParagraphInTable = false
		styleStack = []styleState{initialState} // Reset character style stack.
		return nil
	}

	rules["fldinst"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// The \fldinst group can be inside a `\*` destination, which would normally
		// make its contents ignorable. We must explicitly set ignorable to false
		// for this specific group so we can capture its text content (the hyperlink URL).
		stack.SetIgnorable(false)
		return nil
	}

	// The 'par' rule is defined outside the literal to break the initialization loop,
	// as it needs to refer to the 'rules' map itself to call other rules.
	rules["par"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// A paragraph break also resets any inline styles and closes open toggles.
		closeStyleSpan(stack)
		stack.CloseAllStackToggles()

		if isItemActive {
			// A paragraph break inside a list item closes the item.
			stack.Actions().AppendString("</li>\n")
			isItemActive = false
		} else if isParagraphOpen {
			stack.Actions().AppendString("</p>\n")
			isParagraphOpen = false
		} else if isInCell {
			// Only render blank paragraphs if they appear inside a table cell.
			// This prevents spurious <p>&nbsp;</p> tags from appearing between other elements.
			stack.Actions().AppendString("<p>&nbsp;</p>\n")
		}

		isParagraphInTable = false // A paragraph break resets this flag.
		// Reset paragraph-specific styles for the next paragraph.
		// This is a pragmatic choice to handle documents where styles
		// are not explicitly reset with \pard, which is common.
		leftIndent = 0
		rightIndent = 0
		textAlign = ""
		firstLineIndent = 0
		pBorderTop.reset()
		pBorderBottom.reset()
		pBorderLeft.reset()
		pBorderRight.reset()
		return nil
	}

	// Rules for bold, italic, and underline now just set state flags.
	// The actual styling is handled by the unified <span style="...">.
	rules["b"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.Ignorable() {
			return nil
		}
		currentStyle().isBold = (act.Para == nil || *act.Para != 0) // \b is on, \b0 is off
		openStyleSpan(stack)
		return nil
	}
	rules["i"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.Ignorable() {
			return nil
		}
		currentStyle().isItalic = (act.Para == nil || *act.Para != 0)
		openStyleSpan(stack)
		return nil
	}
	rules["ul"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.Ignorable() {
			return nil
		}
		currentStyle().isUnderline = (act.Para == nil || *act.Para != 0)
		openStyleSpan(stack)
		return nil
	}
	rules["strike"] = rtf.Toggle("<s>", "</s>")

	// Add rules for subscript and superscript.
	// This assumes they are turned off with \sub0 and \super0 or by exiting a group.
	rules["sub"] = rtf.Toggle("<sub>", "</sub>")
	rules["super"] = rtf.Toggle("<sup>", "</sup>")

	// Some RTF writers use \ulnone to disable underlining. The default Toggle for 'ul'
	// only handles \ul0. We can add an explicit rule for \ulnone.
	rules["ulnone"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		currentStyle().isUnderline = false
		openStyleSpan(stack)
		return nil
	}

	// Add a rule for small caps text (\scaps), which is toggled off by \scaps0 or \plain.
	rules["scaps"] = rtf.Toggle(`<span style="font-variant: small-caps;">`, "</span>")

	// Add a rule for hidden text (\v), which is toggled off by \v0 or \plain.
	rules["v"] = rtf.Toggle(`<span style="display:none;">`, "</span>")

	// Add a rule for the tab character. We use an em-space for a good visual representation in HTML.
	rules["tab"] = rtf.As("&emsp;")

	// Rule for list text, which indicates the start of a list item.
	rules["listtext"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		ensureTableCell(stack) // Ensure we are in a <td> if needed.

		if isParagraphOpen {
			stack.Actions().AppendString("</p>\n")
			isParagraphOpen = false
		}

		if !isListActive {
			stack.Actions().AppendString("<ul>\n")
			isListActive = true
		}

		if isItemActive {
			stack.Actions().AppendString("</li>\n")
		}

		// Open the new list item, applying paragraph styles directly to the <li> tag.
		// This prevents creating a nested <p> tag which would cause a line break.
		style := getStyle()
		if style != "" {
			stack.Actions().AppendString(fmt.Sprintf(`<li style="%s;">`, style))
		} else {
			stack.Actions().AppendString("<li>")
		}
		isItemActive = true

		// The content of \listtext (the bullet and tab) should be ignored for rendering.
		stack.SetIgnorable(true)
		return nil
	}

	// --- Table Processing Rules ---

	rules["intbl"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// This marks the upcoming paragraph as being part of a table.
		// The actual tags will be created lazily by the text hook.
		isParagraphInTable = true
		return nil
	}

	rules["cell"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// End of a table cell.
		if isInCell {
			// A cell tag implies the end of any inline styles within it.
			closeStyleSpan(stack)
			// A cell tag implies the end of the paragraph within it.
			if isParagraphOpen {
				stack.Actions().AppendString("</p>\n")
				isParagraphOpen = false
			}

			stack.Actions().AppendString("</td>\n")
			isInCell = false
			currentCellIndex++ // Increment to use the next cell's definition
		}
		return nil
	}

	rules["row"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		// A row end implicitly ends the last cell in that row.
		rules["cell"](rtf.Header{}, stack, act)
		// And it ends the row itself.
		if isInRow {
			stack.Actions().AppendString("</tr>\n")
			isInRow = false
		}
		currentCellIndex = 0 // Reset for the next row.
		return nil
	}

	// Add rule for first-line indent.
	rules["fi"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			firstLineIndent = *act.Para
		}
		return nil
	}

	rules["pict"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// When a \pict group starts, we need to make sure any open paragraph is closed,
		// as a picture is a block-level element in HTML.
		// If we are inside a header, we should ignore the picture
		// to prevent it from being rendered at the top of the document.
		if stack.IsInGroup("header") {
			stack.SetIgnorable(true)
			return nil
		}
		ensureTableCell(stack) // Ensure we are in a <td> if needed.

		// Crucially, we must ensure this group is not ignorable, even if it's inside
		// a `\*` destination, so we can capture the hex data. This is the same
		// logic used for the `fldinst` rule.
		stack.SetIgnorable(false)

		if isParagraphOpen {
			stack.Actions().AppendString("</p>\n")
			isParagraphOpen = false
		}
		if isItemActive {
			stack.Actions().AppendString("</li>\n")
			isItemActive = false
		}

		isInPicture = true
		pictureType = "" // Reset at the start of each picture
		pictureWidthTwips = 0
		pictureHeightTwips = 0
		pictureWidthPixels = 0
		pictureHeightPixels = 0
		pictureScaleX = 100
		pictureScaleY = 100
		// Don't set ignorable, we want to capture the hex data as text.
		return nil
	}

	rules["pngblip"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		if isInPicture {
			pictureType = "image/png"
		}
		return nil
	}
	rules["jpegblip"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		if isInPicture {
			pictureType = "image/jpeg"
		}
		return nil
	}

	// Rules to capture the source width and height of a picture in pixels.
	// This is used with scaling factors if goal dimensions are not specified.
	rules["picw"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if isInPicture && act.Para != nil {
			pictureWidthPixels = *act.Para
		}
		return nil
	}
	rules["pich"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if isInPicture && act.Para != nil {
			pictureHeightPixels = *act.Para
		}
		return nil
	}

	// Rules to capture the desired width and height of a picture.
	// \picwgoal and \pichgoal are specified in twips (1/20th of a point).
	rules["picwgoal"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if isInPicture && act.Para != nil {
			pictureWidthTwips = *act.Para
		}
		return nil
	}
	rules["pichgoal"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if isInPicture && act.Para != nil {
			pictureHeightTwips = *act.Para
		}
		return nil
	}

	// Add rules for picture scaling percentages.
	rules["picscalex"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if isInPicture && act.Para != nil {
			pictureScaleX = *act.Para
		}
		return nil
	}
	rules["picscaley"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if isInPicture && act.Para != nil {
			pictureScaleY = *act.Para
		}
		return nil
	}

	// __textHook__ is a special rule triggered just before any text is written.
	// This is our chance to ensure a block-level element (p or li) is open.
	rules["__textHook__"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// If we are inside a picture group, the "text" is actually hex data.
		// We must not inject any HTML tags here. The post-rule for 'pict' will handle it.
		if isInPicture {
			return nil
		}

		// On the first text element, check if we need to apply the overall page layout.
		// Do not apply this layout if we are inside a header or footer, as they are
		// handled separately at the end of the document.
		if !bodyStyleCheckDone && !stack.IsInGroup("header") && !stack.IsInGroup("footer") {
			bodyStyleCheckDone = true // Ensure this check only runs once for the main body.
			// Calculate the effective content width.
			// RTF units are in "twips". 1 point = 20 twips.
			contentWidth := float64(paperWidth-marginLeft-marginRight) / 20.0
			if contentWidth > 0 {
				stack.Actions().AppendString(fmt.Sprintf(`<div style="width: %.2fpt; margin: auto;">`, contentWidth))
				bodyStyleApplied = true
			}
		}

		if isParagraphInTable {
			ensureTableCell(stack)
		} else if isInTable {
			// We are about to write text that is NOT in a table. Close the active table.
			if isInCell {
				stack.Actions().AppendString("</td>\n")
				isInCell = false
			}
			if isInRow {
				stack.Actions().AppendString("</tr>\n")
				isInRow = false
			}
			stack.Actions().AppendString("</tbody>\n</table>\n")
			isInTable = false
		}

		// If we are about to write text that is NOT part of a list item,
		// but a list is currently active, it means the list has ended.
		// We close the list before opening the new paragraph.
		if !isItemActive && isListActive {
			stack.Actions().AppendString("</ul>\n")
			isListActive = false
		}

		// If we are about to write text and no block is open (paragraph or list item),
		// we must open a new paragraph.
		if !isParagraphOpen && !isItemActive {
			openParagraph(stack)
		}
		openStyleSpan(stack)
		return nil
	}

	rules["colortbl"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// Set this group to be ignorable for rendering, but our custom rules will still fire.
		stack.SetIgnorable(true)
		// The RTF color table is 1-indexed, but \cf0 refers to the 'auto' color.
		// We'll create a 0-indexed slice where colorTable[0] is the auto color.
		colorTable = []string{"#000000"} // Default "auto" color at index 0.
		firstSemicolonInColorTbl = true
		currentR, currentG, currentB = 0, 0, 0
		return nil
	}
	rules[";"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// If we are inside a color table, this is a delimiter.
		if stack.IsInGroup("colortbl") {
			if firstSemicolonInColorTbl {
				// The first semicolon just terminates the \colortbl keyword and should be ignored.
				firstSemicolonInColorTbl = false
			} else {
				// Subsequent semicolons terminate an explicit color definition.
				hexColor := fmt.Sprintf("#%02x%02x%02x", currentR, currentG, currentB)
				colorTable = append(colorTable, hexColor)
			}
			// Reset for the next color definition.
			currentR, currentG, currentB = 0, 0, 0
		} else if !stack.Ignorable() {
			// If not in a color table, treat it as a literal character.
			stack.Actions().AppendString(";")
		}
		return nil
	}

	rules["red"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") && act.Para != nil {
			currentR = *act.Para
		}
		return nil
	}
	rules["green"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") && act.Para != nil {
			currentG = *act.Para
		}
		return nil
	}
	rules["blue"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") && act.Para != nil {
			currentB = *act.Para
		}
		return nil
	}

	// Add rules for font size and font family.
	rules["fs"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") {
			return nil
		}
		if act.Para != nil {
			currentStyle().fontSize = *act.Para
			openStyleSpan(stack)
		}
		return nil
	}
	rules["f"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") {
			return nil
		}
		if act.Para != nil {
			// HACK: The library doesn't parse the font table, so we use a hardcoded map.
			fontMap := map[int]string{
				0:  "Times New Roman",
				8:  "Arial", // Common fallback for \f8
				10: "Arial", // Common fallback for \f10
			}
			currentStyle().fontFamily = fontMap[*act.Para] // Returns "" if not found, which is fine.
			openStyleSpan(stack)
		}
		return nil
	}

	// Add rule for setting the foreground color.
	rules["cf"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") {
			return nil
		}
		if act.Para != nil {
			// The \cf command always sets the current text color, regardless of context.
			currentStyle().currentColorIndex = *act.Para
			openStyleSpan(stack)
		}
		return nil
	}

	// Add a rule for the \plain tag, which resets formatting to default.
	// This rule will close any open toggle tags like bold, italics, etc.
	rules["plain"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// HACK: Do not reset formatting if we are inside a \listtext group, as it's
		// likely being used for the bullet point's formatting, not the main text.
		if stack.IsInGroup("listtext") {
			return nil
		}
		*currentStyle() = initialState // Reset current style state to default.
		stack.CloseAllStackToggles()
		openStyleSpan(stack)
		return nil
	}

	// Add rules for page dimensions and margins.
	rules["paperw"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			paperWidth = *act.Para
		}
		return nil
	}
	rules["margl"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			marginLeft = *act.Para
		}
		return nil
	}
	rules["margr"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			marginRight = *act.Para
		}
		return nil
	}

	// Add rules for left and right indents.
	rules["li"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			leftIndent = *act.Para
		}
		return nil
	}
	rules["ri"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			rightIndent = *act.Para
		}
		return nil
	}

	// Add rules for text alignment.
	rules["ql"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		textAlign = "left"
		return nil
	}
	rules["qr"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		textAlign = "right"
		return nil
	}
	rules["qc"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		textAlign = "center"
		return nil
	}
	rules["qj"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		textAlign = "justify"
		return nil
	}

	// --- Paragraph Border Rules ---
	rules["brdrt"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		currentBorders = []*borderProps{&pBorderTop}
		return nil
	}
	rules["brdrb"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		currentBorders = []*borderProps{&pBorderBottom}
		return nil
	}
	rules["brdrl"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		currentBorders = []*borderProps{&pBorderLeft}
		return nil
	}
	rules["brdrr"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		currentBorders = []*borderProps{&pBorderRight}
		return nil
	}
	rules["brdrbox"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		currentBorders = []*borderProps{&pBorderTop, &pBorderBottom, &pBorderLeft, &pBorderRight}
		return nil
	}

	rules["trowd"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		// Start of a table row's definition block.
		rowCellDefs = nil      // Clear definitions from any previous row.
		rowCellPositions = nil // Clear cell positions for the new row.
		tableLeftIndent = 0    // Reset table indent for the new row.
		currentCellDef = new(cellDef)
		return nil
	}

	rules["trleft"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			tableLeftIndent = *act.Para
		}
		return nil
	}

	rules["cellx"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		// A \cellx command marks the end of the current cell's properties definition
		// within a \trowd block. We save the definition we've built so far and
		// prepare for the next one.
		if currentCellDef != nil {
			rowCellDefs = append(rowCellDefs, *currentCellDef)
			currentCellDef = new(cellDef)
		}
		if act.Para != nil {
			rowCellPositions = append(rowCellPositions, *act.Para)
		}
		return nil
	}

	// --- Cell Border Rules ---
	rules["clbrdrt"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		if currentCellDef != nil {
			currentBorders = []*borderProps{&currentCellDef.borderTop}
		}
		return nil
	}
	rules["clbrdrb"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		if currentCellDef != nil {
			currentBorders = []*borderProps{&currentCellDef.borderBottom}
		}
		return nil
	}
	rules["clbrdrl"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		if currentCellDef != nil {
			currentBorders = []*borderProps{&currentCellDef.borderLeft}
		}
		return nil
	}
	rules["clbrdrr"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		if currentCellDef != nil {
			currentBorders = []*borderProps{&currentCellDef.borderRight}
		}
		return nil
	}

	rules["clcfpat"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if currentCellDef != nil && act.Para != nil {
			currentCellDef.foregroundColorIndex = *act.Para
		}
		return nil
	}

	rules["clcfpat"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if currentCellDef != nil && act.Para != nil {
			currentCellDef.foregroundColorIndex = *act.Para
		}
		return nil
	}

	rules["clcbpat"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if currentCellDef != nil && act.Para != nil {
			currentCellDef.backgroundColorIndex = *act.Para
		}
		return nil
	}

	// Border styles
	borderStyleMap := map[string]string{
		"brdrs":      "solid",
		"brdrth":     "solid", // thick
		"brdrsh":     "outset",
		"brdrdot":    "dotted",
		"brdrdash":   "dashed",
		"brdrhair":   "solid", // hairline
		"brdrdb":     "double",
		"brdrdashd":  "dashed",
		"brdrdashdd": "dashed",
		"brdrtriple": "double",
		"brdrnone":   "none",
	}
	for keyword, cssStyle := range borderStyleMap {
		// Use a closure to capture the cssStyle for each keyword
		func(style string) {
			rules[keyword] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
				for _, b := range currentBorders {
					if b != nil {
						b.style = style
					}
				}
				return nil
			}
		}(cssStyle)
	}

	// Border properties - these can apply to paragraphs or table cells.
	rules["brdrw"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			for _, b := range currentBorders {
				if b != nil {
					b.width = *act.Para
				}
			}
		}
		return nil
	}
	rules["brdrcf"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			for _, b := range currentBorders {
				if b != nil {
					b.colorIndex = *act.Para
				}
			}
		}
		return nil
	}
	rules["brsp"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			for _, b := range currentBorders {
				if b != nil {
					b.space = *act.Para
				}
			}
		}
		return nil
	}

	// --- Table Border Rules ---
	// The border style, width, and color rules are now combined with the paragraph border rules.

	// Add a rule for \pard, which resets to default paragraph properties.
	// In many RTF documents, this implicitly resets all character formatting,
	// so we close all open toggles to prevent styles from leaking between paragraphs.
	rules["pard"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// HACK: Same as the \plain rule, ignore \pard inside \listtext to avoid
		// resetting the style of the list item text that follows the bullet.
		if stack.IsInGroup("listtext") {
			return nil
		}

		// The \pard command resets all paragraph properties to their defaults for
		// the current scope. It does not end the current paragraph, but rather
		// affects the formatting of the paragraph currently being defined or the
		// one that immediately follows.
		leftIndent = 0
		rightIndent = 0
		isParagraphInTable = false // \pard resets all paragraph properties, including table state.
		textAlign = ""             // Reset alignment to default (left)
		firstLineIndent = 0
		pBorderTop.reset()
		pBorderBottom.reset()
		pBorderLeft.reset()
		pBorderRight.reset()
		currentBorders = nil

		// Reset character properties as well, since \pard implies this.
		*currentStyle() = initialState
		openStyleSpan(stack)
		return nil
	}

	// Define the finalizer function. It captures the state variables via closure.
	finalizer := func(actions *rtf.Actions) {
		// Close any open tags at the very end of the document to ensure valid HTML.
		cs := currentStyle()
		if cs.isStyleSpanOpen {
			actions.AppendString("</span>")
		}
		// The finalizer must close any remaining open tags in the correct order.
		if isParagraphOpen {
			actions.AppendString("</p>\n")
		}
		if isInCell {
			actions.AppendString("</td>\n")
		}
		if isInRow {
			actions.AppendString("</tr>\n")
		}
		if isInTable {
			actions.AppendString("</tbody>\n</table>\n")
		}
		if isItemActive {
			actions.AppendString("</li>\n")
		}
		if isListActive {
			actions.AppendString("</ul>\n")
		}
		if bodyStyleApplied {
			actions.AppendString("</div>\n")
		}
		// Now, append the footer content if it exists.
		if footerActions != nil {
			// The footer content is pre-rendered. We wrap it in a <footer> tag.
			// We also need to apply the same page-level centering as the main body.
			// Add a top margin to visually separate the footer from the main content.
			actions.AppendString(`<footer style="margin-top: 20pt;">` + "\n")

			// Calculate the effective content width for centering.
			contentWidth := float64(paperWidth-marginLeft-marginRight) / 20.0
			if contentWidth > 0 {
				actions.AppendString(fmt.Sprintf(`<div style="width: %.2fpt; margin: auto;">`, contentWidth))
			}

			actions.Append(footerActions.Action())

			if contentWidth > 0 {
				actions.AppendString("</div>\n")
			}

			actions.AppendString("</footer>\n")
		}
	}

	return rules, postRules(), finalizer
}

// createHyperlinkAction is a helper to construct the final action for an <a> tag.
func createHyperlinkAction(url string, result *rtf.Actions) *rtf.Actions {
	var newActions rtf.Actions
	if url != "" && result != nil {
		// If we have both a URL and display text, create the full <a> tag.
		newActions.Append(rtf.Action{
			Write: func(b *bytes.Buffer) {
				b.WriteString(`<a href="` + url + `">`)
				// The result actions will correctly handle their own spans.
				result.Execute(b)
				b.WriteString(`</a>`)
			},
		})
	} else if result != nil {
		// If we only have display text (e.g., not a hyperlink field), render it directly.
		newActions.Append(result.Action())
	}
	return &newActions
}
