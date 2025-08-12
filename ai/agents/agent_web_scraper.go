package agents

import (
	"context"
	"fmt"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

const AgentWebScraperName = "webScraperAgent"

func init() {
	// This agent redundant for Post AI with native URL understanding
	if config.C.LiveAI {
		return
	}

	RegisterFactory(AgentWebScraperName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewWebScraperAgent(ctx, client, toolset, bus)
	})
}

type WebScraperAgent struct {
	*Agent
}

// NewWebScraperAgent creates a specialized agent for scraping and analyzing web pages.
func NewWebScraperAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *WebScraperAgent {
	systemInstruction := `You are a web page analysis expert with vision capabilities. 
Your goal is to extract as much meaningful information as possible from the provided web page URL. 
Analyze both the text content and the visual layout/images on the page to generate a comprehensive and detailed report. 
Describe important visual elements like images, charts, and the overall page structure in your analysis.`

	scheme := genai.Schema{
		Type:        genai.TypeObject,
		Description: "A comprehensive analysis or summary of the web page.",
		Properties: map[string]*genai.Schema{
			"result": {Type: genai.TypeString, Description: "A detailed report of the web page content, formatted as a single Markdown string."},
		},
		Required: []string{"result"},
	}

	functions := genai.FunctionDeclaration{
		Name:        "browseWebPage",
		Description: "WEB BROWSER: Scrapes and provides a comprehensive analysis of the content of a web page URL.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The full URL of the web page to analyze.",
				},
			},
			Required: []string{"url"},
		},
		Behavior: genai.BehaviorNonBlocking,
	}
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &functions)

	agentConfig := AgentConfig{
		Name:              AgentWebScraperName,
		Model:             config.C.AI.Model,
		RPM:               config.C.AI.ModelRPM,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		EnableURLContext:  true,
		ResponseSchema:    &scheme,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	webScraperAgent := &WebScraperAgent{Agent: baseAgent}

	return webScraperAgent
}

func (a *WebScraperAgent) WarmUp() time.Duration {
	return a.Agent.WarmUp()
}

func (a *WebScraperAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "browseWebPage":
		return a.handleWebScraperTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *WebScraperAgent) handleWebScraperTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// 1. Parse arguments
	url, urlOK := call.Args["url"].(string)

	if !urlOK || url == "" {
		err = fmt.Errorf("'url' argument is required and must be a non-empty string")
	} else {
		// 2. Process with the agent. The agent is configured with URLContext,
		// so we just pass the URL in the prompt. The model will use its tool.
		prompt := fmt.Sprintf("Please analyze the provided web page and generate a comprehensive report based on your instructions. URL: %s", url)
		resultText, processErr := a.Process(prompt)
		if processErr != nil {
			err = fmt.Errorf("web page processing failed: %w", processErr)
		} else {
			a.Printf("Web page analysis successful for url: '%s'", url)
			result = map[string]any{"result": resultText}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}
