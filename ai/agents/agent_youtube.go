package agents

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

func init() {
	RegisterFactory(AgentYoutubeName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		return NewYoutubeAgent(ctx, client, toolset)
	})
}

type YoutubeAgent struct {
	*Agent
}

// NewYoutubeAgent creates a specialized agent for analyzing YouTube videos.
func NewYoutubeAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *YoutubeAgent {
	systemInstruction := `You are a comprehensive YouTube video analysis expert. Your goal is to extract as much meaningful information as possible from the provided video. Analyze both the audio and visual components to generate a detailed report.

Your response MUST be a single block of text and should be structured using Markdown headings for the following sections:

### Summary
Provide a concise, high-level summary of the video's main topic and purpose.

### Key Topics
Identify the main topics or chapters discussed in the video.

### Detailed Transcript with Visual Context
Provide a full and accurate transcript of the video's audio. Where relevant, interleave descriptions of important visual elements, 
on-screen text, or actions that provide context to the speech. For example: "[Visual: A diagram of a neural network is shown on screen]".

### Key Takeaways
List the most important points, conclusions, or actionable advice presented in the video.

Analyze the video thoroughly to provide a rich and informative response.`

	scheme := genai.Schema{
		Type:        genai.TypeObject,
		Description: "A comprehensive analysis of the YouTube video.",
		Properties:  map[string]*genai.Schema{"result": {Type: genai.TypeString, Description: "A detailed report of the video, including a summary, key topics, takeaways, and a full transcript with visual context, formatted as a single Markdown string."}},
		Required:    []string{"result"},
	}

	functions := genai.FunctionDeclaration{
		Name:        "analyzeYoutubeVideo",
		Description: "YOUTUBE: Performs a comprehensive analysis of a YouTube video, providing a summary, key topics, takeaways, and a full transcript with visual context.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The full URL of the YouTube video to analyze.",
				},
			},
			Required: []string{"url"},
		},
		Behavior: genai.BehaviorNonBlocking, // IMPORTANT: Works only in non blocking mode
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &functions)

	agentConfig := AgentConfig{
		Name:              AgentYoutubeName,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		ResponseSchema:    &scheme,
	}
	// Create the base agent. NewAgent also registers it.
	baseAgent := NewAgent(ctx, client, agentConfig)

	// Create the specialized agent by embedding the base agent.
	youtubeAgent := &YoutubeAgent{Agent: baseAgent}

	// Overwrite the registration in the registry with the specialized agent.
	// This ensures that when tool calls are dispatched, the correct Handle method is called.
	return youtubeAgent
}

func (a *YoutubeAgent) WarmUp() {
	a.Agent.WarmUp()
}

func (a *YoutubeAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "analyzeYoutubeVideo":
		return a.handleYoutubeAnalysisTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *YoutubeAgent) handleYoutubeAnalysisTool(call *genai.FunctionCall) *genai.FunctionResponse {
	var result any
	var err error

	// 1. Parse arguments
	url, urlOK := call.Args["url"].(string)
	query := "Please analyze the provided video and generate a comprehensive report based on your instructions."

	if !urlOK || url == "" {
		err = fmt.Errorf("'url' argument is required and must be a non-empty string")
	} else {
		// 3. Process with the agent. The Gemini API accepts various video MIME types,
		// but "video/mp4" is recommended in documentation for YouTube URLs.
		resultText, processErr := a.Agent.Process(query, genai.NewPartFromURI(url, "video/mp4"))
		if processErr != nil {
			err = fmt.Errorf("YouTube video processing failed: %w", processErr)
		} else {
			log.Printf("YouTube video analysis successful for url: '%s'", url)
			result = map[string]any{"result": resultText}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}
