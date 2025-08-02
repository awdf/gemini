package ai

import (
	"fmt"
	"log"

	"google.golang.org/genai"

	"gemini/inout"
)

// --- File System Tool Implementations (Shared) ---

// executeSingleToolCall dispatches a single tool call to the appropriate Go function
// and returns a structured FunctionResponse. This function is shared between PostAI and LiveAI.
func executeSingleToolCall(call *genai.FunctionCall) *genai.FunctionResponse {
	var result any
	var err error

	// For safety, we print the arguments. In a real application, you might want more structured logging.
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	switch call.Name {
	case "listEmails", "readEmail", "sendEmail":
		// These tools are special and handled by the GmailAgent, as they require an authenticated service.
		// This case is a fallback for when the agent isn't used directly.
		err = fmt.Errorf("the '%s' tool must be handled by the Gmail agent", call.Name)
	case "detectObjects", "verifyObjectDetection", "mouseClick":
		// These tools are special and handled by the ObjectDetectionAgent, which requires access to the session's image buffer.
		// This case is a fallback for non-live mode.
		err = fmt.Errorf("the '%s' tool is only available in live mode", call.Name)
	default:
		err = fmt.Errorf("unknown tool call: %s", call.Name)
	}

	// The model expects a JSON object as a response. If we have an error,
	// we'll return it in a structured way.
	if err != nil {
		log.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	// The response from a tool must be a map[string]any.
	responseMap, ok := result.(map[string]any)
	if !ok {
		// This should not happen with the current tool implementations, but it's a good safeguard.
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
