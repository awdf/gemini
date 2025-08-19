package shell

import (
	"bufio"
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

// StartInteractive starts a persistent `sh` process in a PTY.
// Its output is streamed to the provided channel.
func (e *Executor) StartInteractive(outputChan chan<- string) error {
	e.ptyMutex.Lock()
	defer e.ptyMutex.Unlock()

	if e.activeCmd != nil {
		return fmt.Errorf("an interactive session is already running")
	}

	// Start a generic shell, not a specific command.
	// Use bash to get more advanced features like PS1 prompt string expansion (\w, \$, etc.).
	// Use --noprofile and --norc to prevent user startup files (like .bashrc or .profile)
	// from overriding the custom PS1 prompt we are setting.
	cmd := exec.Command("bash", "--noprofile", "--norc")
	cmd.Dir = e.workspaceDir
	// Set a custom, colored and bold prompt for system mode.
	// - \[\033[1;91m\]: Start bold (1) and light red (91) color.
	// - \[\033[1;94m\]: Start bold (1) and light blue (94) color for the directory.
	// - \[\033[0m\]: Reset color to default.
	// The \[ and \] are crucial to tell bash that the color codes are non-printing characters.
	cmd.Env = append(os.Environ(), "PS1=\\[\033[1;91m\\]system\\[\033[0m\\]:\\[\033[1;94m\\]\\w\\[\033[0m\\]\\$ ")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start interactive pty: %w", err)
	}

	e.activePty = ptmx
	e.activeCmd = cmd

	log.Println("Interactive shell session started.")

	// This goroutine manages the lifecycle of the interactive session.
	go func() {
		// Defer closing the channel to ensure it's closed when the goroutine exits.
		(*e.bus).Publish(config.MainTopic, "mute:shell.interactive.start")

		// Create a pipe. The pty output will be written to the pipe's writer.
		// A goroutine will read from the pipe's reader and send to the channel.
		pr, pw := io.Pipe()

		// This goroutine reads from the pipe, sends line-by-line to the channel,
		// and is responsible for closing the channel when it's done.
		go func() {
			defer close(outputChan)
			scanner := bufio.NewScanner(pr)
			for scanner.Scan() {
				outputChan <- scanner.Text()
			}
			if err := scanner.Err(); err != nil {
				config.DebugPrintf("Interactive shell pipe scanner stopped: %v", err)
			}
		}()

		// Create a MultiWriter to simultaneously write to the user's stdout and the pipe.
		multiWriter := io.MultiWriter(os.Stdout, pw)

		// This will block until the command is done, copying output to both writers.
		// We can ignore the error, as it will be an expected one (EIO or EOF)
		// when the pty is closed by StopInteractive.
		_, _ = io.Copy(multiWriter, ptmx)

		// After io.Copy returns, the command has finished. We must close the pipe
		// writer to signal EOF to the scanner goroutine, allowing it to exit gracefully.
		pw.Close()
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
	err := e.activeCmd.Process.Kill()

	if e.activePty != nil {
		_ = e.activePty.Close()
		e.activePty = nil
	}
	if e.activeCmd != nil {
		// Wait for the process to finish to prevent zombies.
		_ = e.activeCmd.Wait()
		e.activeCmd = nil
	}
	// The channel is now closed by the writer goroutine in StartInteractive.
	(*e.bus).Publish(config.MainTopic, "draw:shell.interactive.done")
	log.Println("Interactive shell session resources cleaned up.")

	return err
}
