package agents

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
	"gemini/images"
	"gemini/inout"
	"gemini/wayland"
)

// ObjectDetectionNormalizationGrid defines the grid size for normalized bounding box coordinates.
const ObjectDetectionNormalizationGrid = 1000

type ObjectDetectionAgent struct {
	*Agent
	roadMap [3]bool
}

// NewObjectDetectionAgent creates a specialized agent for detecting objects in an image.
func NewObjectDetectionAgent(ctx context.Context, client *genai.Client) *ObjectDetectionAgent {
	bounds := helpers.Check(images.DisplayBounds())
	grid := ObjectDetectionNormalizationGrid
	halfGrid := grid / 2
	systemInstruction := fmt.Sprintf(`You are an object detection specialist. 
The user will provide a query describing an object to find in the provided image. 
Your task is to locate that object and return its 2D bounding box.
The image dimensions are %d x %d (width x height). The origin (0,0) is at the top-left corner.
You MUST return the bounding box coordinates normalized to a %dx%d grid.
For example, for a 200x400 image, a point at (x=100, y=200) should be returned as (x=%d, y=%d).
Return the response as a JSON array with labels. Never return masks or code fencing. Limit to 25 objects.
If an object is present multiple times, name them according to their unique characteristic (colors, size, position, unique characteristics, etc..).`,
		bounds.Dx(), bounds.Dy(), grid, grid, halfGrid, halfGrid)

	scheme := genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"objects": {
				Type:        genai.TypeArray,
				Description: "A list of detected objects.",
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"label": {Type: genai.TypeString, Description: "The identified object's label (e.g., 'car', 'person')."},
						"box_2d": {
							Type:        genai.TypeObject,
							Description: fmt.Sprintf("A map containing the bounding box coordinates normalized to a %dx%d grid, where (0,0) is the top-left corner.", ObjectDetectionNormalizationGrid, ObjectDetectionNormalizationGrid),
							Properties: map[string]*genai.Schema{
								"xmin": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized x-coordinate of the left edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
								"ymin": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized y-coordinate of the top edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
								"xmax": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized x-coordinate of the right edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
								"ymax": {Type: genai.TypeInteger, Description: fmt.Sprintf("The normalized y-coordinate of the bottom edge of the box (0-%d).", ObjectDetectionNormalizationGrid)},
							},
							Required: []string{"ymin", "xmin", "xmax", "ymax"},
						},
					},
					Required: []string{"label", "box_2d"},
				},
			},
		},
		Required: []string{"objects"},
	}

	agentConfig := AgentConfig{
		Name:              ObjectDetectionAgentName,
		Model:             config.C.AI.ModelObjectDetection,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.0)),
		ResponseSchema:    &scheme,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	odAgent := &ObjectDetectionAgent{
		Agent:   baseAgent,
		roadMap: [3]bool{false, false, false},
	}

	// Overwrite the registration in the registry with the specialized agent.
	AgentRegistry[odAgent.name] = odAgent

	return odAgent
}

func (a *ObjectDetectionAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "detectObjects":
		return a.handleDetectObjectsTool(call)
	case "verifyObjectDetection":
		return a.handleVerifyObjectDetectionTool(call)
	case "mouseClick":
		return a.handleMouseClickTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

// handleDetectObjectsTool processes the 'detectObjects' tool call.
func (a *ObjectDetectionAgent) handleDetectObjectsTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Get the query from the tool call arguments.
	query, ok := call.Args["query"].(string)
	if !ok || query == "" {
		err = fmt.Errorf("'query' argument is required and must be a non-empty string")
	} else {
		// Add a guardrail to prevent the model from sending overly simplistic queries by
		// checking for a minimum number of words. This is more robust than checking
		// character length, as the model can't bypass it with extra spaces.
		const minWordCount = 5
		if len(strings.Fields(query)) < minWordCount {
			err = fmt.Errorf("query '%s' is not descriptive enough (must be at least %d words). Please provide a more descriptive query, for example: 'the blue \"Submit\" button in the center of the form'", query, minWordCount)
		} else {
			// This tool uses the session's image buffer, passed in the args.
			imageBuf, ok := call.Args["image_buffer"].(*images.ScreenshotBuffer)
			if !ok || imageBuf == nil || imageBuf.Len() == 0 {
				err = fmt.Errorf("no image found in the current session context to detect objects from")
			} else {
				// Get screen dimensions to provide context to the model.
				bounds, boundsErr := images.DisplayBounds()
				if boundsErr != nil {
					err = fmt.Errorf("failed to get display bounds for object detection context: %w", boundsErr)
				} else {
					log.Printf("Object detection image size %d x %d (width x height).", bounds.Dx(), bounds.Dy())
					// Process the image with the agent, using the query from the tool call as the prompt.
					// The image buffer is PNG encoded.
					detectionResult, processErr := a.Process(query, genai.NewPartFromBytes(imageBuf.Bytes(), "image/png"))
					if processErr != nil {
						err = fmt.Errorf("object detection failed: %w", processErr)
					} else {
						log.Printf("Object detection successful for query: '%s'", query)
						result = map[string]any{"detected_objects": detectionResult}
					}
				}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	} else {
		// Mark this step as complete on the roadmap only on success.
		a.roadMap[0] = true
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}

func (a *ObjectDetectionAgent) handleVerifyObjectDetectionTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Enforce the correct tool-use sequence.
	if !a.roadMap[0] {
		err = fmt.Errorf("you must call 'detectObjects' successfully before you can verify the result")
	} else {
		// 1. Parse arguments from the tool call
		xminNorm, xminOK := call.Args["xmin"].(float64)
		yminNorm, yminOK := call.Args["ymin"].(float64)
		xmaxNorm, xmaxOK := call.Args["xmax"].(float64)
		ymaxNorm, ymaxOK := call.Args["ymax"].(float64)

		if !xminOK || !yminOK || !xmaxOK || !ymaxOK {
			err = fmt.Errorf("invalid or missing normalized bounding box arguments (xmin, ymin, xmax, ymax)")
		} else {
			// 2. Get the original screenshot from the session buffer
			imageBuf, ok := call.Args["image_buffer"].(*images.ScreenshotBuffer)
			if !ok || imageBuf == nil || imageBuf.Len() == 0 {
				err = fmt.Errorf("no image found in the current session context to verify")
			} else {
				// 3. Decode the image
				originalImg, decodeErr := png.Decode(bytes.NewReader(imageBuf.Bytes()))
				if decodeErr != nil {
					err = fmt.Errorf("failed to decode screenshot for verification: %w", decodeErr)
				} else {
					// 4. Get image dimensions and denormalize coordinates
					bounds := originalImg.Bounds()
					imgWidth := float64(bounds.Dx())
					imgHeight := float64(bounds.Dy())

					xmin := int((xminNorm / float64(ObjectDetectionNormalizationGrid)) * imgWidth)
					ymin := int((yminNorm / float64(ObjectDetectionNormalizationGrid)) * imgHeight)
					xmax := int((xmaxNorm / float64(ObjectDetectionNormalizationGrid)) * imgWidth)
					ymax := int((ymaxNorm / float64(ObjectDetectionNormalizationGrid)) * imgHeight)

					// 5. Draw the rectangle
					rect := image.Rect(xmin, ymin, xmax, ymax)
					imgWithBox := images.DrawRectangle(originalImg, rect, 3, color.RGBA{R: 255, A: 255}) // Red box, 3px thick

					// 6. Encode the new image back to a PNG buffer
					newImageBuf := new(bytes.Buffer)
					if encodeErr := png.Encode(newImageBuf, imgWithBox); encodeErr != nil {
						err = fmt.Errorf("failed to encode verification image: %w", encodeErr)
					} else {
						// TODO: remove after object detection live testing
						go helpers.Verify(images.SaveImage("Detect.png", newImageBuf.Bytes()))
						// 7. Return the new image and a verification prompt to be sent to the session by the caller.
						parts := []*genai.Part{
							genai.NewPartFromText("Tool have drawn the red box according to provided coordinates. Is the user requested object to detect inside the red box correctly identified? If not, try resolve this issue without user confirmation"),
							genai.NewPartFromBytes(newImageBuf.Bytes(), "image/png"),
						}
						turn := genai.NewContentFromParts(parts, genai.RoleUser)
						content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

						log.Println("Successfully prepared verification image to be sent to live session.")
						result = map[string]any{
							"status":       "Verification image prepared. Awaiting sending.",
							"send_content": content, // Special key for LiveAI to handle
						}
					}
				}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	} else {
		// Mark this step as complete on the roadmap only on success.
		a.roadMap[1] = true
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}

func (a *ObjectDetectionAgent) handleMouseClickTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	// Enforce the correct tool-use sequence.
	if !a.roadMap[0] || !a.roadMap[1] {
		err = fmt.Errorf("you must get positive approve from 'verifyObjectDetection' tool before apply mouse actions")
	} else {
		// 1. Parse arguments
		xNorm, xOK := call.Args["x"].(float64)
		yNorm, yOK := call.Args["y"].(float64)
		clicksFloat, _ := call.Args["clicks"].(float64)

		if !xOK || !yOK {
			err = fmt.Errorf("arguments 'x' and 'y' are required and must be numbers")
		} else {
			// 2. Get image dimensions from session buffer
			imageBuf, ok := call.Args["image_buffer"].(*images.ScreenshotBuffer)
			if !ok || imageBuf == nil || imageBuf.Len() == 0 {
				err = fmt.Errorf("no image found in the current session context to calculate click coordinates")
			} else {
				// 3. Decode image config to get bounds efficiently
				imgConfig, _, decodeErr := image.DecodeConfig(bytes.NewReader(imageBuf.Bytes()))
				if decodeErr != nil {
					err = fmt.Errorf("failed to decode screenshot config for click: %w", decodeErr)
				} else {
					// 4. Denormalize coordinates
					imgWidth := float64(imgConfig.Width)
					imgHeight := float64(imgConfig.Height)

					absX := int((xNorm / float64(ObjectDetectionNormalizationGrid)) * imgWidth)
					absY := int((yNorm / float64(ObjectDetectionNormalizationGrid)) * imgHeight)

					clicks := int(clicksFloat)
					if clicks < 1 {
						clicks = 1
					}

					log.Printf("Performing %d mouse click(s) at absolute pixel coordinates (%d, %d)", clicks, absX, absY)

					// 5. Execute the desktop automation.
					wayland.MoveMouseToPosition(absX, absY)
					time.Sleep(100 * time.Millisecond)
					wayland.MouseLeftClick(clicks)

					result = map[string]any{"status": fmt.Sprintf("%d mouse click(s) performed at (%d, %d)", clicks, absX, absY)}
				}
			}
		}
	}

	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		log.Printf("ERROR: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	a.roadMap = [3]bool{false, false, false}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}
