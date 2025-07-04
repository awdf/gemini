package input

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sync"

	"capgemini.com/flow"
)

// CLI handles reading user input from the command line.
type CLI struct {
	wg      *sync.WaitGroup
	cmdChan chan<- string
}

// NewCLI creates a new CLI instance.
func NewCLI(wg *sync.WaitGroup, cmdChan chan<- string) *CLI {
	return &CLI{
		wg:      wg,
		cmdChan: cmdChan,
	}
}

// Run starts the CLI input loop. It should be run in a goroutine.
func (c *CLI) Run() {
	defer close(c.cmdChan)
	defer c.wg.Done()

	log.Println("CLI input handler started. Type a message and press Enter to send.")
	fmt.Print("> ") // Initial prompt

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
			}
			fmt.Print("> ") // Prompt for next input
		}
	}
}
