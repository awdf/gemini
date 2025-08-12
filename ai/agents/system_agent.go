package agents

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"

	"gemini/inout"
	"gemini/shell"
)

const AgentSystemName = "system"

// SystemAgent defines the tool for executing shell commands.
type SystemAgent struct {
	*Agent
	shellExecutor *shell.Executor
	cli           *inout.CLI
}

// NewSystemAgent creates a new agent and registers its tool definitions with the provided toolset.
func NewSystemAgent(
	ctx context.Context,
	client *genai.Client,
	toolset *genai.Tool,
	bus *EventBus.Bus,
	shellExecutor *shell.Executor,
	cli *inout.CLI,
) *SystemAgent {
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
	return &SystemAgent{
		Agent:         baseAgent,
		shellExecutor: shellExecutor,
		cli:           cli,
	}
}

// Handle for SystemAgent now contains the execution logic.
func (a *SystemAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	if call.Name != "execute_shell_command" {
		return nil // Not for this agent
	}

	command, ok := call.Args["command"].(string)
	if !ok {
		err := fmt.Errorf("invalid 'command' argument, must be a string")
		return a.CreateFunctionResponse(call, nil, err)
	}

	prompt := fmt.Sprintf("AI wants to run the command: '%s'. Allow?", command)
	if !a.cli.Confirm(prompt) {
		log.Println("User denied shell command execution.")
		err := fmt.Errorf("user denied execution")
		return a.CreateFunctionResponse(call, nil, err)
	}

	output, err := a.shellExecutor.Execute(command)
	responseMap := map[string]any{"output": output}
	if err != nil {
		// Include the exit error in the response to the model.
		responseMap["error"] = err.Error()
	}

	// The CreateFunctionResponse helper will log the result and format the response correctly.
	// We pass nil for the error because we've already packaged it into the response map.
	return a.CreateFunctionResponse(call, responseMap, nil)
}

// The following methods satisfy the Callable interface but are no-ops for this agent
// as it does not make direct model calls.

func (a *SystemAgent) WarmUp() time.Duration {
	return 0
}

func (a *SystemAgent) Process(prompt string, parts ...*genai.Part) (string, error) {
	return "", nil
}
