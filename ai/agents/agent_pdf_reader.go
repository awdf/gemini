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
	"gemini/inout"
)

type PdfReaderAgent struct {
	*Agent
}

// NewPdfReaderAgent creates a specialized agent for reading and summarizing PDF documents.
func NewPdfReaderAgent(ctx context.Context, client *genai.Client) *PdfReaderAgent {
	systemInstruction := `You are an PDF document reader specialist. 
The user will provide a query with pdf document, read document please and provide concise and accurate response.`

	scheme := genai.Schema{
		Type:        genai.TypeObject,
		Description: "The summary or answer extracted from the PDF document.",
		Properties:  map[string]*genai.Schema{"summary": {Type: genai.TypeString, Description: "A concise summary of the key points from the PDF document, or a direct answer to the user's query."}},
		Required:    []string{"summary"},
	}

	agentConfig := AgentConfig{
		Name:              PdfReaderAgentName,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		ResponseSchema:    &scheme,
	}

	// Create the base agent. NewAgent also registers it.
	baseAgent := NewAgent(ctx, client, agentConfig)

	// Create the specialized agent by embedding the base agent.
	pdfAgent := &PdfReaderAgent{Agent: baseAgent}

	// Overwrite the registration in the registry with the specialized agent.
	// This ensures that when tool calls are dispatched, the correct Handle method is called.
	AgentRegistry[pdfAgent.name] = pdfAgent
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
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

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

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}
