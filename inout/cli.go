package inout

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"

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

var modes = map[string]string{
	Prompt:    Prompt,
	System:    System,
	VoiceMode: VoiceMode,
	ImageMode: ImageMode,
}

// confirmRequest is used to pass a confirmation prompt and receive a response
// between the blocking Confirm method and the non-blocking Run loop.
type confirmRequest struct {
	prompt       string
	responseChan chan bool
}

// promptRequest is used to pass a text prompt and receive a string response
// between the blocking PromptForInput method and the non-blocking Run loop.
type promptRequest struct {
	prompt       string
	responseChan chan string
}

// CLI handles reading user input from the command line.
type CLI struct {
	wg                  *sync.WaitGroup
	cmdChan             chan<- string
	bus                 *EventBus.Bus
	muted               bool
	isSystemShellActive bool
	aiEnabled           bool
	ready               bool
	mode                string
	confirmChan         chan confirmRequest
	promptChan          chan promptRequest
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
		mode:                config.C.Mode,
		confirmChan:         make(chan confirmRequest),
		promptChan:          make(chan promptRequest),
	}
}

// Confirm displays a prompt to the user and waits for a 'y' or 'n' response.
// It's a blocking call that communicates with the main Run loop via a channel.
func (c *CLI) Confirm(prompt string) bool {
	req := confirmRequest{
		prompt:       prompt,
		responseChan: make(chan bool, 1), // Buffered to prevent blocking.
	}
	c.confirmChan <- req
	log.Printf("Waiting for user confirmation for prompt: '%s'", prompt)
	return <-req.responseChan
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
	// This channel will receive output from the interactive shell.
	outputChan := make(chan string, 100)
	go func() {
		for line := range outputChan {
			// Publish each line of shell output to the event bus
			// so other components (like LiveAI) can listen.
			(*c.bus).Publish(config.AITopic, fmt.Sprintf("shell_output:%s", line))
		}
		log.Println("CLI shell output publisher finished.")
	}()
	if err := desktop.C.StartInteractiveShell(outputChan); err != nil {
		fmt.Printf("Error starting system shell: %v\n", err)
	} else {
		c.isSystemShellActive = true
		log.Println("Entered system mode. Interactive shell started.")
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
	log.Println("Exited system mode. Interactive shell stopped.")
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

		switch {
		case strings.HasPrefix(event, "mute:"): // Normal flow
			c.muted = true
		case strings.HasPrefix(event, "draw:"): // Normal flow
			c.muted = false
			c.draw() // Next prompts
		case strings.HasPrefix(event, "block:"): // Critical flow blocking
			c.ready = false
			c.muted = true
		case strings.HasPrefix(event, "ready:"): // Critical flow unblocking
			c.ready = true
			c.muted = false
			c.draw() // Initial prompt
		default:
			config.DebugPrintf("CLI drop event: %s\n", event)
		}
	}, false))

	// Check if the initial mode is 'system' and start the shell if so.
	if c.mode == System {
		c.startSystemShell()
	}

	scanner := bufio.NewScanner(os.Stdin)
	inputChan := make(chan string)

	// Goroutine to read from stdin, as scanner.Scan() is blocking.
	go func() {
		for scanner.Scan() {
			inputChan <- scanner.Text()
		}
		close(inputChan)
	}()

	shutdownChan := flow.GetListener()
	var activeConfirmation *confirmRequest
	var activePrompt *promptRequest

	for {
		select {
		case <-*shutdownChan: // Listens for Ctrl+C
			log.Println("CLI input handler shutting down.")
			return
		case req := <-c.confirmChan:
			activeConfirmation = &req
			// Mute the regular prompt/soundbar display.
			(*c.bus).Publish(config.MainTopic, "block:cli.confirm.start")
			// Print the confirmation prompt. The newline handles cases where a prompt was already visible.
			fmt.Printf("\n%s [y/N]: ", req.prompt)
		case req := <-c.promptChan:
			activePrompt = &req
			// Mute the regular prompt/soundbar display.
			(*c.bus).Publish(config.MainTopic, "block:cli.prompt.start")
			fmt.Printf("\n%s: ", req.prompt)

		case firstLine, ok := <-inputChan:
			if !ok {
				log.Println("Stdin closed, CLI input handler shutting down.")
				return
			}

			if activeConfirmation != nil {
				response := strings.ToLower(strings.TrimSpace(firstLine)) == "y"
				activeConfirmation.responseChan <- response
				close(activeConfirmation.responseChan)
				activeConfirmation = nil
				(*c.bus).Publish(config.MainTopic, "ready:cli.confirm.done")
				continue // Skip normal processing.
			}

			if activePrompt != nil {
				activePrompt.responseChan <- firstLine
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

			// Check if the input is a command (starts with '/'). This is common to all modes.
			if strings.HasPrefix(firstLine, "/") {
				c.command(firstLine[1:])
				continue
			}

			// If it's not a command, handle it based on the mode.
			if c.mode == System {
				// In system mode, send input to the interactive shell.
				if c.isSystemShellActive {
					if err := desktop.C.SendToShell(firstLine + "\n"); err != nil {
						log.Printf("Error sending input to system shell: %v", err)
					}
				} else {
					log.Println("System mode is active, but interactive shell is not running yet. Input ignored.")
				}
				continue
			}

			// In other modes (prompt, voice, image), non-command input is a prompt for the AI.
			// We'll collect subsequent lines that arrive in a very short window.
			lines := []string{firstLine}
			pasteTimeout := time.NewTimer(50 * time.Millisecond) // A small window to catch subsequent pasted lines.

		collecting:
			for {
				select {
				case nextLine, ok := <-inputChan:
					if !ok {
						pasteTimeout.Stop()
						break collecting
					}
					lines = append(lines, nextLine)
					// Reset the timer each time a new line arrives quickly.
					if !pasteTimeout.Stop() {
						<-pasteTimeout.C // Drain the channel if Stop() returns false.
					}
					pasteTimeout.Reset(50 * time.Millisecond)
				case <-pasteTimeout.C:
					break collecting // Timer fired, we're done collecting.
				}
			}

			fullPrompt := strings.Join(lines, "\n")
			if fullPrompt != "" {
				c.cmdChan <- fullPrompt
			} else {
				c.draw()
			}
		}
	}
}

func (c *CLI) draw() {
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
	log.Println("CLI command received:", cmd)
	parts := strings.Fields(cmd)
	commandName := parts[0]

	// The /prompt command is only active in system mode.
	if c.mode == System && commandName == "prompt" {
		promptText := strings.Join(parts[1:], " ")
		if promptText != "" {
			c.cmdChan <- promptText
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
					c.startSystemShell()
				} else {
					// This will also handle switching from system to another mode.
					c.stopSystemShell()
				}
				c.mode = value
				config.C.Mode = value
				log.Printf("AI mode set to: %s", value)
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
	c.draw()
}
