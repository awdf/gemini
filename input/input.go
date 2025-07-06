package input

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sync"

	"capgemini.com/config"
	"capgemini.com/flow"
	"github.com/asaskevich/EventBus"
)

// CLI handles reading user input from the command line.
type CLI struct {
	wg      *sync.WaitGroup
	cmdChan chan<- string
	bus     *EventBus.Bus
}

// NewCLI creates a new CLI instance.
func NewCLI(wg *sync.WaitGroup, cmdChan chan<- string, bus *EventBus.Bus) *CLI {
	return &CLI{
		wg:      wg,
		cmdChan: cmdChan,
		bus:     bus,
	}
}

// Run starts the CLI input loop. It should be run in a goroutine.
func (c *CLI) Run() {
	defer close(c.cmdChan)
	defer c.wg.Done()

	(*c.bus).Subscribe("main:topic", func(event string) {
		if config.C.Debug {
			log.Printf("CLI received event: %s\n", event)
		}

		if event == "draw" {
			c.draw()
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
	log.Println("CLI input handler started. Type a message and press Enter to send.")
	fmt.Print("> ") // Initial prompt
}
