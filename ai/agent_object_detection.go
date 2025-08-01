package ai

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
	"gemini/images"
)

// ObjectDetectionNormalizationGrid defines the grid size for normalized bounding box coordinates.
const ObjectDetectionNormalizationGrid = 1000

// NewObjectDetectionAgent creates a specialized agent for detecting objects in an image.
func NewObjectDetectionAgent(ctx context.Context, client *genai.Client) *Agent {
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

	agentConfig := AgentConfig{
		Name:              ObjectDetectionAgent,
		Model:             config.C.AI.ModelObjectDetection,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.0)),
		ResponseSchema:    GetObjectDetectionSchema(),
	}
	return NewAgent(ctx, client, agentConfig)
}

// GetObjectDetectionSchema returns the schema for object detection responses.
// It defines a structure for a list of predictions, where each prediction
// has a label and a bounding box with named coordinates.
func GetObjectDetectionSchema() *genai.Schema {
	return &genai.Schema{
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
}
