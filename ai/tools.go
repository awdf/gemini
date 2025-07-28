package ai

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/inout"
	"gemini/wayland"
)

// --- File System Tool Implementations (Shared) ---

// executeSingleToolCall dispatches a single tool call to the appropriate Go function
// and returns a structured FunctionResponse. This function is shared between PostAI and LiveAI.
func executeSingleToolCall(call *genai.FunctionCall, verifyed bool) *genai.FunctionResponse {
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
	case "makeDirectory":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			result, err = makeDirectory(path)
		}
	case "moveFile":
		source, sourceOK := call.Args["source_path"].(string)
		dest, destOK := call.Args["destination_path"].(string)
		if !sourceOK || source == "" || !destOK || dest == "" {
			err = fmt.Errorf("'source_path' and 'destination_path' arguments are required")
		} else {
			result, err = moveFile(source, dest)
		}
	case "copyFile":
		source, sourceOK := call.Args["source_path"].(string)
		dest, destOK := call.Args["destination_path"].(string)
		if !sourceOK || source == "" || !destOK || dest == "" {
			err = fmt.Errorf("'source_path' and 'destination_path' arguments are required")
		} else {
			result, err = copyFile(source, dest)
		}
	case "getFileInfo":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			result, err = getFileInfo(path)
		}
	case "searchFiles":
		pattern, patternOK := call.Args["pattern"].(string)
		path, _ := call.Args["path"].(string) // path is optional
		if !patternOK || pattern == "" {
			err = fmt.Errorf("'pattern' argument is required")
		} else {
			result, err = searchFiles(pattern, path)
		}
	case "appendToFile":
		path, pathOK := call.Args["path"].(string)
		content, contentOK := call.Args["content"].(string)
		if !pathOK || path == "" || !contentOK {
			err = fmt.Errorf("'path' and 'content' arguments are required")
		} else {
			result, err = appendToFile(path, content)
		}
	case "detectObjects":
		// This tool is special and handled in LiveAI, as it requires access to the session's image buffer.
		// This case is a fallback for non-live mode.
		err = fmt.Errorf("the 'detectObjects' tool is only available in live mode")
	case "uploadImage":
		err = fmt.Errorf("the 'uploadImage' tool is only available in live mode")
	case "mouseClick":
		// The genai library unmarshals JSON numbers into float64 by default.
		xFloat, xOK := call.Args["x"].(float64)
		yFloat, yOK := call.Args["y"].(float64)
		clicksFloat, _ := call.Args["clicks"].(float64)
		if !xOK || !yOK {
			err = fmt.Errorf("arguments 'x' and 'y' are required and must be numbers")
		} else {
			clicks := int(clicksFloat)
			if clicks < 1 {
				clicks = 1
			}
			result, err = mouseClick(xFloat, yFloat, clicks, verifyed)
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
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
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

func makeDirectory(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	// MkdirAll creates a directory named path,
	// along with any necessary parents, and returns nil,
	// or else returns an error.
	err = os.MkdirAll(safePath, 0o755)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("directory '%s' created successfully", path)}, nil
}

func moveFile(sourcePath, destinationPath string) (any, error) {
	safeSourcePath, err := getSafePath(sourcePath)
	if err != nil {
		return nil, err
	}
	safeDestinationPath, err := getSafePath(destinationPath)
	if err != nil {
		return nil, err
	}
	err = os.Rename(safeSourcePath, safeDestinationPath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("moved '%s' to '%s' successfully", sourcePath, destinationPath)}, nil
}

func copyFile(sourcePath, destinationPath string) (any, error) {
	safeSourcePath, err := getSafePath(sourcePath)
	if err != nil {
		return nil, err
	}
	safeDestinationPath, err := getSafePath(destinationPath)
	if err != nil {
		return nil, err
	}

	sourceFile, err := os.Open(safeSourcePath)
	if err != nil {
		return nil, err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(safeDestinationPath)
	if err != nil {
		return nil, err
	}
	defer destFile.Close()

	bytesCopied, err := io.Copy(destFile, sourceFile)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":      fmt.Sprintf("file '%s' copied to '%s' successfully", sourcePath, destinationPath),
		"bytesCopied": bytesCopied,
	}, nil
}

func getFileInfo(path string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(safePath)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name":    info.Name(),
		"size":    info.Size(),
		"isDir":   info.IsDir(),
		"modTime": info.ModTime().Format(time.RFC3339),
		"perms":   info.Mode().String(),
	}, nil
}

func searchFiles(pattern, path string) (any, error) {
	// get the absolute path of the workspace root
	baseDir, err := getSafePath("")
	if err != nil {
		return nil, err
	}

	// get the absolute path of the search directory
	searchRoot, err := getSafePath(path)
	if err != nil {
		return nil, err
	}

	var foundFiles []string
	err = filepath.WalkDir(searchRoot, func(currentPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			matched, err := filepath.Match(pattern, d.Name())
			if err != nil {
				return err // Malformed pattern
			}
			if matched {
				// We want to return the path relative to the workspace root for the user
				relPath, err := filepath.Rel(baseDir, currentPath)
				if err != nil {
					// This shouldn't happen if currentPath is inside baseDir
					return err
				}
				foundFiles = append(foundFiles, relPath)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return map[string]any{"files": foundFiles}, nil
}

func appendToFile(path, content string) (any, error) {
	safePath, err := getSafePath(path)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(safePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return nil, err
	}

	return map[string]any{"status": fmt.Sprintf("content appended to file '%s' successfully", path)}, nil
}

func mouseClick(x, y float64, clicks int, verifyed bool) (any, error) {
	if !verifyed {
		return map[string]any{"error": fmt.Sprintln("You must get positive approve from 'verifyObjectDetection' tool before apply mouse actions.")}, nil
	}
	// The coordinates are now absolute pixel coordinates, no normalization needed.
	absX := int(x)
	absY := int(y)

	log.Printf("Performing %d mouse click(s) at absolute pixel coordinates (%d, %d)", clicks, absX, absY)

	// Execute the desktop automation.
	wayland.MoveMouseToPosition(absX, absY)
	// A small delay can help ensure the OS has processed the move event before the click event arrives.
	time.Sleep(100 * time.Millisecond)
	wayland.MouseLeftClick(clicks)

	return map[string]any{"status": fmt.Sprintf("%d mouse click(s) performed at (%d, %d)", clicks, absX, absY)}, nil
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
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "readFile",
				Description: "Read the entire content of a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to read."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "createFile",
				Description: "Create or overwrite a file in the workspace with specified content.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to create."}, "content": {Type: genai.TypeString, Description: "The content to write to the file."}}, Required: []string{"path", "content"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "deleteFile",
				Description: "Delete a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to delete."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "makeDirectory",
				Description: "Create a new directory at the specified path within the workspace. It can create parent directories if they don't exist.",
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path for the new directory."}},
					Required:   []string{"path"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "moveFile",
				Description: "Move or rename a file or directory within the workspace.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"source_path":      {Type: genai.TypeString, Description: "The current path of the file or directory."},
						"destination_path": {Type: genai.TypeString, Description: "The new path for the file or directory."},
					},
					Required: []string{"source_path", "destination_path"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "copyFile",
				Description: "Copy a file from a source path to a destination path within the workspace.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"source_path":      {Type: genai.TypeString, Description: "The path of the file to copy."},
						"destination_path": {Type: genai.TypeString, Description: "The path to copy the file to."},
					},
					Required: []string{"source_path", "destination_path"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "getFileInfo",
				Description: "Get detailed information about a file or directory.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file or directory."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "searchFiles",
				Description: "Search for files recursively in a directory by a name pattern (glob).",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"pattern": {Type: genai.TypeString, Description: "The glob pattern to match file names against (e.g., '*.go', 'data*')."},
						"path":    {Type: genai.TypeString, Description: "The directory to start the search from. Defaults to the workspace root if empty."},
					},
					Required: []string{"pattern"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "appendToFile",
				Description: "Append content to the end of an existing file. If the file does not exist, it will be created.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to append to."}, "content": {Type: genai.TypeString, Description: "The content to append."}}, Required: []string{"path", "content"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "uploadImage",
				Description: "For analyzing a screenshot just taken, use the `detectObjects` tool directly. Upload an image file from the workspace to the session context. Use this tool when the user explicitly asks to analyze a specific file by its name.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the image file to upload."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "verifyObjectDetection",
				Description: "After using 'detectObjects', use this helper tool to draw the returned bounding box on the image. The tool will upload image with red box to context. This helps model ensure the correct object is identified before clicking.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"xmin": {Type: genai.TypeInteger, Description: "The normalized x-coordinate of the left edge of the box (0-1000)."},
						"ymin": {Type: genai.TypeInteger, Description: "The normalized y-coordinate of the top edge of the box (0-1000)."},
						"xmax": {Type: genai.TypeInteger, Description: "The normalized x-coordinate of the right edge of the box (0-1000)."},
						"ymax": {Type: genai.TypeInteger, Description: "The normalized y-coordinate of the bottom edge of the box (0-1000)."},
					},
					Required: []string{"xmin", "ymin", "xmax", "ymax"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "detectObjects",
				Description: "Automaticaly upload image in the session context. Analyzes the image currently in the session context (e.g., from a recent screenshot) to detect specific objects based on a query. Returns a list of detected objects and their bounding boxes.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"query": {Type: genai.TypeString, Description: "A natural language query describing the objects to detect (e.g., 'all the cars', 'the red apple')."},
					},
					Required: []string{"query"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "mouseClick",
				Description: "Moves the mouse to a specified absolute pixel coordinate and performs a left click. This is used to interact with UI elements identified by the 'detectObjects' tool. Detected objects and their bounding boxes must be verified with 'verifyObjectDetection' before 'mouseClick' use, otherwise make decision about error resolving with no user confirmation. The coordinates should be the center of the target object. Can perform multiple clicks for actions like double-clicking.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"x":      {Type: genai.TypeInteger, Description: "The absolute x-coordinate in pixels of the click target."},
						"y":      {Type: genai.TypeInteger, Description: "The absolute y-coordinate in pixels of the click target."},
						"clicks": {Type: genai.TypeInteger, Description: "The number of times to click. Defaults to 1. Use 2 for a double-click."},
					},
					Required: []string{"x", "y"},
				},
				Behavior: genai.BehaviorBlocking,
			},
		},
	}
}
