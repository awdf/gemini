package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

func init() {
	RegisterFactory(AgentPdfReaderName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		return NewPdfReaderAgent(ctx, client, toolset)
	})
}

type PdfReaderAgent struct {
	*Agent
}

// NewPdfReaderAgent creates a specialized agent for reading and summarizing PDF documents.
func NewPdfReaderAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *PdfReaderAgent {
	systemInstruction := `You are an PDF document reader specialist. 
The user will provide a query with pdf document, read document please and provide concise and accurate response.`

	scheme := genai.Schema{
		Type:        genai.TypeObject,
		Description: "The summary or answer extracted from the PDF document.",
		Properties:  map[string]*genai.Schema{"summary": {Type: genai.TypeString, Description: "A concise summary of the key points from the PDF document, or a direct answer to the user's query."}},
		Required:    []string{"summary"},
	}

	functions := genai.FunctionDeclaration{
		Name:        "readPdf",
		Description: "PDF Reader: Reads the content of a PDF file from the workspace and answers a question about it.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the PDF file to read.",
				},
				"query": {
					Type:        genai.TypeString,
					Description: "The question to ask about the PDF document (e.g., 'Summarize this document').",
				},
			},
			Required: []string{"path", "query"},
		},
		Behavior: genai.BehaviorBlocking,
	}
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &functions)

	agentConfig := AgentConfig{
		Name:              AgentPdfReaderName,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		ResponseSchema:    &scheme,
	}

	baseAgent := NewAgent(ctx, client, agentConfig)

	pdfAgent := &PdfReaderAgent{Agent: baseAgent}

	return pdfAgent
}

func (a *PdfReaderAgent) WarmUp() {
	a.Agent.WarmUp()
}

func (a *PdfReaderAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "readPdf":
		return a.handleReadPdfTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *PdfReaderAgent) handleReadPdfTool(call *genai.FunctionCall) *genai.FunctionResponse {
	var result any
	var err error

	// 1. Parse arguments
	path, pathOK := call.Args["path"].(string)
	query, queryOK := call.Args["query"].(string)

	if !pathOK || path == "" || !queryOK || query == "" {
		err = fmt.Errorf("'path' and 'query' arguments are required and must be non-empty strings")
	} else {
		// 2. Get safe path and read file
		safePath, pathErr := config.GetSafePath(path)
		if pathErr != nil {
			err = pathErr
		} else {
			pdfBytes, readErr := os.ReadFile(safePath)
			if readErr != nil {
				err = fmt.Errorf("failed to read PDF file '%s': %w", path, readErr)
			} else {
				// 3. Process with the agent. `a` is the PdfReaderAgent.
				summaryJSON, processErr := a.Process(query, genai.NewPartFromBytes(pdfBytes, "application/pdf"))
				if processErr != nil {
					err = fmt.Errorf("PDF processing failed: %w", processErr)
				} else {
					// The agent returns a JSON string based on the schema; we need to parse it.
					var summaryData struct {
						Summary string `json:"summary"`
					}
					if jsonErr := json.Unmarshal([]byte(summaryJSON), &summaryData); jsonErr != nil {
						err = fmt.Errorf("failed to parse summary from agent response: %w", jsonErr)
					} else {
						log.Printf("PDF processing successful for query: '%s'", query)
						result = map[string]any{"summary": summaryData.Summary}
					}
				}
			}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}
