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
	"bufio"
	"fmt"
	"io"
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

// ShellPauseState defines the states for pausing shell output.
type ShellPauseState string

const (
	Prompt     = "prompt" // {Prompt} = Allow voice and txt prompts
	VoiceMode  = "voice"  // Allow voice output for AI and {Prompt}
	ImageMode  = "image"  // Allow send screenshot images with each {Prompt}
	VideoMode  = "video"  // Allow send video stream
	SystemMode = "system" // System CLI integration mode. Allow execute system commands and stream output to AI

	ShellPauseStart ShellPauseState = "start"
	ShellPauseStop  ShellPauseState = "stop"

	// Defines the input state when in system mode.
	stateProxyingToShell        = iota
	stateIgnoringEscapeSequence // stateReadingCommand and stateReadingPassword are no longer needed.

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
	wg                    *sync.WaitGroup
	cmdChan               chan<- string // Sends processed text commands from the CLI to the main application for AI processing.
	bus                   *EventBus.Bus
	formatter             *Formatter
	muted                 bool
	isSystemShellActive   bool
	aiEnabled             bool
	ready                 bool
	mode                  string
	previousMode          string
	promptChan            chan promptRequest // Receives requests for sensitive modal prompts.
	shellBuffer           strings.Builder
	shellBufferMu         sync.Mutex // Protects access to the shellBuffer and shellPaused status.
	shellBufferSynced     chan struct{}
	systemInputState      int
	shellPaused           bool
	systemProxyLineBuffer strings.Builder // A small buffer to track the current line in proxy mode to detect commands.
	originalTermState     *term.State
	terminal              *term.Terminal // For prompt mode line editing
	terminalMu            sync.RWMutex
	stdinReader           *bufio.Reader // A buffered reader to share between raw mode and line mode.
	modeSwitchRequested   bool          // Signals a switch between system and prompt loops.
	systemAFK             bool          // Auto-finish turn in system mode.
	drawCompleteChan      chan struct{} // Only for draw() method use! Signals that AI response drawing is complete, unblocking the prompt loop.
	drawCompleteBlock     bool          // Trigger that show execution blocked by cli prompt. Not applicable for shell prompt.
}

// commandHandler defines the function signature for a CLI command handler.
type commandHandler func(c *CLI, args []string) (hide bool, exit bool)

// promptRequest is used to pass a text prompt and receive a string response
// between the blocking PromptForInput method and the non-blocking Run loop.
type promptRequest struct {
	prompt       string
	responseChan chan string
}

// terminalReadWriter combines a buffered reader with a writer to satisfy
// the io.ReadWriter interface required by term.NewTerminal. This allows
// sharing the buffered reader between our raw byte processing and the
// terminal's line editor.
type terminalReadWriter struct {
	*bufio.Reader
	io.Writer
}

var (
	modes = map[string]string{
		Prompt:     Prompt,
		SystemMode: SystemMode,
		VoiceMode:  VoiceMode,
		ImageMode:  ImageMode,
		VideoMode:  VideoMode,
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
		"pause":      handlePause,
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
		"afk":        handleAfk,
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
		muted:               false,
		isSystemShellActive: false,
		aiEnabled:           aiEnabled,
		ready:               false,
		mode:                config.C.Mode,
		previousMode:        "",
		systemInputState:    stateProxyingToShell,
		shellPaused:         false,
		modeSwitchRequested: false,
		promptChan:          make(chan promptRequest),
		systemAFK:           false,
		stdinReader:         bufio.NewReader(os.Stdin),
		drawCompleteChan:    make(chan struct{}, 1), // Buffered to be non-blocking
		shellBufferSynced:   make(chan struct{}, 1),
	}
}

// handleTerminalActions listens for window resize events and updates the terminal's size.
func (c *CLI) handleTerminalActions() {
	winchListener := flow.GetWinchListener()
	defer flow.StopWinchListener(winchListener)

	shutdownChan := flow.GetListener()
	// Set initial size.
	c.updateTerminalSize()

	for {
		select {
		case <-*shutdownChan:
			// On exit we check for blocking by prompt and remove it
			if c.drawCompleteBlock {
				log.Println("CLI terminal activity detected, interrupt prompt.")
				c.terminal.Write([]byte{'\n'})
			}
			return
		case <-*winchListener:
			log.Println("CLI terminal resize detected, updating size.")
			c.updateTerminalSize()
		}
	}
}

// updateTerminalSize gets the current terminal dimensions and applies them to the
// term.Terminal instance. It is safe for concurrent use.
func (c *CLI) updateTerminalSize() {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()

	if c.terminal == nil {
		return
	}

	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		log.Printf("Error getting terminal size: %v", err)
		return
	}

	if err := c.terminal.SetSize(width, height); err != nil {
		log.Printf("Error setting terminal size: %v", err)
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

	helpers.SafeSend(c.shellBufferSynced, struct{}{})
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
	isSystemMode := c.mode == SystemMode

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

// setMode centralizes the logic for changing the CLI's operational mode.
// It updates the internal state, the global config, signals the main loop
// to switch, and publishes an event to notify other components.
func (c *CLI) setMode(newMode string) {
	if c.mode == newMode {
		return // No change needed. Silently.
	}

	// When switching *to* system mode, we need to remember where we came from.
	if newMode == SystemMode {
		c.previousMode = c.mode
	}

	c.mode = newMode
	config.C.Mode = newMode
	c.modeSwitchRequested = true
	log.Printf("CLI mode set to: %s", newMode)
	(*c.bus).Publish(config.AITopic, fmt.Sprintf("mode:%s", newMode))
}

// IsBusy returns true if a command is currently executing in the system shell.
func (c *CLI) IsBusy() bool {
	if !c.isSystemShellActive {
		return false
	}
	// This relies on the desktop controller's implementation, which in turn
	// checks the state of the underlying shell executor.
	return desktop.C.IsShellCommandRunning()
}

// startSystemShell starts the interactive shell for system mode.
func (c *CLI) startSystemShell(outputChan chan<- string) {
	if c.isSystemShellActive {
		return
	}

	// The terminal is already in raw mode, managed by the main Run() loop.
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

	var newMode string
	if c.previousMode != "" {
		newMode = c.previousMode
		c.previousMode = "" // Reset for the next time.
	} else {
		newMode = Prompt // Default fallback.
	}
	c.setMode(newMode)
	// When Ctrl+D prssed, we don't need draw prompt for user.
	// Shell close will finish model turn and model response in normal way.
	// After model response actual prompt will be requested by live ai
	// (*c.bus).Publish(config.MainTopic, "draw:cli.stopSystemShell")
}

// processLine handles a line of input received from the prompt mode editor.
// It returns exit requested to the calling loop.
func (c *CLI) processLine(line string) bool {
	fullLine := strings.TrimSpace(line)
	parts := strings.Fields(fullLine)

	if len(parts) > 0 && strings.HasPrefix(parts[0], "/") {
		parts[0] = strings.TrimPrefix(parts[0], "/")
		// A command can be an AI prompt (like /prompt) or an exit command.
		return c.command(strings.Join(parts, " "))
	} else if fullLine != "" {
		helpers.SafeSend(c.cmdChan, fullLine)
		return false // This is a standard AI prompt.
	}
	// Signal the prompt loop to continue.
	(*c.bus).Publish(config.MainTopic, "draw:cli.processLine")
	// Empty line, not a prompt, not an exit.
	return false
}

// processInputByte is the core of the raw mode input state machine. It processes
// a single byte of input and updates the CLI state accordingly.
func (c *CLI) processInputByte(b byte) {
	// This state machine is now only used for system mode.
	switch c.systemInputState {
	case stateIgnoringEscapeSequence:
		// We entered this state on an ESC. We proxy all bytes to the shell.
		// When we see a terminating character (a letter or ~), we assume the
		// sequence for a special key (like arrows or Home/End) is over.
		// We reset our line buffer because its state is now unknown due to
		// un-tracked cursor movement, then return to normal proxying.
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
			c.systemInputState = stateProxyingToShell
			c.systemProxyLineBuffer.Reset()
		}
		// Always proxy the byte in this state.
		if err := desktop.C.SendToShell(string(b)); err != nil {
			log.Printf("Error sending input to system shell: %v", err)
		}
		return // Return immediately.
	case stateProxyingToShell:
		// Intercept Ctrl+D (EOT character) to gracefully exit the shell
		// without closing the main application's stdin.
		if b == 4 {
			log.Println("Ctrl+D detected in system mode. Sending 'exit' to shell.")
			// We call stopSystemShell() directly instead of sending "exit\n" to the
			// shell. This ensures a clean and immediate termination of the PTY
			// process and all associated goroutines.
			c.stopSystemShell()

			// By consuming the Ctrl+D and not proxying it, we prevent the main
			// stdin reader from receiving an EOF. The shell will exit, which
			// will be detected by the shell output handler, triggering a clean
			// switch back to prompt mode.
			return
		}

		// Intercept ESC (byte 27) to handle special keys like arrows, Home, End, etc.
		// These keys send escape sequences that we can't track perfectly without a
		// full terminal emulator. Instead, we enter a state to proxy the sequence
		// and then reset our line buffer, assuming the user may have moved the
		// cursor to the start of the line.
		if b == 27 {
			c.systemInputState = stateIgnoringEscapeSequence
			// Proxy the ESC byte itself and then wait for the rest of the sequence.
			if err := desktop.C.SendToShell(string(b)); err != nil {
				log.Printf("Error sending input to system shell: %v", err)
			}
			return
		}

		// The logic to detect an internal command (starting with '/') is now based
		// on a small line buffer that mirrors user input. This correctly handles
		// cases where the user types and then backspaces to the start of the line.
		// All bytes are still proxied to the shell to maintain interactivity.

		// Update the proxy line buffer based on the input byte.
		if b >= 32 && b < 127 { // Printable characters
			c.systemProxyLineBuffer.WriteByte(b)
		} else if b == '\r' || b == '\n' { // Enter
			c.systemProxyLineBuffer.Reset()
		} else if b == 127 || b == 8 { // Backspace
			if c.systemProxyLineBuffer.Len() > 0 {
				s := c.systemProxyLineBuffer.String()
				c.systemProxyLineBuffer.Reset()
				c.systemProxyLineBuffer.WriteString(s[:len(s)-1])
			}
		}

		// Proxy the byte to the interactive shell.
		if err := desktop.C.SendToShell(string(b)); err != nil {
			log.Printf("Error sending input to system shell: %v", err)
		}
	}
}

// handleBusEvents processes events received from the main application event bus.
func (c *CLI) handleBusEvents(event string) {
	config.DebugPrintf("CLI received event: %s\n", event)

	// The shell_pause event is now handled by a direct call from LiveAI,
	// so it is no longer processed here. We parse other events.
	parts := strings.SplitN(event, ":", 2)
	command := parts[0]

	switch command {
	case "block": // Critical flow blocking
		c.ready = false
	case "ready": // Critical flow unblocking
		c.ready = true
		config.DebugPrintln("CLI Initial prompt preparing to draw.")
		c.draw() // Initial prompt
	case "mute": // Normal flow
		c.muted = true
	case "draw": // Normal flow
		config.DebugPrintln("CLI received draw event, preparing to draw prompt.")
		c.muted = false
		c.draw()
	case "afk":
		if c.systemAFK {
			handleAfk(c, nil)
		}
	default:
		config.DebugPrintf("CLI drop event: %s", event)
	}
}

// startStdinReader starts a goroutine to read from standard input and send the data to a channel.
// It is designed to be cancellable via the 'done' channel.
func (c *CLI) startStdinReader(inputChan chan<- byte, done <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(inputChan)

		for {
			// Check for cancellation before blocking on read.
			select {
			case <-done:
				return
			default:
			}

			b, err := c.stdinReader.ReadByte()
			if err != nil {
				// EOF or other error, the main loop will handle shutdown.
				return
			}

			// Check for cancellation again after read, before sending.
			// This is the critical part to prevent a race condition where a byte is
			// read just as the reader is being asked to stop.
			select {
			case inputChan <- b:
			case <-done:
				// We were cancelled after reading a byte but before we could send it.
				// We must put the byte back into the buffer so the next reader can see it.
				if err := c.stdinReader.UnreadByte(); err != nil {
					log.Printf("FATAL: could not unread byte, input state corrupted: %v", err)
				}
				return
			}
		}
	}()
}

// runSystemModeLoop handles all input and events when the CLI is in 'system' mode.
func (c *CLI) runSystemModeLoop() {
	// This channel will receive output from the interactive shell.
	outputChan := make(chan string, 100)
	c.startSystemShell(outputChan)
	// Use defer to ensure the shell is stopped when this function returns,
	// for any reason (mode switch, shutdown, error).
	defer c.stopSystemShell()

	c.systemInputState = stateProxyingToShell
	shutdownListener := flow.GetListener()

	// This outer loop allows us to re-initialize the input reader after a
	// blocking operation like ReadLine.
	for {
		if c.modeSwitchRequested {
			return
		}

		var readerWg sync.WaitGroup
		inputChan := make(chan byte)
		doneChan := make(chan struct{})
		c.startStdinReader(inputChan, doneChan, &readerWg)

		// Flag to indicate we need to break from the inner loop to call ReadLine.
		var commandInputRequested bool
		var passwordRequest *promptRequest // Store the request here
		// var afkTurnPending bool            // Synchronization flag for AFK mode.

	innerSelectLoop:
		for {
			select {
			case <-*shutdownListener:
				close(doneChan) // Signal the reader to stop.
				return
			case _, ok := <-c.shellBufferSynced:
				if !ok {
					log.Println("Shell exited, buffer sync channel closed.")
					close(doneChan) // Signal the reader to stop.
					return          // This will trigger the deferred c.stopSystemShell()
				}
				// If an AFK turn is pending, it means a command has finished and its
				// output has now been polled by LiveAI. We can now safely submit the
				// next turn to the AI.
				if c.systemAFK && !c.IsBusy() {
					// afkTurnPending = false // Consume the flag.
					log.Println("AFK mode: Command finished, auto-submitting turn to AI.")
					helpers.SafeSend(c.cmdChan, "This is AFK mode. Have task done? No, continue with next step. Yes, use disable_afk_mode tool.")
				}

			case line, ok := <-outputChan:
				if !ok {
					log.Println("Shell exited, terminating system mode loop.")
					close(doneChan) // Signal the reader to stop.
					return          // This will trigger the deferred c.stopSystemShell()
				}
				c.shellBufferMu.Lock()
				c.shellBuffer.WriteString(line + "\n")
				c.shellBufferMu.Unlock()

			case req := <-c.promptChan:
				// A request for a password has arrived.
				passwordRequest = &req
				// Publish a block event to pause other UI components and CLI input.
				(*c.bus).Publish(config.MainTopic, "block:cli.passwordPrompt")
				// Print the prompt *before* waiting. This unblocks the user, who will
				// press a key, which in turn unblocks the waiting reader goroutine.
				c.formatter.Print(passwordRequest.prompt + ": ")
				// Stop the background reader to get exclusive access.
				close(doneChan)
				readerWg.Wait()
				break innerSelectLoop // Exit the loop to call ReadPassword.

			case b, ok := <-inputChan:
				if !ok {
					log.Println("Stdin reader channel closed, likely due to an error. Exiting system mode.")
					return
				}

				if !c.ready {
					log.Println("CLI dropping input received during blocked state.")
					continue
				}

				if c.systemAFK {
					// When AFK mode turned on, any key pressed should break it
					handleAfk(c, nil)
					continue
				}

				// This is the main transition logic.
				if c.systemInputState == stateProxyingToShell && b == '/' && c.systemProxyLineBuffer.Len() == 0 {
					fmt.Print("/") // Echo the slash to the user.

					// Stop the background reader and wait for it to exit completely.
					// This synchronous stop is crucial to prevent any more bytes from
					// being read from the shared buffer before ReadLine takes over.
					close(doneChan)
					readerWg.Wait()
					commandInputRequested = true
					break innerSelectLoop // Exit the select loop to call ReadLine.
				}

				// If not a command, process the byte for proxying.
				// The password handling is now outside this byte-processing logic.
				c.processInputByte(b)
			}
		} // End of innerSelectLoop

		// This code runs after the inner select loop is broken.
		if commandInputRequested {
			// We now have exclusive control of the terminal for reading.
			// Set an empty prompt so ReadLine works inline without overwriting the shell prompt.
			c.terminalMu.RLock()
			c.terminal.SetPrompt("")
			line, err := c.terminal.ReadLine()
			c.terminalMu.RUnlock()
			if err != nil {
				if err != io.EOF {
					log.Printf("ReadLine error: %v", err)
				}
				(*c.bus).Publish(config.MainTopic, "draw:cli.readLineError")
				// Continue the outer loop to restart the proxy reader.
				continue
			}

			// We have the command. Process it.
			c.formatter.Reset() // Echo newline.
			c.systemProxyLineBuffer.Reset()
			if exit := c.command(line); exit {
				return // An exit command was issued.
			}
			// After the command is handled, continue the outer loop to restart the proxy reader.
			continue
		}

		if passwordRequest != nil {
			// We now have exclusive control of the terminal for reading.
			// The prompt was printed *before* the wait to unblock the user.
			c.terminalMu.RLock()
			// Now we read the password without printing a prompt again.
			password, err := c.terminal.ReadPassword("")
			c.terminalMu.RUnlock()

			// Regardless of success or error, we must respond to unblock the caller.
			if err != nil {
				log.Printf("ReadPassword error: %v", err)
				fmt.Print("\r\n")                                  // Ensure we move to a new line on error.
				helpers.SafeSend(passwordRequest.responseChan, "") // Send empty on error
			} else {
				helpers.SafeSend(passwordRequest.responseChan, password)
			}
			close(passwordRequest.responseChan)

			// After handling the password, we need to redraw the shell prompt
			// and continue the main loop to restart the proxy reader.
			// Publish a ready event to unblock the system and redraw the prompt.
			// The draw call is handled by the event handler for "ready".
			(*c.bus).Publish(config.MainTopic, "ready:cli.passwordPrompt")
			continue
		}

		// If the inner loop exited for any other reason, we should exit the main loop.
		break
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

		// CLI owns prompt. In a reason of event based prompt drawing
		// We must wait for draw event fired to show prompt and collect
		// user input
		select {
		case <-*shutdownListener:
			return
		case <-c.drawCompleteChan:
			if c.drawCompleteBlock {
				log.Println("WARNING: Prompt exist. Draw aborted.")
				continue
			}

			c.terminalMu.RLock()
			c.drawCompleteBlock = true
			line, err := c.terminal.ReadLine()
			c.drawCompleteBlock = false
			c.terminalMu.RUnlock()
			if err != nil {
				if err == io.EOF {
					log.Println("Exiting due to EOF from terminal (Ctrl+C, Ctrl+D).")
					c.formatter.Reset()
					flow.Quit()
				} else {
					log.Printf("ReadLine error: %v. Exiting prompt mode.", err)
				}
				continue
			}

			c.processLine(line)
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
	c.terminalMu.Lock()
	// Create the terminal instance for both prompt and system mode.
	// It uses our shared, buffered reader.
	c.terminal = term.NewTerminal(&terminalReadWriter{
		Reader: c.stdinReader,
		Writer: os.Stdout,
	}, "")
	c.terminalMu.Unlock()

	go c.handleTerminalActions()

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

		currentMode := c.mode

		c.modeSwitchRequested = false

		// Both calls is blocking and provide full input processing
		if currentMode == SystemMode {
			c.runSystemModeLoop()
		} else {
			c.runPromptModeLoop()
		}
	}
}

// Method for prompt drawing. Allowed usage only in handleBusEvents
// IMPORTANT: To draw prompt should be used main topic show event.
func (c *CLI) draw() {
	if c.muted || !c.ready {
		config.DebugPrintf("CLI prompt drawing blocked when Muted: %t, Ready: %t", c.muted, c.ready)
		return
	}

	if c.mode == SystemMode {
		config.DebugPrintln("CLI prompt drawing deligated to shell")
		// In system mode, the shell provides its own prompt. We send a newline
		// to ensure it's redrawn after AI output.
		if err := desktop.C.SendToShell("\n"); err != nil {
			log.Printf("Error sending newline to system shell to redraw prompt: %v", err)
		}
	} else {
		// We are in a prompt mode, using term.ReadLine.
		config.DebugPrintln("CLI drawing prompt")
		c.terminalMu.RLock()
		promptStr := fmt.Sprintf(promptPatern, c.mode)
		c.terminal.SetPrompt(promptStr) // Update the prompt for the next ReadLine call.
		c.terminalMu.RUnlock()
		if c.drawCompleteBlock {
			log.Println("WARNING: Prompt already exist and blocks console.")
			// reset current text prompt to draw new after voice
			if _, err := c.terminal.Write([]byte{'\n'}); err != nil {
				log.Printf("Can't reset current text prompt: %v", err)
			} else {
				c.drawCompleteBlock = false
			}
		}
		helpers.SafeSend(c.drawCompleteChan, struct{}{})    // Signal the prompt loop to continue.
		(*c.bus).Publish(config.MainTopic, "show:cli.draw") // Draw soundbar
	}
}

func handleSave(c *CLI, _ []string) (hide bool, exit bool) {
	(*c.bus).Publish(config.AITopic, "save:history.txt")
	c.formatter.PrintRaw("Conversation history save requested to history.txt.\n")
	return false, false
}

func handlePause(c *CLI, _ []string) (hide bool, exit bool) {
	(*c.bus).Publish(config.MainTopic, "pause:cli.handlePause")
	c.formatter.PrintRaw("VAD pause toggled.\n")
	return false, false
}

func handleDebug(_ *CLI, _ []string) (hide bool, exit bool) {
	config.C.Debug = !config.C.Debug
	log.Printf("Debug mode set to: %t", config.C.Debug)
	return false, false
}

func handleVoice(c *CLI, _ []string) (hide bool, exit bool) {
	config.C.AI.VoiceEnabled = !config.C.AI.VoiceEnabled
	log.Printf("Voice output set to: %t", config.C.AI.VoiceEnabled)
	(*c.bus).Publish(config.AITopic, "restart_session:cli.handleVoice")
	return false, false
}

func handleTools(c *CLI, _ []string) (hide bool, exit bool) {
	config.C.AI.EnableTools = !config.C.AI.EnableTools
	log.Printf("AI tools enabled set to: %t", config.C.AI.EnableTools)
	(*c.bus).Publish(config.AITopic, "restart_session:cli.handleTools")
	return false, false
}

func handleTranscript(c *CLI, _ []string) (hide bool, exit bool) {
	config.C.AI.Transcript = !config.C.AI.Transcript
	log.Printf("Separate transcription step set to: %t", config.C.AI.Transcript)
	(*c.bus).Publish(config.AITopic, "restart_session:cli.handleTranscript")
	return false, false
}

func handleHistory(_ *CLI, _ []string) (hide bool, exit bool) {
	config.C.AI.VoiceHistory = !config.C.AI.VoiceHistory
	log.Printf("Voice history set to: %t", config.C.AI.VoiceHistory)
	return false, false
}

func handleCache(_ *CLI, _ []string) (hide bool, exit bool) {
	config.C.AI.EnableCache = !config.C.AI.EnableCache
	log.Printf("AI caching set to: %t", config.C.AI.EnableCache)
	return false, false
}

func handleThoughts(_ *CLI, _ []string) (hide bool, exit bool) {
	config.C.AI.Thoughts = !config.C.AI.Thoughts
	log.Printf("AI thoughts set to: %t", config.C.AI.Thoughts)
	return false, false
}

func handleThinking(c *CLI, args []string) (hide bool, exit bool) {
	hint := func() {
		c.formatter.PrintRaw(fmt.Sprintf("Available levels: %s, %s, %s, %s, %s\n", dynamic, none, low, medium, high))
	}
	if len(args) != 1 {
		c.formatter.PrintRaw("Usage: /thinking <level>\n")
		hint()
	} else {
		level := strings.ToLower(args[0])
		if value, ok := thinkingLevels[level]; !ok {
			c.formatter.PrintRaw(fmt.Sprintf("Unknown thinking level: %s\n", level))
			hint()
		} else {
			config.C.AI.Thinking = value
			log.Printf("AI thinking budget set to: %s (%d)", level, value)
		}
	}
	return false, false
}

func handleMode(c *CLI, args []string) (hide bool, exit bool) {
	hint := func() {
		c.formatter.PrintRaw(fmt.Sprintf("Available AI modes: %s, %s, %s, %s, %s\n", Prompt, SystemMode, VoiceMode, ImageMode, VideoMode))
	}

	if len(args) != 1 {
		c.formatter.PrintRaw("Usage: /mode <name>\n")
		hint()
		return false, false
	}

	mode := strings.ToLower(args[0])
	if mode == c.mode {
		c.formatter.PrintRaw(fmt.Sprintf("Mode is already set to: %s\n", mode))
		return false, false
	}

	value, ok := modes[mode]
	if !ok {
		c.formatter.PrintRaw(fmt.Sprintf("Unknown AI mode: %s\n", mode))
		hint()
		return false, false
	}

	hide = false
	// When System or Video mode switching, model turn automatically finalized.
	// We avoid prompt draw to deligate it after model response draw.
	if c.mode == SystemMode || c.mode == VideoMode {
		hide = true
	}

	// When user enter to System mode prompt is shell responsibility.
	if value == SystemMode {
		hide = true
	}

	c.setMode(value)
	return hide, false
}

func handlePrompt(c *CLI, args []string) (hide bool, exit bool) {
	if c.mode == SystemMode {
		promptText := strings.TrimSpace(strings.Join(args, " "))
		if promptText != "" {
			helpers.SafeSend(c.cmdChan, promptText)
			return true, false
		}
		c.formatter.PrintRaw("Usage: /prompt <text for AI>\n")
		(*c.bus).Publish(config.MainTopic, "draw:cli.promptUsage")
	}
	return false, false
}

func handleAfk(c *CLI, _ []string) (hide bool, exit bool) {
	c.systemAFK = !c.systemAFK
	log.Printf("System AFK mode set to: %t", c.systemAFK)
	if c.systemAFK {
		c.formatter.PrintRaw("System AFK mode enabled. Turns will be auto-submitted after each command.\n")
	} else {
		c.formatter.PrintRaw("System AFK mode disabled.\n")
	}
	return false, false
}

func handleExit(_ *CLI, _ []string) (hide bool, exit bool) {
	flow.Quit()
	return false, true
}

func handleHelp(c *CLI, _ []string) (hide bool, exit bool) {
	type helpEntry struct {
		command     string
		description string
	}

	mainCommands := []helpEntry{
		{"/mode <name>", fmt.Sprintf("Set AI mode (%s, %s, %s, %s, %s)", Prompt, SystemMode, VoiceMode, ImageMode, VideoMode)},
		{"/pause", "Toggle pausing/resuming voice activity detection"},
		{"/debug", "Toggle debug mode"},
		{"/voice", "Toggle voice responses"},
		{"/tools", "Toggle AI tools (e.g., Google Search)"},
		{"/transcript", "Toggle separate transcription step for voice chat"},
		{"/help", "Display this help message"},
		{"/exit", "Exit the application"},
	}

	systemCommands := []helpEntry{
		{"/prompt <text>", "Send a text prompt to the AI"},
		{"/afk", "Toggle AFK(away from keyboard) mode to auto-submit turns after each command"},
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

// command handles internal CLI commands. It returns (hide, exit) to signal
// the calling loop's next action.
func (c *CLI) command(cmd string) (exit bool) {
	var commandName string
	log.Println("CLI command received:", cmd)
	parts := strings.Fields(cmd)

	if len(parts) == 0 {
		return false // No command entered.
	}
	commandName = parts[0]

	if handler, ok := commandHandlers[commandName]; ok {
		hide, exit := handler(c, parts[1:])
		// After any non-AI, non-exit command, we need to unmute and redraw the
		// prompt. This ensures the UI is correctly updated after commands and mode switches.
		if !hide && !exit {
			(*c.bus).Publish(config.MainTopic, "draw:cli.command")
		}
		return exit
	}

	c.formatter.PrintRaw(fmt.Sprintf("Unknown command: %s\n", commandName))
	(*c.bus).Publish(config.MainTopic, "draw:cli.command.unknown")
	return false
}
