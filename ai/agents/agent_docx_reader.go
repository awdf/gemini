package agents

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fumiama/go-docx"
	"google.golang.org/genai"

	"gemini/config"
)

// AgentDocxReaderName is the name of the docx reader agent.
const AgentDocxReaderName = "docxReaderAgent"

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
	// Add this function to the toolset provided by the caller.
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, readDocxFunc)

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
	if call.Name != "readDocx" {
		return nil // Not for this agent
	}

	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' argument is required and must be a non-empty string"))
	}

	htmlContent, err := a.convertDocxToHTML(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	return a.CreateFunctionResponse(call, map[string]any{"html_content": htmlContent}, nil)
}

// convertDocxToHTML reads a .docx file and converts its content to an HTML string.
// It handles paragraphs, text formatting (bold, italic, etc.), hyperlinks, tables, and images.
// DOCX Format Specification (ECMA-376): https://www.ecma-international.org/publications-and-standards/standards/ecma-376/
func (a *DocxAgent) convertDocxToHTML(path string) (string, error) {
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return "", err
	}

	a.Printf("Reading .docx file: %s", safePath)

	readFile, err := os.Open(safePath)
	if err != nil {
		return "", fmt.Errorf("failed to open docx file at %s: %w", safePath, err)
	}
	defer readFile.Close()

	fileinfo, err := readFile.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info for %s: %w", safePath, err)
	}
	size := fileinfo.Size()

	doc, err := docx.Parse(readFile, size)
	if err != nil {
		return "", fmt.Errorf("failed to parse docx file at %s: %w", safePath, err)
	}

	var textBuilder strings.Builder
	// Add a basic stylesheet for readability.
	textBuilder.WriteString(`<!DOCTYPE html><html><head><meta charset="UTF-8"><style>body{font-family: sans-serif; line-height: 1.4;} table{border-collapse: collapse; width: 100%; margin-bottom: 1em;} td,th{border: 1px solid #dddddd; text-align: left; padding: 8px;} tr:nth-child(even){background-color: #f2f2f2;}</style></head><body>`)

	var isListActive bool
	for _, it := range doc.Document.Body.Items {
		// Check if the current item is a paragraph that's part of a list.
		isListItem := false
		if p, ok := it.(*docx.Paragraph); ok && p.Properties != nil && p.Properties.NumProperties != nil {
			isListItem = true
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
	textBuilder.WriteString("</body></html>")
	return textBuilder.String(), nil
}

// writeHTMLNode recursively traverses the DOCX document tree and writes corresponding HTML to the builder.
// pRunProps represents the run properties inherited from the parent paragraph.
func (a *DocxAgent) writeHTMLNode(textBuilder *strings.Builder, doc *docx.Docx, item interface{}, pRunProps *docx.RunProperties) {
	switch v := item.(type) {
	case *docx.Paragraph:
		var pStyles []string
		var defaultRPr *docx.RunProperties
		isListItem := false

		if p := v.Properties; p != nil {
			// Check if this paragraph is a list item.
			if p.NumProperties != nil {
				isListItem = true
			}

			// Extract paragraph-level styles (alignment, indentation).
			if p.Justification != nil {
				textAlign := p.Justification.Val
				if textAlign == "both" {
					textAlign = "justify"
				}
				pStyles = append(pStyles, "text-align:"+textAlign)
			}
			if p.Ind != nil { // The compiler indicates Left and FirstLine are ints, and Right is undefined.
				if p.Ind.Left != 0 {
					pStyles = append(pStyles, fmt.Sprintf("padding-left:%.1fpt", float64(p.Ind.Left)/20.0))
				}
				// Right indent handling removed as p.Ind.Right is undefined in the library version used.
				if p.Ind.FirstLine != 0 {
					pStyles = append(pStyles, fmt.Sprintf("text-indent:%.1fpt", float64(p.Ind.FirstLine)/20.0))
				}
			}
			// Extract the default run properties for this paragraph, which children will inherit.
			if p.RunProperties != nil {
				defaultRPr = p.RunProperties
			}
		}

		styleAttr := ""
		if len(pStyles) > 0 {
			styleAttr = fmt.Sprintf(` style="%s"`, strings.Join(pStyles, "; "))
		}

		var openTag, closeTag string
		if isListItem {
			openTag = fmt.Sprintf("<li%s>", styleAttr)
			closeTag = "</li>\n"
		} else {
			openTag = fmt.Sprintf("<p%s>", styleAttr)
			closeTag = "</p>\n"
		}

		textBuilder.WriteString(openTag)
		for _, pItem := range v.Children {
			a.writeHTMLNode(textBuilder, doc, pItem, defaultRPr)
		}
		textBuilder.WriteString(closeTag)
	case *docx.Table:
		// The library's String() method provides a plain-text representation.
		// We can now render it properly as HTML.
		textBuilder.WriteString("<table>\n")
		for _, row := range v.TableRows {
			textBuilder.WriteString("  <tr>\n")
			for _, cell := range row.TableCells {
				textBuilder.WriteString("    <td>")
				var isListInCellActive bool
				for _, p := range cell.Paragraphs {
					isListItem := p.Properties != nil && p.Properties.NumProperties != nil
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
					a.writeHTMLNode(textBuilder, doc, p, nil)
				}
				if isListInCellActive {
					textBuilder.WriteString("</ul>\n")
				}
				for _, t := range cell.Tables {
					a.writeHTMLNode(textBuilder, doc, t, nil)
				}
				textBuilder.WriteString("</td>\n")
			}
			textBuilder.WriteString("  </tr>\n")
		}
		textBuilder.WriteString("</table>\n")
	case *docx.Run:
		var tags []string
		var styles []string

		// Merge the inherited paragraph properties with the run's specific properties.
		if p := mergeRunProperties(pRunProps, v.RunProperties); p != nil {
			if p.Bold != nil {
				tags = append(tags, "b")
			}
			if p.Italic != nil {
				tags = append(tags, "i")
			}
			if p.Underline != nil && p.Underline.Val != "none" {
				tags = append(tags, "u")
			}
			if p.Strike != nil {
				tags = append(tags, "s")
			}
			if p.Color != nil {
				styles = append(styles, "color:#"+p.Color.Val)
			}
			if p.Fonts != nil {
				// The ascii font is the most common one for western text.
				if p.Fonts.ASCII != "" {
					styles = append(styles, fmt.Sprintf("font-family:'%s'", p.Fonts.ASCII))
				}
			}
			if p.Size != nil {
				// Size is in half-points. Convert to points for CSS.
				sizeVal, err := strconv.Atoi(p.Size.Val)
				if err == nil {
					styles = append(styles, fmt.Sprintf("font-size:%.1fpt", float64(sizeVal)/2.0))
				}
			}
		}

		// Open tags
		for _, tag := range tags {
			textBuilder.WriteString("<" + tag + ">")
		}
		if len(styles) > 0 {
			textBuilder.WriteString(fmt.Sprintf(`<span style="%s">`, strings.Join(styles, "; ")))
		}

		for _, child := range v.Children {
			// Children of a run (like Text or Tab) inherit the same properties.
			a.writeHTMLNode(textBuilder, doc, child, pRunProps)
		}

		// Close tags in reverse order
		if len(styles) > 0 {
			textBuilder.WriteString("</span>")
		}
		for i := len(tags) - 1; i >= 0; i-- {
			textBuilder.WriteString("</" + tags[i] + ">")
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
func mergeRunProperties(paraProps, runProps *docx.RunProperties) *docx.RunProperties {
	// If there are no properties at all, return nil.
	if paraProps == nil && runProps == nil {
		return nil
	}

	// Create a new, empty properties struct to hold the merged result.
	// This is crucial to prevent state from leaking between paragraphs.
	merged := &docx.RunProperties{}

	// Establish a base and an override. The run's properties override the paragraph's.
	base := paraProps
	if base == nil {
		base = &docx.RunProperties{} // Use an empty struct to avoid nil checks later.
	}
	override := runProps
	if override == nil {
		override = &docx.RunProperties{} // Use an empty struct to avoid nil checks later.
	}

	// For each property, check the override first, then fall back to the base.
	if override.Bold != nil {
		merged.Bold = override.Bold
	} else {
		merged.Bold = base.Bold
	}
	if override.Italic != nil {
		merged.Italic = override.Italic
	} else {
		merged.Italic = base.Italic
	}
	if override.Underline != nil {
		merged.Underline = override.Underline
	} else {
		merged.Underline = base.Underline
	}
	if override.Strike != nil {
		merged.Strike = override.Strike
	} else {
		merged.Strike = base.Strike
	}
	if override.Color != nil {
		merged.Color = override.Color
	} else {
		merged.Color = base.Color
	}
	if override.Fonts != nil {
		merged.Fonts = override.Fonts
	} else {
		merged.Fonts = base.Fonts
	}
	if override.Size != nil {
		merged.Size = override.Size
	} else {
		merged.Size = base.Size
	}

	return merged
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
