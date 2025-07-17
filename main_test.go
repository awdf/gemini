package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/genai"
)

func TestMain(t *testing.T) {
	// Best practice: Skip integration tests if the required API key isn't set.
	// This prevents failures in environments where secrets are not configured.
	// You'll need to set this environment variable: export GEMINI_API_KEY="your-key"
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("Skipping test: GEMINI_API_KEY environment variable is not set")
	}

	ctx := context.Background()
	// The client automatically uses the API key from the environment.
	client, err := genai.NewClient(ctx, nil)
	if err != nil {
		t.Fatalf("genai.NewClient failed: %v", err)
	}

	// Use a relative path to make the test portable. You will need to create
	// a `testdata` directory in your project root with a sample `recording.wav` file.
	localAudioPath := filepath.Join("testdata", "recording.wav")
	if _, err := os.Stat(localAudioPath); os.IsNotExist(err) {
		t.Fatalf("test audio file not found at %s. Please create it.", localAudioPath)
	}

	uploadedFile, err := client.Files.UploadFromPath(
		ctx,
		localAudioPath,
		nil,
	)
	if err != nil {
		t.Fatalf("UploadFromPath failed: %v", err)
	}

	parts := []*genai.Part{
		genai.NewPartFromText("Generate a transcript of the speech."),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	// Corrected model name: "gemini-1.5-flash" is a valid model, "gemini-2.5-flash" is not.
	model := "gemini-1.5-flash"
	result, err := client.Models.GenerateContent(ctx, model, contents, nil)
	if err != nil {
		t.Fatalf("GenerateContent failed for model %s: %v", model, err)
	}
	fmt.Println(result.Text())
	t.Log(result.Text())
}
