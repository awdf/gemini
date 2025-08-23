package shell

import (
	"bufio"
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

	// To provide a familiar shell environment, we create a temporary rcfile for bash.
	// This script sources the user's personal ~/.bashrc to load their complete
	// environment, including aliases, functions, and any custom completion logic.
	// It then sets our custom prompt, overriding any from the user's file to
	// ensure a consistent look and feel for system mode.
	usr, err := user.Current()
	if err != nil {
		return fmt.Errorf("could not get current user to find .bashrc: %w", err)
	}
	userBashrcPath := filepath.Join(usr.HomeDir, ".bashrc")
	// ':' and '$' is default console text
	// - \[\033[1;91m\]: Start bold (1) and light red (91) color for "system:".
	// - \[\033[1;94m\]: Start bold (1) and light blue (94) color for the directory.
	// - \[\033[0m\]: Reset color to default.
	// The \[ and \] are crucial to tell bash that the color codes are non-printing characters.
	ps1 := "PS1='\\[\033[1;91m\\]system\\[\033[0m\\]:\\[\033[1;94m\\]\\w\\[\033[0m\\]\\$ '"
	// PROMPT_COMMAND is executed just before the shell displays the prompt (PS1).
	// We use it to print a unique marker with the exit code of the last command.
	// This allows the model to programmatically detect when a command has finished.
	promptCommand := fmt.Sprintf(`PROMPT_COMMAND='printf "%s:%%d\n" $?'`, config.C.Shell.CommandEndMarker)
	rcFileContent := fmt.Sprintf(`
# Source the user's .bashrc to load their aliases, functions, and custom completions.
if [ -f %q ]; then
    . %q
fi
# Set our custom prompt, overriding any from the user's .bashrc.
export %s
export %s
`, userBashrcPath, userBashrcPath, ps1, promptCommand)

	// Using os.CreateTemp is safer than ioutil.TempFile.
	tmpfile, err := os.CreateTemp("", "gemini-bashrc-*.sh")
	if err != nil {
		return fmt.Errorf("could not create temporary rcfile: %w", err)
	}

	if _, err := tmpfile.WriteString(rcFileContent); err != nil {
		tmpfile.Close()
		os.Remove(tmpfile.Name())
		return fmt.Errorf("could not write to temporary rcfile: %w", err)
	}
	if err := tmpfile.Close(); err != nil {
		os.Remove(tmpfile.Name())
		return fmt.Errorf("could not close temporary rcfile: %w", err)
	}

	// Use --rcfile to load our custom config and -i to run in interactive mode,
	// which is necessary for completion to work.
	cmd := exec.Command("bash", "--rcfile", tmpfile.Name(), "-i")
	cmd.Dir = e.workspaceDir

	// Get the initial size of the user's terminal.
	initialSize, err := pty.GetsizeFull(os.Stdout)
	if err != nil {
		os.Remove(tmpfile.Name())
		return fmt.Errorf("could not get terminal size: %w", err)
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		os.Remove(tmpfile.Name()) // Clean up on pty start failure.
		return fmt.Errorf("failed to start interactive pty: %w", err)
	}

	// Set the PTY's initial size to match the user's terminal.
	if err := pty.Setsize(ptmx, initialSize); err != nil {
		ptmx.Close()
		os.Remove(tmpfile.Name())
		return fmt.Errorf("could not set pty size: %w", err)
	}

	e.activePty = ptmx
	e.activeCmd = cmd

	log.Println("Interactive shell session started.")

	// This goroutine manages the lifecycle of the interactive session.
	go func() {
		// Ensure the temporary rcfile is cleaned up when the shell process exits.
		defer os.Remove(tmpfile.Name())
		// Defer closing the channel to ensure it's closed when the goroutine exits.

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
	log.Println("Interactive shell session resources cleaned up.")

	return err
}
