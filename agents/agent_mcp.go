package agents

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

const mask = "_"

type MCPAgent struct {
	*Agent
	mcpConfig McpServerConfig
	functions map[string]*genai.FunctionDeclaration // A map to quickly look up discovered functions.
	cs        *mcp.ClientSession                    // The active client session to the MCP server.
	bus       *EventBus.Bus                         // The event bus for asynchronous communication.
}

func NewMCPAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus, name string, config McpServerConfig) *MCPAgent {
	// Create the base agent first, so we can use its logging methods.
	baseAgent := NewAgent(ctx, client, AgentConfig{
		Name: name,
	})

	mcpAgent := &MCPAgent{
		Agent:     baseAgent,
		bus:       bus,
		functions: make(map[string]*genai.FunctionDeclaration),
		mcpConfig: config,
	}

	if mcpAgent.mcpConfig.Command == "" {
		mcpAgent.Println("WARNING: Could not create MCP agent, MCP server command is not configured.")
		return nil
	}

	// Discover tools from the MCP server
	if err := mcpAgent.discoverTools(ctx, toolset); err != nil {
		mcpAgent.Printf("WARNING: Could not discover MCP tools: %v", err)
		return nil
	}

	mcpAgent.Println("Initialized successfully.")
	return mcpAgent
}

func (a *MCPAgent) discoverTools(ctx context.Context, toolset *genai.Tool) error {
	cmd := exec.Command(a.mcpConfig.Command, a.mcpConfig.Args...)
	client := mcp.NewClient(&mcp.Implementation{Name: a.name, Version: "v1.0.0"}, nil)
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
			Name:        a.name + mask + tool.Name,
			Description: tool.Description,
			Parameters:  convertSchema(tool.InputSchema),
		}
		a.functions[fn.Name] = fn
		toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, fn)
	}

	return nil
}

func convertSchema(js *jsonschema.Schema) *genai.Schema {
	// Schema: https://json-schema.org/draft-07/schema

	if js == nil {
		return nil
	}

	// If MCP Schema don't have type, we must set unknown
	var gType genai.Type = genai.TypeUnspecified
	if len(js.Type) > 0 {
		gType = genai.Type(strings.ToUpper(js.Type))
	} else if js.Not != nil {
		// Because genai.Schema doesn't have a Not(exclude) field, function currently ignores it
		gType = genai.TypeNULL
	}

	gs := &genai.Schema{
		Type:        gType,
		Description: js.Description,
		Properties:  make(map[string]*genai.Schema),
		Required:    js.Required,
	}

	if js.AnyOf != nil {
		gs.AnyOf = make([]*genai.Schema, len(js.AnyOf))
		for i, anyOf := range js.AnyOf {
			gs.AnyOf[i] = convertSchema(anyOf)
		}
	}

	if js.Enum != nil {
		// The jsonschema Enum is []any, but genai.Schema expects []string.
		// We must convert each element.
		gs.Enum = make([]string, 0, len(js.Enum))
		for _, v := range js.Enum {
			gs.Enum = append(gs.Enum, fmt.Sprint(v))
		}
	}

	if js.Items != nil {
		gs.Items = convertSchema(js.Items)
	}

	for key, val := range js.Properties {
		// Prefix parameter names to avoid conflicts with reserved keywords.
		prefixedKey := mask + key
		gs.Properties[prefixedKey] = convertSchema(val)
	}

	for i, req := range gs.Required {
		gs.Required[i] = mask + req
	}

	return gs
}

func (a *MCPAgent) WarmUp() time.Duration {
	return 0
}

func (a *MCPAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	if _, ok := a.functions[call.Name]; ok {
		return a.executeMcpCommand(call)
	}
	return nil
}

func (a *MCPAgent) executeMcpCommand(call *genai.FunctionCall) *genai.FunctionResponse {
	// Normalize the tool name by removing the agent name prefix
	normalizedName := call.Name[len(a.name)+len(mask):]

	normalizedArgs := make(map[string]any)
	for key, value := range call.Args {
		// Remove the underscore prefix we added during discovery.
		normalizedArgs[strings.TrimPrefix(key, mask)] = value
	}

	params := &mcp.CallToolParams{
		Name:      normalizedName,
		Arguments: normalizedArgs,
	}

	result, err := a.cs.CallTool(context.Background(), params)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	// The result from CallTool is a struct. We only care about the Content map.
	a.Printf("Tool call '%s' result content: %v", call.Name, result.Content)
	return a.CreateFunctionResponse(call, result.Content, nil)
}
