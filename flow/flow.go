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
	signalChan = make(chan os.Signal, 1000)
	listeners  = make([]*chan os.Signal, 0)
	exitFunc   = log.Fatalf // For testability
	mu         sync.Mutex
)

func EnableControl() {
	signal.Notify(signalChan, syscall.SIGINT)
	signal.Notify(signalChan, syscall.SIGTERM)
	signal.Notify(signalChan, syscall.SIGQUIT)
	signal.Notify(signalChan, syscall.SIGPIPE)

	go processSignal()
}

func GetListener() *chan os.Signal {
	mu.Lock()
	defer mu.Unlock()
	ch := make(chan os.Signal, 1)
	listeners = append(listeners, &ch)
	return &ch
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
}

func processSignal() {
	for sig := range signalChan {
		signal.Reset(sig)
		fmt.Println()
		log.Printf("got signal, value = \"%s\"", sig)

		switch sig {
		case syscall.SIGPIPE:
			log.Println("SIGPIPE ignored")
		default:
			handle(sig)
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
