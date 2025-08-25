package shell

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/asaskevich/EventBus"
	"github.com/creack/pty"

	"gemini/config"
	"gemini/flow"
)

// Executor is responsible for executing shell commands in a pseudo-terminal.
type Executor struct {
	workspaceDir    string
	bus             *EventBus.Bus
	ptyMutex        sync.Mutex
	activePty       *os.File
	activeCmd       *exec.Cmd
	commandDoneChan chan int
	commandMutex    sync.Mutex
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

// buildRCFileContent generates the content for a temporary bash rcfile.
// This file sources the user's existing .bashrc and then sets a custom
// prompt (PS1) and a PROMPT_COMMAND to inject command-end markers.
func buildRCFileContent() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("could not get current user to find .bashrc: %w", err)
	}
	userBashrcPath := filepath.Join(usr.HomeDir, ".bashrc")

	// PS1 defines the shell prompt's appearance.
	// - \[\033[1;91m\]: Start bold (1) and light red (91) color for "system:".
	// - \[\033[1;94m\]: Start bold (1) and light blue (94) color for the directory (\w).
	// - \[\033[0m\]: Reset color to default.
	// The \[ and \] are crucial to tell bash that the color codes are non-printing characters,
	// preventing line-wrapping issues.
	ps1 := "PS1='\\[\033[1;91m\\]system\\[\033[0m\\]:\\[\033[1;94m\\]\\w\\[\033[0m\\]\\$ '"

	// PROMPT_COMMAND is executed just before the shell displays the prompt (PS1).
	// We use it to print a unique marker with the exit code ($?) of the last command.
	// The marker is framed with non-printable SOH (0x01) and STX (0x02) bytes
	// to allow for robust, byte-level filtering of the output stream.
	promptCommand := promptCommand()

	// The rcfile content first sources the user's personal .bashrc to load their
	// complete environment, then exports our custom prompt and command marker.
	return fmt.Sprintf(`
# Source the user's .bashrc to load their aliases, functions, and custom completions.
if [ -f %q ]; then
    . %q
fi
# Set our custom prompt, overriding any from the user's .bashrc.
export %s
export %s
`, userBashrcPath, userBashrcPath, ps1, promptCommand), nil
}

// createAndWriteRCFile creates a temporary bash rcfile and writes the provided content to it.
// It returns the path to the created file, which the caller is responsible for deleting.
func createAndWriteRCFile(content string) (string, error) {
	tmpfile, err := os.CreateTemp("", "gemini-bashrc-*.sh")
	if err != nil {
		return "", fmt.Errorf("could not create temporary rcfile: %w", err)
	}

	if _, err := tmpfile.WriteString(content); err != nil {
		tmpfile.Close()
		os.Remove(tmpfile.Name()) // Clean up on write error
		return "", fmt.Errorf("could not write to temporary rcfile: %w", err)
	}

	if err := tmpfile.Close(); err != nil {
		os.Remove(tmpfile.Name()) // Clean up on close error
		return "", fmt.Errorf("could not close temporary rcfile: %w", err)
	}

	return tmpfile.Name(), nil
}

// prepareBashCommand creates a temporary rcfile and prepares an exec.Cmd to start
// an interactive bash session using that file. It returns the command and the path
// to the temporary rcfile which must be cleaned up by the caller.
func (e *Executor) prepareBashCommand() (*exec.Cmd, string, error) {
	rcFileContent, err := buildRCFileContent()
	if err != nil {
		return nil, "", fmt.Errorf("could not build rcfile content: %w", err)
	}

	rcFilePath, err := createAndWriteRCFile(rcFileContent)
	if err != nil {
		return nil, "", err // The error from createAndWriteRCFile is already descriptive.
	}

	// Use --rcfile to load our custom config and -i to run in interactive mode,
	// which is necessary for completion to work.
	cmd := exec.Command("bash", "--rcfile", rcFilePath, "-i")
	cmd.Dir = e.workspaceDir

	return cmd, rcFilePath, nil
}

// StartInteractive starts a persistent `sh` process in a PTY.
// Its output is streamed to the provided channel.
func (e *Executor) StartInteractive(outputChan chan<- string) error {
	e.ptyMutex.Lock()
	defer e.ptyMutex.Unlock()

	if e.activeCmd != nil {
		return fmt.Errorf("an interactive session is already running")
	}

	cmd, rcFilePath, err := e.prepareBashCommand()
	if err != nil {
		return err
	}

	// Get the initial size of the user's terminal.
	// initialSize, err := pty.GetsizeFull(os.Stdout)
	// if err != nil {
	// 	os.Remove(tmpfile.Name())
	// 	return fmt.Errorf("could not get terminal size: %w", err)
	// }

	ptmx, err := pty.Start(cmd)
	if err != nil {
		os.Remove(rcFilePath) // Clean up on pty start failure.
		return fmt.Errorf("failed to start interactive pty: %w", err)
	}

	// Set the PTY's initial size to match the user's terminal.
	// if err := pty.Setsize(ptmx, initialSize); err != nil {
	// 	ptmx.Close()
	// 	os.Remove(tmpfile.Name())
	// 	return fmt.Errorf("could not set pty size: %w", err)
	// }

	e.activePty = ptmx
	e.activeCmd = cmd

	log.Println("Interactive shell session started.")

	// This goroutine manages the I/O and lifecycle for the active PTY session.
	go e.manageSessionLifecycle(rcFilePath, ptmx, outputChan)

	return nil
}

// SendCommand sends a command to the interactive shell and returns a channel
// that is closed when the command has finished executing. The command's exit
// code is sent on the channel before it is closed.
func (e *Executor) SendCommand(command string) (<-chan int, error) {
	e.commandMutex.Lock()
	defer e.commandMutex.Unlock()

	if e.commandDoneChan != nil {
		return nil, fmt.Errorf("another command is already in progress")
	}

	// Use a buffered channel of size 1. This allows the sender goroutine
	// to send the exit code and close the channel without blocking, even if
	// the receiver isn't ready yet.
	doneChan := make(chan int, 1)
	e.commandDoneChan = doneChan

	// Send the command to the shell. A newline is required to execute it.
	if err := e.SendInput(command + "\n"); err != nil {
		// If sending fails, clean up and return the error.
		e.commandDoneChan = nil
		close(doneChan) // Close it so the caller doesn't block forever.
		return nil, err
	}

	return doneChan, nil
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
	log.Println("Interactive shell session resources cleaned up.")

	return err
}

// manageSessionLifecycle handles the I/O and lifecycle for an active PTY session.
// It runs in a dedicated goroutine.
func (e *Executor) manageSessionLifecycle(rcfilePath string, ptmx *os.File, outputChan chan<- string) {
	// Ensure the temporary rcfile is cleaned up when the shell process exits.
	defer os.Remove(rcfilePath)

	// --- Resize Handling ---
	// Get a channel for window resize signals from the flow package.
	ch := flow.GetWinchListener()
	go func() {
		for range *ch { // The loop will exit when the channel is closed.
			// When a resize signal is received, get the new size from stdout
			// and apply it to the PTY.
			e.ptyMutex.Lock()
			if e.activePty != nil {
				if err := pty.InheritSize(os.Stdout, e.activePty); err != nil {
					log.Printf("Error resizing PTY: %v", err)
				}
			}
			e.ptyMutex.Unlock()
		}
	}()
	// When this goroutine exits, unregister the listener.
	defer flow.StopWinchListener(ch)

	// Create a pipe. The pty output will be written to the pipe's writer.
	// A goroutine will read from the pipe's reader and send to the channel.
	pr, pw := io.Pipe()

	// Create a custom provider to filter the command-end marker from stdout,
	// so the user doesn't see it, but the AI does.
	stdoutFilter := NewFilteringProvider(os.Stdout, config.C.Shell.GetCommandEndMarker())

	// This goroutine reads from the pipe, sends line-by-line to the channel,
	// and is responsible for closing the channel when it's done.
	go stdoutFilter.Read(outputChan, e, pr)

	// Create a MultiWriter to simultaneously write to the user's (filtered) stdout and the internal pipe.
	multiWriter := io.MultiWriter(stdoutFilter, pw)

	// This will block until the command is done, copying output to both writers.
	// We can ignore the error, as it will be an expected one (EIO or EOF)
	// when the pty is closed by StopInteractive.
	_, _ = io.Copy(multiWriter, ptmx)

	// After io.Copy returns, the command has finished. We must close the pipe
	// writer to signal EOF to the scanner goroutine, allowing it to exit gracefully.
	pw.Close()

	// If a command was running when the shell died, notify the waiter.
	e.commandMutex.Lock()
	if e.commandDoneChan != nil {
		log.Println("Interactive shell exited while a command was in progress. Notifying waiter.")
		// We don't have an exit code, so we just close the channel.
		// This will result in a read of (0, false) on the other side.
		close(e.commandDoneChan)
		e.commandDoneChan = nil
	}
	e.commandMutex.Unlock()
}
