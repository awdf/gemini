package ai

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/helpers"
)

// ObjectDetectionNormalizationGrid defines the grid size for normalized bounding box coordinates.
const ObjectDetectionNormalizationGrid = 1000

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
	EnableTools           bool
	EnableGoogleSearch    bool
	EnableURLContext      bool
	EnableCodeExecution   bool
	EnableFunctionCalling bool
	ResponseSchema        *genai.Schema
}

// NewAgent creates a new AI agent with a specific configuration.
func NewAgent(ctx context.Context, client *genai.Client, agentConfig AgentConfig) *Agent {
	log.Printf("Creating new %s agent with model: %s", agentConfig.Name, agentConfig.Model)

	agent := &Agent{
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

	if agentConfig.EnableTools {
		log.Printf("[%s Agent] Tool use is enabled for this request.", agentConfig.Name)
		var tools []*genai.Tool

		// Standard tools can be combined into a single tool definition.
		if agentConfig.EnableGoogleSearch || agentConfig.EnableURLContext {
			standardTool := &genai.Tool{}
			if agentConfig.EnableGoogleSearch {
				log.Printf("GoogleSearch tool enabled for %s.", agentConfig.Name)
				standardTool.GoogleSearch = &genai.GoogleSearch{}
			}
			if agentConfig.EnableURLContext {
				log.Printf("URLContext tool enabled for %s.", agentConfig.Name)
				standardTool.URLContext = &genai.URLContext{}
			}
			tools = append(tools, standardTool)
		}

		if agentConfig.EnableCodeExecution {
			codeExecutionTool := &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}}
			tools = append(tools, codeExecutionTool)
			log.Printf("Code execution tool enabled for %s.", agentConfig.Name)
		}

		// Function calling tools, don't works togather with sandart tools.
		if agentConfig.EnableFunctionCalling {
			tools = append(tools, getFunctionTools()) // Add file system tools
			log.Printf("Function calling tool enabled for %s.", agentConfig.Name)
		}

		if len(tools) > 0 {
			agent.tools = tools
		}
	}

	return agent
}

// Process sends a prompt (with optional other parts like images or URIs) to the agent's model and returns the text response.
// It's a synchronous, one-shot call.
func (a *Agent) Process(prompt string, otherParts ...*genai.Part) (string, error) {
	log.Printf("[%s Agent] Processing prompt: '%s'", a.name, prompt)
	if len(otherParts) > 0 {
		var partDescriptions []string
		for _, p := range otherParts {
			if p.InlineData != nil {
				partDescriptions = append(partDescriptions, fmt.Sprintf("InlineData(MIME: %s, Size: %d)", p.InlineData.MIMEType, len(p.InlineData.Data)))
			} else if p.FileData != nil {
				partDescriptions = append(partDescriptions, fmt.Sprintf("FileURI(%s)", p.FileData.FileURI))
			}
		}
		log.Printf("[%s Agent] Processing with %d additional parts: %s", a.name, len(otherParts), strings.Join(partDescriptions, ", "))
	}

	startTime := time.Now()

	parts := []*genai.Part{genai.NewPartFromText(prompt)}
	parts = append(parts, otherParts...)

	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	conversation := []*genai.Content{userContent}

	genConfig := &genai.GenerateContentConfig{
		ThinkingConfig:   &genai.ThinkingConfig{ThinkingBudget: helpers.Ptr(int32(0))},
		ResponseMIMEType: "application/json",
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
		log.Printf("ERROR: [%s Agent] Content generation failed: %v", a.name, err)
		return "", fmt.Errorf("[%s Agent] content generation failed: %w", a.name, err)
	}

	duration := time.Since(startTime)
	log.Printf("[%s Agent] Processing successful in %v. Response length: %d", a.name, duration, len(resp.Text()))
	return resp.Text(), nil
}

// WarmUp sends a simple, low-cost prompt to the agent's model to reduce
// the "cold start" latency on the first real request. It runs in a goroutine
// to avoid blocking the application's startup sequence.
func (a *Agent) WarmUp() {
	go func() {
		log.Printf("[%s Agent] Warming up model...", a.name)
		// Use a simple prompt. The goal is just to make the model endpoint "hot".
		// We don't care about the response, only that the call is made.
		_, err := a.Process("ping")
		if err != nil {
			// This is not a fatal error, but we should log it for debugging.
			log.Printf("WARNING: [%s Agent] Warm-up call failed: %v", a.name, err)
		}
	}()
}

// GetObjectDetectionSchema returns the schema for object detection responses.
// It defines a structure for a list of predictions, where each prediction
// has a label and a bounding box with named coordinates.
func GetObjectDetectionSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"objects": {
				Type:        genai.TypeArray,
				Description: "A list of detected objects.",
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"label": {
							Type:        genai.TypeString,
							Description: "The identified object's label (e.g., 'car', 'person').",
						},
						"box_2d": {
							Type:        genai.TypeObject,
							Description: fmt.Sprintf("A map containing the bounding box coordinates normalized to a %dx%d grid, where (0,0) is the top-left corner.", ObjectDetectionNormalizationGrid, ObjectDetectionNormalizationGrid),
							Properties: map[string]*genai.Schema{
								"xmin": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized x-coordinate of the left edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
								"ymin": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized y-coordinate of the top edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
								"xmax": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized x-coordinate of the right edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
								"ymax": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized y-coordinate of the bottom edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
							},
							Required: []string{"ymin", "xmin", "xmax", "ymax"},
						},
					},
					Required: []string{"label", "box_2d"},
				},
			},
		},
		Required: []string{"objects"},
	}
}

func GetPdfReaderSchema() *genai.Schema {
	return &genai.Schema{
		Type:        genai.TypeObject,
		Description: "The summary or answer extracted from the PDF document.",
		Properties: map[string]*genai.Schema{
			"summary": {
				Type:        genai.TypeString,
				Description: "A concise summary of the key points from the PDF document, or a direct answer to the user's query.",
			},
		},
		Required: []string{"summary"},
	}
}

func GetYoutubeAgentSchema() *genai.Schema {
	return &genai.Schema{
		Type:        genai.TypeObject,
		Description: "A comprehensive analysis of the YouTube video.",
		Properties: map[string]*genai.Schema{
			"result": {
				Type:        genai.TypeString,
				Description: "A detailed report of the video, including a summary, key topics, takeaways, and a full transcript with visual context, formatted as a single Markdown string.",
			},
		},
		Required: []string{"result"},
	}
}
