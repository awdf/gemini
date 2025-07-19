package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/inout"
)

// TestMain loads a default configuration for the tests.
func TestMain(m *testing.M) {
	config.Load("dummy-config.toml")
	os.Remove("dummy-config.toml")
	os.Exit(m.Run())
}

// newTestAI is a helper to create an AI instance for testing.
// It uses nil for dependencies that are not relevant to the specific test.
func newTestAI(_ *testing.T) *AI {
	bus := EventBus.New()
	return &AI{
		ctx:       context.Background(),
		wg:        &sync.WaitGroup{},
		formatter: inout.NewFormatter(),
		bus:       &bus,
	}
}

func TestSaveConversationHistory(t *testing.T) {
	ai := newTestAI(t)
	ai.conversationHistory = []*genai.Content{
		{Role: "user", Parts: []*genai.Part{genai.NewPartFromText("Hello")}},
		{Role: "model", Parts: []*genai.Part{genai.NewPartFromText("Hi there!")}},
	}

	t.Run("without initial context", func(t *testing.T) {
		tmpfile, err := os.CreateTemp(t.TempDir(), "history-*.txt")
		require.NoError(t, err)
		tmpfile.Close() // Close because saveConversationHistory opens it again

		err = ai.saveConversationHistory(tmpfile.Name())
		require.NoError(t, err)

		content, err := os.ReadFile(tmpfile.Name())
		require.NoError(t, err)
		expected := "--- USER ---\nHello\n\n--- MODEL ---\nHi there!\n\n"
		assert.Equal(t, expected, string(content))
	})

	t.Run("with initial context", func(t *testing.T) {
		ai.initialContextAdded = true // Mark that initial context was added
		tmpfile, err := os.CreateTemp(t.TempDir(), "history-*.txt")
		require.NoError(t, err)
		tmpfile.Close()

		err = ai.saveConversationHistory(tmpfile.Name())
		require.NoError(t, err)

		content, err := os.ReadFile(tmpfile.Name())
		require.NoError(t, err)
		// The first two entries should be skipped
		assert.Equal(t, "", string(content), "history should be empty when only initial context exists")

		// Add more history
		ai.conversationHistory = append(ai.conversationHistory,
			&genai.Content{Role: "user", Parts: []*genai.Part{genai.NewPartFromText("Another question")}},
			&genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText("Another answer")}},
		)

		err = ai.saveConversationHistory(tmpfile.Name())
		require.NoError(t, err)
		content, err = os.ReadFile(tmpfile.Name())
		require.NoError(t, err)
		expected := "--- USER ---\nAnother question\n\n--- MODEL ---\nAnother answer\n\n"
		assert.Equal(t, expected, string(content))
	})
}

func TestHandleEvents(t *testing.T) {
	ai := newTestAI(t)
	tmpfile, err := os.CreateTemp(t.TempDir(), "history-*.txt")
	require.NoError(t, err)
	tmpfile.Close()

	// Test "save" event
	ai.handleEvents("save:" + tmpfile.Name())

	// Check if the file was created (it will be empty, which is fine for this test)
	_, err = os.Stat(tmpfile.Name())
	assert.NoError(t, err, "file should have been created by the save event")

	// Test malformed event
	// This should just log a warning, so we can't assert much, but we can check it doesn't panic.
	assert.NotPanics(t, func() {
		ai.handleEvents("malformedevent")
	})
}

func TestRetryWithBackoff(t *testing.T) {
	ai := newTestAI(t)
	config.C.AI.Retry.MaxRetries = 2
	config.C.AI.Retry.InitialDelayMs = 10
	config.C.AI.Retry.MaxDelayMs = 50

	t.Run("success on first try", func(t *testing.T) {
		var attempts int
		action := func() error {
			attempts++
			return nil
		}
		err := ai.retryWithBackoff(action)
		assert.NoError(t, err)
		assert.Equal(t, 1, attempts)
	})

	t.Run("success after retry on 503 error", func(t *testing.T) {
		var attempts int
		action := func() error {
			attempts++
			if attempts == 1 {
				return &googleapi.Error{Code: 503, Message: "Service Unavailable"}
			}
			return nil
		}
		err := ai.retryWithBackoff(action)
		assert.NoError(t, err)
		assert.Equal(t, 2, attempts)
	})

	t.Run("failure after all retries", func(t *testing.T) {
		var attempts int
		retryableErr := &googleapi.Error{Code: 429, Message: "Rate limited"}
		action := func() error {
			attempts++
			return retryableErr
		}
		err := ai.retryWithBackoff(action)
		require.Error(t, err)
		assert.Equal(t, 3, attempts) // 1 initial + 2 retries
		assert.ErrorIs(t, err, retryableErr)
	})

	t.Run("failure on non-retryable error", func(t *testing.T) {
		var attempts int
		nonRetryableErr := errors.New("this is a permanent error")
		action := func() error {
			attempts++
			return nonRetryableErr
		}
		err := ai.retryWithBackoff(action)
		require.Error(t, err)
		assert.Equal(t, 1, attempts)
		assert.ErrorIs(t, err, nonRetryableErr)
	})
}

func TestFindCacheableFiles(t *testing.T) {
	t.Run("directory not found", func(t *testing.T) {
		files, err := findCacheableFiles("non-existent-dir")
		assert.NoError(t, err)
		assert.Nil(t, files)
	})

	t.Run("empty directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		files, err := findCacheableFiles(tmpDir)
		assert.NoError(t, err)
		assert.Nil(t, files)
	})

	t.Run("directory with files", func(t *testing.T) {
		tmpDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("a"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file2.md"), []byte("b"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitkeep"), []byte("c"), 0o644))
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "subdir"), 0o755))

		files, err := findCacheableFiles(tmpDir)
		require.NoError(t, err)
		require.Len(t, files, 2)

		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		assert.Contains(t, names, "file1.txt")
		assert.Contains(t, names, "file2.md")
	})
}

func TestRun_Disabled(_ *testing.T) {
	wg := &sync.WaitGroup{}
	bus := EventBus.New()
	fileChan := make(chan string, 2)
	textChan := make(chan string, 2)

	ai := &AI{
		wg:          wg,
		flags:       &Flags{Enabled: false}, // AI is disabled
		fileChan:    fileChan,
		textCmdChan: textChan,
		bus:         &bus,
	}

	wg.Add(1)
	go ai.Run()

	fileChan <- "test1.wav"
	textChan <- "test command"
	fileChan <- "test2.wav"
	textChan <- "another command"

	close(fileChan)
	close(textChan)

	wg.Wait() // This will hang if the channels are not drained.
}

func TestAddInitialContextTurn(t *testing.T) {
	ai := newTestAI(t)
	userContent := genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText("context")}, genai.RoleUser)
	modelResponse := "OK"

	ai.addInitialContextTurn(userContent, modelResponse, "test log")

	assert.True(t, ai.initialContextAdded)
	require.Len(t, ai.conversationHistory, 2)
	assert.Equal(t, userContent, ai.conversationHistory[0])
	assert.Equal(t, "model", ai.conversationHistory[1].Role)
	assert.Equal(t, modelResponse, ai.conversationHistory[1].Parts[0].Text)
}

// TestParsePromptForMultimedia tests the logic for parsing user input into different content parts.
func TestParsePromptForMultimedia(t *testing.T) {
	ai := newTestAI(t)

	// 1. Setup a test HTTP server to mock web responses.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("fake-image-data"))
		case "/page.html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>A web page</body></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// 2. Setup a temporary local file for testing file:// URLs.
	tmpFile, err := os.CreateTemp(t.TempDir(), "test-*.txt")
	require.NoError(t, err)
	_, err = tmpFile.WriteString("local file content")
	require.NoError(t, err)
	tmpFile.Close()
	testFileURI := "file://" + tmpFile.Name()

	testCases := []struct {
		name               string
		prompt             string
		expectedNumParts   int
		expectedFirstPart  string // The text part of the prompt
		expectedMultimedia bool
		expectError        bool
	}{
		{
			name:               "plain text prompt",
			prompt:             "this is a simple test",
			expectedNumParts:   1,
			expectedFirstPart:  "this is a simple test",
			expectedMultimedia: false,
		},
		{
			name:               "youtube url",
			prompt:             "check this video https://www.youtube.com/watch?v=dQw4w9WgXcQ please",
			expectedNumParts:   2,
			expectedFirstPart:  "check this video please",
			expectedMultimedia: true,
		},
		{
			name:               "local file url (text)",
			prompt:             fmt.Sprintf("here is the file %s for context", testFileURI),
			expectedNumParts:   2,
			expectedFirstPart:  "here is the file for context",
			expectedMultimedia: true,
		},
		{
			name:               "web page url (treated as text)",
			prompt:             fmt.Sprintf("look at this site %s/page.html", server.URL),
			expectedNumParts:   1,
			expectedFirstPart:  fmt.Sprintf("look at this site %s/page.html", server.URL),
			expectedMultimedia: false,
		},
		{
			name:               "image url (downloaded as data)",
			prompt:             fmt.Sprintf("here is an image %s/image.png", server.URL),
			expectedNumParts:   2,
			expectedFirstPart:  "here is an image",
			expectedMultimedia: true,
		},
		{
			name:               "unreachable url (treated as text)",
			prompt:             "what about http://localhost:12345/unreachable",
			expectedNumParts:   1,
			expectedFirstPart:  "what about http://localhost:12345/unreachable",
			expectedMultimedia: false,
		},
		{
			name:               "non-existent file url",
			prompt:             "file:///non/existent/file.txt",
			expectedNumParts:   0,    // No parts should be returned on error
			expectedMultimedia: true, // It detects a multimedia type, but fails to process it
			expectError:        true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			parts, multimediaFound, err := ai.parsePromptForMultimedia(tc.prompt)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.expectedMultimedia, multimediaFound, "multimedia flag mismatch")
			require.Len(t, parts, tc.expectedNumParts, "number of parts mismatch")

			if tc.expectedNumParts > 0 && tc.expectedFirstPart != "" {
				assert.Equal(t, tc.expectedFirstPart, parts[0].Text, "text part content mismatch")
			}
		})
	}
}

func TestPrepareInitialFiles(t *testing.T) {
	t.Run("with valid files", func(t *testing.T) {
		ai := newTestAI(t)
		tmpDir := t.TempDir()
		config.C.AI.CacheDir = tmpDir
		config.C.AI.CacheSystemPrompt = "Use these files."

		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("content1"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file2.md"), []byte("content2"), 0o644))

		ai.prepareInitialFiles()

		assert.True(t, ai.initialContextAdded)
		require.Len(t, ai.conversationHistory, 2)

		userContent := ai.conversationHistory[0]
		assert.Equal(t, "user", userContent.Role)
		require.Len(t, userContent.Parts, 3)
		assert.Equal(t, "Use these files.", userContent.Parts[0].Text)
		assert.Equal(t, "content1", userContent.Parts[1].Text)
		assert.Equal(t, "content2", userContent.Parts[2].Text)

		modelContent := ai.conversationHistory[1]
		assert.Equal(t, "model", modelContent.Role)
		assert.Contains(t, modelContent.Parts[0].Text, "OK. I have received the files")
	})

	t.Run("with no files", func(t *testing.T) {
		ai := newTestAI(t)
		tmpDir := t.TempDir()
		config.C.AI.CacheDir = tmpDir

		ai.prepareInitialFiles()

		assert.False(t, ai.initialContextAdded)
		assert.Empty(t, ai.conversationHistory)
	})

	t.Run("with non-existent directory", func(t *testing.T) {
		ai := newTestAI(t)
		config.C.AI.CacheDir = "non-existent-dir"

		ai.prepareInitialFiles()

		assert.False(t, ai.initialContextAdded)
		assert.Empty(t, ai.conversationHistory)
	})
}

// mockIter is a helper to create a mock iterator for testing the Output function.
func mockIter(chunks []*genai.GenerateContentResponse, err error) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		for _, chunk := range chunks {
			if !yield(chunk, nil) {
				return
			}
		}
		if err != nil {
			yield(nil, err)
		}
	}
}

// captureOutput captures everything written to stdout during the execution of a function.
func captureOutput(f func()) string {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	f()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestOutput(t *testing.T) {
	ai := newTestAI(t)
	// Mute voice for this test to avoid trying to play audio, which would fail.
	config.C.AI.VoiceEnabled = false
	t.Cleanup(func() { config.C.AI.VoiceEnabled = true }) // Restore config

	t.Run("simple text response", func(t *testing.T) {
		chunks := []*genai.GenerateContentResponse{
			{Candidates: []*genai.Candidate{
				{Content: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText("Hello, ")}}},
			}},
			{Candidates: []*genai.Candidate{
				{Content: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText("world!")}}},
			}},
		}
		respIter := mockIter(chunks, nil)

		var fullText string
		var err error
		output := captureOutput(func() {
			fullText, err = ai.Output(respIter, 123*time.Millisecond)
		})

		require.NoError(t, err)
		assert.Equal(t, "Hello, world!", fullText)
		assert.Contains(t, output, "Answer:")
		assert.Contains(t, output, "Hello, world!")
		assert.Contains(t, output, "Request execution time: 0.12s")
	})

	t.Run("with thoughts and sources", func(t *testing.T) {
		chunks := []*genai.GenerateContentResponse{
			{Candidates: []*genai.Candidate{
				{Content: &genai.Content{Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "search", Args: map[string]any{"query": "Go"}}},
				}}},
			}},
			{Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText("Go is a language.")}},
					GroundingMetadata: &genai.GroundingMetadata{
						GroundingChunks: []*genai.GroundingChunk{
							{Web: &genai.GroundingChunkWeb{URI: "https://go.dev", Title: "The Go Programming Language"}},
						},
					},
				},
			}},
		}
		respIter := mockIter(chunks, nil)

		var fullText string
		var err error
		output := captureOutput(func() {
			fullText, err = ai.Output(respIter, 0)
		})

		require.NoError(t, err)
		assert.Equal(t, "Go is a language.", fullText)
		assert.Contains(t, output, "Thought:")
		assert.Contains(t, output, "Tool Call: search")
		assert.Contains(t, output, "Answer:")
		assert.Contains(t, output, "Go is a language.")
		assert.Contains(t, output, "Sources:")
		assert.Contains(t, output, "The Go Programming Language")
		assert.Contains(t, output, "https://go.dev")
	})

	t.Run("with error in stream", func(t *testing.T) {
		testErr := errors.New("stream error")
		respIter := mockIter(nil, testErr)

		var err error
		captureOutput(func() {
			_, err = ai.Output(respIter, 0)
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, testErr)
	})
}

func TestCreatePartFromFile(t *testing.T) {
	ai := newTestAI(t)

	t.Run("text file is read directly", func(t *testing.T) {
		tmpFile, err := os.CreateTemp(t.TempDir(), "test-*.txt")
		require.NoError(t, err)
		_, err = tmpFile.WriteString("hello text file")
		require.NoError(t, err)
		tmpFile.Close()

		part, err := ai.createPartFromFile(tmpFile.Name())
		require.NoError(t, err)
		require.NotNil(t, part)
		assert.Equal(t, "hello text file", part.Text)
	})

	t.Run("non-text file path requires client", func(t *testing.T) {
		tmpFile, err := os.CreateTemp(t.TempDir(), "test-*.wav")
		require.NoError(t, err)
		tmpFile.Close()

		assert.Panics(t, func() {
			_, _ = ai.createPartFromFile(tmpFile.Name())
		}, "should panic when calling upload on a nil client")
	})
}
