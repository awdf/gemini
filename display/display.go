package display

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	DEBUG = false
)

// printBar encapsulates the expensive printing logic.
func printBar(rms float64, lastBarLength *int) {
	const barWidth = 100

	// Use a small threshold to avoid printing for near-silent audio.
	if !math.IsNaN(rms) {
		// Since RMS is now normalized to [0.0, 1.0], we can scale it directly to the bar size.
		currentBarLength := int(rms * barWidth)

		if currentBarLength > barWidth {
			currentBarLength = barWidth
		}

		// Only update the display if the integer length of the bar has changed.
		// This is more robust than comparing floats and prevents excessive printing.
		if currentBarLength != *lastBarLength {
			*lastBarLength = currentBarLength
			bar := strings.Repeat("=", currentBarLength)
			gap := strings.Repeat(" ", barWidth-currentBarLength)
			// This is the expensive call we are throttling.
			fmt.Printf("[%s%s]\n\033[1A", bar, gap)
		}
	}
}

// DisplayRMS runs a goroutine to update the RMS volume bar display.
func DisplayRMS(wg *sync.WaitGroup, rmsChan <-chan float64) {
	defer wg.Done()
	// Refresh the display at a fixed rate (e.g., 20 times per second)
	// This is fast enough for a smooth UI but prevents overwhelming the terminal.
	const updateInterval = 50 * time.Millisecond
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	var lastPrintedBarLength = -1 // Initialize to -1 to guarantee the first print.
	var currentRMS float64

	for {
		if DEBUG {
			fmt.Println("Display RMS")
		}

		select {
		case rms, ok := <-rmsChan:
			if !ok {
				fmt.Println("Display work finished")
				return // Channel is closed.
			}
			currentRMS = rms // Keep track of the latest RMS value.
		case <-ticker.C:
			// Ticker fired. Time to update the display.
			printBar(currentRMS, &lastPrintedBarLength)
		}
	}
}
