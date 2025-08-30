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
	chunkChan chan<- []byte // Channel to send processed video chunks
}

// NewVideoStreamComponent creates and initializes a new video streaming component.
// Pipeline: gst-launch-1.0 pipewiresrc ! "video/x-raw" ! queue ! videoconvert ! x264enc pass=quant quantizer=23 tune=zerolatency ! mp4mux streamable=true fragment-duration=100 ! fakesink -v
func NewVideoStreamComponent(
	wg *sync.WaitGroup,
	bus *EventBus.Bus,
	chunkChan chan<- []byte, // Provided by LiveAI
) (*VideoStreamComponent, error) {
	ctx, cancel := context.WithCancel(context.Background())

	v := &VideoStreamComponent{
		wg:        wg,
		bus:       bus,
		ctx:       ctx,
		cancel:    cancel,
		chunkChan: chunkChan,
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
	default:
		log.Printf("WARNING: Unknown video source '%s'. Proceeding with default properties.", config.C.Video.Source)
	}
	// Common elements for processing and encoding into a video stream
	queue := helpers.Check(gst.NewElement("queue"))
	converter := helpers.Check(gst.NewElement("videoconvert"))
	// H.264 encoder and MP4 muxer for streaming
	encoder := helpers.Check(gst.NewElement("x264enc"))
	muxer := helpers.Check(gst.NewElement("mp4mux"))
	v.appSink = helpers.Check(app.NewAppSink())

	// Configure encoder properties individually, matching the working command-line prototype.
	// We use explicit 32-bit types for enums and flags to avoid CGo type-mapping
	// issues on 64-bit systems, which was the likely cause of the negotiation errors.

	// 'pass=quant' corresponds to enum value 4. It requires a 32-bit unsigned int.
	helpers.Verify(encoder.SetProperty("pass", uint32(4)))

	quantizer := config.C.Video.Quantizer
	if quantizer < 0 || quantizer > 51 {
		log.Printf("WARNING: Invalid quantizer value %d in config. Using default of 23.", quantizer)
		quantizer = 23
	}
	helpers.Verify(encoder.SetProperty("quantizer", uint(quantizer)))

	// 'tune=zerolatency' corresponds to flag value 0x4. It also requires a 32-bit unsigned int.
	helpers.Verify(encoder.SetProperty("tune", uint32(0x4)))

	// Configure MP4 muxer for fragmented, streamable output
	helpers.Verify(muxer.SetProperty("streamable", true))
	// Create a new fragment every 100ms. This determines the chunk size.
	helpers.Verify(muxer.SetProperty("fragment-duration", uint32(100)))

	// Configure app.Sink
	v.appSink.SetDrop(false)
	v.appSink.SetMaxBuffers(5) // Allow a small buffer of chunks

	// Build the pipeline
	if err = v.pipeline.AddMany(source, queue, converter, encoder, muxer, v.appSink.Element); err != nil {
		return nil, fmt.Errorf("failed to add GStreamer elements to pipeline: %w", err)
	}

	// Link the elements to match the working command-line prototype:
	// pipewiresrc ! queue ! videoconvert ! x264enc ! mp4mux ! appsink
	// The order of queue -> videoconvert is critical for live sources.
	helpers.Verify(source.Link(queue))
	helpers.Verify(queue.Link(converter))
	helpers.Verify(converter.Link(encoder))
	helpers.Verify(encoder.Link(muxer))
	helpers.Verify(muxer.Link(v.appSink.Element))

	return v, nil
}

// Run starts the video capture and processing loop.
func (v *VideoStreamComponent) Run() {
	defer v.wg.Done()
	defer v.cancel()

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

	// Start the pipeline *after* the bus watch is set up to avoid race conditions.
	log.Println("Starting video stream pipeline...")
	v.pipeline.SetState(gst.StatePlaying)

	// Pull samples (video chunks) from the app.Sink and send them to the channel.
	// This loop replaces the ticker-based logic, as the pipeline now pushes chunks
	// to the appsink at its own rate.
	for {
		// First, check if the context has been cancelled to ensure a timely shutdown.
		select {
		case <-v.ctx.Done():
			log.Println("Video stream component shutting down.")
			v.pipeline.SetState(gst.StateNull)
			return
		default:
			// Continue if not cancelled.
		}

		// Check for End-of-Stream from the sink itself. This is a more reliable
		// way to detect the end of the stream than checking the pipeline state.
		if v.appSink.IsEOS() {
			log.Println("Video sink reached EOS, exiting pull loop.")
			return
		}

		// Use TryPullSample for a non-blocking pull.
		sample := v.appSink.TryPullSample(0)
		if sample == nil {
			// No sample is available right now. Sleep for a short duration
			// to prevent this loop from consuming 100% CPU.
			time.Sleep(10 * time.Millisecond)
			continue
		}

		buffer := sample.GetBuffer()
		if buffer != nil {
			chunkBytes := buffer.Bytes()
			if len(chunkBytes) > 0 {
				// Send chunk, but don't block if the receiver is slow.
				// This ensures we always process the latest frame.
				select {
				case v.chunkChan <- chunkBytes:
					if config.C.Trace {
						log.Printf("Sent video chunk (%d bytes) to channel.", len(chunkBytes))
					}
				default:
					if config.C.Trace {
						log.Println("Chunk channel is full, dropping video chunk.")
					}
				}
			}
			buffer.Unmap()
		}
		// IMPORTANT: Go GStreamer unrefs the sample automatically.
	}
}

// Stop gracefully stops the video streaming component.
func (v *VideoStreamComponent) Stop() {
	log.Println("Stopping video stream component...")
	v.cancel()
	v.pipeline.SendEvent(gst.NewEOSEvent()) // Gracefully end the stream
	// Wait a moment for EOS to propagate before setting to NULL
	time.Sleep(200 * time.Millisecond)
	v.pipeline.SetState(gst.StateNull) // Explicitly set pipeline to NULL
}

// Element returns the GStreamer element for integration into other pipelines (e.g., a multi-source pipeline).
// For this design, the VideoStreamComponent manages its own pipeline, so this might not be directly used,
// but it's kept for consistency or future composite pipelines.
func (v *VideoStreamComponent) Element() *gst.Element {
	return v.pipeline.Element
}
