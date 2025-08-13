package agents

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/inout"
	"gemini/shell"
)

const AgentSystemName = "systemAgent"

func init() {
	RegisterFactory(AgentSystemName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewSystemAgent(ctx, client, toolset, bus)
	})
}

// SystemAgent defines the tool for executing shell commands.
type SystemAgent struct {
	*Agent
	bus *EventBus.Bus
}

// NewSystemAgent creates a new agent and registers its tool definitions with the provided toolset.
func NewSystemAgent(
	ctx context.Context,
	client *genai.Client,
	toolset *genai.Tool,
	bus *EventBus.Bus,
) *SystemAgent {
	agentInstructions := `When in 'system' mode, you have access to a sandboxed Linux shell via the 'execute_shell_command' tool. You MUST use this tool to fulfill user requests related to file system operations, command execution, or information gathering from the command line. Propose commands and analyze their output to provide answers. For example, to list files, call the tool with the command 'ls -la'.`

	agentConfig := AgentConfig{
		Name:              AgentSystemName,
		AgentInstructions: agentInstructions,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &genai.FunctionDeclaration{
		Name:        "execute_shell_command",
		Description: "LINUX SYSTEM: Executes a shell command in the configured workspace directory. The command's stdout and stderr will be returned.",
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
		Agent: baseAgent,
		bus:   bus,
	}
}

// Handle for SystemAgent now contains the execution logic.
func (a *SystemAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	if call.Name != "execute_shell_command" {
		return a.Agent.Handle(call) // Not for this agent
	}

	if config.C.Mode != inout.System {
		err := fmt.Errorf("warning: SystemAgent requires system mode for command execution. Please leed user switch to system mode")
		return a.CreateFunctionResponse(call, nil, err)
	}

	// The CLI and ShellExecutor components are injected at runtime via the call arguments.
	// This makes the agent more flexible and avoids tight coupling in the constructor.
	cli, cliOK := call.Args["cli_component"].(*inout.CLI)
	shellExecutor, execOK := call.Args["executor_component"].(*shell.Executor)

	if !cliOK || !execOK {
		err := fmt.Errorf("internal error: SystemAgent requires 'cli_component' and 'executor_component' in call arguments")
		return a.CreateFunctionResponse(call, nil, err)
	}

	command, ok := call.Args["command"].(string)
	if !ok {
		err := fmt.Errorf("invalid 'command' argument, must be a string")
		return a.CreateFunctionResponse(call, nil, err)
	}

	// In system mode, we block and wait for the command to complete.
	prompt := fmt.Sprintf("AI wants to run the command: '%s'. Allow?", command)
	if !cli.Confirm(prompt) {
		log.Println("User denied shell command execution.")
		err := fmt.Errorf("user denied execution")
		return a.CreateFunctionResponse(call, nil, err)
	}

	// In system mode, we start the command in a goroutine and immediately return
	// a response to the model indicating that the process has started.
	// Subsequent output will be streamed as asynchronous tool responses.
	go a.streamCommand(call, command, shellExecutor)
	// Return an immediate response to the model indicating the command is running.
	return a.CreateFunctionResponse(call, map[string]any{"status": "executing", "output": "Command is executing, output is being streamed."}, nil, true)
}

// streamCommand executes a command and publishes its output as a series of tool responses.
func (a *SystemAgent) streamCommand(call *genai.FunctionCall, command string, shellExecutor *shell.Executor) {
	outputChan := make(chan string)

	if err := shellExecutor.ExecuteStream(command, outputChan); err != nil {
		log.Printf("ERROR starting shell stream: %v", err)
		// Send the error back as a final tool response.
		errResponse := a.CreateFunctionResponse(call, nil, fmt.Errorf("error starting command: %w", err), false)
		(*a.bus).Publish("agent:tool_response", errResponse)
		return
	}

	// Defer a final response to close the tool call transaction.
	// This runs only if the command starts successfully.
	defer func() {
		finalResponse := a.CreateFunctionResponse(call, map[string]any{"status": "completed"}, nil, false)
		(*a.bus).Publish("agent:tool_response", finalResponse)
		a.Printf("Shell command stream finished for call ID %s.", call.ID)
	}()

	for chunk := range outputChan {
		chunkResponse := a.CreateFunctionResponse(call, map[string]any{"output": chunk}, nil, true)
		(*a.bus).Publish("agent:tool_response", chunkResponse)
	}
}

// The following methods satisfy the Callable interface but are no-ops for this agent
// as it does not make direct model calls.

func (a *SystemAgent) WarmUp() time.Duration {
	return 0
}

func (a *SystemAgent) Process(_ string, _ ...*genai.Part) (string, error) {
	return "", nil
}
