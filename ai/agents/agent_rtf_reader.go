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

// isParagraphOpen is a package-level variable to track paragraph state across
// the extendedHTMLRules and the finalizer function. It's reset for each conversion.
var (
	isParagraphOpen  bool
	bodyStyleApplied bool
)

// rtfIgnoreList creates a custom ignore list that allows processing of field results.
// By default, the library ignores `field`, `fldinst`, and `fldrslt`. We remove them
// so our custom rules can process them.
func rtfIgnoreList() []string {
	defaultList := rtf.IgnoreList()
	var newList []string
	for _, item := range defaultList {
		switch item {
		case "field", "fldinst", "fldrslt":
			// Skip these so they are not ignored by the parser.
			continue
		default:
			newList = append(newList, item)
		}
	}
	return newList
}

// extendedHTMLRules enhances the default HTML rules with additional formatting.
func extendedHTMLRules() rtf.RuleSet {
	// Start with the library's default HTML rules.
	// RTF format doc: https://www.biblioscape.com/rtf15_spec.htm

	// Reset paragraph state for this conversion run.
	isParagraphOpen = false
	bodyStyleApplied = false
	var leftIndent, rightIndent, paperWidth, marginLeft, marginRight int
	var textAlign string // Can be "left", "right", "center", "justify"

	// getStyle generates the CSS for paragraph indentation.
	getStyle := func() string {
		style := ""
		// RTF indents are in "twips". 1 point = 20 twips.
		if leftIndent > 0 {
			style += fmt.Sprintf("padding-left: %.2fpt;", float64(leftIndent)/20.0)
		}
		if rightIndent > 0 {
			style += fmt.Sprintf("padding-right: %.2fpt;", float64(rightIndent)/20.0)
		}
		if textAlign != "" {
			style += fmt.Sprintf("text-align: %s;", textAlign)
		}
		return style
	}

	// openParagraph closes any existing paragraph and opens a new one with the current style.
	openParagraph := func(stack rtf.StackType) {
		if isParagraphOpen {
			stack.Actions().AppendString("</p>\n")
		}

		if !bodyStyleApplied && paperWidth > 0 {
			// 1 point = 20 twips.
			contentWidthPt := float64(paperWidth-marginLeft-marginRight) / 20.0
			stack.Actions().AppendString(fmt.Sprintf(`<div style="width: %.2fpt; margin: auto;">`, contentWidthPt))
			bodyStyleApplied = true
		}

		style := getStyle()
		if style != "" {
			stack.Actions().AppendString(fmt.Sprintf(`<p style="%s">`, style))
		} else {
			stack.Actions().AppendString("<p>")
		}
		isParagraphOpen = true
	}

	// Start with a clean ruleset for full control over paragraph structure.
	rules := rtf.RuleSet{
		"line": rtf.As("<br>\n"),
		"b":    rtf.Toggle("<b>", "</b>"),
		"ul":   rtf.Toggle("<u>", "</u>"),
		"i":    rtf.Toggle("<i>", "</i>"),
	}

	// Add a rule for italics (\i and \i0).

	// Add a rule for strikethrough (\strike and \strike0).
	rules["strike"] = rtf.Toggle("<s>", "</s>")

	// Add rules for subscript and superscript.
	// This assumes they are turned off with \sub0 and \super0 or by exiting a group.
	rules["sub"] = rtf.Toggle("<sub>", "</sub>")
	rules["super"] = rtf.Toggle("<sup>", "</sup>")

	// Some RTF writers use \ulnone to disable underlining. The default Toggle for 'ul'
	// only handles \ul0. We can add an explicit rule for \ulnone.
	rules["ulnone"] = rtf.As("</u>")

	// Add a rule for small caps text (\scaps), which is toggled off by \scaps0 or \plain.
	rules["scaps"] = rtf.Toggle(`<span style="font-variant: small-caps;">`, "</span>")

	// Add a rule for hidden text (\v), which is toggled off by \v0 or \plain.
	rules["v"] = rtf.Toggle(`<span style="display:none;">`, "</span>")

	// Add a rule for the tab character. We use an em-space for a good visual representation in HTML.
	rules["tab"] = rtf.As("&emsp;")

	// Add a rule for the \plain tag, which resets formatting to default.
	// This rule will close any open toggle tags like bold, italics, etc.
	rules["plain"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		// HACK: Do not reset formatting if we are inside a \listtext group,
		// as it's likely being used for layout, not style reset.
		if stack.IsInGroup("listtext") {
			return nil
		}
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

	// Overwrite the default 'par' rule to handle paragraph closing.
	rules["par"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		if isParagraphOpen {
			stack.Actions().AppendString("</p>\n")
			isParagraphOpen = false
		}
		return nil
	}

	// Add rules for text alignment.
	rules["ql"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		textAlign = "left" // Set state and re-open the paragraph with the new style.
		openParagraph(stack)
		return nil
	}
	rules["qr"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		textAlign = "right" // Set state and re-open the paragraph with the new style.
		openParagraph(stack)
		return nil
	}
	rules["qc"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		textAlign = "center" // Set state and re-open the paragraph with the new style.
		openParagraph(stack)
		return nil
	}
	rules["qj"] = func(_ rtf.Header, stack rtf.StackType, _ rtf.Action) error {
		textAlign = "justify" // Set state and re-open the paragraph with the new style.
		openParagraph(stack)
		return nil
	}

	// Add a rule for \pard, which resets to default paragraph properties.
	// In many RTF documents, this implicitly resets all character formatting,
	// so we close all open toggles to prevent styles from leaking between paragraphs.
	rules["pard"] = func(_ rtf.Header, stack rtf.StackType, act rtf.Action) error {
		// HACK: Same as the \plain rule, ignore \pard inside \listtext.
		if stack.IsInGroup("listtext") {
			return nil
		}
		// Reset indents to default and start a new paragraph.
		leftIndent = 0
		rightIndent = 0
		textAlign = "" // Reset alignment to default (left)
		// paperWidth, marginLeft, and marginRight are document-level and should not be reset here.
		openParagraph(stack)
		stack.CloseAllStackToggles()
		return nil
	}

	return rules
}

// hyperlinkPostRules defines the post-processing rules for handling `\field` groups
// that contain hyperlinks. It uses a closure to maintain state between different
// parts of the field.
func hyperlinkPostRules() rtf.PostRuleSet {
	var url string
	var resultActions *rtf.Actions

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
			// This rule runs after the \fldrslt group is parsed.
			// We must capture a *copy* of the parsed actions (the link's display text)
			// because the original 'actions' will be modified and its pointer is shared.
			resultActions = actions.Clone()

			*actions = rtf.Actions{}
			return nil
		},
		"field": func(actions *rtf.Actions) error {
			// This rule runs after the entire \field group is parsed.
			// We replace all of its child actions with a single new action
			// that renders the complete <a> tag.
			*actions = *createHyperlinkAction(url, resultActions)
			// Reset state for the next potential link in the document.
			url, resultActions = "", nil
			return nil
		},
	}
}

type RtfReaderAgent struct {
	*Agent
}

// NewRtfReaderAgent creates a specialized agent for converting RTF documents to HTML.
func NewRtfReaderAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *RtfReaderAgent {
	systemInstruction := `You are an RTF document conversion specialist. You can convert RTF files into HTML format, which can then be analyzed or displayed.`

	functions := genai.FunctionDeclaration{
		Name:        "convertRtfToHtml",
		Description: "RTF Reader: Converts the content of an RTF file from the workspace into HTML format for analysis.",
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
				// Define a finalizer function to close the last paragraph tag if it's still open.
				finalizer := func(actions *rtf.Actions) {
					if isParagraphOpen {
						actions.AppendString("</p>\n")
					}
					if bodyStyleApplied {
						actions.AppendString("</div>\n")
					}
				}
				// Use our extended rules, custom ignore list, and new post-rules.
				html, convertErr := rtf.Convert(string(rtfBytes), extendedHTMLRules(), rtfIgnoreList(), hyperlinkPostRules(), finalizer)
				if convertErr != nil {
					err = fmt.Errorf("RTF to HTML conversion failed: %w", convertErr)
				} else {
					log.Printf("RTF conversion successful for file: '%s'", path)
					result = map[string]any{"html_content": html}
				}
			}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}

// createHyperlinkAction is a helper to construct the final action for an <a> tag.
func createHyperlinkAction(url string, result *rtf.Actions) *rtf.Actions {
	var newActions rtf.Actions
	if url != "" && result != nil {
		// If we have both a URL and display text, create the full <a> tag.
		newActions.Append(rtf.Action{
			Write: func(b *bytes.Buffer) {
				b.WriteString(`<a href="` + url + `">`)
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
