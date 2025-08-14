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
	"sync"

	"github.com/asaskevich/EventBus"
	"github.com/creack/pty"

	"gemini/config"
)

// Executor is responsible for executing shell commands in a pseudo-terminal.
type Executor struct {
	workspaceDir string
	bus          *EventBus.Bus
	// State for the interactive session
	ptyMutex  sync.Mutex
	activePty *os.File
	activeCmd *exec.Cmd
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

// ExecuteWithInput demonstrates how to send input to a command in response to a prompt.
// It reads the command's output, and when it sees `promptToExpect`, it writes `inputToSend`.
func (e *Executor) ExecuteWithInput(command string, promptToExpect string, inputToSend string) (string, error) {
	(*e.bus).Publish(config.MainTopic, "mute:shell.execute.interactive")

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = e.workspaceDir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		(*e.bus).Publish(config.MainTopic, "draw:shell.execute.interactive.fail")
		return "", fmt.Errorf("failed to start pty: %w", err)
	}
	// Close the pty when the function returns. This will also cause the reading goroutine to exit.
	defer func() { _ = ptmx.Close() }()
	// Restore the CLI prompt when we're done.
	defer (*e.bus).Publish(config.MainTopic, "draw:shell.execute.interactive.done")

	// This goroutine will handle reading output and writing input.
	var outputBuf bytes.Buffer
	inputSent := false
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Create a MultiWriter to simultaneously write to the user's stdout and our capture buffer.
		mw := io.MultiWriter(os.Stdout, &outputBuf)

		// This buffer is for reading from the pty.
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				// An error (like io.EOF) is expected when the process exits.
				break
			}

			// Write the read chunk to both stdout and our capture buffer.
			if _, wErr := mw.Write(buf[:n]); wErr != nil {
				log.Printf("Error writing to multi-writer: %v", wErr)
				break
			}

			// Check if we should send the input.
			if !inputSent && strings.Contains(outputBuf.String(), promptToExpect) {
				if _, wErr := ptmx.Write([]byte(inputToSend + "\n")); wErr != nil {
					log.Printf("Error writing input to pty: %v", wErr)
					break
				}
				inputSent = true
			}
		}
	}()

	// Wait for the command to finish execution.
	waitErr := cmd.Wait()

	// Wait for the reading goroutine to finish processing all output.
	<-done

	return outputBuf.String(), waitErr
}

// StartInteractive starts a persistent `sh` process in a PTY.
// Its output is streamed to the provided channel.
func (e *Executor) StartInteractive(outputChan chan<- string) error {
	e.ptyMutex.Lock()
	defer e.ptyMutex.Unlock()

	if e.activeCmd != nil {
		return fmt.Errorf("an interactive session is already running")
	}

	// Start a generic shell, not a specific command.
	cmd := exec.Command("sh")
	cmd.Dir = e.workspaceDir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start interactive pty: %w", err)
	}

	e.activePty = ptmx
	e.activeCmd = cmd

	log.Println("Interactive shell session started.")

	// This goroutine manages the lifecycle of the interactive session.
	go func() {
		// This defer block ensures cleanup happens when the goroutine exits.
		defer func() {
			e.ptyMutex.Lock()
			if e.activePty != nil {
				_ = e.activePty.Close()
				e.activePty = nil
			}
			if e.activeCmd != nil {
				// Wait for the process to finish to prevent zombies.
				_ = e.activeCmd.Wait()
				e.activeCmd = nil
			}
			e.ptyMutex.Unlock()
			close(outputChan)
			(*e.bus).Publish(config.MainTopic, "draw:shell.interactive.done")
			log.Println("Interactive shell session resources cleaned up.")
		}()

		(*e.bus).Publish(config.MainTopic, "mute:shell.interactive.start")

		// Stream output directly from ptmx to both the user's stdout and the output channel.
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println(line) // Let the user see the output in real-time.
			outputChan <- line
		}

		if err := scanner.Err(); err != nil {
			// This error is expected when the PTY is closed.
			config.DebugPrintf("Interactive shell scanner finished with error: %v", err)
		}
	}()

	return nil
}

// SendInput sends a string to the active interactive shell's stdin.
func (e *Executor) SendInput(input string) error {
	e.ptyMutex.Lock()
	defer e.ptyMutex.Unlock()

	if e.activePty == nil {
		return fmt.Errorf("no active interactive session to send input to")
	}

	_, err := e.activePty.Write([]byte(input))
	return err
}

// StopInteractive terminates the active interactive shell session.
func (e *Executor) StopInteractive() error {
	e.ptyMutex.Lock()
	defer e.ptyMutex.Unlock()

	if e.activeCmd == nil || e.activeCmd.Process == nil {
		return fmt.Errorf("no active interactive session to stop")
	}

	// Killing the process will cause the PTY read in the goroutine to fail,
	// which will trigger the deferred cleanup logic in that goroutine.
	return e.activeCmd.Process.Kill()
}
