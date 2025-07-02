package display

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"capgemini.com/config"
)

// RMSDisplay manages the state and logic for displaying the RMS volume bar.
type RMSDisplay struct {
	barWidth             int
	updateInterval       time.Duration
	lastPrintedBarLength int
	currentRMS           float64
	wg                   *sync.WaitGroup
	rmsChan              <-chan float64
}

// NewRMSDisplay creates and initializes a new RMSDisplay instance.
func NewRMSDisplay(wg *sync.WaitGroup, rmsChan <-chan float64) *RMSDisplay {
	return &RMSDisplay{
		barWidth:             config.C.Display.BarWidth,
		updateInterval:       config.C.Display.UpdateInterval(),
		lastPrintedBarLength: -1, // Initialize to -1 to guarantee the first print.
		currentRMS:           0.0,
		wg:                   wg,
		rmsChan:              rmsChan,
	}
}

// printBar encapsulates the expensive printing logic.
func (d *RMSDisplay) printBar() {
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
			// This is the expensive call we are throttling.
			fmt.Printf("\033[s\n[%s%s]\033[u", bar, gap)
		}
	}
}

// Run starts the goroutine to update the RMS volume bar display.
func (d *RMSDisplay) Run() {
	defer d.wg.Done()
	// Refresh the display at a fixed rate (e.g., 20 times per second)
	// This is fast enough for a smooth UI but prevents overwhelming the terminal.
	ticker := time.NewTicker(d.updateInterval)
	defer ticker.Stop()

	for {
		if config.C.Debug {
			log.Println("Display RMS")
		}
		select {
		case rms, ok := <-d.rmsChan:
			if !ok {
				log.Println("Display work finished")
				return // Channel is closed.
			}
			d.currentRMS = rms // Keep track of the latest RMS value.
		case <-ticker.C:
			// Ticker fired. Time to update the display.
			d.printBar()
		}
	}
}
