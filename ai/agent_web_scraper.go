package ai

import (
	"context"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

// NewWebScraperAgent creates a specialized agent for scraping and analyzing web pages.
func NewWebScraperAgent(ctx context.Context, client *genai.Client) *Agent {
	systemInstruction := `You are a web page analysis expert with vision capabilities. 
Your goal is to extract as much meaningful information as possible from the provided web page URL. 
Analyze both the text content and the visual layout/images on the page to generate a comprehensive and detailed report. 
Describe important visual elements like images, charts, and the overall page structure in your analysis.`
	agentConfig := AgentConfig{
		Name:              WebScraperAgentName,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		EnableURLContext:  true,
		ResponseSchema:    GetWebScraperSchema(),
	}
	return NewAgent(ctx, client, agentConfig)
}

func GetWebScraperSchema() *genai.Schema {
	return &genai.Schema{
		Type:        genai.TypeObject,
		Description: "A comprehensive analysis or summary of the web page.",
		Properties: map[string]*genai.Schema{
			"result": {Type: genai.TypeString, Description: "A detailed report of the web page content, formatted as a single Markdown string."},
		},
		Required: []string{"result"},
	}
}
