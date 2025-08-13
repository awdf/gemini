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

	behavior := genai.BehaviorBlocking
	if config.C.LiveAI {
		behavior = genai.BehaviorNonBlocking
	}

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
		Behavior: behavior,
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

	// IMPORTANT! Works only in system mode
	if config.C.Mode != inout.System {
		err := fmt.Errorf("warning: SystemAgent requires system mode for command execution. Please leed user switch to system mode")
		return a.CreateFunctionResponse(call, nil, err)
	}

	// The CLI and ShellExecutor components are injected at runtime via the call arguments.
	// This makes the agent more flexible and avoids tight coupling in the constructor.
	_, cliOK := call.Args["cli_component"].(*inout.CLI)
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

	// TODO: It is secure. But really annoying. Solve do i need it or don't?
	// // In system mode, we block and wait for the command to complete.
	// prompt := fmt.Sprintf("AI wants to run the command: '%s'. Allow?", command)
	// if !cli.Confirm(prompt) {
	// 	log.Println("User denied shell command execution.")
	// 	return a.CreateFunctionResponse(call, nil, fmt.Errorf("user denied execution"))
	// }

	if config.C.LiveAI {
		go a.streamCommandLive(call, command, shellExecutor)
		return a.CreateFunctionResponse(call, map[string]any{"status": "executing", "output": "Command is executing, output is being streamed."}, nil, true)
	}

	// PostAI path is non-interactive.
	return a.executeCommandPost(call, command, shellExecutor)
}

// executeCommandPost handles command execution for the non-interactive PostAI mode.
// It runs the command synchronously and returns the full output in a single response.
func (a *SystemAgent) executeCommandPost(call *genai.FunctionCall, command string, shellExecutor *shell.Executor) *genai.FunctionResponse {
	a.Printf("Executing command in PostAI mode: %s", command)
	output, err := shellExecutor.Execute(command)
	if err != nil {
		// The error from Execute often includes the command's output, so we return both.
		responseMap := map[string]any{"output": output, "error": err.Error()}
		return a.CreateFunctionResponse(call, responseMap, nil)
	}
	responseMap := map[string]any{"output": output, "status": "completed"}
	return a.CreateFunctionResponse(call, responseMap, nil)
}

// streamCommandLive handles command execution for the interactive LiveAI mode.
// It streams the command's output as a series of asynchronous tool responses.
func (a *SystemAgent) streamCommandLive(call *genai.FunctionCall, command string, shellExecutor *shell.Executor) {
	outputChan := make(chan string)

	if err := shellExecutor.ExecuteStream(command, outputChan); err != nil {
		log.Printf("ERROR starting shell stream: %v", err)
		// Send the error back as a final tool response.
		finalResponse := a.CreateFunctionResponse(call, nil, fmt.Errorf("error starting command: %w", err), false)
		(*a.bus).Publish(config.AgentTopic, finalResponse)
		return
	}

	// Defer the final response which will contain the full output.
	defer func() {
		// The final response contains the full buffered output to ensure the model has
		// complete context for follow-up questions. It also signals the end of the tool call.
		finalResponse := a.CreateFunctionResponse(call, map[string]any{"status": "completed"}, nil, false)
		(*a.bus).Publish(config.AgentTopic, finalResponse)
		a.Printf("Shell command stream finished for call ID %s.", call.ID)
	}()

	// Stream intermediate chunks and buffer them for the final response.
	for chunk := range outputChan {
		// Send intermediate chunks to the model. This provides live feedback but the model
		// may not retain the full context from these chunks.
		chunkResponse := a.CreateFunctionResponse(call, map[string]any{"output": chunk}, nil, true)
		(*a.bus).Publish(config.AgentTopic, chunkResponse)
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
