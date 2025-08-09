package agents

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gemini/config"

	"github.com/fumiama/go-docx"
	"google.golang.org/genai"
)

// AgentDocxReaderName is the name of the docx reader agent.
const AgentDocxReaderName = "docxReaderAgent"

type Style struct {
	Type    string `xml:"type,attr"`
	StyleID string `xml:"styleId,attr"`
	Name    struct {
		Val string `xml:"val,attr"`
	} `xml:"name"`
	RPr struct {
		Color struct {
			Val string `xml:"val,attr"`
		} `xml:"color"`
		Sz struct {
			Val string `xml:"val,attr"`
		} `xml:"sz"`
		SzCs struct {
			Val string `xml:"val,attr"`
		} `xml:"szCs"`
		B struct {
			Val string `xml:"val,attr"`
		} `xml:"b"`
		I struct {
			Val string `xml:"val,attr"`
		} `xml:"i"`
		U struct {
			Val string `xml:"val,attr"`
		} `xml:"u"`
	} `xml:"rPr"`
	PPr struct {
		NumPr   *struct{} `xml:"numPr"`
		Spacing struct {
			Before string `xml:"before,attr"`
			After  string `xml:"after,attr"`
		} `xml:"spacing"`
		Jc struct {
			Val string `xml:"val,attr"`
		} `xml:"jc"`
		PBdr struct {
			Bottom struct {
				Val   string `xml:"val,attr"`
				Sz    string `xml:"sz,attr"`
				Space string `xml:"space,attr"`
				Color string `xml:"color,attr"`
			} `xml:"bottom"`
		} `xml:"pBdr"`
	}
}

type Styles struct {
	XMLName xml.Name `xml:"styles"`
	Styles  []Style  `xml:"style"`
}

func (a *DocxAgent) extractStyles(xmlContent string) string {
	var styles Styles
	err := xml.Unmarshal([]byte(xmlContent), &styles)
	if err != nil {
		log.Printf("Error unmarshalling styles XML: %v", err)
		return ""
	}

	var css strings.Builder
	for _, s := range styles.Styles {
		if s.StyleID == "" {
			continue
		}
		// Sanitize style ID for CSS class name
		className := regexp.MustCompile("[^a-zA-Z0-9-]").ReplaceAllString(s.StyleID, "")
		if className == "" {
			continue
		}

		// Replacer for docx style names. Replsaces with HTML tag names
		switch className {
		case "Heading", "Heading1", "Heading2", "Heading3", "Heading4", "Heading5", "Heading6":
			// Skip numbering properties for headings to avoid list-like behavior.
			if s.PPr.NumPr != nil {
				s.PPr.NumPr = nil
			}
			className = strings.Replace(className, "Heading", "h", 1)
			css.WriteString(fmt.Sprintf("%s {\n", className))
		default:
			css.WriteString(fmt.Sprintf(".%s {\n", className))
		}

		// Font size (w:sz is in half-points)
		if s.RPr.Sz.Val != "" {
			if sz, err := strconv.Atoi(s.RPr.Sz.Val); err == nil {
				css.WriteString(fmt.Sprintf("  font-size: %dpt;\n", sz/2))
			}
		}
		// Color
		if s.RPr.Color.Val != "" && s.RPr.Color.Val != "auto" {
			css.WriteString(fmt.Sprintf("  color: #%s;\n", s.RPr.Color.Val))
		}
		// Bold
		if s.RPr.B.Val != "" && s.RPr.B.Val != "0" {
			css.WriteString("  font-weight: bold;\n")
		}
		// Italic
		if s.RPr.I.Val != "" && s.RPr.I.Val != "0" {
			css.WriteString("  font-style: italic;\n")
		}
		// Underline
		if s.RPr.U.Val != "" && s.RPr.U.Val != "none" {
			css.WriteString("  text-decoration: underline;\n")
		}

		// Paragraph alignment
		if s.PPr.Jc.Val != "" {
			css.WriteString(fmt.Sprintf("  text-align: %s;\n", s.PPr.Jc.Val))
		}

		// Spacing (w:spacing is in twentieths of a point)
		if s.PPr.Spacing.Before != "" {
			if val, err := strconv.Atoi(s.PPr.Spacing.Before); err == nil {
				css.WriteString(fmt.Sprintf("  margin-top: %dpt;\n", val/20))
			}
		}
		if s.PPr.Spacing.After != "" {
			if val, err := strconv.Atoi(s.PPr.Spacing.After); err == nil {
				css.WriteString(fmt.Sprintf("  margin-bottom: %dpt;\n", val/20))
			}
		}

		// Border
		if s.PPr.PBdr.Bottom.Val != "" && s.PPr.PBdr.Bottom.Val != "none" {
			sz := "1"
			if s.PPr.PBdr.Bottom.Sz != "" {
				if val, err := strconv.Atoi(s.PPr.PBdr.Bottom.Sz); err == nil {
					sz = fmt.Sprintf("%d", val/8) // Borders are in eighths of a point
				}
			}
			color := "black"
			if s.PPr.PBdr.Bottom.Color != "" && s.PPr.PBdr.Bottom.Color != "auto" {
				color = "#" + s.PPr.PBdr.Bottom.Color
			}
			css.WriteString(fmt.Sprintf("  border-bottom: %spx solid %s;\n", sz, color))
			if s.PPr.PBdr.Bottom.Space != "" {
				if val, err := strconv.Atoi(s.PPr.PBdr.Bottom.Space); err == nil {
					css.WriteString(fmt.Sprintf("  padding-bottom: %dpt;\n", val)) // Space is in points
				}
			}
		}

		css.WriteString("}\n")
	}
	return css.String()
}

func init() {
	RegisterFactory(AgentDocxReaderName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		return NewDocxAgent(ctx, client, toolset)
	})
}

// DocxAgent handles reading .docx files.
type DocxAgent struct {
	*Agent
}

// NewDocxAgent creates a new DocxAgent and registers its function with the toolset.
func NewDocxAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *DocxAgent {
	readDocxFunc := &genai.FunctionDeclaration{
		Name:        "readDocx",
		Description: "DOCX Reader: Reads the content of a .docx file from the workspace and returns it as HTML. You MUST use this tool to read the content of any DOCX file before you can analyze or summarize it.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the .docx file within the workspace directory.",
				},
			},
			Required: []string{"path"},
		},
	}
	getXMLFunc := &genai.FunctionDeclaration{
		Name:        "getDocxXML",
		Description: "DOCX Debugger: Reads the raw 'word/document.xml' content from a .docx file. This is useful for debugging parsing issues by inspecting the underlying XML structure.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the .docx file within the workspace directory.",
				},
			},
			Required: []string{"path"},
		},
	}
	getStylesXMLFunc := &genai.FunctionDeclaration{
		Name:        "getDocxStylesXML",
		Description: "DOCX Debugger: Reads the raw 'word/styles.xml' content from a .docx file. This is useful for debugging styling issues.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the .docx file within the workspace directory.",
				},
			},
			Required: []string{"path"},
		},
	}

	// Add this function to the toolset provided by the caller.
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, readDocxFunc, getXMLFunc, getStylesXMLFunc)

	// Create the base agent.
	agentConfig := AgentConfig{
		Name: AgentDocxReaderName,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	docxAgent := &DocxAgent{Agent: baseAgent}

	return docxAgent
}

// WarmUp for DocxAgent does nothing as it only executes local tools.
func (a *DocxAgent) WarmUp() time.Duration {
	return 0
}

// Handle processes a function call for the docx agent.
func (a *DocxAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "readDocx":
		return a.handleReadDocx(call)
	case "getDocxXML":
		return a.handleGetDocxXML(call)
	case "getDocxStylesXML":
		return a.handleGetDocxStylesXML(call)
	default:
		return nil // Not for this agent
	}
}

func (a *DocxAgent) handleReadDocx(call *genai.FunctionCall) *genai.FunctionResponse {
	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' argument is required and must be a non-empty string"))
	}

	// The conversion function now returns the body content as a fragment.
	htmlBody, cssStyles, err := a.convertDocxToHTML(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	// We wrap the fragment in a full HTML document here, making the conversion
	// function more reusable.
	var fullHTML strings.Builder
	fullHTML.WriteString(`<!DOCTYPE html><html><head><meta charset="UTF-8"><style>
body{font-family: sans-serif; line-height: 1.4; }
table{border-collapse: collapse; width: 100%; margin-bottom: 1em; border-spacing: 0;}
td,th{padding: 8px; text-align: left; border: none;}
tr:nth-child(even){background-color: #f2f2f2; }`)
	fullHTML.WriteString(cssStyles)
	fullHTML.WriteString("</style></head><body>")
	fullHTML.WriteString(htmlBody)
	fullHTML.WriteString("</body></html>")

	return a.CreateFunctionResponse(call, map[string]any{"html_content": fullHTML.String()}, nil)
}

func (a *DocxAgent) handleGetDocxXML(call *genai.FunctionCall) *genai.FunctionResponse {
	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' argument is required and must be a non-empty string"))
	}

	xmlContent, err := a.getDocxMainXML(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	return a.CreateFunctionResponse(call, map[string]any{"xml_content": xmlContent}, nil)
}

func (a *DocxAgent) handleGetDocxStylesXML(call *genai.FunctionCall) *genai.FunctionResponse {
	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' argument is required and must be a non-empty string"))
	}

	xmlContent, err := a.getDocxStylesXML(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	return a.CreateFunctionResponse(call, map[string]any{"xml_content": xmlContent}, nil)
}

// getDocxMainXML extracts the raw word/document.xml content from a .docx file.
func (a *DocxAgent) getDocxMainXML(path string) (string, error) {
	return a.extractXMLFileFromDocx(path, "word/document.xml")
}

func (a *DocxAgent) getDocxStylesXML(path string) (string, error) {
	return a.extractXMLFileFromDocx(path, "word/styles.xml")
}

func (a *DocxAgent) extractXMLFileFromDocx(path, xmlFileToExtract string) (string, error) {
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return "", err
	}

	a.Printf("Extracting '%s' from .docx file: %s", xmlFileToExtract, safePath)

	// A .docx file is a zip archive.
	r, err := zip.OpenReader(safePath)
	if err != nil {
		return "", fmt.Errorf("failed to open docx as zip archive: %w", err)
	}
	defer r.Close()

	// Find the main document XML file.
	var docFile *zip.File
	for _, f := range r.File {
		if f.Name == xmlFileToExtract {
			docFile = f
			break
		}
	}

	if docFile == nil {
		return "", fmt.Errorf("could not find '%s' in the docx file", xmlFileToExtract)
	}

	// Open and read the XML file.
	rc, err := docFile.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open '%s': %w", xmlFileToExtract, err)
	}
	defer rc.Close()

	xmlBytes, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("failed to read '%s': %w", xmlFileToExtract, err)
	}

	return string(xmlBytes), nil
}

// borderToCSS converts a WTableBorder to a CSS border string.
func (a *DocxAgent) borderToCSS(b *docx.WTableBorder) string {
	if b == nil || b.Val == "nil" || b.Val == "none" {
		return "none"
	}
	// Default values
	style := "solid"
	// Size is in eighths of a point. Default to 1px if not specified or zero.
	width := "1px"
	color := "#000000"

	// Map docx border styles to CSS border styles
	switch b.Val {
	case "single":
		style = "solid"
	case "double":
		style = "double"
	case "dotted":
		style = "dotted"
	case "dashed":
		style = "dashed"
	default:
		if b.Val != "" {
			style = b.Val
		}
	}

	if b.Size > 0 {
		width = fmt.Sprintf("%.2fpt", float64(b.Size)/8.0)
	}
	if b.Color != "" && b.Color != "auto" {
		color = "#" + b.Color
	}
	return fmt.Sprintf("%s %s %s", width, style, color)
}

// convertDocxToHTML reads a .docx file and converts its content to an HTML string.
// It handles paragraphs, text formatting (bold, italic, etc.), hyperlinks, tables, and images.
// DOCX Format Specification (ECMA-376): https://www.ecma-international.org/publications-and-standards/standards/ecma-376/
func (a *DocxAgent) convertDocxToHTML(path string) (htmlBody string, css string, err error) {
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return "", "", err
	}

	a.Printf("Reading .docx file: %s", safePath)

	readFile, err := os.Open(safePath)
	if err != nil {
		return "", "", fmt.Errorf("failed to open docx file at %s: %w", safePath, err)
	}
	defer readFile.Close()

	fileinfo, err := readFile.Stat()
	if err != nil {
		return "", "", fmt.Errorf("failed to get file info for %s: %w", safePath, err)
	}
	size := fileinfo.Size()

	doc, err := docx.Parse(readFile, size)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse docx file at %s: %w", safePath, err)
	}

	// Generate all CSS classes from the styles defined in the document.
	stylesXML, err := a.getDocxStylesXML(path)
	if err != nil {
		a.Printf("Warning: could not extract styles.xml from %s: %v. Proceeding without custom styles.", path, err)
	} else {
		css = a.extractStyles(stylesXML)
	}

	var textBuilder strings.Builder

	var bodyWrapperOpen, bodyWrapperClose string

	// --- Start of Page Layout Logic ---
	// Find the SectPr in the body items to apply page layout styles.
	var sectPr *docx.SectPr
	for _, item := range doc.Document.Body.Items {
		if sp, ok := item.(*docx.SectPr); ok {
			sectPr = sp
			break // Assuming one SectPr at the end of the body.
		}
	}

	if sectPr != nil {
		if sectPr.PgSz != nil && sectPr.PgMar != nil {
			pageWidth := sectPr.PgSz.W
			leftMargin := sectPr.PgMar.Left
			rightMargin := sectPr.PgMar.Right

			// Calculate content width in points (1 point = 20 twips)
			contentWidthTwips := pageWidth - leftMargin - rightMargin
			if contentWidthTwips > 0 {
				contentWidthPt := float64(contentWidthTwips) / 20.0
				bodyWrapperOpen = fmt.Sprintf(`<div style="width: %.2fpt; margin: 0 auto;">`, contentWidthPt)
				bodyWrapperClose = "</div>"
			}
		}
	}

	// --- End of Page Layout Logic ---

	textBuilder.WriteString(bodyWrapperOpen)

	var isListActive bool
	for _, it := range doc.Document.Body.Items {
		// SectPr is for page layout and is handled above; skip it for content rendering.
		if _, ok := it.(*docx.SectPr); ok {
			continue
		}

		// Check if the current item is a paragraph that's part of a list.
		isListItem := false
		if p, ok := it.(*docx.Paragraph); ok && p.Properties != nil {
			// A paragraph is a list item if it has numbering properties AND it is not a heading.
			// Headings can sometimes have numbering properties as a formatting artifact.
			isHeading := false
			if p.Properties.Style != nil && strings.HasPrefix(p.Properties.Style.Val, "Heading") {
				isHeading = true
			}
			if !isHeading && p.Properties.NumProperties != nil {
				isListItem = true
			}
		}

		// Manage the opening and closing of the <ul> tag.
		if isListItem {
			if !isListActive {
				textBuilder.WriteString("<ul>\n")
				isListActive = true
			}
		} else {
			if isListActive {
				textBuilder.WriteString("</ul>\n")
				isListActive = false
			}
		}
		// Top-level items have no inherited run properties.
		a.writeHTMLNode(&textBuilder, doc, it, nil)
	}
	if isListActive {
		textBuilder.WriteString("</ul>\n")
	}

	textBuilder.WriteString(bodyWrapperClose)

	return textBuilder.String(), css, nil
}

// escapeCSSClassName cleans a string to be used as a CSS class name.
func escapeCSSClassName(name string) string {
	// A simple implementation: replace invalid characters.
	// A more robust one would handle more edge cases.
	return strings.NewReplacer(" ", "_", "(", "", ")", "", ":", "").Replace(name)
}

// convertRPrToCSS converts docx.RunProperties to a slice of CSS style strings.
func (a *DocxAgent) convertRPrToCSS(p *docx.RunProperties) []string {
	var styles []string
	if p == nil {
		return styles
	}
	if p.Bold != nil && p.Bold.Val != "0" && p.Bold.Val != "false" {
		styles = append(styles, "font-weight: bold")
	} else if p.Bold != nil {
		styles = append(styles, "font-weight: normal")
	}
	if p.Italic != nil && p.Italic.Val != "0" && p.Italic.Val != "false" {
		styles = append(styles, "font-style: italic")
	} else if p.Italic != nil {
		styles = append(styles, "font-style: normal")
	}
	if p.Underline != nil && p.Underline.Val != "none" && p.Underline.Val != "false" {
		styles = append(styles, "text-decoration: underline")
	}
	if p.Strike != nil && p.Strike.Val != "0" && p.Strike.Val != "false" {
		styles = append(styles, "text-decoration: line-through")
	}
	if p.Color != nil {
		styles = append(styles, "color: #"+p.Color.Val)
	}
	if p.Fonts != nil && p.Fonts.ASCII != "" {
		styles = append(styles, fmt.Sprintf("font-family: '%s'", p.Fonts.ASCII))
	}
	if p.Size != nil {
		if sizeVal, err := strconv.Atoi(p.Size.Val); err == nil {
			styles = append(styles, fmt.Sprintf("font-size: %.1fpt", float64(sizeVal)/2.0))
		}
	}
	return styles
}

// convertPPrToCSS converts docx.ParagraphProperties to a slice of CSS style strings.
func (a *DocxAgent) convertPPrToCSS(p *docx.ParagraphProperties) []string {
	var styles []string
	if p == nil {
		return styles
	}

	// Alignment
	if p.Justification != nil {
		textAlign := p.Justification.Val
		if textAlign == "both" {
			textAlign = "justify"
		}
		styles = append(styles, "text-align: "+textAlign)
	}

	// Indentation (in twips, 20 twips = 1 point)
	if p.Ind != nil {
		if p.Ind.Left != 0 {
			styles = append(styles, fmt.Sprintf("padding-left: %.1fpt", float64(p.Ind.Left)/20.0))
		}
		if p.Ind.FirstLine != 0 {
			styles = append(styles, fmt.Sprintf("text-indent: %.1fpt", float64(p.Ind.FirstLine)/20.0))
		}
		// Note: Right indent is not available in the current library version.
	}

	// Spacing (in twips, 20 twips = 1 point)
	if p.Spacing != nil {
		if p.Spacing.Before > 0 {
			styles = append(styles, fmt.Sprintf("margin-top: %.1fpt", float64(p.Spacing.Before)/20.0))
		}
		if p.Spacing.AfterSpace > 0 {
			styles = append(styles, fmt.Sprintf("margin-bottom: %.1fpt", float64(p.Spacing.AfterSpace)/20.0))
		}
		// The 'line' attribute is in 240ths of a line. 240 is single spacing.
		// A simple conversion to a multiplier is line-height: (value / 240).
		if p.Spacing.Line > 0 {
			// A value of 240 is single, 360 is 1.5, 480 is double.
			// We can convert this to a unitless line-height multiplier.
			styles = append(styles, fmt.Sprintf("line-height: %.2f", float64(p.Spacing.Line)/240.0))
		}
	}

	return styles
}

// getStyleChain recursively finds the inheritance chain for a style.
// It returns a slice of style IDs from the most base to the most specific.
func (a *DocxAgent) getStyleChain(doc *docx.Docx, styleID string, visited map[string]bool) []string {
	if styleID == "" || visited[styleID] {
		return nil
	}
	visited[styleID] = true

	var chain []string
	for _, s := range doc.Styles.Styles {
		if s.StyleID == styleID {
			// 1. Get the chain from the base style first.
			if s.BasedOn != nil && s.BasedOn.Val != "" {
				chain = append(chain, a.getStyleChain(doc, s.BasedOn.Val, visited)...)
			}
			// 2. Then, add the linked character style.
			if s.Link != nil && s.Link.Val != "" {
				// The linked style can also have its own base, so we get its full chain.
				chain = append(chain, a.getStyleChain(doc, s.Link.Val, visited)...)
			}
			// 3. Finally, add the current style itself.
			chain = append(chain, styleID)
			return chain
		}
	}
	return nil
}

// isParagraphEffectivelyEmpty checks if a paragraph contains any renderable content.
// A paragraph is considered empty if it has no children, or if its children
// (like runs) do not contain any visible elements like text, tabs, or images.
func (a *DocxAgent) isParagraphEffectivelyEmpty(p *docx.Paragraph) bool {
	// This function iterates through all parts of a paragraph to see if it contains
	// any visible content. If it only contains formatting or empty text, it's skipped.
	var contentText string
	var hasVisibleElement bool

	for _, child := range p.Children {
		if run, ok := child.(*docx.Run); ok {
			for _, runChild := range run.Children {
				if text, ok := runChild.(*docx.Text); ok {
					contentText += text.Text
				} else {
					// Any non-text element within a run (e.g., tab, image, line break)
					// is considered visible content.
					hasVisibleElement = true
				}
			}
		} else {
			// Any non-run element at the paragraph level (e.g., hyperlink) is content.
			hasVisibleElement = true
		}
	}
	return !hasVisibleElement && strings.TrimSpace(contentText) == ""
}

// writeHTMLNode recursively traverses the DOCX document tree and writes corresponding HTML to the builder.
// pRunProps represents the run properties inherited from the parent paragraph.
func (a *DocxAgent) writeHTMLNode(textBuilder *strings.Builder, doc *docx.Docx, item interface{}, pRunProps *docx.RunProperties) {
	switch v := item.(type) {
	case *docx.Paragraph:
		var pStyles []string
		var classNames []string
		var directRPr *docx.RunProperties // Direct formatting from the paragraph itself.
		isListItem := false
		headingLevel := 0

		if p := v.Properties; p != nil {
			// Get the full inheritance chain of style names to use as CSS classes.
			if p.Style != nil {
				chain := a.getStyleChain(doc, p.Style.Val, make(map[string]bool))
				for _, styleName := range chain {
					classNames = append(classNames, escapeCSSClassName(styleName))
				}
			}

			// The paragraph's own run properties are treated as a direct override.
			if p.RunProperties != nil {
				directRPr = p.RunProperties
			}

			// --- Start of Improved Heading Detection ---

			// Heuristic 1: Check for a style name that indicates a heading (e.g., "Heading1", "heading 2")
			// or if the style ID is a number between 1 and 6.
			if p.Style != nil {
				styleVal := strings.ToLower(p.Style.Val)
				if strings.HasPrefix(styleVal, "heading") {
					// Attempt to parse the level from the style name, e.g., "Heading3" -> 3
					levelStr := ""
					for _, char := range p.Style.Val {
						if char >= '0' && char <= '9' {
							levelStr += string(char)
						}
					}

					if level, err := strconv.Atoi(levelStr); err == nil && level > 0 && level < 7 {
						headingLevel = level
					} else {
						// If parsing fails (e.g., "Heading" with no number), default to h1.
						headingLevel = 1
					}
				} else {
					// Also check if the style ID itself is a number from 1 to 6, which can indicate a heading level.
					if level, err := strconv.Atoi(p.Style.Val); err == nil && level > 0 && level < 7 {
						headingLevel = level
					}
				}
			}

			// Heuristic 2: If not identified by style ID, check for significant spacing before the paragraph.
			// This is a good general heuristic for titles or headings that don't use a named style.
			if headingLevel == 0 && p.Spacing != nil && p.Spacing.Before >= 240 {
				// This is a strong indicator of a heading. We'll default to <h3>
				// as it's a common level for subheadings in a CV.
				headingLevel = 3
			}

			// Heuristic 3: If still not found, check for formatting cues.
			// A paragraph with a single, large, bold run is likely a heading.
			// This is kept as a fallback for unusually formatted documents.
			if headingLevel == 0 && len(v.Children) == 1 {
				if run, ok := v.Children[0].(*docx.Run); ok {
					// Merge paragraph-level default properties with the run's specific properties
					// to get the final, effective style of the text. This is more accurate than
					// just checking the run's direct properties. We pass nil for the base style here.
					finalRunProps := mergeRunProperties(directRPr, run.RunProperties)
					if finalRunProps != nil && finalRunProps.Bold != nil && finalRunProps.Size != nil {
						// Check for bold text that is at least 16pt (32 half-points).
						if size, err := strconv.Atoi(finalRunProps.Size.Val); err == nil && size >= 32 {
							headingLevel = 2 // Treat as H2
						}
					}
				}
			}

			// Heuristic 4: Check for short, fully-bolded lines that act as headings.
			// This is for headings that don't use a named style and might not be large,
			// like "Contacts:" in a CV.
			if headingLevel == 0 {
				var totalText string
				allRunsAreBold := true
				isSimpleTextParagraph := true

				if len(v.Children) == 0 {
					isSimpleTextParagraph = false
				}

				for _, child := range v.Children {
					if run, ok := child.(*docx.Run); ok {
						hasTextInRun := false
						for _, runChild := range run.Children {
							if text, ok := runChild.(*docx.Text); ok {
								totalText += text.Text
								hasTextInRun = true
							}
						}
						// Only check for boldness if the run actually contains text.
						if hasTextInRun {
							finalRunProps := mergeRunProperties(directRPr, run.RunProperties)
							if finalRunProps == nil || finalRunProps.Bold == nil || finalRunProps.Bold.Val == "0" || finalRunProps.Bold.Val == "false" {
								allRunsAreBold = false
								break
							}
						}
					} else {
						// If there's anything other than a run (e.g., a hyperlink), it's not a simple heading.
						isSimpleTextParagraph = false
						break
					}
				}

				trimmedText := strings.TrimSpace(totalText)
				if isSimpleTextParagraph && allRunsAreBold && len(trimmedText) > 0 && len(trimmedText) < 60 && !strings.HasSuffix(trimmedText, ".") {
					headingLevel = 4 // A good default for this kind of inferred heading.
				}
			}
			// --- End of Improved Heading Detection ---

			// A paragraph is only a list item if it has numbering properties AND it has not been identified as a heading.
			if headingLevel == 0 && p.NumProperties != nil {
				isListItem = true
			}

			// Convert all direct paragraph properties to inline styles.
			pStyles = a.convertPPrToCSS(p)
		}

		classAttr := ""
		if len(classNames) > 0 {
			classAttr = fmt.Sprintf(` class="%s"`, strings.Join(classNames, " "))
		}

		styleAttr := ""
		if len(pStyles) > 0 {
			styleAttr = fmt.Sprintf(` style="%s"`, strings.Join(pStyles, "; "))
		}

		var openTag, closeTag string
		if isListItem {
			openTag = fmt.Sprintf("<li%s%s>", classAttr, styleAttr)
			closeTag = "</li>\n"
		} else if headingLevel > 0 {
			openTag = fmt.Sprintf("<h%d%s>", headingLevel, styleAttr)
			closeTag = fmt.Sprintf("</h%d>\n", headingLevel)
		} else {
			openTag = fmt.Sprintf("<p%s%s>", classAttr, styleAttr)
			closeTag = "</p>\n"
		}

		textBuilder.WriteString(openTag)
		for _, pItem := range v.Children {
			// Pass down only the direct run properties from the paragraph.
			// The class attributes on the parent element handle the main styling.
			a.writeHTMLNode(textBuilder, doc, pItem, directRPr)
		}
		textBuilder.WriteString(closeTag)
	case *docx.Table:
		// Get table-wide border properties. These are the defaults.
		tblBorders := v.TableProperties.TableBorders

		textBuilder.WriteString("<table>\n")
		for i, row := range v.TableRows {
			textBuilder.WriteString("  <tr>\n")
			for j, cell := range row.TableCells {
				var cellStyles []string
				if cell.TableCellProperties != nil && cell.TableCellProperties.Shade != nil {
					if fill := cell.TableCellProperties.Shade.Fill; fill != "" && fill != "auto" {
						cellStyles = append(cellStyles, "background-color:#"+fill)
					}
				}

				// --- New Border Logic ---
				tcBorders := cell.TableCellProperties.TableBorders

				// Determine each border side, prioritizing cell-specific borders.
				// Top border
				var topBorder *docx.WTableBorder
				if tcBorders != nil && tcBorders.Top != nil {
					topBorder = tcBorders.Top
				} else if i == 0 && tblBorders != nil { // First row uses table's top border
					topBorder = tblBorders.Top
				} else if tblBorders != nil { // Other rows use table's horizontal interior border
					topBorder = tblBorders.InsideH
				}
				cellStyles = append(cellStyles, "border-top: "+a.borderToCSS(topBorder))

				// Bottom border
				var bottomBorder *docx.WTableBorder
				if tcBorders != nil && tcBorders.Bottom != nil {
					bottomBorder = tcBorders.Bottom
				} else if i == len(v.TableRows)-1 && tblBorders != nil { // Last row
					bottomBorder = tblBorders.Bottom
				} else if tblBorders != nil {
					bottomBorder = tblBorders.InsideH
				}
				cellStyles = append(cellStyles, "border-bottom: "+a.borderToCSS(bottomBorder))

				// Left border (handles start/left)
				var leftBorder *docx.WTableBorder
				if tcBorders != nil && (tcBorders.Start != nil || tcBorders.Left != nil) {
					leftBorder = tcBorders.Start
					if leftBorder == nil {
						leftBorder = tcBorders.Left
					}
				} else if j == 0 && tblBorders != nil { // First column
					leftBorder = tblBorders.Start
					if leftBorder == nil {
						leftBorder = tblBorders.Left
					}
				} else if tblBorders != nil {
					leftBorder = tblBorders.InsideV
				}
				cellStyles = append(cellStyles, "border-left: "+a.borderToCSS(leftBorder))

				// Right border (handles end/right)
				var rightBorder *docx.WTableBorder
				if tcBorders != nil && (tcBorders.End != nil || tcBorders.Right != nil) {
					rightBorder = tcBorders.End
					if rightBorder == nil {
						rightBorder = tcBorders.Right
					}
				} else if j == len(row.TableCells)-1 && tblBorders != nil { // Last column
					rightBorder = tblBorders.End
					if rightBorder == nil {
						rightBorder = tblBorders.Right
					}
				} else if tblBorders != nil {
					rightBorder = tblBorders.InsideV
				}
				cellStyles = append(cellStyles, "border-right: "+a.borderToCSS(rightBorder))

				styleAttr := ""
				if len(cellStyles) > 0 {
					styleAttr = fmt.Sprintf(` style="%s"`, strings.Join(cellStyles, "; "))
				}
				textBuilder.WriteString(fmt.Sprintf("    <td%s>", styleAttr))
				var isListInCellActive bool
				// When converting a table, we must iterate over all items in a cell,
				// not just paragraphs. This ensures that nested tables and other elements
				// are properly rendered.
				for _, item := range cell.Items {
					isListItem := false
					if p, ok := item.(*docx.Paragraph); ok && p.Properties != nil && p.Properties.NumProperties != nil {
						isListItem = true
					}

					if isListItem {
						if !isListInCellActive {
							textBuilder.WriteString("<ul>\n")
							isListInCellActive = true
						}
					} else {
						if isListInCellActive {
							textBuilder.WriteString("</ul>\n")
							isListInCellActive = false
						}
					}
					a.writeHTMLNode(textBuilder, doc, item, nil)
				}
				if isListInCellActive {
					textBuilder.WriteString("</ul>\n")
				}
				textBuilder.WriteString("</td>\n")
			}
			textBuilder.WriteString("  </tr>\n")
		}
		textBuilder.WriteString("</table>\n")
	case *docx.Run:
		var styles []string
		// A run represents a direct formatting override. We merge any direct properties
		// from the parent paragraph with the run's own properties.
		finalRunProps := mergeRunProperties(pRunProps, v.RunProperties)

		if finalRunProps != nil {
			styles = a.convertRPrToCSS(finalRunProps)
		}

		if len(styles) > 0 {
			textBuilder.WriteString(fmt.Sprintf(`<span style="%s">`, strings.Join(styles, "; ")))
		}

		for _, child := range v.Children {
			// Children of a run (like Text or Tab) don't have their own properties,
			// but we pass pRunProps down in case of nested structures.
			a.writeHTMLNode(textBuilder, doc, child, pRunProps)
		}

		if len(styles) > 0 {
			textBuilder.WriteString("</span>")
		}
	case *docx.Hyperlink:
		link, err := doc.ReferTarget(v.ID)
		if err == nil {
			textBuilder.WriteString(fmt.Sprintf(`<a href="%s" target="_blank">`, html.EscapeString(link)))
		}
		a.writeHTMLNode(textBuilder, doc, &v.Run, pRunProps)
		if err == nil {
			textBuilder.WriteString("</a>")
		}
	case *docx.Text:
		textBuilder.WriteString(html.EscapeString(v.Text))
	case *docx.Tab:
		textBuilder.WriteString("&emsp;")
	case *docx.BarterRabbet:
		if v.Type == "page" {
			textBuilder.WriteString(`<hr style="page-break-after:always; visibility:hidden;">`)
		} else {
			textBuilder.WriteString("<br>")
		}
	case *docx.Drawing:
		a.writeImage(textBuilder, doc, v) // Images don't inherit text run properties.
	default:
		// For unhandled types, we can log them for future development.
		a.Printf("Unhandled DOCX node type: %T", v)
	}
}

// mergeRunProperties combines inherited properties from a paragraph with a run's specific properties.
// The run's own properties take precedence over the inherited ones.
func mergeRunProperties(base, override *docx.RunProperties) *docx.RunProperties {
	if base == nil && override == nil {
		return nil
	}
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}

	// Create a new struct, starting with the base properties.
	// This is a shallow copy, which is fine since we replace pointers, not modify them.
	merged := *base

	// Now, apply the override properties. If a property exists in the override,
	// it replaces the one from the base.
	if override.Bold != nil {
		merged.Bold = override.Bold
	}
	if override.Italic != nil {
		merged.Italic = override.Italic
	}
	if override.Underline != nil {
		merged.Underline = override.Underline
	}
	if override.Strike != nil {
		merged.Strike = override.Strike
	}
	if override.Color != nil {
		merged.Color = override.Color
	}
	if override.Fonts != nil {
		merged.Fonts = override.Fonts
	}
	if override.Size != nil {
		merged.Size = override.Size
	}

	return &merged
}

// writeImage extracts image data from the DOCX package and writes an <img> tag.
func (a *DocxAgent) writeImage(textBuilder *strings.Builder, doc *docx.Docx, drawing *docx.Drawing) {
	var relID string
	var descr string

	// Extract relationship ID and description from either inline or anchor drawings.
	if drawing.Inline != nil && drawing.Inline.Graphic != nil && drawing.Inline.Graphic.GraphicData != nil && drawing.Inline.Graphic.GraphicData.Pic != nil {
		pic := drawing.Inline.Graphic.GraphicData.Pic
		if pic.BlipFill != nil && pic.BlipFill.Blip.Embed != "" {
			relID = pic.BlipFill.Blip.Embed
		}
		if drawing.Inline.DocPr != nil {
			descr = drawing.Inline.DocPr.Name
		}
	} else if drawing.Anchor != nil && drawing.Anchor.Graphic != nil && drawing.Anchor.Graphic.GraphicData != nil && drawing.Anchor.Graphic.GraphicData.Pic != nil {
		pic := drawing.Anchor.Graphic.GraphicData.Pic
		if pic.BlipFill != nil && pic.BlipFill.Blip.Embed != "" {
			relID = pic.BlipFill.Blip.Embed
		}
		if drawing.Anchor.DocPr != nil {
			descr = drawing.Anchor.DocPr.Name
		}
	}

	if relID == "" {
		textBuilder.WriteString("[Unsupported Drawing Type]")
		return
	}

	// Find the relationship target (e.g., "media/image1.png") using the library's helper.
	imgTarget, err := doc.ReferTarget(relID)
	if err != nil {
		textBuilder.WriteString(fmt.Sprintf("[Image not found for relID: %s]", relID))
		return
	}

	// The library provides a direct way to access media data.
	// The target is usually prefixed with "media/", which we need to strip.
	media := doc.Media(strings.TrimPrefix(imgTarget, "media/"))
	if media != nil && len(media.Data) > 0 {
		encoded := base64.StdEncoding.EncodeToString(media.Data)
		mimeType := http.DetectContentType(media.Data)
		src := fmt.Sprintf("data:%s;base64,%s", mimeType, encoded)
		alt := html.EscapeString(descr)
		textBuilder.WriteString(fmt.Sprintf(`<img src="%s" alt="%s" style="max-width:100%%; height:auto;" />`, src, alt))
		return // Success
	}

	// Fallback if the image could not be read.
	textBuilder.WriteString(fmt.Sprintf("[Image: %s]", html.EscapeString(descr)))
}
