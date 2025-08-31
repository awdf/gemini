package flow

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var (
	signalChan     = make(chan os.Signal, 1000)
	listeners      = make([]*chan os.Signal, 0)
	winchListeners = make([]*chan os.Signal, 0)
	exitFunc       = log.Fatalf // For testability
	mu             sync.Mutex
)

func EnableControl() {
	signal.Notify(signalChan,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGPIPE,
		syscall.SIGWINCH)

	go processSignal()
}

func GetListener() *chan os.Signal {
	mu.Lock()
	defer mu.Unlock()
	ch := make(chan os.Signal, 1)
	listeners = append(listeners, &ch)
	return &ch
}

// GetWinchListener returns a new channel that will receive SIGWINCH signals.
func GetWinchListener() *chan os.Signal {
	mu.Lock()
	defer mu.Unlock()
	ch := make(chan os.Signal, 1)
	winchListeners = append(winchListeners, &ch)
	return &ch
}

// StopWinchListener removes a channel from the list of SIGWINCH listeners and closes it.
func StopWinchListener(ch *chan os.Signal) {
	mu.Lock()
	defer mu.Unlock()
	for i, listener := range winchListeners {
		if listener == ch {
			winchListeners = append(winchListeners[:i], winchListeners[i+1:]...)
			close(*listener)
			break
		}
	}
}

func Terminate() {
	signalChan <- syscall.SIGTERM
}

var Quit = func() {
	signalChan <- syscall.SIGQUIT
}

func Interrupt() {
	signalChan <- syscall.SIGINT
}

// Reset is a test helper to clear listeners between tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	listeners = make([]*chan os.Signal, 0)
	winchListeners = make([]*chan os.Signal, 0)
}

func processSignal() {
	for sig := range signalChan {
		switch sig {
		case syscall.SIGPIPE:
			log.Println("SIGPIPE ignored")
		case syscall.SIGWINCH:
			handleWinch(sig)
		default:
			signal.Reset(sig)
			fmt.Println()
			log.Printf("got signal, value = \"%s\"", sig)
			handle(sig)
			return
		}
	}
}

func handleWinch(sig os.Signal) {
	mu.Lock()
	listenersCopy := make([]*chan os.Signal, len(winchListeners))
	copy(listenersCopy, winchListeners)
	mu.Unlock()

	// It's okay if there are no winch listeners.
	for _, l := range listenersCopy {
		// Use a non-blocking send to avoid getting stuck if a listener is not ready.
		// SIGWINCH can come in bursts, and we only care about the latest one.
		select {
		case *l <- sig:
		default:
			// Listener is busy, drop the signal. This is typical for SIGWINCH.
		}
	}
}

func handle(sig os.Signal) {
	mu.Lock()
	// Make a copy of the listeners slice to avoid holding the lock while sending
	// on channels, which could lead to deadlocks if a listener calls back into
	// this package.
	listenersCopy := make([]*chan os.Signal, len(listeners))
	copy(listenersCopy, listeners)
	mu.Unlock()

	if len(listenersCopy) == 0 {
		exitFunc("service exiting, reason - signal \"%s\"", sig)
	}

	for _, l := range listenersCopy {
		*l <- sig
	}
}
