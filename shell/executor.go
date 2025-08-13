package shell

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/asaskevich/EventBus"
	"github.com/creack/pty"

	"gemini/config"
)

// Executor is responsible for executing shell commands in a pseudo-terminal.
type Executor struct {
	workspaceDir string
	bus          *EventBus.Bus
}

// NewExecutor creates a new shell command executor.
func NewExecutor(bus *EventBus.Bus) (*Executor, error) {
	// Resolve the workspace directory path, expanding tilde.
	workspaceDir := config.C.AI.WorkspaceDir
	if strings.HasPrefix(workspaceDir, "~/") {
		usr, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("could not get current user to expand workspace path: %w", err)
		}
		workspaceDir = strings.Replace(workspaceDir, "~", usr.HomeDir, 1)
	}

	// Ensure the workspace directory exists.
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return nil, fmt.Errorf("could not create workspace directory '%s': %w", workspaceDir, err)
	}

	log.Printf("Shell executor initialized. Commands will run in: %s", workspaceDir)

	return &Executor{
		workspaceDir: workspaceDir,
		bus:          bus,
	}, nil
}

// Execute runs a command in a pseudo-terminal, streaming its output to stdout
// and returning the captured output as a string.
func (e *Executor) Execute(command string) (string, error) {
	// Mute the CLI prompt and soundbar before executing the command.
	(*e.bus).Publish(config.MainTopic, "mute:shell.execute")

	// Use the system's default shell to interpret the command.
	// This allows for shell features like pipes, redirection, etc.
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = e.workspaceDir // Run the command in the configured workspace.

	// Start the command in a pseudo-terminal.
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to start pty: %w", err)
	}
	// Make sure to close the pty at the end. This will terminate the goroutine.
	defer func() { _ = ptmx.Close() }()

	// Set the pty's window size to match the current terminal's size.
	// This is important for programs that format their output based on terminal width.
	if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
		log.Printf("WARNING: could not set pty size: %v", err)
	}

	// Create a buffer to capture the command's output for the AI.
	var outputBuf bytes.Buffer
	// Create a MultiWriter to simultaneously write to the user's stdout and the AI's buffer.
	multiWriter := io.MultiWriter(os.Stdout, &outputBuf)

	// In a goroutine, copy the pty's output to the multi-writer.
	// This runs until the pty is closed.
	go func() {
		// We can ignore the error, as it will be an expected one (EIO or EOF)
		// when the pty is closed by the defer statement.
		_, _ = io.Copy(multiWriter, ptmx)
	}()

	// Wait for the command to finish.
	err = cmd.Wait()
	return outputBuf.String(), err
}

// ExecuteStream runs a command and streams its output to the provided channel.
// It returns immediately, with errors from command execution logged asynchronously.
func (e *Executor) ExecuteStream(command string, outputChan chan<- string) error {
	(*e.bus).Publish(config.MainTopic, "mute:shell.execute.stream")

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = e.workspaceDir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		(*e.bus).Publish(config.MainTopic, "draw:shell.execute.stream.fail")
		close(outputChan)
		return fmt.Errorf("failed to start pty: %w", err)
	}

	go func() {
		defer func() { _ = ptmx.Close() }()
		// defer (*e.bus).Publish(config.MainTopic, "draw:shell.execute.stream.done")
		// The outputChan is closed by the scanner goroutine when it's done.

		if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
			log.Printf("WARNING: could not set pty size: %v", err)
		}

		// Create a pipe. The pty output will be written to the pipe's writer.
		// A goroutine will read from the pipe's reader and send to the channel.
		pr, pw := io.Pipe()

		// This goroutine reads from the pipe and sends line-by-line to the channel.
		go func() {
			defer close(outputChan)
			scanner := bufio.NewScanner(pr)
			for scanner.Scan() {
				outputChan <- scanner.Text()
			}
			if err := scanner.Err(); err != nil {
				log.Printf("Shell stream pipe scanner error: %v", err)
			}
		}()

		// Create a MultiWriter to simultaneously write to the user's stdout and the pipe.
		multiWriter := io.MultiWriter(os.Stdout, pw)

		// This will block until the command is done, copying output to both writers.
		_, _ = io.Copy(multiWriter, ptmx)

		// After io.Copy returns, the command has finished. We must close the pipe
		// writer to signal EOF to the scanner goroutine, allowing it to exit gracefully.
		pw.Close()

		if err := cmd.Wait(); err != nil {
			log.Printf("Shell stream command finished with error: %v", err)
		}
	}()

	return nil
}
