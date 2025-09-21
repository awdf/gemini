package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"gemini/inout"
)

var (
	execCommand = exec.Command
	userCurrent = user.Current
)

const (
	AgentDockerName = "dockerAgent"
	clientName      = "dockerhub"
)

type McpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

type McpSettings struct {
	McpServers map[string]McpServerConfig `json:"mcpServers"`
}

func init() {
	return
	// disabled for now, schema generation is not working well with the mcp package

	RegisterFactory(AgentDockerName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewDockerAgent(ctx, client, toolset, bus)
	})
}

type DockerAgent struct {
	*Agent
	mcpConfig McpServerConfig
	functions map[string]*genai.FunctionDeclaration
	formatter *inout.Formatter
	cs        *mcp.ClientSession
	bus       *EventBus.Bus
}

func NewDockerAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *DockerAgent {
	formatter := inout.NewFormatter()
	dockerAgent := &DockerAgent{
		formatter: formatter,
		bus:       bus,
	}

	usr, err := userCurrent()
	if err != nil {
		formatter.Printlnf("WARNING: Could not get current user for MCP settings: %v", err)
		return nil
	}
	settingsPath := filepath.Join(usr.HomeDir, ".gemini", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		formatter.Printlnf("WARNING: Could not read MCP settings file at %s: %v", settingsPath, err)
		return nil
	}

	var settings McpSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		formatter.Printlnf("WARNING: Could not parse MCP settings file at %s: %v", settingsPath, err)
		return nil
	}

	mcpConfig, ok := settings.McpServers[clientName]
	if !ok {
		formatter.Printlnf("WARNING: Could not find '%s' configuration in MCP settings.", clientName)
		return nil
	}

	if mcpConfig.Command == "" {
		formatter.Println("WARNING: Could not create Docker agent, MCP server command is not configured.")
		return nil
	}

	baseAgent := NewAgent(ctx, client, AgentConfig{
		Name: AgentDockerName,
	})

	dockerAgent.Agent = baseAgent
	dockerAgent.mcpConfig = mcpConfig
	dockerAgent.functions = make(map[string]*genai.FunctionDeclaration)

	// Discover tools from the MCP server
	if err := dockerAgent.discoverTools(ctx, toolset); err != nil {
		formatter.Printlnf("WARNING: Could not discover Docker tools: %v", err)
		return nil
	}

	dockerAgent.Println("Initialized successfully.")
	return dockerAgent
}

func (a *DockerAgent) discoverTools(ctx context.Context, toolset *genai.Tool) error {
	cmd := exec.Command(a.mcpConfig.Command, a.mcpConfig.Args...)
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "v1.0.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	a.cs = cs

	tools := cs.Tools(ctx, nil)
	for tool, err := range tools {
		if err != nil {
			return err
		}
		a.Printf("Discovered tool: %s", tool.Name)
		fn := &genai.FunctionDeclaration{
			Name:        clientName + "." + tool.Name,
			Description: tool.Description,
			Parameters:  convertSchema(tool.InputSchema),
		}
		a.functions[fn.Name] = fn
		toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, fn)
	}

	return nil
}

func convertSchema(js *jsonschema.Schema) *genai.Schema {
	if js == nil {
		return nil
	}

	gs := &genai.Schema{
		Type:        genai.Type(js.Type),
		Description: js.Description,
		Properties:  make(map[string]*genai.Schema),
		Required:    js.Required,
	}

	if js.Items != nil {
		gs.Items = convertSchema(js.Items)
	}

	for key, val := range js.Properties {
		gs.Properties[key] = convertSchema(val)
	}

	return gs
}

func (a *DockerAgent) WarmUp() time.Duration {
	return 0
}

func (a *DockerAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	if _, ok := a.functions[call.Name]; ok {
		return a.executeMcpCommand(call)
	}
	return nil
}

func (a *DockerAgent) executeMcpCommand(call *genai.FunctionCall) *genai.FunctionResponse {
	// Normalize the tool name by removing the "dockerhub." prefix
	normalizedName := call.Name[len(clientName)+1:]

	params := &mcp.CallToolParams{
		Name:      normalizedName,
		Arguments: call.Args,
	}

	result, err := a.cs.CallTool(context.Background(), params)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	// The result from CallTool is a struct. We only care about the Content map.
	a.formatter.Printlnf("Tool call '%s' result content: %v", call.Name, result.Content)
	return a.CreateFunctionResponse(call, result.Content, nil)
}
