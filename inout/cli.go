package inout

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/asaskevich/EventBus"
	"golang.org/x/term"

	"gemini/config"
	"gemini/desktop"
	"gemini/flow"
	"gemini/helpers"
)

const (
	Prompt    = "prompt" // {Prompt} = Allow voice and txt prompts
	VoiceMode = "voice"  // Allow voice output for AI and {Prompt}
	ImageMode = "image"  // Allow send screenshot images with each {Prompt}
	System    = "system" // System CLI integration mode. Allow execute system commands and stream output to AI
)

// ShellPauseState defines the states for pausing shell output.
type ShellPauseState string

const (
	ShellPauseStart ShellPauseState = "start"
	ShellPauseStop  ShellPauseState = "stop"
)

// promptRequest is used to pass a text prompt and receive a string response
// between the blocking PromptForInput method and the non-blocking Run loop.
type promptRequest struct {
	prompt       string
	responseChan chan string
}

var modes = map[string]string{
	Prompt:    Prompt,
	System:    System,
	VoiceMode: VoiceMode,
	ImageMode: ImageMode,
}

const (
	// Defines the input state when in system mode.
	stateProxyingToShell = iota
	stateReadingCommand
	stateReadingModalPrompt
	stateIgnoringEscapeSequence
	stateReadingPrompt // A state for the main prompt/image mode line editor.
)

// CLI handles reading user input from the command line.
type CLI struct {
	wg                  *sync.WaitGroup
	cmdChan             chan<- string
	bus                 *EventBus.Bus
	muted               bool
	isSystemShellActive bool
	aiEnabled           bool
	ready               bool
	modeMu              sync.Mutex
	mode                string
	previousMode        string
	promptChan          chan promptRequest // Receives requests for modal prompts
	activePrompt        *promptRequest     // The currently active modal prompt
	shellBuffer         strings.Builder
	shellBufferMu       sync.Mutex
	systemInputState    int
	preEscapeState      int // Remembers the state before an escape sequence
	shellPaused         bool
	systemCommandBuffer bytes.Buffer
	originalTermState   *term.State
	systemAtLineStart   bool
}

const (
	// IMPORTANT: On such terminals like KDE Konsole move down is not works without reserved next line.
	// Sequence: reserve next line for soundbar, move up, print, clear line
	promptPatern = "\n\033[A\033[1;91m%s\033[0m>\033[K"
	// Sequence: Save cursor, move to start of line, move down, clear line, print, restore cursor.
	soundbarPatern = "\0337\r\033[B\033[K[%s%s]\0338"
)

const (
	dynamic = "dynamic"
	none    = "none"
	low     = "low"
	medium  = "medium"
	high    = "high"
)

var thinkingLevels = map[string]int32{
	dynamic: -1,
	none:    0,
	low:     512,
	medium:  8192,
	high:    24576,
}

// NewCLI creates a new CLI instance.
func NewCLI(wg *sync.WaitGroup, cmdChan chan<- string, bus *EventBus.Bus, aiEnabled bool) *CLI {
	if aiEnabled {
		fmt.Println("Use keyboard to send text prompts to the AI.")
	}

	return &CLI{
		wg:                  wg,
		cmdChan:             cmdChan,
		bus:                 bus,
		muted:               true,
		isSystemShellActive: false,
		aiEnabled:           aiEnabled,
		ready:               false,
		modeMu:              sync.Mutex{},
		mode:                config.C.Mode,
		previousMode:        "",
		promptChan:          make(chan promptRequest),
		activePrompt:        nil,
		systemAtLineStart:   true,
		systemInputState:    stateProxyingToShell,
		preEscapeState:      stateProxyingToShell, // Default pre-escape state
		shellPaused:         false,
	}
}

// ReceiveShellOutput retrieves and clears the buffered shell output since the
// last call. It is safe for concurrent use.
func (c *CLI) ReceiveShellOutput() string {
	if !c.isSystemShellActive {
		return ""
	}

	c.shellBufferMu.Lock()
	defer c.shellBufferMu.Unlock()

	if c.shellPaused || c.shellBuffer.Len() == 0 {
		return ""
	}

	content := c.shellBuffer.String()
	c.shellBuffer.Reset()
	return content
}

// ReceiveShellPause starts or stops the polling of shell output.
// This is used to prevent sending shell output to the AI when it's in a
// state where it cannot process it (e.g., during a session restart).
func (c *CLI) ReceiveShellPause(state ShellPauseState) {
	c.shellBufferMu.Lock()
	defer c.shellBufferMu.Unlock()

	switch state {
	case ShellPauseStart:
		c.shellPaused = true
		log.Println("CLI system mode shell output paused")
	case ShellPauseStop:
		c.shellPaused = false
		log.Println("CLI system mode shell output released")
	default:
		log.Printf("Unknown shell pause state: %s", state)
	}
}

// PromptForInput displays a prompt to the user and waits for a line of text input.
// It's a blocking call that communicates with the main Run loop via a channel.
func (c *CLI) PromptForInput(prompt string) string {
	c.modeMu.Lock()
	isSystemMode := c.mode == System
	c.modeMu.Unlock()

	if !isSystemMode {
		log.Println("WARNING: PromptForInput called outside of system mode. This is not supported.")
		return "" // Return empty string to unblock the caller.
	}

	req := promptRequest{
		prompt:       prompt,
		responseChan: make(chan string, 1), // Buffered to prevent blocking.
	}
	c.promptChan <- req
	log.Printf("Waiting for user text input for prompt: '%s'", prompt)
	return <-req.responseChan
}

// startSystemShell starts the interactive shell for system mode.
func (c *CLI) startSystemShell() {
	if c.isSystemShellActive {
		return
	}

	// The terminal is already in raw mode, managed by the main Run() loop.

	// This channel will receive output from the interactive shell.
	outputChan := make(chan string, 100)
	go func() {
		// Exit from shell on Ctrl+D or exit command
		for line := range outputChan {
			c.shellBufferMu.Lock()
			// The PTY is already connected to the user's terminal, so it handles displaying the output.
			// We just need to capture it for the AI, not print it again.
			c.shellBuffer.WriteString(line + "\n")
			c.shellBufferMu.Unlock()
		}
		log.Println("CLI shell output publisher finished.")
		// When the shell exits, its output channel is closed. We can now safely
		// stop the system shell mode from this goroutine.
		c.stopSystemShell()
	}()
	if err := desktop.C.StartInteractiveShell(outputChan); err != nil {
		fmt.Printf("Error starting system shell: %v\n", err)
	} else {
		c.isSystemShellActive = true
		log.Println("CLI entered system mode. Interactive shell started.")
	}
}

// stopSystemShell stops the interactive shell when leaving system mode.
func (c *CLI) stopSystemShell() {
	if !c.isSystemShellActive {
		return
	}
	if err := desktop.C.StopInteractiveShell(); err != nil {
		fmt.Printf("Error stopping system shell: %v\n", err)
	}

	c.isSystemShellActive = false
	log.Println("CLI exited system mode. Interactive shell stopped.")
	if c.previousMode != "" {
		c.mode = c.previousMode
		c.previousMode = "" // Reset for the next time.
	} else {
		c.mode = Prompt // Default fallback.
	}
	// When leaving system mode, we return to the standard prompt reading state.
	c.systemInputState = stateReadingPrompt
	c.draw()
}

// handleSystemLineEditor provides a minimal line editor for raw terminal mode.
// It's used for both internal commands and modal prompts.
func (c *CLI) handleSystemLineEditor(b byte, isModal bool) {
	switch b {
	case 27: // ESC key (start of an escape sequence for arrow keys, etc.)
		c.preEscapeState = c.systemInputState
		c.systemInputState = stateIgnoringEscapeSequence
	case '\t': // Tab key
		// Explicitly do nothing to ignore it.
	case '\r', '\n': // Enter key
		fmt.Print("\r\n") // Echo newline.
		text := c.systemCommandBuffer.String()
		c.systemCommandBuffer.Reset()
		c.systemInputState = stateProxyingToShell // Always return to proxying.
		c.systemAtLineStart = true

		if isModal {
			if c.activePrompt != nil {
				c.activePrompt.responseChan <- text
				close(c.activePrompt.responseChan)
				c.activePrompt = nil
			}
			(*c.bus).Publish(config.MainTopic, "ready:cli.prompt.done")
			c.draw() // Redraw shell prompt
		} else { // It's a regular command
			if text != "" {
				c.command(text)
			} else {
				c.draw() // User typed "/" then Enter.
			}
		}
	case 127, 8: // Backspace
		if c.systemCommandBuffer.Len() > 0 {
			c.systemCommandBuffer.Truncate(c.systemCommandBuffer.Len() - 1)
			fmt.Print("\b \b") // Erase character on screen.
		}
	case 3: // Ctrl+C
		fmt.Print("^C\r\n")
		c.systemCommandBuffer.Reset()
		c.systemInputState = stateProxyingToShell
		c.systemAtLineStart = true

		if isModal {
			if c.activePrompt != nil {
				c.activePrompt.responseChan <- "" // Send empty string on abort
				close(c.activePrompt.responseChan)
				c.activePrompt = nil
			}
			(*c.bus).Publish(config.MainTopic, "ready:cli.prompt.done")
		}
		// Redraw to get a fresh shell prompt.
		c.draw()
	default:
		// Echo printable characters and add to buffer.
		if b >= 32 && b < 127 {
			c.systemCommandBuffer.WriteByte(b)
			fmt.Print(string(b))
		}
	}
}

// handlePromptLineEditor processes user input for prompt/image modes.
// Since the terminal is in raw mode, it uses a line editor.
func (c *CLI) handlePromptLineEditor(b byte) {
	switch b {
	case 27: // ESC key
		// This is the start of an escape sequence. We need to ignore the
		// subsequent bytes that form the sequence (e.g., '[A' for up arrow).
		// We switch to a temporary state to do this.
		c.preEscapeState = stateReadingPrompt
		c.systemInputState = stateIgnoringEscapeSequence
	case '\t': // Tab key
		// Explicitly do nothing to ignore it, preventing any terminal-specific behavior.
	case '\r', '\n': // Enter key
		fmt.Print("\r\n") // Echo newline.
		line := c.systemCommandBuffer.String()
		c.systemCommandBuffer.Reset()

		// The line editor gives us the raw line. We now parse it to see
		// if it's an internal command (starts with /) or a prompt for the AI.
		fullLine := strings.TrimSpace(line)
		parts := strings.Fields(fullLine)

		if len(parts) > 0 && strings.HasPrefix(parts[0], "/") {
			// It's a command for the CLI.
			parts[0] = strings.TrimPrefix(parts[0], "/")
			c.command(strings.Join(parts, " "))
		} else if fullLine != "" {
			// It's a prompt for the AI.
			c.cmdChan <- fullLine
		} else {
			// The buffer contained only whitespace. Redraw the prompt.
			c.draw()
		}
	case 127, 8: // Backspace
		if c.systemCommandBuffer.Len() > 0 {
			c.systemCommandBuffer.Truncate(c.systemCommandBuffer.Len() - 1)
			fmt.Print("\b \b") // Erase character on screen.
		}
	case 3: // Ctrl+C
		// In prompt mode, Ctrl+C should exit the application, similar to /exit.
		fmt.Print("^C\r\n")
		flow.Quit()
	default:
		// Echo printable characters and add to buffer.
		if b >= 32 && b < 127 {
			c.systemCommandBuffer.WriteByte(b)
			fmt.Print(string(b))
		}
	}
}

// processInputByte is the core of the raw mode input state machine. It processes
// a single byte of input and updates the CLI state accordingly.
func (c *CLI) processInputByte(b byte) {
	switch c.systemInputState {
	case stateIgnoringEscapeSequence:
		// Most ANSI sequences end with a letter or '~'. We wait for one to switch back.
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
			c.systemInputState = c.preEscapeState // Return to the state we were in before.
		}
		// Consume the byte and do nothing else.
		// We return immediately because this state overrides all other processing.
		return
	case stateProxyingToShell:
		// Only switch to command reading state if '/' is the first character on a new line.
		// All bytes, including control characters like Ctrl+C and Ctrl+D, are passed
		// directly to the underlying shell for true interactive behavior. The only
		// character we intercept is '/' at the beginning of a line to handle
		// internal commands.
		if b == '/' && c.systemAtLineStart {
			c.systemInputState = stateReadingCommand
			c.systemCommandBuffer.Reset()
			fmt.Print("/")              // Echo the slash to the user.
			c.systemAtLineStart = false // We've started typing the command.
		} else {
			// A printable character means we are no longer at the start of a line.
			// Control characters (like arrows, tab, ctrl+d) do not change this state.
			if b >= 32 && b < 127 {
				c.systemAtLineStart = false
			} else if b == '\r' || b == '\n' {
				c.systemAtLineStart = true // Enter marks the end of a line.
			}
			// Proxy the byte to the interactive shell.
			if err := desktop.C.SendToShell(string(b)); err != nil {
				log.Printf("Error sending input to system shell: %v", err)
			}
		}
	case stateReadingCommand:
		c.handleSystemLineEditor(b, false)
	case stateReadingModalPrompt:
		c.handleSystemLineEditor(b, true)
	case stateReadingPrompt:
		c.handlePromptLineEditor(b)
	}
}

// handleBusEvents processes events received from the main application event bus.
func (c *CLI) handleBusEvents(event string) {
	config.DebugPrintf("CLI received event: %s\n", event)

	// The shell_pause event is now handled by a direct call from LiveAI,
	// so it is no longer processed here. We parse other events.
	parts := strings.SplitN(event, ":", 2)
	command := parts[0]

	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	switch command {
	case "mute": // Normal flow
		c.muted = true
	case "draw": // Normal flow
		c.muted = false // The prompt is drawn by the main loop after this.
		c.drawLocked()  // Next prompts
	case "block": // Critical flow blocking
		c.ready = false
		c.muted = true
	case "ready": // Critical flow unblocking
		c.ready = true
		c.muted = false
		c.drawLocked() // Initial prompt
	default:
		config.DebugPrintf("CLI drop event: %s", event)
	}
}

// startStdinReader starts a goroutine to read from standard input and send the data to a channel.
func (c *CLI) startStdinReader(inputChan chan<- []byte) {
	go func() {
		defer close(inputChan)
		reader := bufio.NewReader(os.Stdin)
		buf := make([]byte, 128)
		for {
			n, err := reader.Read(buf)
			if err != nil {
				log.Printf("Stdin read error: %v", err)
				return
			}
			if n > 0 {
				// Create a new slice with the exact size of the data read.
				// This prevents a data race where the buffer could be overwritten
				// before the receiver has processed the previous chunk.
				data := make([]byte, n)
				copy(data, buf[:n])
				inputChan <- data
			}
		}
	}()
}

// Run starts the CLI input loop. It should be run in a goroutine.
func (c *CLI) Run() {
	defer close(c.cmdChan)
	defer c.wg.Done()

	if !c.aiEnabled {
		log.Println("AI is disabled, CLI input will not be processed.")
		// Block until shutdown, but don't read from stdin.
		<-*flow.GetListener()
		log.Println("CLI input handler shutting down (AI disabled).")
		return
	}

	// Put the terminal into raw mode for the entire duration of the application.
	// This gives us full control over input handling, fixing issues like the Tab key.
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		var err error
		c.originalTermState, err = term.GetState(fd)
		if err != nil {
			log.Fatalf("Failed to get terminal state: %v", err)
		}
		if _, err := term.MakeRaw(fd); err != nil {
			log.Fatalf("Failed to set terminal to raw mode: %v", err)
		}
		defer term.Restore(fd, c.originalTermState)
	}

	helpers.Verify((*c.bus).SubscribeAsync(config.MainTopic, c.handleBusEvents, false))

	// Check if the initial mode is 'system' and start the shell if so.
	if c.mode == System {
		c.startSystemShell()
		c.systemInputState = stateProxyingToShell
	} else {
		// If starting in a non-system mode, set the input state accordingly.
		c.systemInputState = stateReadingPrompt
	}

	// This goroutine reads raw bytes from stdin. It cannot be easily cancelled,
	// so it will run for the lifetime of the application. This is an acceptable
	// trade-off for achieving raw terminal I/O.
	inputChan := make(chan []byte)
	c.startStdinReader(inputChan)

	shutdownChan := flow.GetListener()
	for {
		select {
		case <-*shutdownChan: // Listens for Ctrl+C
			if c.activePrompt != nil {
				// Ensure we don't block the caller if a prompt is active during shutdown.
				c.activePrompt.responseChan <- ""
				close(c.activePrompt.responseChan)
				c.activePrompt = nil
			}
			log.Println("CLI input handler shutting down.")
			return
		case req := <-c.promptChan:
			c.activePrompt = &req
			// We know we are in system mode because of the check in PromptForInput.
			// Switch the system input handler to the modal prompt state.
			c.systemInputState = stateReadingModalPrompt
			c.systemCommandBuffer.Reset()
			// Mute the regular prompt/soundbar display and show the modal prompt.
			(*c.bus).Publish(config.MainTopic, "block:cli.prompt.start")
			fmt.Printf("\n%s: ", req.prompt)

		case inputBytes, ok := <-inputChan:
			if !ok {
				log.Println("Stdin closed, CLI input handler shutting down.")
				return
			}

			if !c.ready {
				log.Println("CLI dropping input received during blocked state.")
				continue
			}

			// Process every byte from the input chunk through the state machine.
			// This unified approach correctly handles state transitions within a single input chunk.
			for _, b := range inputBytes {
				c.processInputByte(b)
			}
		}
	}
}

func (c *CLI) draw() {
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	c.drawLocked()
}

// drawLocked performs the drawing without acquiring a lock.
// It assumes the caller already holds c.modeMu.
func (c *CLI) drawLocked() {
	if c.muted || !c.ready {
		return
	}
	// In system mode, the interactive shell provides its own prompt.
	// The CLI should not draw its own prompt or soundbar to avoid interference.
	if c.isSystemShellActive {
		// After a model response in system mode, the shell prompt might be
		// overwritten or not visible. We send a newline to the interactive
		// shell to trigger it to print a fresh prompt, ensuring the user
		// knows they can enter another command.
		if err := desktop.C.SendToShell("\n"); err != nil {
			log.Printf("Error sending newline to system shell to redraw prompt: %v", err)
		}
		return
	}
	fmt.Printf(promptPatern, c.mode) // Initial prompt
	// Publish a separate event for the sound bar AFTER the CLI prompt is printed.
	// This creates a specific drawing order and prevents a race condition
	// where the sound bar could be drawn before or over the prompt.
	(*c.bus).Publish(config.MainTopic, "show:cli.run")
}

func (c *CLI) command(cmd string) {
	c.modeMu.Lock()
	defer c.modeMu.Unlock()

	var commandName string
	log.Println("CLI command received:", cmd)
	parts := strings.Fields(cmd)

	if len(parts) == 0 {
		commandName = ""
	} else {
		commandName = parts[0]
	}

	// Internal commands
	switch commandName {
	case "save":
		(*c.bus).Publish(config.AITopic, "save:history.txt")
		fmt.Println("Conversation history save requested to history.txt.")
	case "debug":
		config.C.Debug = !config.C.Debug
		log.Printf("Debug mode set to: %t", config.C.Debug)
	case "voice":
		config.C.AI.VoiceEnabled = !config.C.AI.VoiceEnabled
		log.Printf("Voice output set to: %t", config.C.AI.VoiceEnabled)
		// In live mode, changing this requires a session restart.
		(*c.bus).Publish(config.AITopic, "restart_session:voice_toggle")
	case "tools":
		config.C.AI.EnableTools = !config.C.AI.EnableTools
		log.Printf("AI tools enabled set to: %t", config.C.AI.EnableTools)
		// In live mode, changing this requires a session restart.
		(*c.bus).Publish(config.AITopic, "restart_session:tools_toggle")
	case "transcript":
		config.C.AI.Transcript = !config.C.AI.Transcript
		log.Printf("Separate transcription step set to: %t", config.C.AI.Transcript)
		// In live mode, changing this requires a session restart.
		(*c.bus).Publish(config.AITopic, "restart_session:transcript_toggle")
	case "history":
		config.C.AI.VoiceHistory = !config.C.AI.VoiceHistory
		log.Printf("Voice history set to: %t", config.C.AI.VoiceHistory)
	case "cache":
		config.C.AI.EnableCache = !config.C.AI.EnableCache
		log.Printf("AI caching set to: %t", config.C.AI.EnableCache)
	case "thoughts":
		config.C.AI.Thoughts = !config.C.AI.Thoughts
		log.Printf("AI thoughts set to: %t", config.C.AI.Thoughts)
	case "thinking":
		hint := func() {
			fmt.Printf("Available levels: %s, %s, %s, %s, %s\n", dynamic, none, low, medium, high)
		}
		if len(parts) != 2 {
			fmt.Println("Usage: /thinking <level>")
			hint()
		} else {
			level := strings.ToLower(parts[1])
			value, ok := thinkingLevels[level]
			if !ok {
				fmt.Printf("Unknown thinking level: %s\n", level)
				hint()
			} else {
				config.C.AI.Thinking = value
				log.Printf("AI thinking budget set to: %s (%d)", level, value)
			}
		}
	case "mode":
		hint := func() {
			fmt.Printf("Available AI modes: %s, %s, %s, %s\n", Prompt, System, VoiceMode, ImageMode)
		}
		if len(parts) != 2 {
			fmt.Println("Usage: /mode <name>")
			hint()
		} else {
			mode := strings.ToLower(parts[1])
			value, ok := modes[mode]
			if !ok {
				fmt.Printf("Unknown AI mode: %s\n", mode)
				hint()
			} else {
				if value == System {
					// Store the current mode so we can return to it after exiting the shell.
					c.previousMode = c.mode
					// This will set the terminal to raw mode and start the shell.
					// The Run loop will then handle input differently.
					c.startSystemShell()
					c.systemInputState = stateProxyingToShell
				} else {
					if c.mode == System { // Switching out of system mode
						if err := desktop.C.SendToShell("exit\n"); err != nil {
							log.Printf("Error sending exit command to system shell: %v", err)
						}
					}
				}
				c.mode = value
				config.C.Mode = value
				if value != System {
					c.systemInputState = stateReadingPrompt
				}
				log.Printf("CLI mode set to: %s", value)
				(*c.bus).Publish(config.AITopic, fmt.Sprintf("mode:%s", value))
			}
		}
	case "prompt":
		// The /prompt command is only active in system mode.
		if c.mode == System {
			promptText := strings.TrimSpace(strings.Join(parts[1:], " "))
			if promptText != "" {
				c.cmdChan <- promptText
				// Expected model output, so we do not activate new prompt
			} else {
				fmt.Println("Usage: /prompt <text for AI>")
				c.draw()
			}
		}
		return // Wait for model answer, no prompt draw need
	case "exit":
		// The /exit command always terminates the application.
		flow.Quit()
		// No need in new prompt, works done
		return
	case "help":
		fmt.Println("Available commands:\r")
		fmt.Printf("/mode <name>		- Set AI mode (%s, %s, %s, %s)\r\n", Prompt, System, VoiceMode, ImageMode)
		fmt.Println("/prompt <text>		- Send a text prompt to the AI (only in 'system' mode)\r")
		fmt.Println("/debug      		- Toggle debug mode\r")
		fmt.Println("/voice      		- Toggle voice responses\r")
		fmt.Println("/tools      		- Toggle AI tools (e.g., Google Search)\r")
		fmt.Println("/transcript 		- Toggle separate transcription step for voice chat\r")
		fmt.Println("/help       		- Display this help message\r")
		fmt.Println("/exit       		- Exit the application\r")
		fmt.Println("\nPost AI Commands:\r")
		fmt.Printf("/thinking <level> 	- Set AI thinking budget (%s, %s, %s, %s, %s)\r\n", dynamic, none, low, medium, high)
		fmt.Println("/thoughts   		- Toggle AI thoughts visibility\r")
		fmt.Println("/cache      		- Toggle AI caching\r")
		fmt.Println("/save       		- Save conversation history to history.txt\r")
		fmt.Println("/history    		- Toggle including voice prompts in conversation history\r")
	default:
		fmt.Printf("Unknown command: %s\n", commandName)
	}
	c.drawLocked()
}
