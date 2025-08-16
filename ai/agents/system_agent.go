package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	outputChan           chan string
	reportTo             *genai.FunctionCall
	mu                   sync.Mutex
}

// NewSystemAgent creates a new agent and registers its tool definitions with the provided toolset.
func NewSystemAgent(
	ctx context.Context,
	client *genai.Client,
	toolset *genai.Tool,
	bus *EventBus.Bus,
) *SystemAgent {
	agentInstructions := `You have access to a sandboxed Linux shell. To use it, you MUST follow this sequence:
1. Call 'start_interactive_shell' to begin a session.
2. Call 'execute_in_shell' one or more times to run commands. The output will be streamed back to you(can be any length). You MUST be silent until 'execute_in_shell' (one command or more) execution will complete.
3. If a command requires input (like a password or confirmation), use 'send_input_to_shell'.
4. When you are finished, you MUST call 'stop_interactive_shell' to clean up the session and report to user execution status.

To execute commands requiring a password (like 'sudo'), you MUST use the following secure workflow:
0. IMPORTANT! This is secure workflow! No output! No responses to user! No any other questions! Only interaction by 'get_secret_from_user' tool allowed.
1. When you see a password prompt (the secure workflow started), call the 'get_secret_from_user' tool. Provide a 'placeholder_name' (e.g., 'sudo_password') and a 'prompt_text' for the user.
2. The tool will securely get the password from the user and confirm it's stored.
3. Once confirmed, use the 'send_input_to_shell' tool with the placeholder you created (e.g., '{{sudo_password}}') to submit the password.
4. For subsequent commands, you can reuse the placeholder directly, e.g., 'echo "{{sudo_password}}" | sudo -S other_command'.
5. Continue execution of password depended command interrupted by this secure workflow (the secure workflow finished).
6. User may to enter wrong password, in this case repeat secure workflow from scratch`

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
		Description: "SYSTEM SHELL: Executes a command in the active interactive shell and returns immediately. The command's output will be streamed back asynchronously. Please wait for shell prompt to be shure that command have been executed.",
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
	switch call.Name {
	case "start_interactive_shell", "execute_in_shell", "send_input_to_shell", "stop_interactive_shell":
		// Default agent execution flow
	case "get_secret_from_user":
		// Triggered critical secure flow
		return a.handleGetSecretFromUser(call)
	default:
		// Do inherited Handler. I future able common logic on skip
		return a.Agent.Handle(call)
	}

	// All interactive tools require system mode.
	if config.C.Mode != inout.System {
		err := fmt.Errorf("interactive shell tools require 'system' mode. Please ask the user to switch to system mode first using the '/mode system' command")
		return a.CreateFunctionResponse(call, nil, err)
	}

	return a.handleInteractiveShell(call)
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

	switch call.Name {
	case "start_interactive_shell":
		a.outputChan = make(chan string, 1000)
		if err := shellExecutor.StartInteractive(a.outputChan); err != nil {
			return a.CreateFunctionResponse(call, nil, err)
		}
		// Return an immediate, non-blocking response to the model.
		return a.CreateFunctionResponse(call, map[string]any{"status": "interactive shell started"}, nil)

	case "execute_in_shell":
		if a.outputChan == nil {
			err := fmt.Errorf("no active interactive shell. You must call 'start_interactive_shell' first")
			return a.CreateFunctionResponse(call, nil, err, false)
		}
		command, ok := call.Args["command"].(string)
		if !ok || command == "" {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid 'command' argument, must be a non-empty string"), false)
		}
		// Start a goroutine to stream the shell's output back to the model.
		go a.streamOutput(call, a.outputChan)
		// Append a newline to simulate the user pressing 'Enter'.
		if err := shellExecutor.SendInput(substitutePlaceholders(command) + "\n"); err != nil {
			return a.CreateFunctionResponse(call, nil, err)
		}
		// This is a non-blocking tool. We return an intermediate response to acknowledge the command was sent.
		return a.CreateFunctionResponse(call, map[string]any{"status": "command sent successfully"}, nil, true)

	case "send_input_to_shell":
		if a.outputChan == nil {
			err := fmt.Errorf("no active interactive shell. You must call 'start_interactive_shell' first")
			return a.CreateFunctionResponse(call, nil, err, false)
		}
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
		if a.outputChan == nil {
			err := fmt.Errorf("no active interactive shell to stop. You must call 'start_interactive_shell' first")
			return a.CreateFunctionResponse(call, nil, err, false)
		}
		if err := shellExecutor.StopInteractive(); err != nil {
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
	a.mu.Lock()
	if a.reportTo != nil {
		a.reportTo = call
		a.mu.Unlock()
		return
	}

	// Set first call to report
	a.reportTo = call
	a.mu.Unlock()

	// The final response signals the end of the tool call. It's sent when the channel is closed.
	defer func() {
		a.mu.Lock()
		// When the stream ends, we need to get the final call to report to,
		// and then reset the agent's state for the next session.
		reportCall := a.reportTo
		a.reportTo = nil
		a.mu.Unlock()

		if reportCall != nil {
			finalResponse := a.CreateFunctionResponse(reportCall, map[string]any{"status": "completed"}, nil, false)
			(*a.bus).Publish(config.AgentTopic, finalResponse)
			a.Printf("Interactive shell stream finished for call ID %s.", reportCall.ID)
		}
	}()

	// Batch output to avoid overwhelming the model with too many intermediate responses.
	ticker := time.NewTicker(250 * time.Millisecond) // Send updates every 250ms
	defer ticker.Stop()
	var outputBatch strings.Builder

	for {
		select {
		case chunk, ok := <-outputChan:
			if !ok { // Channel closed
				// Send any remaining output before finishing.
				if outputBatch.Len() > 0 {
					a.mu.Lock()
					reportCall := a.reportTo
					a.mu.Unlock()
					if reportCall != nil {
						// This is the last chunk of output, but not the final response for the tool call.
						// The final response is sent in the defer block.
						chunkResponse := a.CreateFunctionResponse(reportCall, map[string]any{"output": outputBatch.String()}, nil, true)
						(*a.bus).Publish(config.AgentTopic, chunkResponse)
					}
				}
				return // Exit the goroutine
			}
			outputBatch.WriteString(chunk + "\n") // The outputChan sends line by line.

		case <-ticker.C:
			if outputBatch.Len() > 0 {
				a.mu.Lock()
				reportCall := a.reportTo
				a.mu.Unlock()
				if reportCall != nil {
					chunkResponse := a.CreateFunctionResponse(reportCall, map[string]any{"output": outputBatch.String()}, nil, true)
					(*a.bus).Publish(config.AgentTopic, chunkResponse)
				}
				outputBatch.Reset()
			}
		}
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
