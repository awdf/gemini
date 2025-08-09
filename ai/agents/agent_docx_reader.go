package agents

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gemini/config"

	"github.com/fumiama/go-docx"
	"google.golang.org/genai"
)

// AgentDocxReaderName is the name of the docx reader agent.
const AgentDocxReaderName = "docxReaderAgent"

func (a *DocxAgent) generateCSSFromStyles(styles *docx.Styles) string {
	if styles == nil {
		return ""
	}

	// This refactored implementation correctly uses the library's data structures
	// and helper functions (convertPPrToCSS, convertRPrToCSS) to generate
	// accurate CSS for all defined styles, including paragraph borders.
	var css strings.Builder
	for _, s := range styles.Styles { // s is a docx.StyleDefinition
		if s.StyleID == "" {
			continue
		}
		className := escapeCSSClassName(s.StyleID)
		if className == "" {
			continue
		}

		// All styles are rendered as classes. This is more robust and avoids conflicts.
		finalSelector := "." + className

		var styleRules []string
		if s.ParagraphProperties != nil {
			styleRules = append(styleRules, a.convertPPrToCSS(s.ParagraphProperties)...)
		}
		if s.RunProperties != nil {
			styleRules = append(styleRules, a.convertRPrToCSS(s.RunProperties)...)
		}

		if len(styleRules) > 0 {
			css.WriteString(fmt.Sprintf("%s {\n", finalSelector))
			for _, rule := range styleRules {
				css.WriteString(fmt.Sprintf("  %s;\n", rule))
			}
			css.WriteString("}\n")
		}
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
.banded-rows tr:nth-child(even){background-color: #f2f2f2; }`)
	fullHTML.WriteString(cssStyles)
	fullHTML.WriteString("</style></head><body>")
	fullHTML.WriteString(htmlBody)
	fullHTML.WriteString("</body></html>")

	return a.CreateFunctionResponse(call, map[string]any{"html_content": fullHTML.String()}, nil)
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
	css = a.generateCSSFromStyles(&doc.Styles)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse docx file at %s: %w", safePath, err)
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
	if p.Underline != nil {
		// If an underline is specified, handle both enabling and disabling it.
		if p.Underline.Val != "none" && p.Underline.Val != "false" {
			styles = append(styles, "text-decoration: underline")
		} else {
			styles = append(styles, "text-decoration: none")
		}
	}
	if p.Strike != nil && p.Strike.Val != "0" && p.Strike.Val != "false" {
		styles = append(styles, "text-decoration: line-through")
	}
	if p.Color != nil {
		// The 'auto' color means it should inherit from its parent.
		// We only set a color if it's a specific hex value.
		if p.Color.Val != "auto" {
			styles = append(styles, "color: #"+p.Color.Val)
		}
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

	// Paragraph Borders (for horizontal lines, etc.)
	if p.ParaBorders != nil {
		if p.ParaBorders.Top != nil {
			styles = append(styles, "border-top: "+a.borderToCSS(p.ParaBorders.Top))
		}
		if p.ParaBorders.Bottom != nil {
			styles = append(styles, "border-bottom: "+a.borderToCSS(p.ParaBorders.Bottom))
		}
		if p.ParaBorders.Left != nil {
			styles = append(styles, "border-left: "+a.borderToCSS(p.ParaBorders.Left))
		}
		if p.ParaBorders.Right != nil {
			styles = append(styles, "border-right: "+a.borderToCSS(p.ParaBorders.Right))
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
		// Use a case-insensitive comparison to match the heading detection logic,
		// which also uses ToLower. This prevents bugs where a style like "heading 2"
		// is detected but the style definition "Heading 2" is not found.
		if strings.EqualFold(s.StyleID, styleID) {
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

// findStyleByID finds a style definition by its ID.
func (a *DocxAgent) findStyleByID(doc *docx.Docx, styleID string) *docx.StyleDefinition {
	if styleID == "" || doc == nil || doc.Styles.Styles == nil {
		return nil
	}
	for i := range doc.Styles.Styles {
		if strings.EqualFold(doc.Styles.Styles[i].StyleID, styleID) {
			return &doc.Styles.Styles[i]
		}
	}
	return nil
}

// mergeTableBorders combines two WTableBorders structs.
// Properties from 'override' will take precedence over 'base'.
func mergeTableBorders(base, override *docx.WTableBorders) *docx.WTableBorders {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}

	merged := *base // Start with a shallow copy of the base.

	if override.Top != nil {
		merged.Top = override.Top
	}
	if override.Left != nil {
		merged.Left = override.Left
	}
	if override.Bottom != nil {
		merged.Bottom = override.Bottom
	}
	if override.Right != nil {
		merged.Right = override.Right
	}
	if override.InsideH != nil {
		merged.InsideH = override.InsideH
	}
	if override.InsideV != nil {
		merged.InsideV = override.InsideV
	}
	if override.Start != nil {
		merged.Start = override.Start
	}
	if override.End != nil {
		merged.End = override.End
	}

	return &merged
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
			openTag = fmt.Sprintf("<h%d%s%s>", headingLevel, classAttr, styleAttr)
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
		var tblBorders *docx.WTableBorders
		var tblLook *docx.WTableLook
		var tblStyleClass string

		if v.TableProperties != nil {
			// Start with the table's direct border properties.
			tblBorders = v.TableProperties.TableBorders
			tblLook = v.TableProperties.Look

			// If the table has a style, find it and merge its properties.
			if v.TableProperties.Style != nil && v.TableProperties.Style.Val != "" {
				styleID := v.TableProperties.Style.Val
				tblStyleClass = escapeCSSClassName(styleID)

				styleDef := a.findStyleByID(doc, styleID)
				if styleDef != nil && styleDef.TableProperties != nil {
					stylePr := styleDef.TableProperties
					// The style's properties are the base, and the table's direct
					// properties are the override.
					tblBorders = mergeTableBorders(stylePr.TableBorders, tblBorders)

					// If the table doesn't have a direct Look property, use the one from the style.
					if tblLook == nil {
						tblLook = stylePr.Look
					}
				}
			}
		}

		var allClasses []string
		if tblStyleClass != "" {
			allClasses = append(allClasses, tblStyleClass)
		}

		// Check for horizontal banding (alternating row colors).
		// The NoHBand property being 0 means banding is ON.
		var bandedRows bool
		if tblLook != nil {
			// The noHBand attribute is an explicit override. If it's present, it wins.
			if tblLook.NoHBand != nil {
				// Banding is ON if noHBand is explicitly set to 0 ("false" or "0").
				bandedRows = (*tblLook.NoHBand == 0)
			} else if tblLook.Val != "" {
				// If noHBand is not present, fall back to the 'val' bitmask.
				valInt, err := strconv.ParseInt(tblLook.Val, 16, 32)
				if err == nil {
					// Horizontal banding is controlled by bit 0x0200.
					bandedRows = (valInt&0x0200 != 0)
				}
			}
		}
		if bandedRows {
			allClasses = append(allClasses, "banded-rows")
		}

		classAttr := ""
		if len(allClasses) > 0 {
			classAttr = fmt.Sprintf(` class="%s"`, strings.Join(allClasses, " "))
		}

		textBuilder.WriteString(fmt.Sprintf("<table%s>\n", classAttr))
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
					// A paragraph is a list item if it has numbering properties AND it is not a heading.
					// This check is now consistent with the main body's list detection.
					isListItem := false
					if p, ok := item.(*docx.Paragraph); ok && p.Properties != nil && p.Properties.NumProperties != nil {
						isHeading := false
						if p.Properties.Style != nil && strings.HasPrefix(p.Properties.Style.Val, "Heading") {
							isHeading = true
						}
						if !isHeading {
							isListItem = true
						}
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

		if v.RunProperties != nil {
			// If a run has its own <w:rPr> block, it signifies a direct formatting override.
			// This override should also reset any text decorations inherited from a paragraph's
			// style, unless the run itself specifies a decoration.
			hasTextDecoration := false
			for _, s := range styles {
				if strings.HasPrefix(s, "text-decoration:") {
					hasTextDecoration = true
					break
				}
			}

			if !hasTextDecoration {
				// Since no decoration (underline, strike, etc.) was specified in the run's
				// own properties, we explicitly disable it to prevent inheritance.
				styles = append(styles, "text-decoration: none")
			}
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
