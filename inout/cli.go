package inout

/*
Package inout - CLI Component Design Notes for Future Visits

This file implements a complex Command-Line Interface (CLI) with several key
design decisions made to handle concurrency, state management, and different
input modes robustly. These notes summarize the resolutions to challenges
encountered during development to prevent re-litigating them.

1.  Dual-Mode Input Handling:
    The CLI supports two fundamentally different modes: 'system' (for raw shell
    proxying) and 'prompt' (for rich line editing).
    - Resolution: The main Run() function acts as a dispatcher, switching
      between two dedicated loops: runSystemModeLoop() and runPromptModeLoop().
      This cleanly separates the logic for each mode.

2.  Handling Sensitive Input (e.g., Passwords):
    The PromptForInput() function must read sensitive data in 'system' mode
    without echoing it to the screen and without freezing the application.
    - Challenge: A simple blocking call like term.ReadPassword() would deadlock
      the event loop, making the app unresponsive to shutdown signals.
    - Resolution: A custom, non-blocking password editor (handlePasswordEditor)
      is implemented. The system loop enters a special 'stateReadingPassword'
      where it processes input bytes without echoing them, ensuring the main
      event loop remains active and responsive.

3.  Startup Synchronization:
    The CLI must not attempt to read user input until the entire application,
    including external components like VAD, is ready.
    - Challenge: A race condition caused the input loop to start and block
      before the initial prompt could be drawn.
    - Resolution: The main Run() loop polls a 'c.ready' flag and waits until
      it is set by a system-wide 'ready' event from the event bus. This
      ensures the prompt is drawn before the first ReadLine() call.

4.  Command Dispatching for Readability:
    The original command handler was a large, monolithic switch statement that
    was difficult to read and maintain.
    - Resolution: The logic was refactored into a dispatcher pattern. A map
      (commandHandlers) routes command strings to small, single-purpose
      handler functions (e.g., handleMode, handleHelp), dramatically
      improving code clarity and testability.

5.  Communication Patterns:
    The CLI's interaction with the rest of the system is clearly defined to
    manage coupling and concurrency.
    - Inbound (to CLI): Other components can call CLI methods directly for
      synchronous tasks (e.g., `PromptForInput`).
    - Outbound (from CLI): The CLI communicates its results and state changes
      asynchronously by publishing events to the event bus, avoiding direct
      dependencies on other components.
*/

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"
	"golang.org/x/term"

	"gemini/config"
	"gemini/desktop"
	"gemini/flow"
	"gemini/helpers"
)

// ShellPauseState defines the states for pausing shell output.
type ShellPauseState string

const (
	Prompt    = "prompt" // {Prompt} = Allow voice and txt prompts
	VoiceMode = "voice"  // Allow voice output for AI and {Prompt}
	ImageMode = "image"  // Allow send screenshot images with each {Prompt}
	VideoMode = "video"  // Allow send video stream
	System    = "system" // System CLI integration mode. Allow execute system commands and stream output to AI

	ShellPauseStart ShellPauseState = "start"
	ShellPauseStop  ShellPauseState = "stop"

	// Defines the input state when in system mode.
	stateProxyingToShell = iota
	stateReadingCommand
	stateReadingPassword

	promptPatern = "\n\033[A\r\033[1;91m%s\033[0m> \033[K"
	// Sequence: Save cursor, move to start of line, move down, clear line, print, restore cursor.
	soundbarPatern   = "\0337\r\033[B\033[K[%s%s]\0338"
	helpFormatString = "* `%s` - %s\n"

	// Thinking levels
	dynamic = "dynamic"
	none    = "none"
	low     = "low"
	medium  = "medium"
	high    = "high"
)

// CLI handles reading user input from the command line.
type CLI struct {
	wg                  *sync.WaitGroup
	cmdChan             chan<- string // Sends processed text commands from the CLI to the main application for AI processing.
	bus                 *EventBus.Bus
	formatter           *Formatter
	muted               bool
	isSystemShellActive bool
	aiEnabled           bool
	ready               bool
	modeMu              sync.Mutex // Protects access to mode, previousMode, ready, and muted status.
	mode                string
	previousMode        string
	promptChan          chan promptRequest // Receives requests for sensitive modal prompts.
	activePrompt        *promptRequest     // The currently active modal prompt
	shellBuffer         strings.Builder
	shellBufferMu       sync.Mutex // Protects access to the shellBuffer and shellPaused status.
	systemInputState    int
	preEscapeState      int // Remembers the state before an escape sequence
	shellPaused         bool
	systemCommandBuffer strings.Builder
	originalTermState   *term.State
	systemAtLineStart   bool
	terminal            *term.Terminal // For prompt mode line editing
	modeSwitchRequested bool           // Signals a switch between system and prompt loops.
	drawCompleteChan    chan struct{}  // Signals that AI response drawing is complete, unblocking the prompt loop.
	shellExitChan       chan struct{}  // Signals that the interactive shell process has exited.
}

// commandHandler defines the function signature for a CLI command handler.
type commandHandler func(c *CLI, args []string) (isAIPrompt bool, exit bool)

// promptRequest is used to pass a text prompt and receive a string response
// between the blocking PromptForInput method and the non-blocking Run loop.
type promptRequest struct {
	prompt       string
	responseChan chan string
}

// stdInOut is a helper struct that combines io.Reader and io.Writer.
// It's used to create a terminal instance that reads from stdin and writes to stdout.
type stdInOut struct {
	io.Reader
	io.Writer
}

var (
	modes = map[string]string{
		Prompt:    Prompt,
		System:    System,
		VoiceMode: VoiceMode,
		ImageMode: ImageMode,
		VideoMode: VideoMode,
	}

	thinkingLevels = map[string]int32{
		dynamic: -1,
		none:    0,
		low:     512,
		medium:  8192,
		high:    24576,
	}

	// commandHandlers maps command names to their handler functions.
	commandHandlers = map[string]commandHandler{
		"save":       handleSave,
		"debug":      handleDebug,
		"voice":      handleVoice,
		"tools":      handleTools,
		"transcript": handleTranscript,
		"history":    handleHistory,
		"cache":      handleCache,
		"thoughts":   handleThoughts,
		"thinking":   handleThinking,
		"mode":       handleMode,
		"prompt":     handlePrompt,
		"exit":       handleExit,
		"help":       handleHelp,
	}
)

// NewCLI creates a new CLI instance.
func NewCLI(wg *sync.WaitGroup, cmdChan chan<- string, bus *EventBus.Bus, aiEnabled bool) *CLI {
	if aiEnabled {
		fmt.Println("Use keyboard to send text prompts to the AI.")
	}

	return &CLI{
		wg:                  wg,
		cmdChan:             cmdChan,
		bus:                 bus,
		formatter:           NewFormatter(),
		muted:               true,
		isSystemShellActive: false,
		aiEnabled:           aiEnabled,
		ready:               false,
		modeMu:              sync.Mutex{},
		mode:                config.C.Mode,
		previousMode:        "",
		activePrompt:        nil,
		systemAtLineStart:   true,
		systemInputState:    stateProxyingToShell,
		preEscapeState:      stateProxyingToShell, // Default pre-escape state
		shellPaused:         false,
		modeSwitchRequested: false,
		promptChan:          make(chan promptRequest),
		drawCompleteChan:    make(chan struct{}, 1), // Buffered to be non-blocking
		shellExitChan:       make(chan struct{}, 1),
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
	helpers.SafeSend(c.promptChan, req)
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
		// When the shell exits, its output channel is closed. Signal the main
		// system mode loop that it's time to exit.
		helpers.SafeSend(c.shellExitChan, struct{}{})
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
	// When leaving system mode, the main loop will switch to prompt mode.
	c.draw()
}

// handleSystemLineEditor provides a minimal line editor for raw terminal mode.
// It's used for both internal commands and modal prompts.
func (c *CLI) handleSystemLineEditor(b byte) {
	switch b {
	case 27, '\t': // ESC key, Tab key
		// Ignore.
	case '\r', '\n': // Enter key
		fmt.Print("\r\n") // Echo newline.
		text := c.systemCommandBuffer.String()
		c.systemCommandBuffer.Reset()
		c.systemInputState = stateProxyingToShell // Always return to proxying.
		c.systemAtLineStart = true
		if text != "" {
			isAIPrompt, exit := c.command(text)
			if exit {
				// An exit was requested. The runSystemModeLoop will
				// catch the shutdown signal. We just need to stop
				// processing here to avoid drawing a new prompt.
				return
			}
			// If the command was not an AI prompt, redraw the shell prompt.
			if !isAIPrompt {
				c.draw()
			}
		} else {
			c.draw() // User typed "/" then Enter.
		}
	case 127, 8: // Backspace
		if c.systemCommandBuffer.Len() > 0 {
			// Correctly handle backspace for strings.Builder
			s := c.systemCommandBuffer.String()
			c.systemCommandBuffer.Reset()
			c.systemCommandBuffer.WriteString(s[:len(s)-1])
			fmt.Print("\b \b") // Erase character on screen.
		}
	case 3: // Ctrl+C
		fmt.Print("^C\r\n")
		c.systemCommandBuffer.Reset()
		c.systemInputState = stateProxyingToShell
		c.systemAtLineStart = true
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

// handlePasswordEditor is a special-purpose line editor for reading sensitive
// information. It processes input bytes without echoing them to the terminal.
func (c *CLI) handlePasswordEditor(b byte) {
	switch b {
	case '\r', '\n': // Enter key
		fmt.Print("\r\n") // Echo newline to confirm input.
		password := c.systemCommandBuffer.String()
		c.systemCommandBuffer.Reset()
		c.systemInputState = stateProxyingToShell // Return to normal operation.
		c.systemAtLineStart = true

		if c.activePrompt != nil {
			helpers.SafeSend(c.activePrompt.responseChan, password)
			close(c.activePrompt.responseChan)
			c.activePrompt = nil
		}
		(*c.bus).Publish(config.MainTopic, "ready:cli.prompt.done")
		c.draw() // Redraw the normal shell prompt.

	case 127, 8: // Backspace
		if c.systemCommandBuffer.Len() > 0 {
			s := c.systemCommandBuffer.String()
			c.systemCommandBuffer.Reset()
			c.systemCommandBuffer.WriteString(s[:len(s)-1])
			// Do not echo anything for backspace.
		}
	case 3: // Ctrl+C
		fmt.Print("^C\r\n")
		c.systemCommandBuffer.Reset()
		c.systemInputState = stateProxyingToShell
		c.systemAtLineStart = true
		if c.activePrompt != nil {
			helpers.SafeSend(c.activePrompt.responseChan, "") // Send empty string on abort.
			close(c.activePrompt.responseChan)
			c.activePrompt = nil
		}
		(*c.bus).Publish(config.MainTopic, "ready:cli.prompt.done")
		c.draw()
	default:
		// Add printable characters to buffer, but do not echo them.
		if b >= 32 && b < 127 {
			c.systemCommandBuffer.WriteByte(b)
		}
	}
}

// processLine handles a line of input received from the prompt mode editor.
// It returns (isAIPrompt, exitRequested) to the calling loop.
func (c *CLI) processLine(line string) (isAIPrompt bool, exitRequested bool) {
	fullLine := strings.TrimSpace(line)
	parts := strings.Fields(fullLine)

	if len(parts) > 0 && strings.HasPrefix(parts[0], "/") {
		parts[0] = strings.TrimPrefix(parts[0], "/")
		// A command can be an AI prompt (like /prompt) or an exit command.
		isAIPrompt, exit := c.command(strings.Join(parts, " "))
		return isAIPrompt, exit
	} else if fullLine != "" {
		helpers.SafeSend(c.cmdChan, fullLine)
		return true, false // This is a standard AI prompt.
	}
	return false, false // Empty line, not a prompt, not an exit.
}

// processInputByte is the core of the raw mode input state machine. It processes
// a single byte of input and updates the CLI state accordingly.
func (c *CLI) processInputByte(b byte) {
	// This state machine is now only used for system mode.
	switch c.systemInputState {
	case stateProxyingToShell:
		// Intercept Ctrl+D (EOT character) to gracefully exit the shell
		// without closing the main application's stdin.
		if b == 4 {
			log.Println("Ctrl+D detected in system mode. Sending 'exit' to shell.")
			if err := desktop.C.SendToShell("exit\n"); err != nil {
				log.Printf("Error sending exit command to system shell: %v", err)
			}
			// By consuming the Ctrl+D and not proxying it, we prevent the main
			// stdin reader from receiving an EOF. The shell will exit, which
			// will be detected by the shell output handler, triggering a clean
			// switch back to prompt mode.
			return
		}
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
		c.handleSystemLineEditor(b)
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
		config.DebugPrintln("CLI received draw event, preparing to draw prompt.")
		c.muted = false
		if c.mode == System {
			c.drawLocked()
		} else {
			// We are in prompt mode. Signal the prompt loop to continue.
			helpers.SafeSend(c.drawCompleteChan, struct{}{})
		}
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
// It is designed to be cancellable via the 'done' channel.
func (c *CLI) startStdinReader(inputChan chan<- []byte, done <-chan struct{}) {
	go func() {
		defer close(inputChan)

		// First, try to set a deadline to see if the file descriptor supports it.
		err := os.Stdin.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		_ = os.Stdin.SetReadDeadline(time.Time{}) // Immediately cancel it.

		if err != nil {
			// The file type does not support deadlines. Log this and fall back
			// to a simple blocking read loop. This loop will not be cancellable
			// via the 'done' channel while it's blocked on Read().
			log.Println("stdin does not support deadlines; sensitive prompts in system mode may not be interruptible.")
			for {
				// We can check for cancellation *before* the blocking call.
				select {
				case <-done:
					return
				default:
				}
				buf := make([]byte, 128)
				n, readErr := os.Stdin.Read(buf)
				if readErr != nil {
					if readErr != io.EOF {
						log.Printf("Stdin read error: %v", readErr)
					}
					return
				}
				if n > 0 {
					data := make([]byte, n)
					copy(data, buf[:n])
					inputChan <- data
				}
			}
		}

		// If we are here, deadlines are supported. Use the non-blocking loop.
		buf := make([]byte, 128)
		for {
			select {
			case <-done:
				return
			default:
			}

			// Set a deadline on the read to make it non-blocking. This allows the
			// loop to periodically check the 'done' channel for cancellation.
			readErr := os.Stdin.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if readErr != nil {
				// This shouldn't happen if the initial check passed, but handle it.
				log.Printf("Failed to set read deadline on stdin: %v", err)
				return
			}

			n, readErr := os.Stdin.Read(buf)

			// A zero-time deadline effectively cancels the deadline.
			_ = os.Stdin.SetReadDeadline(time.Time{})

			if readErr != nil {
				if os.IsTimeout(readErr) {
					continue // This is an expected error when no input is available.
				}
				// If the error is not a timeout, it's a real issue.
				log.Printf("Stdin read error: %v", readErr)
				return
			}

			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				inputChan <- data
			}
		}
	}()
}

// runSystemModeLoop handles all input and events when the CLI is in 'system' mode.
func (c *CLI) runSystemModeLoop() {
	c.startSystemShell()
	c.systemInputState = stateProxyingToShell

	inputChan := make(chan []byte, 1) // Use a small buffer
	doneChan := make(chan struct{})
	c.startStdinReader(inputChan, doneChan)

	shutdownListener := flow.GetListener()

	for {
		if c.modeSwitchRequested {
			c.stopSystemShell()
			return
		}

		select {
		case <-*shutdownListener:
			return
		case <-c.shellExitChan:
			log.Println("Shell exited, terminating system mode loop.")
			c.stopSystemShell()
			return
		case req := <-c.promptChan:
			c.activePrompt = &req
			c.systemInputState = stateReadingPassword
			c.systemCommandBuffer.Reset()
			(*c.bus).Publish(config.MainTopic, "block:cli.prompt.start")
			fmt.Printf("\n%s: ", req.prompt)

		case inputBytes, ok := <-inputChan:
			if !ok {
				// This case is now less likely to be hit for Ctrl+D, but is kept
				// as a safeguard for other stdin closure scenarios.
				log.Println("Stdin closed, exiting system mode loop.")
				c.stopSystemShell()
				return
			}

			// The CLI should only drop input if it's not in a special input
			// state (like reading a password) that must be handled even when
			// the rest of the system is "blocked".
			if c.systemInputState != stateReadingPassword && !c.ready {
				log.Println("CLI dropping input received during blocked state.")
				continue
			}

			for _, b := range inputBytes {
				if c.systemInputState == stateReadingPassword {
					c.handlePasswordEditor(b)
				} else {
					c.processInputByte(b)
				}
			}
		}
	}
}

// runPromptModeLoop handles all input and events when the CLI is in 'prompt', 'image', or 'voice' mode.
func (c *CLI) runPromptModeLoop() {
	log.Println("CLI entered prompt mode. Using advanced line editor.")
	shutdownListener := flow.GetListener()

	for {
		if c.modeSwitchRequested {
			return
		}

		c.draw()

		line, err := c.terminal.ReadLine()
		if err != nil {
			if err == io.EOF {
				log.Println("Exiting due to EOF from terminal (Ctrl+D).")
				flow.Quit()
				<-*shutdownListener
			} else {
				log.Printf("ReadLine error: %v. Exiting prompt mode.", err)
			}
			return
		}

		isAIPrompt, exitRequested := c.processLine(line)

		if exitRequested {
			<-*shutdownListener
			return
		}

		if c.modeSwitchRequested {
			return
		}

		if isAIPrompt {
			select {
			case <-c.drawCompleteChan:
			case <-*shutdownListener:
				return
			}
		}
	}
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
	// Create the terminal instance for prompt mode.
	c.terminal = term.NewTerminal(&stdInOut{os.Stdin, os.Stdout}, "")

	helpers.Verify((*c.bus).SubscribeAsync(config.MainTopic, c.handleBusEvents, false))

	shutdownListener := flow.GetListener()

	// This is now a dispatcher loop that switches between system and prompt mode handlers.
	for {
		select {
		case <-*shutdownListener:
			log.Println("CLI input handler shutting down.")
			return
		default:
		}

		c.modeMu.Lock()
		isReady := c.ready
		currentMode := c.mode
		c.modeMu.Unlock()

		// The main loop must wait until the application signals it's ready.
		// This prevents a race condition where the input loop starts and blocks
		// before the initial prompt can be drawn.
		if !isReady {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		c.modeSwitchRequested = false

		if currentMode == System {
			c.runSystemModeLoop()
		} else {
			c.runPromptModeLoop()
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

	if c.mode == System {
		// In system mode, the shell provides its own prompt. We send a newline
		// to ensure it's redrawn after AI output.
		if err := desktop.C.SendToShell("\n"); err != nil {
			log.Printf("Error sending newline to system shell to redraw prompt: %v", err)
		}
	} else {
		// We are in a prompt mode, using term.ReadLine.
		config.DebugPrintln("CLI drawind prompt")
		promptStr := fmt.Sprintf(promptPatern, c.mode)
		c.terminal.SetPrompt(promptStr)
		(*c.bus).Publish(config.MainTopic, "show:cli.run")
	}
}

func handleSave(c *CLI, _ []string) (isAIPrompt bool, exit bool) {
	(*c.bus).Publish(config.AITopic, "save:history.txt")
	fmt.Println("Conversation history save requested to history.txt.")
	return false, false
}

func handleDebug(_ *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.Debug = !config.C.Debug
	log.Printf("Debug mode set to: %t", config.C.Debug)
	return false, false
}

func handleVoice(c *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.AI.VoiceEnabled = !config.C.AI.VoiceEnabled
	log.Printf("Voice output set to: %t", config.C.AI.VoiceEnabled)
	(*c.bus).Publish(config.AITopic, "restart_session:voice_toggle")
	return false, false
}

func handleTools(c *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.AI.EnableTools = !config.C.AI.EnableTools
	log.Printf("AI tools enabled set to: %t", config.C.AI.EnableTools)
	(*c.bus).Publish(config.AITopic, "restart_session:tools_toggle")
	return false, false
}

func handleTranscript(c *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.AI.Transcript = !config.C.AI.Transcript
	log.Printf("Separate transcription step set to: %t", config.C.AI.Transcript)
	(*c.bus).Publish(config.AITopic, "restart_session:transcript_toggle")
	return false, false
}

func handleHistory(_ *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.AI.VoiceHistory = !config.C.AI.VoiceHistory
	log.Printf("Voice history set to: %t", config.C.AI.VoiceHistory)
	return false, false
}

func handleCache(_ *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.AI.EnableCache = !config.C.AI.EnableCache
	log.Printf("AI caching set to: %t", config.C.AI.EnableCache)
	return false, false
}

func handleThoughts(_ *CLI, _ []string) (isAIPrompt bool, exit bool) {
	config.C.AI.Thoughts = !config.C.AI.Thoughts
	log.Printf("AI thoughts set to: %t", config.C.AI.Thoughts)
	return false, false
}

func handleThinking(_ *CLI, args []string) (isAIPrompt bool, exit bool) {
	hint := func() {
		fmt.Printf("Available levels: %s, %s, %s, %s, %s\n", dynamic, none, low, medium, high)
	}
	if len(args) != 1 {
		fmt.Println("Usage: /thinking <level>")
		hint()
	} else {
		level := strings.ToLower(args[0])
		if value, ok := thinkingLevels[level]; !ok {
			fmt.Printf("Unknown thinking level: %s\n", level)
			hint()
		} else {
			config.C.AI.Thinking = value
			log.Printf("AI thinking budget set to: %s (%d)", level, value)
		}
	}
	return false, false
}

func handleMode(c *CLI, args []string) (isAIPrompt bool, exit bool) {
	hint := func() {
		fmt.Printf("Available AI modes: %s, %s, %s, %s, %s\n", Prompt, System, VoiceMode, ImageMode, VideoMode)
	}
	if len(args) != 1 {
		fmt.Println("Usage: /mode <name>")
		hint()
		return false, false
	}

	mode := strings.ToLower(args[0])
	value, ok := modes[mode]
	if !ok {
		fmt.Printf("Unknown AI mode: %s\n", mode)
		hint()
		return false, false
	}

	// Handle transitions to/from system mode
	if value == System {
		c.previousMode = c.mode
	} else if c.mode == System { // Switching out of system mode
		if err := desktop.C.SendToShell("exit\n"); err != nil {
			log.Printf("Error sending exit command to system shell: %v", err)
		}
	}
	c.mode = value
	config.C.Mode = value
	c.modeSwitchRequested = true
	log.Printf("CLI mode set to: %s", value)
	(*c.bus).Publish(config.AITopic, fmt.Sprintf("mode:%s", value))
	return false, false
}

func handlePrompt(c *CLI, args []string) (isAIPrompt bool, exit bool) {
	if c.mode == System {
		promptText := strings.TrimSpace(strings.Join(args, " "))
		if promptText != "" {
			helpers.SafeSend(c.cmdChan, promptText)
			return true, false
		}
		fmt.Println("Usage: /prompt <text for AI>")
		c.draw()
	}
	return false, false
}

func handleExit(_ *CLI, _ []string) (isAIPrompt bool, exit bool) {
	flow.Quit()
	return false, true
}

func handleHelp(c *CLI, _ []string) (isAIPrompt bool, exit bool) {
	type helpEntry struct {
		command     string
		description string
	}

	mainCommands := []helpEntry{
		{"/mode <name>", fmt.Sprintf("Set AI mode (%s, %s, %s, %s, %s)", Prompt, System, VoiceMode, ImageMode, VideoMode)},
		{"/debug", "Toggle debug mode"},
		{"/voice", "Toggle voice responses"},
		{"/tools", "Toggle AI tools (e.g., Google Search)"},
		{"/transcript", "Toggle separate transcription step for voice chat"},
		{"/help", "Display this help message"},
		{"/exit", "Exit the application"},
	}

	systemCommands := []helpEntry{
		{"/prompt <text>", "Send a text prompt to the AI"},
	}

	postAICommands := []helpEntry{
		{"/thinking <level>", fmt.Sprintf("Set AI thinking budget (%s, %s, %s, %s, %s)", dynamic, none, low, medium, high)},
		{"/thoughts", "Toggle AI thoughts visibility"},
		{"/cache", "Toggle AI caching"},
		{"/save", "Save conversation history to history.txt"},
		{"/history", "Toggle including voice prompts in conversation history"},
	}

	var builder strings.Builder

	builder.WriteString("**Available commands:**\n\n")
	for _, cmd := range mainCommands {
		builder.WriteString(fmt.Sprintf(helpFormatString, cmd.command, cmd.description))
	}

	builder.WriteString("\n**System Mode Commands:**\n\n")
	for _, cmd := range systemCommands {
		builder.WriteString(fmt.Sprintf(helpFormatString, cmd.command, cmd.description))
	}

	builder.WriteString("\n**Post AI Commands:**\n\n")
	for _, cmd := range postAICommands {
		builder.WriteString(fmt.Sprintf(helpFormatString, cmd.command, cmd.description))
	}

	c.formatter.Print(builder.String())

	return false, false
}

// command handles internal CLI commands. It returns (isAIPrompt, exit) to signal
// the calling loop's next action.
func (c *CLI) command(cmd string) (isAIPrompt bool, exit bool) {
	c.modeMu.Lock()
	defer c.modeMu.Unlock()

	var commandName string
	log.Println("CLI command received:", cmd)
	parts := strings.Fields(cmd)

	if len(parts) == 0 {
		return false, false // No command entered.
	}
	commandName = parts[0]

	if handler, ok := commandHandlers[commandName]; ok {
		return handler(c, parts[1:])
	}

	fmt.Printf("Unknown command: %s\n", commandName)
	return false, false
}
