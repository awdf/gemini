package ai

import (
	"context"
	"fmt"
	"iter"
	"os"
	"sync"

	"google.golang.org/genai"
)

const (
	DEBUG = false
)

// Model
const model = "gemini-2.5-flash"

// Context
var ctx context.Context

// Client
var client *genai.Client

// Flash model thinking
// Dynamic:-1
// None:0
// low:512,
// Medium:8,192,
// High:24,576
var thinking int32 = -1
var thoughts = false

var isInit = false

func Init() {

	ctx = context.Background()
	client = Control(genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  os.Getenv("GOOGLE_API_KEY"),
		Backend: genai.BackendGeminiAPI,
	}))

	isInit = true
}

func Chat(wg *sync.WaitGroup, fileChan <-chan string) {
	defer wg.Done()

	if !isInit {
		Init()
	}

	for file := range fileChan {
		fmt.Printf("Chat: Processing %s\n", file)
		err := VoiceQuestion(file, "Step 1: Generate a transcript of the speech. Step 2: Respond to the question from the speech.")
		if err != nil {
			// If there was an error, log it and DO NOT delete the file.
			fmt.Fprintf(os.Stderr, "Chat: AI processing failed for %s, leaving file for retry: %v\n", file, err)
		} else {
			// Only remove the file if processing was successful.
			fmt.Printf("Chat: Successfully processed %s. Removing file.\n", file)
			os.Remove(file)
		}
	}
	fmt.Println("AI Chat work finished")
}

func TextQuestion(prompt string) {

	if !isInit {
		Init()
	}

	resp := client.Models.GenerateContentStream(
		ctx,
		model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: thoughts,
				ThinkingBudget:  &thinking,
			},
		},
	)

	if err := Output(resp); err != nil {
		fmt.Fprintf(os.Stderr, "Error processing text question: %v\n", err)
	}
}

func VoiceQuestion(wavPath string, prompt string) error {

	if !isInit {
		Init()
	}

	uploadedFile, err := client.Files.UploadFromPath(
		ctx,
		wavPath,
		nil,
	)
	if err != nil {
		return fmt.Errorf("file upload failed: %w", err)
	}

	parts := []*genai.Part{
		genai.NewPartFromText(prompt),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	resp := client.Models.GenerateContentStream(
		ctx,
		model,
		contents,
		&genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{
				IncludeThoughts: thoughts,
				ThinkingBudget:  &thinking,
			},
		},
	)

	return Output(resp)
}

func Output(resp iter.Seq2[*genai.GenerateContentResponse, error]) error {
	for chunk, err := range resp {
		if err != nil {
			return err
		}
		// Add nil checks for robustness
		if chunk == nil || len(chunk.Candidates) == 0 || chunk.Candidates[0].Content == nil {
			continue
		}

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
	return nil
}

// Control is a helper function to check errors during GStreamer element creation.
func Control[T any](object T, err error) T {
	if err != nil {
		panic(err)
	}
	return object
}

// Verify is a helper function to check errors during GStreamer linking.
func Verify(err error) {
	if err != nil {
		panic(err)
	}
}
