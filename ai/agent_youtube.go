package ai

import (
	"context"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

// NewYoutubeAgent creates a specialized agent for analyzing YouTube videos.
func NewYoutubeAgent(ctx context.Context, client *genai.Client) *Agent {
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
	agentConfig := AgentConfig{
		Name:              YoutubeAgent,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		ResponseSchema:    GetYoutubeAgentSchema(),
	}
	return NewAgent(ctx, client, agentConfig)
}

func GetYoutubeAgentSchema() *genai.Schema {
	return &genai.Schema{
		Type:        genai.TypeObject,
		Description: "A comprehensive analysis of the YouTube video.",
		Properties:  map[string]*genai.Schema{"result": {Type: genai.TypeString, Description: "A detailed report of the video, including a summary, key topics, takeaways, and a full transcript with visual context, formatted as a single Markdown string."}},
		Required:    []string{"result"},
	}
}
