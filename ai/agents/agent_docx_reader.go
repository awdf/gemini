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
		// The items in the body are of type interface{}. We must use a type assertion
		// to check if an item implements the fmt.Stringer interface (which provides the String() method).
		if stringer, ok := it.(fmt.Stringer); ok {
			textBuilder.WriteString(stringer.String())
			textBuilder.WriteString("\n") // Add a newline to separate block elements
		}
	}

	return textBuilder.String(), nil
}
