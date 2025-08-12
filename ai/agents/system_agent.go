package agents

import (
	"context"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"
)

func init() {
	RegisterFactory(AgentSystemName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewSystemAgent(ctx, client, toolset, bus)
	})
}

const AgentSystemName = "system"

// SystemAgent defines the tool for executing shell commands.
// It does not handle the execution itself; that is done by the main AI components
// as a special case to allow for security checks and user confirmation.
type SystemAgent struct {
	*Agent
}

// NewSystemAgent creates a new agent and registers its tool definitions with the provided toolset.
func NewSystemAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *SystemAgent {
	agentInstructions := `When in 'system' mode, you have access to a sandboxed Linux shell via the 'execute_shell_command' tool. You MUST use this tool to fulfill user requests related to file system operations, command execution, or information gathering from the command line. Propose commands and analyze their output to provide answers. For example, to list files, call the tool with the command 'ls -la'.`

	agentConfig := AgentConfig{
		Name:              AgentSystemName,
		AgentInstructions: agentInstructions,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &genai.FunctionDeclaration{
		Name:        "execute_shell_command",
		Description: "Executes a shell command in the configured workspace directory. The command's stdout and stderr will be returned.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"command": {
					Type:        genai.TypeString,
					Description: "The shell command to execute (e.g., 'ls -l', 'cat file.txt').",
				},
			},
			Required: []string{"command"},
		},
	})
	return &SystemAgent{Agent: baseAgent}
}

// Handle for SystemAgent is a no-op. The tool call is handled in the main AI loops.
func (a *SystemAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	return nil
}

// The following methods satisfy the Callable interface but are no-ops for this agent
// as it does not make direct model calls.

func (a *SystemAgent) WarmUp() time.Duration {
	return 0
}

func (a *SystemAgent) Process(prompt string, parts ...*genai.Part) (string, error) {
	return "", nil
}
