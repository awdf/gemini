package main

import (
	"context"
	"fmt"
	"log"
	"testing"

	"google.golang.org/genai"
)

func TestMain(t *testing.T) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}

	localAudioPath := "/home/awdf/Workspace/go/src/capgemini/recording-1.wav"
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

	result, err := client.Models.GenerateContent(
		ctx,
		"gemini-1.5-flash",
		contents,
		nil,
	)
	if err != nil {
		t.Fatalf("GenerateContent failed: %v", err)
	}
	fmt.Println(result.Text())
	t.Log(result.Text())
}
