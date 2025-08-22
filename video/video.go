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
	wg        *sync.WaitGroup
	pipeline  *gst.Pipeline
	appSink   *app.Sink
	bus       *EventBus.Bus
	ctx       context.Context
	cancel    context.CancelFunc
	frameChan chan<- []byte // Channel to send processed video frames
	ticker    *time.Ticker
}

// NewVideoStreamComponent creates and initializes a new video streaming component.
func NewVideoStreamComponent(
	wg *sync.WaitGroup,
	bus *EventBus.Bus,
	frameChan chan<- []byte, // Provided by LiveAI
) (*VideoStreamComponent, error) {
	ctx, cancel := context.WithCancel(context.Background())

	v := &VideoStreamComponent{
		wg:        wg,
		bus:       bus,
		ctx:       ctx,
		cancel:    cancel,
		frameChan: frameChan,
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
		// If you need to target a specific PipeWire stream, you would set 'target-object' property.
		// For example: source.SetProperty("target-object", "node.id:42") or "node.name:my_webcam"
		// This would require adding a new field to config.VideoConfig.
	default:
		log.Printf("WARNING: Unknown video source '%s'. Proceeding with default properties.", config.C.Video.Source)
	}

	// Common elements for processing and encoding
	queue := helpers.Check(gst.NewElement("queue"))
	converter := helpers.Check(gst.NewElement("videoconvert"))
	scaler := helpers.Check(gst.NewElement("videoscale"))
	rate := helpers.Check(gst.NewElement("videorate"))
	encoder := helpers.Check(gst.NewElement("jpegenc"))
	capsFilter := helpers.Check(gst.NewElement("capsfilter"))
	v.appSink = helpers.Check(app.NewAppSink())

	// Configure encoder quality
	if config.C.Video.Quality >= 0 && config.C.Video.Quality <= 100 {
		helpers.Verify(encoder.SetProperty("quality", config.C.Video.Quality))
		log.Printf("JPEG encoder quality set to %d.", config.C.Video.Quality)
	} else {
		log.Printf("WARNING: Invalid JPEG quality %d. Using default encoder quality.", config.C.Video.Quality)
	}

	// Configure app.Sink
	v.appSink.SetDrop(false)
	v.appSink.SetMaxBuffers(1) // Only keep the latest frame

	// Build the pipeline: Corrected AddMany and Link calls
	// gst.Pipeline.AddMany returns error, so check it explicitly.
	if err = v.pipeline.AddMany(source, queue, scaler, rate, capsFilter, converter, encoder, v.appSink.Element); err != nil {
		return nil, fmt.Errorf("failed to add GStreamer elements to pipeline: %w", err)
	}

	// 1. Link source to queue, then to scaler and rate converter.
	helpers.Verify(source.Link(queue))
	helpers.Verify(queue.Link(scaler))
	helpers.Verify(scaler.Link(rate))

	// 2. Link to the caps filter that enforces the desired format.
	helpers.Verify(rate.Link(capsFilter))
	finalCaps := gst.NewCapsFromString(fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", config.C.Video.Width, config.C.Video.Height, config.C.Video.FrameRate))
	helpers.Verify(capsFilter.SetProperty("caps", finalCaps))

	// 3. Link to the converter (as a final adapter) and then to the encoder and sink.
	helpers.Verify(capsFilter.Link(converter))
	helpers.Verify(converter.Link(encoder))
	helpers.Verify(encoder.Link(v.appSink.Element))

	v.ticker = time.NewTicker(time.Second / time.Duration(config.C.Video.FrameRate))

	return v, nil
}

// Run starts the video capture and processing loop.
func (v *VideoStreamComponent) Run() {
	defer v.wg.Done()
	defer v.cancel()
	defer v.ticker.Stop()

	log.Println("Starting video stream pipeline...")
	v.pipeline.SetState(gst.StatePlaying)

	// Listen for bus messages (errors, EOS) from the pipeline
	bus := v.pipeline.GetBus()
	bus.AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			log.Println("Video pipeline received EOS.")
			flow.Quit() // Signal application shutdown
			return false
		case gst.MessageError:
			err := msg.ParseError()
			log.Printf("ERROR: Video pipeline error: %s (debug: %s)", err.Error(), err.DebugString())
			flow.Quit() // Signal application shutdown
			return false
		}
		return true
	})

	// Pull samples from the app.Sink and send them to the frame channel.
	for {
		select {
		case <-v.ctx.Done():
			log.Println("Video stream component shutting down.")
			v.pipeline.SetState(gst.StateNull)
			return
		case <-v.ticker.C:
			// Attempt to pull a sample at the configured frame rate.
			sample := v.appSink.TryPullSample(0) // Non-blocking pull
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
					// This ensures we always process the latest frame.
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
	v.cancel()
	v.pipeline.SetState(gst.StateNull) // Explicitly set pipeline to NULL
}

// Element returns the GStreamer element for integration into other pipelines (e.g., a multi-source pipeline).
// For this design, the VideoStreamComponent manages its own pipeline, so this might not be directly used,
// but it's kept for consistency or future composite pipelines.
func (v *VideoStreamComponent) Element() *gst.Element {
	return v.pipeline.Element
}
