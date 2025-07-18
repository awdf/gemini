package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

// TestGeminiAPI_Transcription is an integration test that verifies connectivity
// and basic functionality with the Google Gemini API for audio transcription.
// It requires a valid GEMINI_API_KEY and a test audio file.
func TestGeminiAPI_Transcription(t *testing.T) {
	// Best practice: Skip integration tests if the required API key isn't set.
	// This prevents failures in environments where secrets are not configured.
	// You'll need to set this environment variable: export GEMINI_API_KEY="your-key"
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("Skipping integration test: GEMINI_API_KEY environment variable is not set")
	}

	ctx := context.Background()
	// The client automatically uses the API key from the environment.
	client, err := genai.NewClient(ctx, nil)
	require.NoError(t, err, "genai.NewClient should not fail")

	// Use a relative path to make the test portable. You will need to create
	// a `testdata` directory in your project root with a sample `recording.mp3` file.
	localAudioPath := filepath.Join("testdata", "recording.mp3")
	_, err = os.Stat(localAudioPath)
	require.False(t, os.IsNotExist(err), "test audio file must exist at %s", localAudioPath)

	// Use the older client.Files.UploadFromPath method, consistent with ai/ai.go
	uploadedFile, err := client.Files.UploadFromPath(ctx, localAudioPath, nil)
	require.NoError(t, err, "UploadFromPath should not fail")

	// Use the older NewPartFrom... helpers
	parts := []*genai.Part{
		genai.NewPartFromText("Generate a transcript of the speech."),
		genai.NewPartFromURI(uploadedFile.URI, uploadedFile.MIMEType),
	}

	// The older API requires wrapping parts in a Content struct.
	contents := []*genai.Content{
		genai.NewContentFromParts(parts, genai.RoleUser),
	}

	// Use a known valid and recent model. Note that the default config.toml
	// specifies "gemini-2.5-flash", which may be a typo for "gemini-1.5-flash".
	// Using "-latest" is a good practice for tests.
	modelName := "gemini-1.5-flash-latest"

	// Use the older client.Models.GenerateContent method
	resp, err := client.Models.GenerateContent(ctx, modelName, contents, nil)
	require.NoError(t, err, "GenerateContent failed for model %s", modelName)

	require.NotNil(t, resp, "GenerateContent returned a nil response")
	require.NotEmpty(t, resp.Candidates, "GenerateContent returned no candidates")

	transcript := resp.Text()
	fmt.Println("Transcript:", transcript)
	t.Log("Transcript:", transcript)
	assert.NotEmpty(t, transcript, "The transcript should not be empty")
}

func TestParseFlags(t *testing.T) {
	// Save original os.Args and flag set
	oldArgs := os.Args
	oldFlagSet := flag.CommandLine
	defer func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlagSet
	}()

	tests := []struct {
		name          string
		args          []string
		expectedFlags CliFlags
	}{
		{
			name: "no flags",
			args: []string{"cmd"},
			expectedFlags: CliFlags{
				Voice:      false,
				Transcript: false,
				AIEnabled:  true,
				ConfigPath: "config.toml",
			},
		},
		{
			name: "all flags set",
			args: []string{"cmd", "--voice", "--ts", "--no-ai", "--config", "myconfig.toml"},
			expectedFlags: CliFlags{
				Voice:      true,
				Transcript: true,
				AIEnabled:  false,
				ConfigPath: "myconfig.toml",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset flags for each test run
			flag.CommandLine = flag.NewFlagSet(tt.name, flag.ExitOnError)
			os.Args = tt.args

			flags := parseFlags()

			assert.Equal(t, tt.expectedFlags.Voice, flags.Voice)
			assert.Equal(t, tt.expectedFlags.Transcript, flags.Transcript)
			assert.Equal(t, tt.expectedFlags.AIEnabled, flags.AIEnabled)
			assert.Equal(t, tt.expectedFlags.ConfigPath, flags.ConfigPath)
		})
	}
}
