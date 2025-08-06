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

	return a.CreateFunctionResponse(call, map[string]any{"content": htmlContent}, nil)
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
	for _, it := range doc.Document.Body.Items {
		a.writeHTMLNode(&textBuilder, doc, it)
	}
	textBuilder.WriteString("</body></html>")
	return textBuilder.String(), nil
}

// writeHTMLNode recursively traverses the DOCX document tree and writes corresponding HTML to the builder.
func (a *DocxAgent) writeHTMLNode(textBuilder *strings.Builder, doc *docx.Docx, item interface{}) {
	switch v := item.(type) {
	case *docx.Paragraph:
		textBuilder.WriteString("<p>")
		for _, pItem := range v.Children {
			a.writeHTMLNode(textBuilder, doc, pItem)
		}
		textBuilder.WriteString("</p>\n")
	case *docx.Table:
		// The library's String() method provides a plain-text representation.
		// While not ideal for full HTML conversion, it's a robust fallback.
		// We wrap it in <pre> to preserve some of the spacing.
		textBuilder.WriteString("<table><tr><td><pre>")
		textBuilder.WriteString(html.EscapeString(v.String()))
		textBuilder.WriteString("</pre></td></tr></table>\n")
	case *docx.Run:
		var tags []string
		var styles []string
		if p := v.RunProperties; p != nil {
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
			a.writeHTMLNode(textBuilder, doc, child)
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
		a.writeHTMLNode(textBuilder, doc, &v.Run)
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
		a.writeImage(textBuilder, doc, v)
	default:
		// For unhandled types, we can log them for future development.
		a.Printf("Unhandled DOCX node type: %T", v)
	}
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
