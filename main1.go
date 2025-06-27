package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"google.golang.org/genai"
)

func main1() {
	const prompt = "Explain how AI works in a few words"
	const model = "gemini-2.5-flash"

	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  os.Getenv("GOOGLE_API_KEY"),
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatal(err)
	}

	//Flash	thinking
	// Dynamic:-1
	// None:0
	// low:512,
	// Medium:8,192,
	// High:24,576
	var thinking int32 = -1 // Disables thinking

	resp := client.Models.GenerateContentStream(
		ctx,
		model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: true,
				ThinkingBudget:  &thinking,
			},
		},
	)

	for chunk := range resp {
		for _, part := range chunk.Candidates[0].Content.Parts {
			if len(part.Text) == 0 {
				continue
			}

			if part.Thought {
				fmt.Printf("Thought: %s\n", part.Text)
			} else {
				fmt.Printf("Answer: %s\n", part.Text)
			}
		}
	}

}
