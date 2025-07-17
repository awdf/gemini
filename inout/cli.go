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
	"gemini/flow"
	"gemini/helpers"
)

// CLI handles reading user input from the command line.
type CLI struct {
	wg         *sync.WaitGroup
	cmdChan    chan<- string
	bus        *EventBus.Bus
	muted      bool
	aiEnabled  bool
	warmUpDone bool
}

const (
	// IMPORTANT: On such terminals like KDE Konsole move down is not works without reserved next line.
	// Sequence: reserve next line for soundbar, move up, print, clear line
	promptPatern = "\n\033[A>\033[K"
	// Sequence: Save cursor, move to start of line, move down, clear line, print, restore cursor.
	soundbarPatern = "\0337\r\033[B\033[K[%s%s]\0338"
)

var thinkingLevels = map[string]int32{
	"dynamic": -1,
	"none":    0,
	"low":     512,
	"medium":  8192,
	"high":    24576,
}

// NewCLI creates a new CLI instance.
func NewCLI(wg *sync.WaitGroup, cmdChan chan<- string, bus *EventBus.Bus, aiEnabled bool) *CLI {
	if aiEnabled {
		fmt.Println("Use keyboard to send text prompts to the AI.")
	}

	return &CLI{
		wg:         wg,
		cmdChan:    cmdChan,
		bus:        bus,
		muted:      true,
		aiEnabled:  aiEnabled,
		warmUpDone: false,
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

	helpers.Verify((*c.bus).SubscribeAsync("main:topic", func(event string) {
		config.DebugPrintf("CLI received event: %s\n", event)

		switch {
		case strings.HasPrefix(event, "mute:"):
			c.muted = true
		case strings.HasPrefix(event, "draw:"):
			c.muted = false
			c.draw() // Next prompts
		case strings.HasPrefix(event, "ready:"):
			c.warmUpDone = true
			c.muted = false
			c.draw() // Initial prompt
		default:
			config.DebugPrintf("CLI drop event: %s\n", event)
		}
	}, false))

	scanner := bufio.NewScanner(os.Stdin)
	inputChan := make(chan string)

	// Goroutine to read from stdin, as scanner.Scan() is blocking.
	go func() {
		for scanner.Scan() {
			inputChan <- scanner.Text()
		}
		close(inputChan)
	}()

	for {
		select {
		case <-*flow.GetListener(): // Listens for Ctrl+C
			log.Println("CLI input handler shutting down.")
			return
		case firstLine, ok := <-inputChan:
			if !ok {
				log.Println("Stdin closed, CLI input handler shutting down.")
				return
			}

			// If the first line looks like a command, process it immediately
			// and don't wait for more lines. This preserves the existing behavior
			// for single-line commands and prevents multi-line pastes from being
			// misinterpreted as a single, large command.
			if strings.HasPrefix(firstLine, "/") {
				c.command(firstLine[1:])
				continue
			}

			// It's not a command, so it might be part of a multi-line paste.
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
	if c.muted || !c.warmUpDone {
		return
	}
	fmt.Print(promptPatern) // Initial prompt
	// Publish a separate event for the sound bar AFTER the CLI prompt is printed.
	// This creates a specific drawing order and prevents a race condition
	// where the sound bar could be drawn before or over the prompt.
	(*c.bus).Publish("main:topic", "show:cli.run")
}

func (c *CLI) command(cmd string) {
	log.Println("Received command:", cmd)
	parts := strings.Fields(cmd)
	commandName := parts[0]

	switch commandName {
	case "exit":
		flow.Quit()
	case "save":
		(*c.bus).Publish("ai:topic", "save:history.txt")
		fmt.Println("Conversation history save requested to history.txt.")
	case "debug":
		config.C.Debug = !config.C.Debug
		log.Printf("Debug mode set to: %t", config.C.Debug)
	case "voice":
		config.C.AI.VoiceEnabled = !config.C.AI.VoiceEnabled
		log.Printf("Voice output set to: %t", config.C.AI.VoiceEnabled)
	case "tools":
		config.C.AI.EnableTools = !config.C.AI.EnableTools
		log.Printf("AI tools enabled set to: %t", config.C.AI.EnableTools)
	case "transcript":
		config.C.AI.Transcript = !config.C.AI.Transcript
		log.Printf("Separate transcription step set to: %t", config.C.AI.Transcript)
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
		if len(parts) != 2 {
			fmt.Println("Usage: /thinking <level>")
			fmt.Println("Available levels: dynamic, none, low, medium, high")
		} else {
			level := strings.ToLower(parts[1])
			value, ok := thinkingLevels[level]
			if !ok {
				fmt.Printf("Unknown thinking level: %s\n", level)
				fmt.Println("Available levels: dynamic, none, low, medium, high")
			} else {
				config.C.AI.Thinking = value
				log.Printf("AI thinking budget set to: %s (%d)", level, value)
			}
		}
	case "help":
		fmt.Println("Available commands:")
		fmt.Println("/exit       		- Exit the application")
		fmt.Println("/save       		- Save conversation history to history.txt")
		fmt.Println("/debug      		- Toggle debug mode")
		fmt.Println("/voice      		- Toggle voice responses")
		fmt.Println("/tools      		- Toggle AI tools (e.g., Google Search)")
		fmt.Println("/transcript 		- Toggle separate transcription step for voice chat")
		fmt.Println("/history    		- Toggle including voice prompts in conversation history")
		fmt.Println("/cache      		- Toggle AI caching")
		fmt.Println("/thoughts   		- Toggle AI thoughts visibility")
		fmt.Println("/thinking <level> 	- Set AI thinking budget (dynamic, none, low, medium, high)")
		fmt.Println("/help       		- Display this help message")
	default:
		fmt.Printf("Unknown command: %s\n", commandName)
	}
	c.draw()
}
