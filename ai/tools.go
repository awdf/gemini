package ai

import (
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/config"
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
	case "listFiles":
		// The model might not provide a path if it wants the root, so we default to ".".
		path, _ := call.Args["path"].(string)
		if path == "" {
			path = "."
		}
		result, err = listFiles(path)
	case "readFile":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			result, err = readFile(path)
		}
	case "createFile":
		path, pathOK := call.Args["path"].(string)
		content, contentOK := call.Args["content"].(string)
		if !pathOK || path == "" || !contentOK {
			// The model can sometimes forget to provide content.
			err = fmt.Errorf("'path' (string) and 'content' (string) arguments are required")
		} else {
			result, err = createFile(path, content)
		}
	case "deleteFile":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			result, err = deleteFile(path)
		}
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
		ID:       call.ID,
		Name:     call.Name,
		Response: responseMap,
	}
}

// expandPath handles tilde expansion for file paths (e.g., "~/Documents").
func expandPath(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}

	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	homeDir := usr.HomeDir

	if path == "~" {
		return homeDir, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(homeDir, path[2:]), nil
	}

	return path, fmt.Errorf("unsupported tilde expansion: only '~' and '~/' are supported")
}

// getSafePath joins the base directory with a user-provided path and ensures
// it doesn't escape the base directory.
func getSafePath(userPath string) (string, error) {
	baseDir := config.C.AI.WorkspaceDir
	if baseDir == "" {
		return "", fmt.Errorf("workspace directory is not configured")
	}

	expandedBaseDir, err := expandPath(baseDir)
	if err != nil {
		return "", fmt.Errorf("could not expand workspace directory path '%s': %w", baseDir, err)
	}

	if err := os.MkdirAll(expandedBaseDir, 0o755); err != nil {
		return "", fmt.Errorf("could not create workspace directory: %w", err)
	}

	absBase, err := filepath.Abs(expandedBaseDir)
	if err != nil {
		return "", fmt.Errorf("could not get absolute path for workspace: %w", err)
	}

	finalPath := filepath.Join(absBase, userPath)

	if !strings.HasPrefix(finalPath, absBase) {
		return "", fmt.Errorf("path traversal detected: access to '%s' is not allowed as it is outside the workspace", userPath)
	}

	return finalPath, nil
}

func listFiles(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(safePath)
	if err != nil {
		return nil, err
	}

	var files []map[string]any
	for _, entry := range entries {
		info, err := entry.Info()
		fileInfo := map[string]any{
			"name":  entry.Name(),
			"isDir": entry.IsDir(),
		}
		if err == nil {
			fileInfo["size"] = info.Size()
			fileInfo["modTime"] = info.ModTime().Format(time.RFC3339)
		}
		files = append(files, fileInfo)
	}
	return map[string]any{"files": files}, nil
}

func readFile(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(safePath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": string(content)}, nil
}

func createFile(path string, content string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	err = os.WriteFile(safePath, []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("file '%s' created successfully", path)}, nil
}

func deleteFile(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	err = os.Remove(safePath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("file '%s' deleted successfully", path)}, nil
}

func getFileSystemTool() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        "listFiles",
				Description: "List files and directories in a given path relative to the workspace. Use '.' for the current directory.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"path": {Type: genai.TypeString, Description: "The directory path to list. Defaults to the workspace root if empty."},
					},
				},
			},
			{
				Name:        "readFile",
				Description: "Read the entire content of a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to read."}}, Required: []string{"path"}},
			},
			{
				Name:        "createFile",
				Description: "Create or overwrite a file in the workspace with specified content.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to create."}, "content": {Type: genai.TypeString, Description: "The content to write to the file."}}, Required: []string{"path", "content"}},
			},
			{
				Name:        "deleteFile",
				Description: "Delete a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to delete."}}, Required: []string{"path"}},
			},
		},
	}
}
