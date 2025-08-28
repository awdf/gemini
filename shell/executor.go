package shell

import (
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
	"golang.org/x/term"

	"gemini/config"
	"gemini/flow"
)

const (
	DisableOutput     = false
	markersPerCommand = 2
)

// Executor is responsible for executing shell commands in a pseudo-terminal.
type Executor struct {
	workspaceDir       string
	bus                *EventBus.Bus
	ptyMutex           sync.Mutex
	activePty          *os.File
	activeCmd          *exec.Cmd
	commandDoneChan    chan int
	shellReadyChan     chan struct{}
	commandMarkerCount int
	commandMutex       sync.Mutex
	rcFilePath         string
	isShellReady       bool
}

// NewExecutor creates a new shell command executor.
func NewExecutor(bus *EventBus.Bus, workspaceDir string) (*Executor, error) {
	// Resolve the workspace directory path, expanding tilde.
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

// removeRCFile removes the temporary rcfile and clears its path from the executor.
// It is designed to be called from the manageSessionLifecycle goroutine's defer statement,
// ensuring cleanup happens regardless of how the session ends.
func (e *Executor) removeRCFile() {
	path := e.rcFilePath
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: failed to remove temporary file %s: %v", path, err)
	}
	e.rcFilePath = ""
}

// Contains constants above SOH \x01 and STX \x02
func promptCommand() string {
	// The marker is printed on its own line (note the \n) to ensure that line-buffered readers will process it immediately.
	return fmt.Sprintf(`PROMPT_COMMAND='printf "\x01%s:%%d\x02" $?'`, config.C.Shell.GetCommandEndMarker())
}

// createRCFile builds the content for a temporary bash rcfile, writes it to disk,
// and stores the path in the executor. This method encapsulates the setup logic
// for the interactive shell environment.
func (e *Executor) createRCFile() error {
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

	// The rcfile content exports our custom prompt and command marker.
	// NOTE: We are intentionally NOT sourcing the user's personal .bashrc.
	// This makes shell startup fast and ensures the test environment is hermetic,
	// preventing a user's local shell configuration from causing slowness or test failures.
	rcFileContent := fmt.Sprintf(`
# Set our custom prompt, overriding any from the user's .bashrc.
export %s
export %s
`, ps1, promptCommand)

	tmpfile, err := os.CreateTemp("", "gemini-bashrc-*.sh")
	if err != nil {
		return fmt.Errorf("could not create temporary rcfile: %w", err)
	}

	if _, err := tmpfile.WriteString(rcFileContent); err != nil {
		tmpfile.Close()
		os.Remove(tmpfile.Name()) // Clean up on write error
		return fmt.Errorf("could not write to temporary rcfile: %w", err)
	}

	if err := tmpfile.Close(); err != nil {
		os.Remove(tmpfile.Name()) // Clean up on close error
		return fmt.Errorf("could not close temporary rcfile: %w", err)
	}

	e.rcFilePath = tmpfile.Name()
	return nil
}

// prepareBashCommand creates a temporary rcfile and prepares an exec.Cmd to start
// an interactive bash session using that file. It returns the command and the path
// to the temporary rcfile which must be cleaned up by the caller.
func (e *Executor) prepareBashCommand() (*exec.Cmd, error) {
	if err := e.createRCFile(); err != nil {
		return nil, err
	}

	// Use --rcfile to load our custom config and -i to run in interactive mode,
	// which is necessary for completion to work.
	cmd := exec.Command("bash", "--rcfile", e.rcFilePath, "-i")
	cmd.Dir = e.workspaceDir

	return cmd, nil
}

// configurePty sets the pseudo-terminal to a raw-like state by disabling echo.
// This prevents commands sent to the shell from being mirrored back into the output stream.
func (e *Executor) configurePty(ptmx *os.File) error {
	fd := int(ptmx.Fd())
	if !term.IsTerminal(fd) {
		return nil // Not a terminal, nothing to configure.
	}

	// Put the PTY master into raw mode. This is the idiomatic way to disable
	// terminal processing features like echoing. We don't need to save the old
	// state because we want the PTY to be in this mode for its entire lifetime.
	_, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set pty to raw mode: %w", err)
	}

	return nil
}

// StartInteractive starts a persistent `sh` process in a PTY.
// Its output is streamed to the provided channel.
// The `ptyWriter` is where the PTY's output will be written for the user to see.
func (e *Executor) StartInteractive(outputChan chan<- string, userTerminal *os.File) (err error) {
	if e.activeCmd != nil {
		return fmt.Errorf("an interactive session is already running")
	}

	cmd, err := e.prepareBashCommand()
	if err != nil {
		return err
	}

	// Use a single mutex for the entire setup to avoid races.
	e.ptyMutex.Lock()
	defer func() {
		if err != nil {
			e.ptyMutex.Unlock() // Unlock on failure so Stop can be called.
		}
	}()

	// Get the initial size of the user's terminal.
	initialSize, err := pty.GetsizeFull(userTerminal)
	if err != nil {
		// It is not critical error
		log.Printf("WARNING: could not get terminal size: %v", err)
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		e.removeRCFile() // Clean up on pty start failure.
		return fmt.Errorf("failed to start interactive pty: %w", err)
	}

	// Set the PTY's initial size to match the user's terminal.
	if err := pty.Setsize(ptmx, initialSize); err != nil {
		// It is not critical error
		log.Printf("WARNING: could not set pty size: %v", err)
	}

	// Disable echoing on the PTY to prevent commands from being mirrored in the output.
	// Needed in manual debug reason
	if DisableOutput {
		if err := e.configurePty(ptmx); err != nil {
			ptmx.Close()
			e.removeRCFile()
			// The error from configurePty is already descriptive.
			return err
		}
	}

	e.activePty = ptmx
	e.activeCmd = cmd
	e.isShellReady = false // Reset the ready flag for the new session.
	e.shellReadyChan = make(chan struct{})

	log.Println("Interactive shell session started.")

	// This goroutine manages the I/O and lifecycle for the active PTY session.
	go e.manageSessionLifecycle(ptmx, outputChan, userTerminal)

	// Unlock before waiting to prevent deadlocks if another goroutine needs the lock.
	e.ptyMutex.Unlock()

	// Wait for the shell to be fully initialized and the first marker to be consumed.
	e.commandMutex.Lock()
	defer e.commandMutex.Unlock()
	<-e.shellReadyChan
	log.Println("Interactive shell is synchronized and ready.")

	return nil
}

// manageSessionLifecycle handles the I/O and lifecycle for an active PTY session.
// It runs in a dedicated goroutine.
func (e *Executor) manageSessionLifecycle(ptmx *os.File, outputChan chan<- string, userTerminal *os.File) {
	// Ensure the temporary rcfile is cleaned up when the shell process exits.
	// This acts as a fallback if StopInteractive is not called (e.g., user types 'exit').
	defer e.removeRCFile()

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
	stdoutFilter := NewFilteringProvider(e, userTerminal)

	// This goroutine reads from the pipe, sends line-by-line to the channel,
	// and is responsible for closing the channel when it's done.
	go stdoutFilter.Send(outputChan, pr)

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

// SendCommand sends a command to the interactive shell and returns a channel that
// will receive the single, final exit code of the command upon completion.
func (e *Executor) SendCommand(command string) (<-chan int, error) {
	e.commandMutex.Lock()
	defer e.commandMutex.Unlock()

	if e.commandDoneChan != nil {
		return nil, fmt.Errorf("another command is already in progress")
	}

	// The internal channel still needs to handle the flush marker and the command marker.
	internalChan := make(chan int, markersPerCommand)
	e.commandDoneChan = internalChan
	e.commandMarkerCount = 0 // Reset the counter for the new command.

	// The channel returned to the caller will only receive the final exit code.
	resultChan := make(chan int, 1)

	// This goroutine acts as a mediator. It consumes the two markers from the
	// internal channel and passes only the second one (the command's actual
	// exit code) to the result channel.
	go func() {
		defer close(resultChan)
		var exitCode int
		var ok bool
		// Drain the internal channel. The last value received is the one we want.
		for code := range internalChan {
			exitCode = code
			ok = true
		}
		// If we received at least one value, send the last one to the caller.
		if ok {
			resultChan <- exitCode
		}
		// If 'ok' is false, it means the shell died and the channel was closed
		// without sending any values. The resultChan will just be closed, and the
		// caller will receive (0, false).
	}()

	// We send two commands back-to-back, each terminated by a newline, to
	// make the command-end marker detection robust.
	// 1. `printf ''`: A silent, no-op command. This is crucial for flushing the
	//    prompt and capturing the exit code of whatever command ran *before*
	//    this `SendCommand` call. This gives us a reliable "before" marker.
	// 2. The actual command from the user.
	// This two-step process ensures that the filtering logic will always receive
	// exactly two command-end markers after this function is called, preventing
	// timeouts caused by race conditions where the initial prompt marker is missed.
	// Using a simple newline (`\n`) sends an empty command. This is better than
	// `printf ''` because an empty command inherits the exit code ($?) of the
	// *previous* command, correctly capturing the shell's state before we run
	// our new command.
	flushAndExecuteCmd := fmt.Sprintf("\n%s\n", command)

	// Send the command to the shell. A newline is required to execute it.
	if err := e.SendInput(flushAndExecuteCmd); err != nil {
		// If sending fails, clean up and return the error.
		e.commandDoneChan = nil
		close(internalChan) // Close it to unblock the mediator goroutine.
		return nil, err
	}

	return resultChan, nil
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

	// The rcfile is cleaned up by the deferred call in manageSessionLifecycle,
	// which is guaranteed to run when the process is killed.

	// The channel is now closed by the writer goroutine in StartInteractive.
	log.Println("Interactive shell session resources cleaned up.")

	return err
}
