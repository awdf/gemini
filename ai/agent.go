package ai

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

// Agent names used as keys in the agent map.
const (
	ObjectDetectionAgent = "objectDetection"
	PdfReaderAgent       = "pdfReaderAgent"
	YoutubeAgent         = "youtubeAgent"
	WebScraperAgent      = "webScraperAgent"
	GmailClientAgent     = "gmailAgent"
)

type Callable interface {
	Process(prompt string, parts ...*genai.Part) (string, error)
	WarmUp()
}

// Agent is a specialized, self-contained AI processor for specific tasks.
// It operates without the main application's event bus or pipeline, making it
// suitable for on-demand, synchronous processing like in tool calls.
type Agent struct {
	name              string
	ctx               context.Context
	client            *genai.Client
	modelName         string
	systemInstruction *genai.Content
	responseSchema    *genai.Schema
	temperature       *float32
	tools             []*genai.Tool
}

// AgentConfig defines the configuration for an Agent.
type AgentConfig struct {
	Name                  string
	Model                 string
	SystemInstruction     string
	Temperature           *float32
	EnableGoogleSearch    bool
	EnableURLContext      bool
	EnableCodeExecution   bool
	EnableFunctionCalling bool
	ResponseSchema        *genai.Schema
}

// agentRegistry holds all created agent instances, keyed by their name.
var agentRegistry = make(map[string]Callable)

// Agents post initialization
func Registerate(agent *Agent) {
	if config.C.AI.EnableFunctionCalling && config.C.AI.AgentWarmUp {
		agent.WarmUp()
	}
}

// NewAgent creates a new AI agent with a specific configuration.
func NewAgent(ctx context.Context, client *genai.Client, agentConfig AgentConfig) *Agent {
	log.Printf("Creating new %s agent with model: %s", agentConfig.Name, agentConfig.Model)

	agent := Agent{
		name:           agentConfig.Name,
		ctx:            ctx,
		client:         client,
		modelName:      agentConfig.Model,
		responseSchema: agentConfig.ResponseSchema,
		temperature:    agentConfig.Temperature,
	}

	if agentConfig.SystemInstruction != "" {
		agent.systemInstruction = genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(agentConfig.SystemInstruction)}, "")
	}

	// Determine if any tools are enabled by checking the specific configuration flags.
	toolsEnabled := agentConfig.EnableGoogleSearch || agentConfig.EnableURLContext || agentConfig.EnableCodeExecution || agentConfig.EnableFunctionCalling
	if toolsEnabled {
		log.Printf("[%s] Tool use is enabled.", agentConfig.Name)
		var tools []*genai.Tool

		// Standard tools can be combined into a single tool definition.
		if agentConfig.EnableGoogleSearch || agentConfig.EnableURLContext {
			standardTool := &genai.Tool{}
			if agentConfig.EnableGoogleSearch {
				log.Printf("[%s] GoogleSearch tool enabled", agentConfig.Name)
				standardTool.GoogleSearch = &genai.GoogleSearch{}
			}
			if agentConfig.EnableURLContext {
				log.Printf("[%s] URLContext tool enabled", agentConfig.Name)
				standardTool.URLContext = &genai.URLContext{}
			}
			tools = append(tools, standardTool)
		}

		if agentConfig.EnableCodeExecution {
			codeExecutionTool := &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}}
			tools = append(tools, codeExecutionTool)
			log.Printf("[%s] Code execution tool enabled", agentConfig.Name)
		}

		// Function calling tools, don't works togather with sandart tools.
		if agentConfig.EnableFunctionCalling {
			tools = append(tools, getFunctionTools()) // Add file system tools
			log.Printf("[%s] Function calling tool enabled", agentConfig.Name)
		}

		if len(tools) > 0 {
			agent.tools = tools
		}
	}

	// Register the newly created agent.
	if _, exists := agentRegistry[agent.name]; exists {
		log.Printf("WARNING: Agent with name '%s' is being re-registered. This may indicate a configuration issue.", agent.name)
	}
	agentRegistry[agent.name] = &agent
	log.Printf("Agent '%s' registered successfully.", agent.name)

	return &agent
}

// Process sends a prompt (with optional other parts like images or URIs) to the agent's model and returns the text response.
// It's a synchronous, one-shot call.
func (a *Agent) Process(prompt string, otherParts ...*genai.Part) (string, error) {
	log.Printf("[%s] Processing prompt: '%s'", a.name, prompt)
	if len(otherParts) > 0 {
		var partDescriptions []string
		for _, p := range otherParts {
			if p.InlineData != nil {
				partDescriptions = append(partDescriptions, fmt.Sprintf("InlineData(MIME: %s, Size: %d)", p.InlineData.MIMEType, len(p.InlineData.Data)))
			} else if p.FileData != nil {
				partDescriptions = append(partDescriptions, fmt.Sprintf("FileURI(%s)", p.FileData.FileURI))
			}
		}
		log.Printf("[%s] Processing with %d additional parts: %s", a.name, len(otherParts), strings.Join(partDescriptions, ", "))
	}

	startTime := time.Now()

	parts := []*genai.Part{genai.NewPartFromText(prompt)}
	parts = append(parts, otherParts...)

	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	conversation := []*genai.Content{userContent}

	genConfig := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: helpers.Ptr(int32(0))},
	}

	if len(a.tools) == 0 && a.responseSchema != nil {
		// Only set the response MIME type if we are NOT using tools, as they are incompatible.
		// The presence of a ResponseSchema is sufficient to get JSON output when using tools.
		genConfig.ResponseMIMEType = "application/json"
	}

	if a.temperature != nil {
		genConfig.Temperature = a.temperature
	} else {
		genConfig.Temperature = helpers.Ptr(float32(0.5)) // Default temperature if not specified
	}
	if a.systemInstruction != nil {
		genConfig.SystemInstruction = a.systemInstruction
	}
	if len(a.tools) > 0 {
		genConfig.Tools = a.tools
	}
	if a.responseSchema != nil {
		genConfig.ResponseSchema = a.responseSchema
	}

	resp, err := a.client.Models.GenerateContent(a.ctx, a.modelName, conversation, genConfig)
	if err != nil {
		log.Printf("[%s] ERROR: Content generation failed: %v", a.name, err)
		return "", fmt.Errorf("[%s] content generation failed: %w", a.name, err)
	}

	duration := time.Since(startTime)
	log.Printf("[%s] Processing successful in %v. Response length: %d", a.name, duration, len(resp.Text()))
	return resp.Text(), nil
}

// WarmUp sends a simple, low-cost prompt to the agent's model to reduce
// the "cold start" latency on the first real request. It runs in a goroutine
// to avoid blocking the application's startup sequence.
func (a *Agent) WarmUp() {
	go func() {
		log.Printf("[%s] Warming up model...", a.name)
		// Use a simple prompt. The goal is just to make the model endpoint "hot".
		// We don't care about the response, only that the call is made.
		_, err := a.Process("ping")
		if err != nil {
			// This is not a fatal error, but we should log it for debugging.
			log.Printf("[%s] WARNING: Warm-up call failed: %v", a.name, err)
		}
	}()
}
