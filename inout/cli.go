package inout

import (
	"bufio"
	"bytes"
	"fmt"
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

const (
	Prompt    = "prompt" // {Prompt} = Allow voice and txt prompts
	VoiceMode = "voice"  // Allow voice output for AI and {Prompt}
	ImageMode = "image"  // Allow send screenshot images with each {Prompt}
	System    = "system" // System CLI integration mode. Allow execute system commands and stream output to AI
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
	promptChan          chan promptRequest
	shellBuffer         strings.Builder
	shellBufferMu       sync.Mutex
	systemInputState    int
	systemCommandBuffer bytes.Buffer
	originalTermState   *term.State
}

const (
	// IMPORTANT: On such terminals like KDE Konsole move down is not works without reserved next line.
	// Sequence: reserve next line for soundbar, move up, print, clear line
	promptPatern = "\n\033[A%s>\033[K"
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
		systemInputState:    stateProxyingToShell,
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

	if c.shellBuffer.Len() == 0 {
		return ""
	}

	content := c.shellBuffer.String()
	c.shellBuffer.Reset()
	return content
}

// PromptForInput displays a prompt to the user and waits for a line of text input.
// It's a blocking call that communicates with the main Run loop via a channel.
func (c *CLI) PromptForInput(prompt string) string {
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

	// Get the file descriptor for stdin and check if it's a terminal.
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Println("Cannot start system shell: stdin is not a terminal.")
		return
	}

	// Save the original terminal state and switch to raw mode.
	var err error
	c.originalTermState, err = term.GetState(fd)
	if err != nil {
		fmt.Printf("Error getting terminal state: %v\n", err)
		return
	}
	if _, err := term.MakeRaw(fd); err != nil {
		fmt.Printf("Error setting terminal to raw mode: %v\n", err)
		return
	}

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

	// Restore the terminal to its original state.
	if c.originalTermState != nil {
		fd := int(os.Stdin.Fd())
		if err := term.Restore(fd, c.originalTermState); err != nil {
			log.Printf("Error restoring terminal state: %v", err)
		}
		c.originalTermState = nil
	}

	c.isSystemShellActive = false
	log.Println("CLI exited system mode. Interactive shell stopped.")
	if c.previousMode != "" {
		c.mode = c.previousMode
		c.previousMode = "" // Reset for the next time.
	} else {
		c.mode = Prompt // Default fallback.
	}
	c.draw()
}

// handleSystemModeInput processes user input when the CLI is in 'system' mode.
// It implements a state machine to differentiate between proxying input directly
// to the interactive shell and capturing a CLI command (e.g., "/prompt").
func (c *CLI) handleSystemModeInput(inputBytes []byte) {
	if !c.isSystemShellActive {
		return
	}
	// In system mode, we have a mini state machine to either proxy
	// input to the shell or read a CLI command (starting with '/').
	for _, b := range inputBytes {
		if c.systemInputState == stateProxyingToShell {
			if b == '/' {
				// Transition to command reading state
				c.systemInputState = stateReadingCommand
				c.systemCommandBuffer.Reset()
				fmt.Print("/") // Echo the slash to the user
			} else {
				// Proxy the byte to the interactive shell
				if err := desktop.C.SendToShell(string(b)); err != nil {
					log.Printf("Error sending input to system shell: %v", err)
				}
			}
		} else { // c.systemInputState == stateReadingCommand
			switch b {
			case '\r', '\n': // Enter key
				fmt.Print("\r\n") // Echo newline
				commandStr := c.systemCommandBuffer.String()
				c.systemCommandBuffer.Reset()
				c.systemInputState = stateProxyingToShell
				if commandStr != "" {
					c.command(commandStr)
				} else {
					c.draw() // User typed "/" then Enter, redraw prompt.
				}
			case 127, 8: // Backspace
				if c.systemCommandBuffer.Len() > 0 {
					c.systemCommandBuffer.Truncate(c.systemCommandBuffer.Len() - 1)
					fmt.Print("\b \b") // Erase character on screen
				}
			case 3: // Ctrl+C or Escape
				fmt.Println("^C")
				c.systemCommandBuffer.Reset()
				c.systemInputState = stateProxyingToShell
				c.draw() // Redraw to get a fresh shell prompt
			default:
				if b >= 32 && b < 127 {
					c.systemCommandBuffer.WriteByte(b)
					fmt.Print(string(b))
				}
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

	helpers.Verify((*c.bus).SubscribeAsync(config.MainTopic, func(event string) {
		config.DebugPrintf("CLI received event: %s\n", event)

		c.modeMu.Lock()
		defer c.modeMu.Unlock()
		switch {
		case strings.HasPrefix(event, "mute:"): // Normal flow
			c.muted = true
		case strings.HasPrefix(event, "draw:"): // Normal flow
			c.muted = false // The prompt is drawn by the main loop after this.
			c.drawLocked()  // Next prompts
		case strings.HasPrefix(event, "block:"): // Critical flow blocking
			c.ready = false
			c.muted = true
		case strings.HasPrefix(event, "ready:"): // Critical flow unblocking
			c.ready = true
			c.muted = false
			c.drawLocked() // Initial prompt
		default:
			config.DebugPrintf("CLI drop event: %s\n", event)
		}
	}, false))

	// Check if the initial mode is 'system' and start the shell if so.
	if c.mode == System {
		c.startSystemShell()
	}

	// This goroutine reads raw bytes from stdin. It cannot be easily cancelled,
	// so it will run for the lifetime of the application. This is an acceptable
	// trade-off for achieving raw terminal I/O.
	inputChan := make(chan []byte)
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
				inputChan <- buf[:n]
			}
		}
	}()

	shutdownChan := flow.GetListener()
	var activePrompt *promptRequest
	var promptBuffer strings.Builder
	// A short timeout to detect the end of a paste or multi-line input.
	pasteTimeout := time.NewTimer(50 * time.Millisecond)
	// The timer should be initially stopped so it doesn't fire immediately.
	if !pasteTimeout.Stop() {
		<-pasteTimeout.C
	}
	for {
		select {
		case <-*shutdownChan: // Listens for Ctrl+C
			log.Println("CLI input handler shutting down.")
			return
		case req := <-c.promptChan:
			activePrompt = &req
			// Mute the regular prompt/soundbar display.
			(*c.bus).Publish(config.MainTopic, "block:cli.prompt.start")
			fmt.Printf("\n%s: ", req.prompt)

		case inputBytes, ok := <-inputChan:
			if !ok {
				log.Println("Stdin closed, CLI input handler shutting down.")
				return
			}

			c.modeMu.Lock()
			currentMode := c.mode
			c.modeMu.Unlock()

			if activePrompt != nil {
				activePrompt.responseChan <- string(inputBytes)
				close(activePrompt.responseChan)
				activePrompt = nil
				(*c.bus).Publish(config.MainTopic, "ready:cli.prompt.done")
				continue // Skip normal processing.
			}

			// Case: Do not process any input until the VAD has signaled it's ready.
			// 		 This prevents sending commands before the AI/LiveAI components are ready.
			// Case: Do not process any input until modal question dialog.
			if !c.ready {
				log.Println("CLI dropping input received during blocked state.")
				continue
			}

			if currentMode == System {
				c.handleSystemModeInput(inputBytes)
				continue
			}

			// In prompt mode, buffer input to handle large pastes that might
			// arrive in multiple chunks from the os.Stdin.Read() call.
			promptBuffer.Write(inputBytes)
			pasteTimeout.Reset(50 * time.Millisecond)

		case <-pasteTimeout.C:
			// The paste timeout fired, meaning the user has stopped typing or pasting.
			// We can now process the entire buffered input as a single command.
			if promptBuffer.Len() == 0 {
				// Nothing was buffered, so just continue.
				continue
			}

			// Get the complete input from the buffer and reset it for the next time.
			line := promptBuffer.String()
			promptBuffer.Reset()

			// In "cooked" mode, the terminal driver handles echoing, backspace, etc.
			// We receive the final, edited text. We just need to process it.
			// The TrimSpace handles any leading/trailing newlines from the input.
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "/") {
				// It's a command for the CLI.
				c.command(trimmedLine[1:])
			} else if trimmedLine != "" {
				// It's a prompt for the AI.
				c.cmdChan <- trimmedLine
			} else {
				// The buffer contained only whitespace. Redraw the prompt.
				c.draw()
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

	log.Println("CLI command received:", cmd)
	parts := strings.Fields(cmd)
	commandName := parts[0]

	// The /prompt command is only active in system mode.
	if c.mode == System && commandName == "prompt" {
		promptText := strings.TrimSpace(strings.Join(parts[1:], " "))
		if promptText != "" {
			c.cmdChan <- promptText
			// Expected model output, so we do not activate new prompt
		} else {
			fmt.Println("Usage: /prompt <text for AI>")
			c.draw()
		}
		return // Command handled, exit the function.
	}

	switch commandName {
	case "exit":
		flow.Quit()
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
				} else {
					if c.mode == System { // Switching out of system mode
						c.handleSystemModeInput([]byte("exit\n"))
					}
				}
				// Switching *out* of system mode is handled when the shell exits.
				c.mode = value
				config.C.Mode = value
				log.Printf("CLI mode set to: %s", value)
				(*c.bus).Publish(config.AITopic, fmt.Sprintf("mode:%s", value))
			}
		}
	case "help":
		fmt.Println("Available commands:")
		fmt.Printf("/mode <name>		- Set AI mode (%s, %s, %s, %s)\n", Prompt, System, VoiceMode, ImageMode)
		fmt.Println("/prompt <text>		- Send a text prompt to the AI (only in 'system' mode)")
		fmt.Println("/debug      		- Toggle debug mode")
		fmt.Println("/voice      		- Toggle voice responses")
		fmt.Println("/tools      		- Toggle AI tools (e.g., Google Search)")
		fmt.Println("/transcript 		- Toggle separate transcription step for voice chat")
		fmt.Println("/help       		- Display this help message")
		fmt.Println("/exit       		- Exit the application")
		fmt.Println("\nPost AI Commands:")
		fmt.Printf("/thinking <level> 	- Set AI thinking budget (%s, %s, %s, %s, %s)\n", dynamic, none, low, medium, high)
		fmt.Println("/thoughts   		- Toggle AI thoughts visibility")
		fmt.Println("/cache      		- Toggle AI caching")
		fmt.Println("/save       		- Save conversation history to history.txt")
		fmt.Println("/history    		- Toggle including voice prompts in conversation history")
	default:
		fmt.Printf("Unknown command: %s\n", commandName)
	}
	c.drawLocked()
}
