package ai

import (
	"context"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

// NewPdfReaderAgent creates a specialized agent for reading and summarizing PDF documents.
func NewPdfReaderAgent(ctx context.Context, client *genai.Client) *Agent {
	systemInstruction := `You are an PDF document reader specialist. 
The user will provide a query with pdf document, read document please and provide concise and accurate response.`
	agentConfig := AgentConfig{
		Name:              PdfReaderAgent,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.0)),
		ResponseSchema:    GetPdfReaderSchema(),
	}
	return NewAgent(ctx, client, agentConfig)
}

func GetPdfReaderSchema() *genai.Schema {
	return &genai.Schema{
		Type:        genai.TypeObject,
		Description: "The summary or answer extracted from the PDF document.",
		Properties:  map[string]*genai.Schema{"summary": {Type: genai.TypeString, Description: "A concise summary of the key points from the PDF document, or a direct answer to the user's query."}},
		Required:    []string{"summary"},
	}
}
