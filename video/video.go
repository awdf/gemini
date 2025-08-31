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
			source.SetProperty("device", config.C.Video.Device)
			log.Printf("Using v4l2src with device: %s", config.C.Video.Device)
		} else {
			log.Println("Using v4l2src with default device.")
		}
	case "gnomescreencast":
		if config.C.Video.MonitorID >= 0 {
			source.SetProperty("monitor-id", uint(config.C.Video.MonitorID))
			log.Printf("Using gnomesscreencast with monitor-id: %d", config.C.Video.MonitorID)
		} else {
			log.Println("Using gnomesscreencast (default monitor).")
		}
	case "pipewiresrc": // No monitor-id property for pipewiresrc
		log.Println("Using pipewiresrc (PipeWire video source). It typically does not use 'monitor-id' property.")
	case "videotestsrc":
		log.Println("Using videotestsrc for testing purposes.")
		source.SetProperty("is-live", true) // Critical for test sources in live pipelines
	default:
		log.Printf("WARNING: Unknown video source '%s'. Proceeding with default properties.", config.C.Video.Source)
	}
	// Common elements for processing and encoding into JPEG frames
	// Add a generic capsfilter right after the source. This is a common pattern to
	// resolve negotiation errors with live sources, as it simplifies the initial
	// negotiation for the source element, matching the working command-line prototype.
	sourceCapsFilter := helpers.Check(gst.NewElement("capsfilter"))
	helpers.Verify(sourceCapsFilter.SetProperty("caps", gst.NewCapsFromString("video/x-raw")))
	converter := helpers.Check(gst.NewElement("videoconvert"))
	scaler := helpers.Check(gst.NewElement("videoscale"))
	rate := helpers.Check(gst.NewElement("videorate"))
	queue := helpers.Check(gst.NewElement("queue"))

	// Configure the capsfilter to enforce the desired output format at creation time.
	// This drives the videoscale and videorate elements upstream.
	finalCaps := gst.NewCapsFromString(fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1",
		config.C.Video.Width, config.C.Video.Height, config.C.Video.FrameRate))
	prop := map[string]any{
		"caps": finalCaps,
	}
	capsFilter := helpers.Check(gst.NewElementWithProperties("capsfilter", prop))
	encoder := helpers.Check(gst.NewElement("jpegenc"))
	v.appSink = helpers.Check(app.NewAppSink())

	// Configure encoder quality
	if config.C.Video.Quality >= 0 && config.C.Video.Quality <= 100 {
		helpers.Verify(encoder.SetProperty("quality", config.C.Video.Quality))
		log.Printf("JPEG encoder quality set to %d.", config.C.Video.Quality)
	} else {
		log.Printf("WARNING: Invalid JPEG quality %d. Using default encoder quality.", config.C.Video.Quality)
	}

	// Configure app.Sink
	v.appSink.SetDrop(true)    // Drop old frames to always get the latest
	v.appSink.SetMaxBuffers(1) // Only keep the latest frame

	// Build the pipeline
	if err = v.pipeline.AddMany(source, sourceCapsFilter, converter, scaler, rate, queue, capsFilter, encoder, v.appSink.Element); err != nil {
		return nil, fmt.Errorf("failed to add GStreamer elements to pipeline: %w", err)
	}

	// Link the elements to precisely match the working command-line prototype.
	// An initial generic capsfilter stabilizes the source negotiation. The 'queue'
	// element then decouples the upstream elements from the final format-enforcing
	// capsfilter, which is a critical pattern for robust programmatic pipelines.
	helpers.Verify(source.Link(sourceCapsFilter))
	helpers.Verify(sourceCapsFilter.Link(converter))
	helpers.Verify(converter.Link(scaler))
	helpers.Verify(scaler.Link(rate))
	helpers.Verify(rate.Link(queue))
	helpers.Verify(queue.Link(capsFilter))
	helpers.Verify(capsFilter.Link(encoder))
	helpers.Verify(encoder.Link(v.appSink.Element))

	return v, nil
}

// Run starts the video capture and processing loop.
func (v *VideoStreamComponent) Run() {
	defer v.cancel()

	// Listen for bus messages (errors, EOS) from the pipeline
	bus := v.pipeline.GetBus()
	bus.AddWatch(func(msg *gst.Message) bool {
		v.mu.Lock()
		stopping := v.isStopping
		v.mu.Unlock()

		switch msg.Type() {
		case gst.MessageEOS:
			log.Println("Video pipeline received EOS.")
			// A natural EOS should not terminate the whole application.
			// It just means this component's work is done.
			v.cancel()
			return false
		case gst.MessageError:
			if stopping {
				log.Println("Ignoring video pipeline error during shutdown.")
				return false
			}
			err := msg.ParseError()
			log.Printf("ERROR: Video pipeline error: %s (debug: %s)", err.Error(), err.DebugString())
			flow.Quit() // Signal application shutdown
			return false
		}
		return true
	})

	// Start the pipeline *after* the bus watch is set up to avoid race conditions.
	log.Println("Starting video stream pipeline...")
	v.pipeline.SetState(gst.StatePlaying)

	// Use a ticker to pull frames at the configured rate.
	ticker := time.NewTicker(time.Second / time.Duration(config.C.Video.FrameRate))
	defer ticker.Stop()
	shutdownChan := flow.GetListener()

	for {
		select {
		case <-*shutdownChan:
			log.Println("Video stream component shutting down.")
			v.pipeline.SetState(gst.StateNull)
			return
		case <-v.ctx.Done():
			// This is triggered by Stop() or an EOS on the bus.
			// The pipeline state is already handled, so we just exit the loop.
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
