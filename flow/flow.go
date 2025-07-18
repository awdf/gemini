package flow

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

var (
	signalChan = make(chan os.Signal, 1000)
	listeners  = make([]*chan os.Signal, 0)
	exitFunc   = log.Fatalf // For testability
)

func EnableControl() {
	signal.Notify(signalChan, syscall.SIGINT)
	signal.Notify(signalChan, syscall.SIGTERM)
	signal.Notify(signalChan, syscall.SIGQUIT)
	signal.Notify(signalChan, syscall.SIGPIPE)

	go processSignal()
}

func GetListener() *chan os.Signal {
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
	if len(listeners) == 0 {
		exitFunc("service exiting, reason - signal \"%s\"", sig)
	}

	for _, l := range listeners {
		*l <- sig
	}
}
