package video

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"

	"gemini/config"
	"gemini/flow"
	"gemini/helpers"
)

// VideoStreamComponent encapsulates the GStreamer pipeline for video capture and processing.
type VideoStreamComponent struct {
	pipeline   *gst.Pipeline
	appSink    *app.Sink
	bus        *EventBus.Bus
	ctx        context.Context
	cancel     context.CancelFunc
	frameChan  chan<- []byte // Channel to send processed video frames
	mu         sync.Mutex    // To protect isStopping
	isStopping bool
}

// NewVideoStreamComponent creates and initializes a new video streaming component.
// Example Pipeline: gst-launch-1.0 pipewiresrc ! "video/x-raw" ! videoconvert ! videoscale ! videorate ! queue ! "video/x-raw,width=640,height=480,framerate=10/1" ! jpegenc ! fakesink -v
// Example: gst-launch-1.0 v4l2src device=/dev/video2 ! videoconvert ! autovideosink
func NewVideoStreamComponent(
	bus *EventBus.Bus,
	frameChan chan<- []byte, // Provided by LiveAI
) (*VideoStreamComponent, error) {
	ctx, cancel := context.WithCancel(context.Background())

	v := &VideoStreamComponent{
		bus:        bus,
		ctx:        ctx,
		cancel:     cancel,
		frameChan:  frameChan,
		isStopping: false,
	}

	var err error
	v.pipeline, err = gst.NewPipeline("")
	if err != nil {
		return nil, fmt.Errorf("failed to create video pipeline: %w", err)
	}

	// 1. Create the source element based on configuration
	source, err := gst.NewElement(config.C.Video.Source)
	if err != nil {
		return nil, fmt.Errorf("failed to create video source element '%s': %w", config.C.Video.Source, err)
	}

	// Configure source-specific properties
	switch config.C.Video.Source {
	case "v4l2src":
		if config.C.Video.Device != "" {
			helpers.Verify(source.SetProperty("device", config.C.Video.Device))
			log.Printf("Using v4l2src with device: %s", config.C.Video.Device)
		} else {
			log.Println("Using v4l2src with default device.")
		}
	case "pipewiresrc":
		helpers.Verify(source.SetProperty("do-timestamp", true))
		if config.C.Video.MonitorID != -1 {
			helpers.Verify(source.SetProperty("monitor-id", uint(config.C.Video.MonitorID)))
			log.Printf("Using pipewiresrc with monitor-id: %d", config.C.Video.MonitorID)
		} else {
			log.Println("Using pipewiresrc with default device (screen capture).")
		}
	case "videotestsrc":
		log.Println("Using videotestsrc for testing purposes.")
		helpers.Verify(source.SetProperty("is-live", true))
	default:
		log.Printf("WARNING: Unknown video source '%s'. Proceeding with default properties.", config.C.Video.Source)
	}

	// Common elements for processing and encoding into JPEG frames
	converter := helpers.Check(gst.NewElement("videoconvert"))
	rate := helpers.Check(gst.NewElement("videorate"))

	// The capsfilter enforces framerate for all sources. For pipewiresrc,
	// it also enforces width and height to ensure a consistent stream size,
	// which is often desirable for screen recording. Other sources will use
	// their natural dimensions.
	var finalCapsStr string
	if config.C.Video.Source == "pipewiresrc" {
		finalCapsStr = fmt.Sprintf("video/x-raw,framerate=%d/1,width=%d,height=%d",
			config.C.Video.FrameRate, config.C.Video.Width, config.C.Video.Height)
	} else {
		finalCapsStr = fmt.Sprintf("video/x-raw,framerate=%d/1", config.C.Video.FrameRate)
	}
	finalCaps := gst.NewCapsFromString(finalCapsStr)
	capsFilter := helpers.Check(gst.NewElement("capsfilter"))
	helpers.Verify(capsFilter.SetProperty("caps", finalCaps))

	encoder := helpers.Check(gst.NewElement("jpegenc"))
	helpers.Verify(encoder.SetProperty("quality", config.C.Video.Quality))

	// The appsink is the final element that allows our application to pull frames.
	v.appSink = helpers.Check(app.NewAppSink())

	// Add all elements to the pipeline at once.
	if err = v.pipeline.AddMany(source, converter, rate, capsFilter, encoder, v.appSink.Element); err != nil {
		return nil, fmt.Errorf("failed to add GStreamer elements to pipeline: %w", err)
	}

	// Link all the elements together in sequence.
	helpers.Verify(gst.ElementLinkMany(source, converter, rate, capsFilter, encoder, v.appSink.Element))

	return v, nil
}

// Run starts the video capture and processing loop.
func (v *VideoStreamComponent) Run() {
	defer v.cancel()

	// Listen for bus messages (errors, EOS) from the pipeline
	bus := v.pipeline.GetBus()
	bus.AddWatch(v.handleBusMessage)

	// Start the pipeline *after* the bus watch is set up to avoid race conditions.
	log.Println("Starting video stream pipeline...")
	v.pipeline.SetState(gst.StatePlaying)

	// Use a ticker to pull frames at the configured rate.
	ticker := time.NewTicker(time.Second / time.Duration(config.C.Video.FrameRate))
	defer ticker.Stop()

	for {
		select {
		case <-v.ctx.Done():
			// This is triggered by Stop() or an EOS on the bus.
			log.Println("Video stream component Run loop exiting.")
			return // The pipeline state is already being handled by Stop() or the bus watch.
		case <-ticker.C:
			// Pull the latest sample from the sink.
			sample := v.appSink.TryPullSample(0)
			if sample == nil {
				if config.C.Trace {
					log.Println("No video sample available to pull.")
				}
				continue
			}

			buffer := sample.GetBuffer()
			if buffer != nil {
				frameBytes := buffer.Bytes()
				if len(frameBytes) > 0 {
					// Send frame, but don't block if the receiver is slow.
					select {
					case v.frameChan <- frameBytes:
						if config.C.Trace {
							log.Printf("Sent video frame (%d bytes) to channel.", len(frameBytes))
						}
					default:
						if config.C.Trace {
							log.Println("Frame channel is full, dropping video frame.")
						}
					}
				}
				buffer.Unmap()
			}
			// IMPORTANT: Go GStreamer unrefs the sample automatically.
		}
	}
}

// Stop gracefully stops the video streaming component.
func (v *VideoStreamComponent) Stop() {
	log.Println("Stopping video stream component...")

	v.mu.Lock()
	v.isStopping = true
	v.mu.Unlock()

	// Cancel the context first to ensure the Run loop's select statement exits promptly.
	v.cancel()

	// Setting the pipeline to NULL is the most direct way to stop it.
	// This will cause the bus to emit an error or EOS, which the Run loop's
	// bus watch will handle. It also immediately stops the flow of samples.
	v.pipeline.SetState(gst.StateNull)
}

// Element returns the GStreamer element for integration into other pipelines (e.g., a multi-source pipeline).
// For this design, the VideoStreamComponent manages its own pipeline, so this might not be directly used,
// but it's kept for consistency or future composite pipelines.
func (v *VideoStreamComponent) Element() *gst.Element {
	return v.pipeline.Element
}

// handleBusMessage is the callback function for the GStreamer bus watch.
// It processes messages like EOS and Error from the pipeline.
func (v *VideoStreamComponent) handleBusMessage(msg *gst.Message) bool {
	v.mu.Lock()
	stopping := v.isStopping
	v.mu.Unlock()

	switch msg.Type() {
	case gst.MessageEOS:
		log.Println("Video pipeline received EOS.")
		// A natural EOS should not terminate the whole application.
		// It just means this component's work is done.
		v.cancel()
		return false // Stop watching the bus
	case gst.MessageError:
		if stopping {
			log.Println("Ignoring video pipeline error during shutdown.")
			return false // Stop watching the bus
		}
		err := msg.ParseError()
		log.Printf("ERROR: Video pipeline error: %s (debug: %s)", err.Error(), err.DebugString())
		flow.Quit()  // Signal application shutdown
		return false // Stop watching the bus
	}
	return true // Continue watching the bus
}
