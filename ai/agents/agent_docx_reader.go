package agents

import (
	"context"
	"fmt"
	"os"
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
		Description: "DOCX Reader: Reads the content of a .docx file from the workspace and returns it as text. You MUST use this tool to read the content of any DOCX file before you can analyze or summarize it.",
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

	content, err := a.readDocxFile(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	return a.CreateFunctionResponse(call, map[string]any{"content": content}, nil)
}

// readDocxFile reads the content of a docx file from the configured workspace.
// Specification: https://pkg.go.dev/github.com/fumiama/go-docx
func (a *DocxAgent) readDocxFile(path string) (string, error) {
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
	for _, it := range doc.Document.Body.Items {
		a.Write(&textBuilder, doc, it)
	}

	return textBuilder.String(), nil
}

func (a *DocxAgent) Write(textBuilder *strings.Builder, doc *docx.Docx, item interface{}) {
	switch v := item.(type) {
	case *docx.Paragraph:
		// A paragraph can contain simple text runs and complex fields like hyperlinks.
		// We need to iterate through its items to correctly extract all text.

		for _, pItem := range v.Children {
			a.Write(textBuilder, doc, pItem)
		}
		textBuilder.WriteByte('\n')
	case *docx.Table:
		// For tables, the default String() method is generally sufficient.
		textBuilder.WriteString(v.String())
		textBuilder.WriteString("\n")
	case *docx.Run:
		// A Run is a container for elements with the same properties.
		// The visible text is in its children (e.g., *docx.Text).
		// We should not process InstrText here, as it contains field codes, not display text.
		for _, child := range v.Children {
			a.Write(textBuilder, doc, child)
		}
	case *docx.Hyperlink:
		// The hyperlink's display text is contained within its Run element.
		// We process the Run by calling the Write method recursively, which will
		// in turn handle the children of the Run (like *docx.Text).
		a.Write(textBuilder, doc, &v.Run)
		link, err := doc.ReferTarget(v.ID)
		if err == nil {
			textBuilder.WriteString(" (")
			textBuilder.WriteString(link)
			textBuilder.WriteByte(')')
		}
	case *docx.Text:
		textBuilder.WriteString(v.Text)
	case *docx.Tab:
		textBuilder.WriteByte('\t')
	case *docx.BarterRabbet:
		// Handle different types of breaks. For text extraction,
		// page breaks can be represented distinctly.
		if v.Type == "page" {
			textBuilder.WriteString("\n\n--- Page Break ---\n\n")
		} else {
			// This covers "textWrapping" (a simple line break), "column" breaks, and default cases.
			textBuilder.WriteByte('\n')
		}
	case *docx.Drawing:
		if v.Inline != nil {
			textBuilder.WriteString(v.Inline.String())
		}
		if v.Anchor != nil {
			textBuilder.WriteString(v.Anchor.String())
		}
	default:
		// Fallback for any other printable types.
		if stringer, ok := v.(fmt.Stringer); ok {
			textBuilder.WriteString(stringer.String())
			textBuilder.WriteString("\n")
		}
	}
}
