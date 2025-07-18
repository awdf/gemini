package inout

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"

	"gemini/config"
	"gemini/helpers"
)

// RMSDisplay manages the state and logic for displaying the RMS volume bar.
type RMSDisplay struct {
	barWidth             int
	updateInterval       time.Duration
	lastPrintedBarLength int
	currentRMS           float64
	wg                   *sync.WaitGroup
	rmsChan              <-chan float64
	bus                  *EventBus.Bus
	warmUpDone           bool
	muted                bool
	Mu                   sync.RWMutex
}

// NewRMSDisplay creates and initializes a new RMSDisplay instance.
func NewRMSDisplay(wg *sync.WaitGroup, rmsChan <-chan float64, bus *EventBus.Bus) *RMSDisplay {
	return &RMSDisplay{
		barWidth:             config.C.Display.BarWidth,
		updateInterval:       config.C.Display.UpdateInterval(),
		lastPrintedBarLength: -1, // Initialize to -1 to guarantee the first print.
		currentRMS:           0.0,
		wg:                   wg,
		rmsChan:              rmsChan,
		bus:                  bus,
		muted:                true,
		warmUpDone:           false,
	}
}

// printBar encapsulates the expensive printing logic.
func (d *RMSDisplay) printBar() {
	d.Mu.Lock() // Full lock because we write to lastPrintedBarLength
	defer d.Mu.Unlock()
	if !d.warmUpDone || d.muted {
		return
	}

	// Use a small threshold to avoid printing for near-silent audio.
	if !math.IsNaN(d.currentRMS) {
		// Since RMS is now normalized to [0.0, 1.0], we can scale it directly to the bar size.
		currentBarLength := int(d.currentRMS * float64(d.barWidth))

		if currentBarLength > d.barWidth {
			currentBarLength = d.barWidth
		}

		// Only update the display if the integer length of the bar has changed.
		// This is more robust than comparing floats and prevents excessive printing.
		if currentBarLength != d.lastPrintedBarLength {
			d.lastPrintedBarLength = currentBarLength
			bar := strings.Repeat("=", currentBarLength)
			gap := strings.Repeat(" ", d.barWidth-currentBarLength)
			fmt.Printf(soundbarPatern, bar, gap)
		}
	}

	if config.C.Trace {
		log.Printf("PrintBar RMS: %f", d.currentRMS)
	}
}

// Run starts the goroutine to update the RMS volume bar display.
func (d *RMSDisplay) Run() {
	defer d.wg.Done()

	helpers.Verify((*d.bus).SubscribeAsync("main:topic", func(event string) {
		config.DebugPrintf("Bar received event: %s\n", event)
		d.Mu.Lock() // Full lock to write state
		defer d.Mu.Unlock()
		switch {
		case strings.HasPrefix(event, "mute:"):
			d.muted = true
		case strings.HasPrefix(event, "draw:"):
			d.muted = false
		case strings.HasPrefix(event, "show:"):
			d.muted = false
			// The main loop's ticker will handle the redraw, avoiding deadlocks.
		case strings.HasPrefix(event, "ready:"):
			d.warmUpDone = true
			d.muted = false
		default:
			config.DebugPrintf("Bar drop event: %s\n", event)
		}
	}, false))

	// Refresh the display at a fixed rate (e.g., 20 times per second)
	// This is fast enough for a smooth UI but prevents overwhelming the terminal.
	ticker := time.NewTicker(d.updateInterval)
	defer ticker.Stop()

	for {
		if config.C.Trace {
			log.Println("Display RMS")
		}
		select {
		case rms, ok := <-d.rmsChan:
			if !ok {
				log.Println("Display work finished")
				return // Channel is closed.
			}
			d.Mu.Lock()        // Full lock to write state
			d.currentRMS = rms // Keep track of the latest RMS value.
			d.Mu.Unlock()
		case <-ticker.C:
			// Ticker fired. Time to update the display.
			d.printBar()
		}
	}
}
