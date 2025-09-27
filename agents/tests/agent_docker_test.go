package tests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"google.golang.org/genai"

	"gemini/agents"
)

const dockerhubSearchToolName = "dockerhub_search"

// setupDockerAgentTest is a helper function to set up the DockerAgent for testing.
func setupDockerAgentTest(t *testing.T) (context.Context, context.CancelFunc, *genai.Tool, *agents.DockerAgent) {
	t.Helper()

	// Give Docker time to pull the image if it's not present locally.
	t.Log("Giving Docker up to 2 minutes to pull the mcp-server image if needed...")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)

	tmpDir := t.TempDir()
	geminiDir := filepath.Join(tmpDir, ".gemini")
	err := os.MkdirAll(geminiDir, 0o755)
	assert.NoError(t, err)

	settings := agents.McpSettings{
		McpServers: map[string]agents.McpServerConfig{
			"dockerhub": {
				Command: "docker",
				Args:    []string{"run", "--rm", "-i", "docker/mcp-server"},
			},
		},
	}
	settingsFile := filepath.Join(geminiDir, "settings.json")
	settingsData, err := json.Marshal(settings)
	assert.NoError(t, err)
	err = os.WriteFile(settingsFile, settingsData, 0o644)
	assert.NoError(t, err)

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	t.Cleanup(func() {
		os.Setenv("HOME", origHome)
		cancel() // Ensure context is cancelled
	})

	toolset := &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{},
	}
	bus := EventBus.New()
	agent := agents.NewDockerAgent(ctx, nil, toolset, &bus)
	assert.NotNil(t, agent, "NewDockerAgent should not return nil")

	return ctx, cancel, toolset, agent
}

func TestDockerAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode.")
	}

	_, _, toolset, agent := setupDockerAgentTest(t)
	var err error

	// Assert that the agent has discovered tools from the MCP server.
	assert.NotEmpty(t, toolset.FunctionDeclarations, "Agent should discover tools from MCP server")
	t.Logf("Discovered %d tools from the MCP server.", len(toolset.FunctionDeclarations))

	// Find the 'search' tool declaration.
	var searchTool *genai.FunctionDeclaration
	for _, fn := range toolset.FunctionDeclarations {
		if fn.Name == dockerhubSearchToolName {
			searchTool = fn
			break
		}
	}
	assert.NotNil(t, searchTool, "The 'search' tool should be discovered")

	// Execute the 'search' tool call via the agent's Handle method.
	t.Log("Executing 'search' tool call with query 'ubuntu'...")
	searchCall := &genai.FunctionCall{
		Name: dockerhubSearchToolName,
		Args: map[string]any{
			"query": "ubuntu",
		},
	}

	response := agent.Handle(searchCall)
	assert.NotNil(t, response)
	assert.NotNil(t, response.Response)

	// The response.Response is a map[string]any, where the actual content is under the "output" key.
	outputMap := response.Response
	t.Logf("outputMap: %+v", outputMap)

	// The "output" value is a slice of maps, where each map contains "type" and "text".
	responseContent, ok := outputMap["output"].([]mcp.Content)
	t.Logf("responseContent: %+v, ok: %t", responseContent, ok)
	assert.True(t, ok, "outputMap[\"output\"] should be a slice of []*mcp.Content")
	assert.NotEmpty(t, responseContent, "Response content should not be empty")

	// Extract the text content from the first element.
	firstResponsePart := responseContent[0]
	responseBytes, err := firstResponsePart.MarshalJSON()
	assert.NoError(t, err, "Should be able to marshal first response part to JSON")

	// Unmarshal the JSON string to get the actual search results.
	var searchResult struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	err = json.Unmarshal(responseBytes, &searchResult)
	assert.NoError(t, err, "Should be able to unmarshal search results JSON")

	assert.NotEmpty(t, searchResult.Text, "The search for 'ubuntu' should return at least one result")
	t.Logf("Search for 'ubuntu' returned %d results.", len(searchResult.Text))
}

func TestDockerAgent_ToolDiscoveryAndSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode.")
	}

	t.Run("Tool Discovery", func(t *testing.T) {
		_, _, toolset, _ := setupDockerAgentTest(t)

		// Assert that the agent has discovered tools from the MCP server.
		assert.NotEmpty(t, toolset.FunctionDeclarations, "Agent should discover tools from MCP server")
		t.Logf("Discovered %d tools from the MCP server.", len(toolset.FunctionDeclarations))

		// Optionally, check for a specific expected tool, e.g., "search"
		var searchToolFound bool
		for _, fn := range toolset.FunctionDeclarations {
			if fn.Name == dockerhubSearchToolName {
				searchToolFound = true
				break
			}
		}
		assert.True(t, searchToolFound, "The 'search' tool should be among the discovered tools")
	})

	t.Run("Search Tool Execution", func(t *testing.T) {
		_, _, toolset, agent := setupDockerAgentTest(t)
		var err error

		// Ensure the 'search' tool is available before attempting to call it.
		var searchTool *genai.FunctionDeclaration
		for _, fn := range toolset.FunctionDeclarations {
			if fn.Name == dockerhubSearchToolName {
				searchTool = fn
				break
			}
		}
		assert.NotNil(t, searchTool, "Pre-condition: The 'search' tool must be discovered for this test")

		// Execute the 'search' tool call via the agent's Handle method.
		t.Log("Executing 'search' tool call with query 'alpine'...")
		searchCall := &genai.FunctionCall{
			Name: dockerhubSearchToolName,
			Args: map[string]any{
				"query": "alpine",
			},
		}

		response := agent.Handle(searchCall)
		assert.NotNil(t, response, "Response should not be nil")
		assert.NotNil(t, response.Response, "Response.Response should not be nil")

		// The response.Response is a map[string]any, where the actual content is under the "output" key.
		outputMap := response.Response

		// The "output" value is a slice of maps, where each map contains "type" and "text".
		responseContent, ok := outputMap["output"].([]mcp.Content)
		assert.True(t, ok, "outputMap[\"output\"] should be a slice of []*genai.Part")
		assert.NotEmpty(t, responseContent, "Response content should not be empty")

		// Extract the text content from the first element.
		firstResponsePart := responseContent[0]
		responseBytes, err := firstResponsePart.MarshalJSON()
		assert.NoError(t, err, "Should be able to marshal first response part to JSON")

		// Unmarshal the JSON string to get the actual search results.
		// Unmarshal the JSON string to get the actual search results.
		var searchResult struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}

		err = json.Unmarshal(responseBytes, &searchResult)
		assert.NoError(t, err, "Should be able to unmarshal search results JSON")

		assert.NotEmpty(t, searchResult.Text, "The search for 'alpine' should return at least one result")
		t.Logf("Search for 'alpine' returned %d results.", len(searchResult.Text))
	})
}
