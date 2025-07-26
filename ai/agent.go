package ai

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

// Agent is a specialized, self-contained AI processor for specific tasks.
// It operates without the main application's event bus or pipeline, making it
// suitable for on-demand, synchronous processing like in tool calls.
type Agent struct {
	ctx               context.Context
	client            *genai.Client
	modelName         string
	systemInstruction *genai.Content
	responseSchema    *genai.Schema
	tools             []*genai.Tool
}

// AgentConfig defines the configuration for an Agent.
type AgentConfig struct {
	Model             string
	SystemInstruction string
	EnableTools       bool
	ResponseSchema    *genai.Schema
}

// NewAgent creates a new AI agent with a specific configuration.
func NewAgent(ctx context.Context, agentConfig AgentConfig) *Agent {
	client := helpers.Check(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  config.C.AI.APIKey, // Still using global API key for now
		Backend: genai.BackendGeminiAPI,
	}))

	log.Printf("Creating new Agent with model: %s", agentConfig.Model)

	agent := &Agent{
		ctx:            ctx,
		client:         client,
		modelName:      agentConfig.Model,
		responseSchema: agentConfig.ResponseSchema,
	}

	if agentConfig.SystemInstruction != "" {
		agent.systemInstruction = genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(agentConfig.SystemInstruction)}, "")
	}

	if agentConfig.EnableTools {
		// This is a simplified tool setup for an agent.
		if config.C.AI.EnableFunctionCalling {
			log.Println("Agent created with file system tools.")
			agent.tools = append(agent.tools, getFileSystemTool())
		}
	}

	return agent
}

// Process sends a prompt (with an optional image) to the agent's model and returns the text response.
// It's a synchronous, one-shot call.
func (a *Agent) Process(prompt string, imageData []byte, mimeType string) (string, error) {
	log.Printf("Agent processing prompt: '%s'", prompt)
	if len(imageData) > 0 {
		log.Printf("Agent processing with image data (MIME: %s, Size: %d bytes)", mimeType, len(imageData))
	}

	startTime := time.Now()

	parts := []*genai.Part{genai.NewPartFromText(prompt)}
	if len(imageData) > 0 && mimeType != "" {
		parts = append(parts, genai.NewPartFromBytes(imageData, mimeType))
	}

	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	conversation := []*genai.Content{userContent}

	genConfig := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
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
		log.Printf("ERROR: Agent content generation failed: %v", err)
		return "", fmt.Errorf("agent content generation failed: %w", err)
	}

	duration := time.Since(startTime)
	log.Printf("Agent processing successful in %v. Response length: %d", duration, len(resp.Text()))
	return resp.Text(), nil
}

// GetObjectDetectionSchema returns the schema for object detection responses.
// It defines a structure for a list of predictions, where each prediction
// has a label and a bounding box with named coordinates.
func GetObjectDetectionSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"predictions": {
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
							Description: "A map containing the bounding box coordinates.",
							Properties: map[string]*genai.Schema{
								"ymin": {Type: genai.TypeInteger, Description: "The minimum y-coordinate, normalized to 1000."},
								"xmin": {Type: genai.TypeInteger, Description: "The minimum x-coordinate, normalized to 1000."},
								"ymax": {Type: genai.TypeInteger, Description: "The maximum y-coordinate, normalized to 1000."},
								"xmax": {Type: genai.TypeInteger, Description: "The maximum x-coordinate, normalized to 1000."},
							},
							Required: []string{"ymin", "xmin", "ymax", "xmax"},
						},
					},
					Required: []string{"label", "box_2d"},
				},
			},
		},
		Required: []string{"predictions"},
	}
}
