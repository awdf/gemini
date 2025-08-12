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
		Agent: baseAgent,
		bus:   bus,
	}
}

// Handle for SystemAgent now contains the execution logic.
func (a *SystemAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	if call.Name != "execute_shell_command" {
		return nil // Not for this agent
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

	// The logic for LiveAI vs. PostAI is now handled by the caller (LiveAI or AI).
	// LiveAI will call streamCommand, and PostAI will call Execute.
	// This agent now only needs to decide how to execute based on the mode.
	if config.C.LiveAI {
		// In live mode, we start the command and stream its output via the event bus.
		// The main LiveAI loop will pick up the text chunks and send them to the model.
		go a.streamCommand(command, cli, shellExecutor)
		// Return an immediate response to the model indicating the command is running.
		return a.CreateFunctionResponse(call, map[string]any{"status": "executing", "output": "Command is executing, output is being streamed."}, nil)
	}

	// In PostAI mode, we block and wait for the command to complete.
	prompt := fmt.Sprintf("AI wants to run the command: '%s'. Allow?", command)
	if !cli.Confirm(prompt) {
		log.Println("User denied shell command execution.")
		err := fmt.Errorf("user denied execution")
		return a.CreateFunctionResponse(call, nil, err)
	}

	output, err := shellExecutor.Execute(command)
	responseMap := map[string]any{"output": output}
	if err != nil {
		// Include the exit error in the response to the model.
		responseMap["error"] = err.Error()
	}

	return a.CreateFunctionResponse(call, responseMap, nil)
}

// streamCommand executes a command and publishes its output to the event bus.
func (a *SystemAgent) streamCommand(command string, cli *inout.CLI, shellExecutor *shell.Executor) {
	// We need user confirmation even for streaming.
	prompt := fmt.Sprintf("AI wants to run the command: '%s'. Allow?", command)
	if !cli.Confirm(prompt) {
		log.Println("User denied shell command execution.")
		(*a.bus).Publish("live:stream_text", "[User denied execution]")
		return
	}

	outputChan := make(chan string)
	if err := shellExecutor.ExecuteStream(command, outputChan); err != nil {
		log.Printf("ERROR starting shell stream: %v", err)
		// Also send the error to the model via the stream.
		(*a.bus).Publish("live:stream_text", fmt.Sprintf("[Error starting command: %v]", err))
		return
	}

	for chunk := range outputChan {
		(*a.bus).Publish("live:stream_text", chunk)
	}
}

// The following methods satisfy the Callable interface but are no-ops for this agent
// as it does not make direct model calls.

func (a *SystemAgent) WarmUp() time.Duration {
	return 0
}

func (a *SystemAgent) Process(prompt string, parts ...*genai.Part) (string, error) {
	return "", nil
}
