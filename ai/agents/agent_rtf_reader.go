package agents

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aiq/go-rtf"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

func init() {
	RegisterFactory(AgentRtfReaderName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		return NewRtfReaderAgent(ctx, client, toolset)
	})
}

// rtfIgnoreList creates a custom ignore list that allows processing of field results.
// By default, the library ignores `field`, `fldinst`, and `fldrslt`. We remove them
// so our custom rules can process them.
func rtfIgnoreList() []string {
	defaultList := rtf.IgnoreList()
	var newList []string
	for _, item := range defaultList {
		switch item {
		case "field", "fldinst", "fldrslt", "listtext":
			// Skip these so they are not ignored by the parser.
			continue
		default:
			newList = append(newList, item)
		}
	}
	return newList
}

// extendedHTMLRules creates a new, stateful ruleset and a finalizer for a single RTF conversion.
// It returns both so they can share the same state via a closure, ensuring that each
// conversion is independent and does not suffer from stale state.
// It now also returns a set of post-processing rules for handling complex fields like hyperlinks.
// Specification: https://www.biblioscape.com/rtf15_spec.htm
func extendedHTMLRules() (rtf.RuleSet, rtf.PostRuleSet, rtf.Finalizer) {
	// --- State variables for a single conversion run ---
	var (
		isParagraphOpen               bool
		bodyStyleApplied              bool
		bodyStyleCheckDone            bool
		isStyleSpanOpen               bool
		isBold, isItalic, isUnderline bool
		isListActive, isItemActive    bool
		isInTable, isInRow, isInCell  bool
		isParagraphInTable            bool // Flag to mark a paragraph as part of a table.
		// Table border properties
		tableBorderWidth      int
		tableBorderStyle      string
		tableBorderColorIndex int
		firstLineIndent       int
	)
	var leftIndent, rightIndent, paperWidth, marginLeft, marginRight int
	fontSize := 24 // RTF default font size is 12pt (24 half-points). Initialize it here.
	var fontFamily string
	var lastAppliedStyle string // Track the last style applied to avoid redundant spans.
	var colorTable []string     // stores hex colors like "#RRGGBB"
	var currentR, currentG, currentB int
	var currentColorIndex int
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
		return strings.Join(styles, "; ")
	}

	// getInlineStyle generates the CSS for inline elements like <span>.
	getInlineStyle := func() string {
		var styles []string
		// RTF default font size is 12pt (24 half-points).
		if fontSize != 24 {
			styles = append(styles, fmt.Sprintf("font-size:%.0fpt", float64(fontSize)/2.0))
		}
		if fontFamily != "" {
			styles = append(styles, fmt.Sprintf("font-family:'%s'", fontFamily))
		}
		if currentColorIndex >= 0 && currentColorIndex < len(colorTable) {
			styles = append(styles, fmt.Sprintf("color:%s", colorTable[currentColorIndex]))
		}
		if isBold {
			styles = append(styles, "font-weight:bold")
		}
		if isItalic {
			styles = append(styles, "font-style:italic")
		}
		if isUnderline {
			styles = append(styles, "text-decoration:underline")
		}
		return strings.Join(styles, "; ")
	}

	// closeStyleSpan closes the generic style span if it's open.
	closeStyleSpan := func(stack rtf.StackType) {
		if isStyleSpanOpen {
			stack.Actions().AppendString("</span>")
			isStyleSpanOpen = false
		}
	}

	// openStyleSpan closes any existing style span and opens a new one if needed.
	openStyleSpan := func(stack rtf.StackType) {
		currentStyle := getInlineStyle()
		// Only change the span if the style has actually changed.
		if currentStyle != lastAppliedStyle {
			closeStyleSpan(stack) // Close the old span first.
			if currentStyle != "" {
				stack.Actions().AppendString(fmt.Sprintf(`<span style="%s">`, currentStyle))
				isStyleSpanOpen = true
			}
			lastAppliedStyle = currentStyle
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

	// postRules defines the post-processing rules for handling `\field` groups
	// that contain hyperlinks. It's defined as a closure to get access to the state
	// variables of extendedHTMLRules, which is necessary for resetting style state.
	postRules := func() rtf.PostRuleSet {
		var url string

		return rtf.PostRuleSet{
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
				// text actions with the full <a> tag, followed by a style reset.
				linkTextActions := actions.Clone()

				// The linkAction will now handle closing the span if it was open.
				linkAction := createHyperlinkAction(url, linkTextActions, isStyleSpanOpen)

				// Replace the group's actions with the complete link.
				*actions = *linkAction

				// Now, reset the state for whatever comes next in the parent (\field) group.
				isStyleSpanOpen = false // The span was closed inside the link.
				isUnderline = false
				currentColorIndex = 0
				lastAppliedStyle = "reset_by_fldrslt"

				url = "" // Reset url for the next link
				return nil
			},
			"field": func(actions *rtf.Actions) error {
				// The field group's actions now correctly contain the link and any following text.
				// No special processing is needed here, but the rule must exist to be in the PostRuleSet.
				return nil
			},
		}
	}

	// Start with a clean ruleset for full control over paragraph structure.
	rules := rtf.RuleSet{
		"line": rtf.As("<br>\n"),
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
		return nil
	}

	// Rules for bold, italic, and underline now just set state flags.
	// The actual styling is handled by the unified <span style="...">.
	rules["b"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.Ignorable() {
			return nil
		}
		isBold = (act.Para == nil || *act.Para != 0) // \b is on, \b0 is off
		closeStyleSpan(stack)                        // Close the current span; the text hook will open a new one if needed.
		return nil
	}
	rules["i"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.Ignorable() {
			return nil
		}
		isItalic = (act.Para == nil || *act.Para != 0)
		closeStyleSpan(stack)
		return nil
	}
	rules["ul"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.Ignorable() {
			return nil
		}
		isUnderline = (act.Para == nil || *act.Para != 0)
		closeStyleSpan(stack)
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
		isUnderline = false
		closeStyleSpan(stack)
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
			// A cell tag implies the end of the paragraph within it.
			if isParagraphOpen {
				stack.Actions().AppendString("</p>\n")
				isParagraphOpen = false
			}
			stack.Actions().AppendString("</td>\n")
			isInCell = false
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
		return nil
	}

	// Add rule for first-line indent.
	rules["fi"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			firstLineIndent = *act.Para
		}
		return nil
	}

	// __textHook__ is a special rule triggered just before any text is written.
	// This is our chance to ensure a block-level element (p or li) is open.
	rules["__textHook__"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// On the first text element, check if we need to apply the overall page layout.
		if !bodyStyleCheckDone {
			bodyStyleCheckDone = true // Ensure this check only runs once.
			// Calculate the effective content width.
			// RTF units are in "twips". 1 point = 20 twips.
			contentWidth := float64(paperWidth-marginLeft-marginRight) / 20.0
			if contentWidth > 0 {
				stack.Actions().AppendString(fmt.Sprintf(`<div style="width: %.2fpt; margin: auto;">`, contentWidth))
				bodyStyleApplied = true
			}
		}

		if isParagraphInTable {
			// This is the "lazy" part. Create table structure only when text is imminent.
			if !isInTable {
				style := ""
				if tableBorderWidth > 0 {
					// Use border-collapse for a cleaner look when cells have borders.
					style = ` style="border-collapse: collapse;"`
				}
				stack.Actions().AppendString(fmt.Sprintf("<table%s>\n<tbody>\n", style))
				isInTable = true
			}
			if !isInRow {
				stack.Actions().AppendString("<tr>\n")
				isInRow = true
			}
			if !isInCell {
				style := ""
				if tableBorderWidth > 0 {
					widthPt := float64(tableBorderWidth) / 20.0
					bStyle := "solid" // Default
					if tableBorderStyle != "" {
						bStyle = tableBorderStyle
					}
					bColor := "#000000" // Default
					if tableBorderColorIndex > 0 && tableBorderColorIndex < len(colorTable) {
						bColor = colorTable[tableBorderColorIndex]
					}
					// HACK: If the border color is white, it's likely for layout.
					// Override it to a visible color for better HTML rendering.
					if bColor == "#ffffff" {
						bColor = "#cccccc" // A light gray is less intrusive than black.
					}
					styleStr := fmt.Sprintf("border: %.2fpt %s %s; padding: 5px;", widthPt, bStyle, bColor)
					style = fmt.Sprintf(` style="%s"`, styleStr)
				}
				stack.Actions().AppendString(fmt.Sprintf("<td%s>", style))
				isInCell = true
			}
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
			// Reset table styles for the next table
			tableBorderWidth = 0
			tableBorderStyle = ""
			tableBorderColorIndex = 0
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
		if !isStyleSpanOpen {
			openStyleSpan(stack)
		}
		return nil
	}

	rules["colortbl"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// Set this group to be ignorable for rendering, but our custom rules will still fire.
		stack.SetIgnorable(true)
		// We re-initialize the color table here to handle multiple tables in a doc.
		colorTable = nil // Start with an empty table.
		currentR, currentG, currentB = 0, 0, 0
		return nil
	}
	rules[";"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// If we are inside a color table, this is a delimiter.
		if stack.IsInGroup("colortbl") {
			// A semicolon terminates a color definition. Add the color defined by the preceding \red, \green, \blue tags.
			hexColor := fmt.Sprintf("#%02x%02x%02x", currentR, currentG, currentB)
			colorTable = append(colorTable, hexColor)
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
			fontSize = *act.Para
			closeStyleSpan(stack)
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
			fontFamily = fontMap[*act.Para] // Returns "" if not found, which is fine.
			closeStyleSpan(stack)
		}
		return nil
	}

	// Add rule for setting the foreground color.
	rules["cf"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		if stack.IsInGroup("colortbl") {
			return nil
		}
		if act.Para != nil {
			currentColorIndex = *act.Para
			closeStyleSpan(stack)
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
		closeStyleSpan(stack)
		fontSize = 24
		fontFamily = ""
		lastAppliedStyle = ""
		currentColorIndex = 0
		isBold, isItalic, isUnderline = false, false, false
		stack.CloseAllStackToggles()
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

	// --- Table Border Rules ---
	rules["brdrs"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		tableBorderStyle = "solid"
		return nil
	}
	rules["brdrdot"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		tableBorderStyle = "dotted"
		return nil
	}
	rules["brdrdash"] = func(_ rtf.Header, _ rtf.StackType, _ rtf.Action) error {
		tableBorderStyle = "dashed"
		return nil
	}
	rules["brdrw"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			tableBorderWidth = *act.Para
		}
		return nil
	}
	rules["brdrcf"] = func(_ rtf.Header, _ rtf.StackType, act rtf.Action) error {
		if act.Para != nil {
			// This index is 1-based in RTF, but our colorTable is 0-based.
			// However, the first color in the table is often a dummy/auto color,
			// so using the index directly often works out. We'll assume it's correct.
			tableBorderColorIndex = *act.Para
		}
		return nil
	}

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

		// Reset character properties as well, since \pard implies this.
		fontSize = 24
		fontFamily = ""
		lastAppliedStyle = ""
		currentColorIndex = 0
		isBold, isItalic, isUnderline = false, false, false
		return nil
	}

	// Define the finalizer function. It captures the state variables via closure.
	finalizer := func(actions *rtf.Actions) {
		// Close any open tags at the very end of the document to ensure valid HTML.
		if isStyleSpanOpen {
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
	}

	return rules, postRules(), finalizer
}

type RtfReaderAgent struct {
	*Agent
}

// NewRtfReaderAgent creates a specialized agent for converting RTF documents to HTML.
func NewRtfReaderAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *RtfReaderAgent {
	systemInstruction := `You are an RTF document conversion specialist. You can convert RTF files into HTML format, which can then be analyzed or displayed.`

	functions := genai.FunctionDeclaration{
		Name:        "convertRtfToHtml",
		Description: "RTF Reader: Converts an RTF file into HTML. You MUST use this tool to read the content of any RTF file before you can analyze or summarize it.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the RTF file to convert.",
				},
			},
			Required: []string{"path"},
		},
		Behavior: genai.BehaviorBlocking,
	}
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &functions)

	agentConfig := AgentConfig{
		Name:              AgentRtfReaderName,
		Model:             config.C.AI.Model,
		RPM:               config.C.AI.ModelRPM,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.0)),
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
	case "convertRtfToHtml":
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
				rules, postRules, finalizer := extendedHTMLRules()

				// Use our extended rules, custom ignore list, and new post-rules.
				html, convertErr := rtf.Convert(string(rtfBytes), rules, rtfIgnoreList(), postRules, finalizer)
				if convertErr != nil {
					err = fmt.Errorf("RTF to HTML conversion failed: %w", convertErr)
				} else {
					// The new state-based styling should prevent empty/invalid tags.
					log.Printf("RTF conversion successful for file: '%s'", path)
					result = map[string]any{"html_content": html}
				}
			}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}

// createHyperlinkAction is a helper to construct the final action for an <a> tag.
func createHyperlinkAction(url string, result *rtf.Actions, spanWasOpen bool) *rtf.Actions {
	var newActions rtf.Actions
	if url != "" && result != nil {
		// If we have both a URL and display text, create the full <a> tag.
		newActions.Append(rtf.Action{
			Write: func(b *bytes.Buffer) {
				b.WriteString(`<a href="` + url + `">`)
				result.Execute(b)
				// If a style span was opened for the link text, we must close it here,
				// inside the <a> tag, to prevent the style from leaking.
				if spanWasOpen {
					b.WriteString(`</span>`)
				}
				b.WriteString(`</a>`)
			},
		})
	} else if result != nil {
		// If we only have display text (e.g., not a hyperlink field), render it directly.
		newActions.Append(result.Action())
	}
	return &newActions
}

func createStyleResetAction(
	isStyleSpanOpen, isUnderline *bool,
	currentColorIndex *int,
	lastAppliedStyle *string,
) rtf.Action {
	return rtf.Action{
		Write: func(b *bytes.Buffer) {
			if *isStyleSpanOpen {
				b.WriteString("</span>")
				*isStyleSpanOpen = false
			}
			*isUnderline = false
			*currentColorIndex = 0 // Reset to default color
			*lastAppliedStyle = "reset_by_hyperlink_field"
		},
	}
}
