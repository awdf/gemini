package inout

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"capgemini.com/config"
	"capgemini.com/flow"
	"github.com/asaskevich/EventBus"
)

// CLI handles reading user input from the command line.
type CLI struct {
	wg        *sync.WaitGroup
	cmdChan   chan<- string
	bus       *EventBus.Bus
	muted     bool
	aiEnabled bool
}

const (
	//IMPORTANT: On such terminals like KDE Konsole move down is not works without reserved next line.
	// Sequence: reserve next line for soundbar, move up, print, clear line
	promptPatern = "\n\033[A>\033[K"
	// Sequence: Save cursor, move to start of line, move down, clear line, print, restore cursor.
	soundbarPatern = "\0337\r\033[B\033[K[%s%s]\0338"
)

// NewCLI creates a new CLI instance.
func NewCLI(wg *sync.WaitGroup, cmdChan chan<- string, bus *EventBus.Bus, aiEnabled bool) *CLI {
	if aiEnabled {
		fmt.Println("Use keyboard to send text prompts to the AI.")
	}

	return &CLI{
		wg:        wg,
		cmdChan:   cmdChan,
		bus:       bus,
		muted:     true,
		aiEnabled: aiEnabled,
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

	(*c.bus).Subscribe("main:topic", func(event string) {
		if config.C.Debug {
			log.Printf("CLI received event: %s\n", event)
		}

		switch {
		case strings.HasPrefix(event, "mute:"):
			c.muted = true
		case strings.HasPrefix(event, "draw:"):
			c.muted = false
			c.draw()
		default:
		}
	})

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
		case line, ok := <-inputChan:
			if !ok {
				log.Println("Stdin closed, CLI input handler shutting down.")
				return
			}
			if line != "" {
				c.cmdChan <- line
			} else {
				c.draw()
			}
		}
	}
}

func (c *CLI) draw() {
	if c.muted {
		return
	}
	fmt.Print(promptPatern) // Initial prompt
}
