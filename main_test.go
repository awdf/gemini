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
	uploadedFile, _ := client.Files.UploadFromPath(
		ctx,
		localAudioPath,
		nil,
	)

	parts := []*genai.Part{
		genai.NewPartFromText("Generate a transcript of the speech."),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	result, _ := client.Models.GenerateContent(
		ctx,
		"gemini-2.5-flash",
		contents,
		nil,
	)
	fmt.Println(result.Text())
	t.Log(result.Text())
}
