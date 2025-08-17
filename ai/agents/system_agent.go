package agents

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/desktop"
	"gemini/inout"
)

const AgentSystemName = "systemAgent"

func init() {
	RegisterFactory(AgentSystemName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		// Enable the SystemAgent in all modes to provide the execute_python tool.
		return NewSystemAgent(ctx, client, toolset, bus)
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
	agentInstructions := `To execute commands requiring a password (like 'sudo'), you MUST use the following secure workflow:
0. IMPORTANT! This is secure workflow! No output! No responses to user! No any other questions! Only interaction by 'get_secret_from_user' tool allowed.
1. When you see a password prompt (the secure workflow started), call 'get_secret_from_user'. Provide a 'placeholder_name' (e.g., 'sudo_password') and a 'prompt_text' for the user.
2. The tool will securely get the password from the user and confirm it's stored.
3. Once confirmed, use 'send_input_to_shell' with 'input': '{{sudo_password}}'} to submit the password.
4. For subsequent commands, you can reuse the placeholder directly, e.g., by providing a base64-encoded command string like 'submit_shell_command(command='ZWNobyAie3tzdWRvX3Bhc3N3b3JkfX0iIHwgc3VkbyAtUyBvdGhlcl9jb21tYW5k')'.
5. Continue execution of password depended command interrupted by this secure workflow (the secure workflow finished).
6. User may to enter wrong password, in this case repeat secure workflow from scratch`

	agentConfig := AgentConfig{
		Name:              AgentSystemName,
		AgentInstructions: agentInstructions,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	// --- Interactive Shell Tools ---
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &genai.FunctionDeclaration{
		Name:        "submit_shell_command",
		Description: "SYSTEM SHELL: Submits a Base64-encoded command to the user's active interactive shell for execution. The shell must be started by the user by switching to '/mode system'. The command's output will appear in the user's terminal and be added to the conversation context for you to see in the next turn.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{"command": {
				Type:        genai.TypeString,
				Description: "The Base64-encoded command to execute in the shell. A newline is automatically appended.",
			}},
			Required: []string{"command"},
		},
		Behavior: genai.BehaviorBlocking,
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
	switch call.Name {
	case "submit_shell_command", "send_input_to_shell":
		// Default agent execution flow
		// All interactive tools require system mode.
		if config.C.Mode != inout.System {
			err := fmt.Errorf("interactive shell tools require the user to be in 'system' mode. Please ask the user to switch to system mode first using the '/mode system' command")
			return a.CreateFunctionResponse(call, nil, err)
		}
		return a.handleInteractiveShell(call)
	case "get_secret_from_user":
		// Triggered critical secure flow
		return a.handleGetSecretFromUser(call)
	default:
		// Do inherited Handler. I future able common logic on skip
		return a.Agent.Handle(call)
	}
}

// handleInteractiveShell dispatches calls for the new interactive tools.
// This is only supported in LiveAI mode.
func (a *SystemAgent) handleInteractiveShell(call *genai.FunctionCall) *genai.FunctionResponse {
	// Helper function to substitute placeholders in a command string.
	substitutePlaceholders := func(text string) string {
		for placeholder, secret := range a.passwordPlaceholders {
			text = strings.ReplaceAll(text, fmt.Sprintf("{{%s}}", placeholder), secret)
		}
		return text
	}

	switch call.Name {
	case "submit_shell_command":
		encodedCommand, ok := call.Args["command"].(string)
		if !ok || encodedCommand == "" {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid 'command' argument, must be a non-empty string"))
		}
		decodedBytes, err := base64.StdEncoding.DecodeString(encodedCommand)
		if err != nil {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to decode base64 command: %w", err))
		}
		command := string(decodedBytes)
		a.Printf("Submitting shell command: %s", command)
		// Begin from 'Enter' as shell prompt have overridden by model answer
		// Append a newline to simulate the user pressing 'Enter'.
		if err := desktop.C.SendToShell("\n" + substitutePlaceholders(command) + "\n"); err != nil {
			// This will fail if the user is not in system mode, which is correct.
			return a.CreateFunctionResponse(call, nil, err)
		}
		// The output will be streamed to the user's terminal and sent to the AI
		// as context via the 'shell_output' event by the CLI.
		// This tool call is now "fire and forget". The model will see the output
		// in the subsequent context.
		return a.CreateFunctionResponse(call, map[string]any{"status": "command sent to user's shell"}, nil)

	case "send_input_to_shell":
		input, ok := call.Args["input"].(string)
		if !ok || input == "" {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid 'input' argument, must be a non-empty string"))
		}
		if err := desktop.C.SendToShell(substitutePlaceholders(input) + "\n"); err != nil {
			// This will fail if the user is not in system mode, which is correct.
			return a.CreateFunctionResponse(call, nil, err)
		}
		// This is a blocking tool. The model will wait for this response before proceeding.
		return a.CreateFunctionResponse(call, map[string]any{"status": "input sent successfully"}, nil)
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

// The following methods satisfy the Callable interface but are no-ops for this agent
// as it does not make direct model calls.

func (a *SystemAgent) WarmUp() time.Duration {
	return 0
}

func (a *SystemAgent) Process(_ string, _ ...*genai.Part) (string, error) {
	return "", nil
}
