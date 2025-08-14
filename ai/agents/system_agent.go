package agents

import (
	"context"
	"fmt"
	"strings"
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
		// Decided not use system agent in post ai as it takes too long and absolutely not convenient.
		if config.C.LiveAI {
			return NewSystemAgent(ctx, client, toolset, bus)
		}
		return nil
	})
}

// SystemAgent defines the tool for executing shell commands.
type SystemAgent struct {
	*Agent
	bus                  *EventBus.Bus
	passwordPlaceholders map[string]string
}

// NewSystemAgent creates a new agent and registers its tool definitions with the provided toolset.
func NewSystemAgent(
	ctx context.Context,
	client *genai.Client,
	toolset *genai.Tool,
	bus *EventBus.Bus,
) *SystemAgent {
	agentInstructions := `You have access to a sandboxed Linux shell.
To execute commands requiring a password (like 'sudo'), you MUST use the following secure workflow:
1. When you see a password prompt, call the 'get_secret_from_user' tool. Provide a 'placeholder_name' (e.g., 'sudo_password') and a 'prompt_text' for the user.
2. The tool will securely get the password from the user and confirm it's stored.
3. Once confirmed, use the 'send_input_to_shell' tool with the placeholder you created (e.g., '{{sudo_password}}') to submit the password.
4. For subsequent commands, you can reuse the placeholder directly, e.g., 'echo "{{sudo_password}}" | sudo -S other_command'.`

	agentConfig := AgentConfig{
		Name:              AgentSystemName,
		AgentInstructions: agentInstructions,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	// --- Interactive Shell Tools ---
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &genai.FunctionDeclaration{
		Name:        "start_interactive_shell",
		Description: "SYSTEM SHELL: Starts a persistent, stateful, interactive shell session. Output will be streamed back. Use 'execute_in_shell' to run commands and 'send_input_to_shell' to provide input (like passwords).",
		Behavior:    genai.BehaviorBlocking, // This tool returns immediately while the shell runs.
	}, &genai.FunctionDeclaration{
		Name:        "execute_in_shell",
		Description: "SYSTEM SHELL: Executes a command in the active interactive shell and returns immediately. The command's output will be streamed back asynchronously.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{"command": {Type: genai.TypeString, Description: "The command to execute in the shell. A newline is automatically appended."}},
			Required:   []string{"command"},
		},
		Behavior: genai.BehaviorNonBlocking,
	}, &genai.FunctionDeclaration{
		Name:        "send_input_to_shell",
		Description: "SYSTEM SHELL: Sends a line of text to the active interactive shell's standard input. Use this to respond to prompts like passwords or confirmations.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{"input": {Type: genai.TypeString, Description: "The text to send to the shell's stdin. A newline is automatically appended."}},
			Required:   []string{"input"},
		},
		Behavior: genai.BehaviorBlocking,
	}, &genai.FunctionDeclaration{
		Name:        "stop_interactive_shell",
		Description: "SYSTEM SHELL: Stops the currently active interactive shell session and cleans up its resources.",
		Behavior:    genai.BehaviorBlocking,
	}, &genai.FunctionDeclaration{
		Name:        "get_secret_from_user",
		Description: "SYSTEM SHELL: Prompts the human user for a secret (like a password) and stores it behind a placeholder name for later use. If the secret for a given placeholder name already exists, it will not prompt the user again.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"placeholder_name": {Type: genai.TypeString, Description: "A descriptive name for the secret (e.g., 'sudo_password'). This will be used to create the placeholder like '{{sudo_password}}'."},
				"prompt_text":      {Type: genai.TypeString, Description: "The text to display to the user when asking for the secret (e.g., 'Please enter the sudo password')."},
			},
			Required: []string{"placeholder_name", "prompt_text"},
		},
		Behavior: genai.BehaviorBlocking,
	})

	return &SystemAgent{
		Agent:                baseAgent,
		bus:                  bus,
		passwordPlaceholders: make(map[string]string),
	}
}

// Handle for SystemAgent now contains the execution logic.
func (a *SystemAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	// All interactive tools require system mode.
	if config.C.Mode != inout.System {
		err := fmt.Errorf("interactive shell tools require 'system' mode. Please ask the user to switch to system mode first using the '/mode system' command")
		return a.CreateFunctionResponse(call, nil, err)
	}

	switch call.Name {
	case "start_interactive_shell", "execute_in_shell", "send_input_to_shell", "stop_interactive_shell":
		return a.handleInteractiveShell(call)
	case "get_secret_from_user":
		return a.handleGetSecretFromUser(call)
	default:
	}

	// Do inherited Handler. I future able common logic on skip
	return a.Agent.Handle(call)
}

// handleInteractiveShell dispatches calls for the new interactive tools.
// This is only supported in LiveAI mode.
func (a *SystemAgent) handleInteractiveShell(call *genai.FunctionCall) *genai.FunctionResponse {
	shellExecutor, execOK := call.Args["executor_component"].(*shell.Executor)
	if !execOK {
		err := fmt.Errorf("internal error: SystemAgent requires executor component")
		return a.CreateFunctionResponse(call, nil, err)
	}

	// Helper function to substitute placeholders in a command string.
	substitutePlaceholders := func(text string) string {
		for placeholder, secret := range a.passwordPlaceholders {
			text = strings.ReplaceAll(text, fmt.Sprintf("{{%s}}", placeholder), secret)
		}
		return text
	}

	outputChan := make(chan string)
	switch call.Name {
	case "start_interactive_shell":
		if err := shellExecutor.StartInteractive(outputChan); err != nil {
			return a.CreateFunctionResponse(call, nil, err)
		}
		// Return an immediate, non-blocking response to the model.
		return a.CreateFunctionResponse(call, map[string]any{"status": "interactive shell started"}, nil)

	case "execute_in_shell":
		command, ok := call.Args["command"].(string)
		if !ok || command == "" {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid 'command' argument, must be a non-empty string"))
		}
		// Start a goroutine to stream the shell's output back to the model.
		go a.streamOutput(call, outputChan)
		// Append a newline to simulate the user pressing 'Enter'.
		if err := shellExecutor.SendInput(substitutePlaceholders(command) + "\n"); err != nil {
			return a.CreateFunctionResponse(call, nil, err)
		}
		// This is a non-blocking tool. We return an intermediate response to acknowledge the command was sent.
		return a.CreateFunctionResponse(call, map[string]any{"status": "command sent successfully"}, nil, true)

	case "send_input_to_shell":
		input, ok := call.Args["input"].(string)
		if !ok || input == "" {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid 'input' argument, must be a non-empty string"))
		}
		if err := shellExecutor.SendInput(substitutePlaceholders(input) + "\n"); err != nil {
			return a.CreateFunctionResponse(call, nil, err)
		}
		// This is a blocking tool. The model will wait for this response before proceeding.
		return a.CreateFunctionResponse(call, map[string]any{"status": "input sent successfully"}, nil)

	case "stop_interactive_shell":
		if err := shellExecutor.StopInteractive(outputChan); err != nil {
			return a.CreateFunctionResponse(call, nil, err)
		}
		// This is a blocking tool call that also terminates the non-blocking 'start_interactive_shell' call.
		return a.CreateFunctionResponse(call, map[string]any{"status": "interactive shell stopped"}, nil)
	}
	return a.CreateFunctionResponse(call, nil, fmt.Errorf("unknown interactive tool: %s", call.Name))
}

// handleGetSecretFromUser securely prompts the user for input and stores it in a placeholder.
func (a *SystemAgent) handleGetSecretFromUser(call *genai.FunctionCall) *genai.FunctionResponse {
	cli, cliOK := call.Args["cli_component"].(*inout.CLI)
	if !cliOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("internal error: get_secret_from_user requires cli component"))
	}

	placeholderName, nameOK := call.Args["placeholder_name"].(string)
	promptText, promptOK := call.Args["prompt_text"].(string)
	if !nameOK || !promptOK || placeholderName == "" || promptText == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("arguments 'placeholder_name' and 'prompt_text' are required and must be non-empty strings"))
	}

	// Check if the placeholder already has a value.
	if _, exists := a.passwordPlaceholders[placeholderName]; exists {
		a.Printf("Secret for placeholder '%s' already exists. Skipping user prompt.", placeholderName)
		result := map[string]any{"status": fmt.Sprintf("Secret for placeholder '%s' is already available. You can now use '{{%s}}'.", placeholderName, placeholderName)}
		return a.CreateFunctionResponse(call, result, nil)
	}

	// Securely prompt the user for the password.
	password := cli.PromptForInput(promptText)
	a.passwordPlaceholders[placeholderName] = password

	a.Printf("Secret for placeholder '%s' has been set.", placeholderName)
	result := map[string]any{"status": fmt.Sprintf("Secret for placeholder '%s' has been set. You can now use '{{%s}}' in other tools.", placeholderName, placeholderName)}
	return a.CreateFunctionResponse(call, result, nil)
}

// streamOutput is a helper goroutine that reads from a channel and streams the
// content back to the model as a series of non-blocking tool responses.
func (a *SystemAgent) streamOutput(call *genai.FunctionCall, outputChan <-chan string) {
	// The final response signals the end of the tool call. It's sent when the channel is closed.
	defer func() {
		finalResponse := a.CreateFunctionResponse(call, map[string]any{"status": "completed"}, nil, false)
		(*a.bus).Publish(config.AgentTopic, finalResponse)
		a.Printf("Interactive shell stream finished for call ID %s.", call.ID)
	}()

	for chunk := range outputChan {
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
