package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/asaskevich/EventBus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"gemini/config"
)

// newTestAI creates a minimal AI struct for testing purposes.
func newTestAI(t *testing.T) *AI {
	config.C.AI.APIKey = "dummy-key-for-testing"
	bus := EventBus.New()

	// A dummy client is sufficient for tests that don't make real API calls.
	// The `createPartFromFile` method is special-cased for text files
	// to avoid uploads, which allows this to work.
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: "dummy"})
	require.NoError(t, err)

	return &AI{
		ctx:    context.Background(),
		wg:     &sync.WaitGroup{},
		bus:    &bus,
		client: client,
	}
}

func TestParsePromptForMultimedia(t *testing.T) {
	// Setup a test server to mock URL responses
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "fakedata")
		case "/webpage.html":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "<html></html>")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Setup a dummy file for file:// tests
	tempDir := t.TempDir()
	localFilePath := filepath.Join(tempDir, "test.txt")
	err := os.WriteFile(localFilePath, []byte("hello from file"), 0o644)
	require.NoError(t, err)
	localFileURI := "file://" + filepath.ToSlash(localFilePath)

	ai := newTestAI(t)

	tests := []struct {
		name               string
		prompt             string
		expectedText       string
		expectedPartsCount int
		expectedMultimedia bool
		expectError        bool
		partCheck          func(t *testing.T, parts []*genai.Part)
	}{
		{
			name:               "plain text prompt",
			prompt:             "hello world",
			expectedText:       "hello world",
			expectedPartsCount: 1,
			expectedMultimedia: false,
		},
		{
			name:               "youtube url",
			prompt:             "check this video https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			expectedText:       "check this video",
			expectedPartsCount: 2,
			expectedMultimedia: true,
			partCheck: func(t *testing.T, parts []*genai.Part) {
				uriPart := parts[1]
				require.NotNil(t, uriPart.FileData, "FileData should not be nil for a URI part")
				assert.Equal(t, "https://www.youtube.com/watch?v=dQw4w9WgXcQ", uriPart.FileData.FileURI)
				assert.Equal(t, "video/*", uriPart.FileData.MIMEType)
			},
		},
		{
			name:               "local file url (text)",
			prompt:             fmt.Sprintf("use this file %s as context", localFileURI),
			expectedText:       "use this file as context",
			expectedPartsCount: 2,
			expectedMultimedia: true,
			partCheck: func(t *testing.T, parts []*genai.Part) {
				textPart := parts[1]
				assert.Equal(t, "hello from file", textPart.Text)
			},
		},
		{
			name:               "web page url (treated as text)",
			prompt:             fmt.Sprintf("summarize %s/webpage.html", server.URL),
			expectedText:       fmt.Sprintf("summarize %s/webpage.html", server.URL),
			expectedPartsCount: 1,
			expectedMultimedia: false,
		},
		{
			name:               "image url (downloaded as data)",
			prompt:             fmt.Sprintf("what is in this image %s/image.png", server.URL),
			expectedText:       "what is in this image",
			expectedPartsCount: 2,
			expectedMultimedia: true,
			partCheck: func(t *testing.T, parts []*genai.Part) {
				bytesPart := parts[1]
				require.NotNil(t, bytesPart.InlineData)
				assert.Equal(t, "image/png", bytesPart.InlineData.MIMEType)
				assert.Equal(t, "fakedata", string(bytesPart.InlineData.Data))
			},
		},
		{
			name:               "unreachable url (treated as text)",
			prompt:             "check http://localhost:12345/unreachable",
			expectedText:       "check http://localhost:12345/unreachable",
			expectedPartsCount: 1,
			expectedMultimedia: false,
		},
		{
			name:        "non-existent file url",
			prompt:      "use file:///non/existent/file.txt",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, multimediaFound, err := ai.parsePromptForMultimedia(tt.prompt)

			if tt.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedMultimedia, multimediaFound)
			require.Len(t, parts, tt.expectedPartsCount)

			if tt.expectedText != "" {
				textPart := parts[0]
				require.NotNil(t, textPart, "first part should not be nil")
				assert.Equal(t, tt.expectedText, textPart.Text)
			}

			if tt.partCheck != nil {
				tt.partCheck(t, parts)
			}
		})
	}
}

func TestSaveConversationHistory(t *testing.T) {
	ai := newTestAI(t)
	ai.conversationHistory = []*genai.Content{
		genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText("Hello")}, "user"),
		genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText("Hi there!")}, "model"),
	}

	tempDir := t.TempDir()
	historyFile := filepath.Join(tempDir, "history.txt")

	err := ai.saveConversationHistory(historyFile)
	require.NoError(t, err)

	content, err := os.ReadFile(historyFile)
	require.NoError(t, err)

	expectedContent := "--- USER ---\nHello\n\n--- MODEL ---\nHi there!\n\n"
	assert.Equal(t, expectedContent, string(content))
}
