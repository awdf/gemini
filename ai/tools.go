package ai

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/inout"
	"gemini/wayland"
)

const maxKeyCode = 127 // The highest known key code is 126 (KEY_RIGHTMETA), so the array size is 127.

// keyCodeToName provides a constant-size array for a fast, index-based lookup of key codes.
// It uses an array literal with explicit keys to correctly map non-contiguous key codes
// from linux/input-event-codes.h to their string names. This is both efficient and readable.
var keyCodeToName = [maxKeyCode]string{
	1:   "KEY_ESC",
	2:   "KEY_1",
	3:   "KEY_2",
	4:   "KEY_3",
	5:   "KEY_4",
	6:   "KEY_5",
	7:   "KEY_6",
	8:   "KEY_7",
	9:   "KEY_8",
	10:  "KEY_9",
	11:  "KEY_0",
	12:  "KEY_MINUS",
	13:  "KEY_EQUAL",
	14:  "KEY_BACKSPACE",
	15:  "KEY_TAB",
	16:  "KEY_Q",
	17:  "KEY_W",
	18:  "KEY_E",
	19:  "KEY_R",
	20:  "KEY_T",
	21:  "KEY_Y",
	22:  "KEY_U",
	23:  "KEY_I",
	24:  "KEY_O",
	25:  "KEY_P",
	26:  "KEY_LEFTBRACE",
	27:  "KEY_RIGHTBRACE",
	28:  "KEY_ENTER",
	29:  "KEY_LEFTCTRL",
	30:  "KEY_A",
	31:  "KEY_S",
	32:  "KEY_D",
	33:  "KEY_F",
	34:  "KEY_G",
	35:  "KEY_H",
	36:  "KEY_J",
	37:  "KEY_K",
	38:  "KEY_L",
	39:  "KEY_SEMICOLON",
	40:  "KEY_APOSTROPHE",
	41:  "KEY_GRAVE",
	42:  "KEY_LEFTSHIFT",
	43:  "KEY_BACKSLASH",
	44:  "KEY_Z",
	45:  "KEY_X",
	46:  "KEY_C",
	47:  "KEY_V",
	48:  "KEY_B",
	49:  "KEY_N",
	50:  "KEY_M",
	51:  "KEY_COMMA",
	52:  "KEY_DOT",
	53:  "KEY_SLASH",
	54:  "KEY_RIGHTSHIFT",
	55:  "KEY_KPASTERISK",
	56:  "KEY_LEFTALT",
	57:  "KEY_SPACE",
	58:  "KEY_CAPSLOCK",
	59:  "KEY_F1",
	60:  "KEY_F2",
	61:  "KEY_F3",
	62:  "KEY_F4",
	63:  "KEY_F5",
	64:  "KEY_F6",
	65:  "KEY_F7",
	66:  "KEY_F8",
	67:  "KEY_F9",
	68:  "KEY_F10",
	69:  "KEY_NUMLOCK",
	70:  "KEY_SCROLLLOCK",
	71:  "KEY_KP7",
	72:  "KEY_KP8",
	73:  "KEY_KP9",
	74:  "KEY_KPMINUS",
	75:  "KEY_KP4",
	76:  "KEY_KP5",
	77:  "KEY_KP6",
	78:  "KEY_KPPLUS",
	79:  "KEY_KP1",
	80:  "KEY_KP2",
	81:  "KEY_KP3",
	82:  "KEY_KP0",
	83:  "KEY_KPDOT",
	87:  "KEY_F11",
	88:  "KEY_F12",
	96:  "KEY_KPENTER",
	97:  "KEY_RIGHTCTRL",
	98:  "KEY_KPSLASH",
	99:  "KEY_SYSRQ",
	100: "KEY_RIGHTALT",
	102: "KEY_HOME",
	103: "KEY_UP",
	104: "KEY_PAGEUP",
	105: "KEY_LEFT",
	106: "KEY_RIGHT",
	107: "KEY_END",
	108: "KEY_DOWN",
	109: "KEY_PAGEDOWN",
	110: "KEY_INSERT",
	111: "KEY_DELETE",
	125: "KEY_LEFTMETA",
	126: "KEY_RIGHTMETA",
}

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
	case "uploadImage":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			err = fmt.Errorf("'path' argument is required and must be a non-empty string")
		} else {
			// This tool prepares an image to be sent to a live session via the 'send_content' mechanism.
			// The logic is encapsulated in the uploadImage function to align with other file tools.
			result, err = uploadImage(path)
		}
	case "listEmails", "readEmail", "sendEmail":
		// These tools are special and handled by the GmailAgent, as they require an authenticated service.
		// This case is a fallback for when the agent isn't used directly.
		err = fmt.Errorf("the '%s' tool must be handled by the Gmail agent", call.Name)
	case "detectObjects", "verifyObjectDetection", "mouseClick":
		// These tools are special and handled in LiveAI, as they require access to the session's image buffer.
		// This case is a fallback for non-live mode.
		err = fmt.Errorf("the '%s' tool is only available in live mode", call.Name)
	case "typeText":
		text, ok := call.Args["text"].(string)
		if !ok || text == "" {
			err = fmt.Errorf("argument 'text' is required and must be a non-empty string")
		} else {
			result, err = typeText(text)
		}
	case "keyAction":
		codesArg, codesOK := call.Args["key_codes"].([]interface{})
		if !codesOK {
			err = fmt.Errorf("argument 'key_codes' (array of integers) is required")
		} else {
			var keyCodes []int
			validCodes := true
			for _, v := range codesArg {
				if code, ok := v.(float64); ok {
					keyCodes = append(keyCodes, int(code))
				} else {
					err = fmt.Errorf("invalid item in 'key_codes' array; all must be integers")
					validCodes = false
					break
				}
			}
			if validCodes {
				result, err = keyAction(keyCodes)
			}
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

func listFiles(path string) (any, error) {
	safePath, err := config.GetSafePath(path)
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
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(safePath)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": string(content)}, nil
}

// createFile ensures a file is created at the specified path with the given
// content. It follows a "remove-then-create" logic: if an item (file, symlink,
// or empty directory) already exists at the path, it is removed before the new
// file is created. This ensures a clean state and handles edge cases like
// replacing symlinks. The function also creates any necessary parent directories.
// createFile ensures a file is created at the specified path with the given
// content. It follows a "remove-then-create" logic: if an item (file, symlink,
// or empty directory) already exists at the path, it is removed before the new
// file is created. This ensures a clean state and handles edge cases like
// replacing symlinks. The function also creates any necessary parent directories.
func createFile(path string, content string) (any, error) {
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return nil, err
	}

	// Ensure the parent directory exists to avoid errors when writing the file.
	if err := os.MkdirAll(filepath.Dir(safePath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create parent directory: %w", err)
	}

	// Remove the file if it already exists. This is done to honor the request
	// to "remove before create", which also handles replacing things like symlinks.
	// We ignore "not found" errors, but fail on others (e.g., permission denied, or non-empty directory).
	if err := os.Remove(safePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove existing item at path '%s': %w", path, err)
	}

	// Ensure the parent directory exists to avoid errors when writing the file.
	if err := os.MkdirAll(filepath.Dir(safePath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create parent directory: %w", err)
	}

	// Remove the file if it already exists. This is done to honor the request
	// to "remove before create", which also handles replacing things like symlinks.
	// We ignore "not found" errors, but fail on others (e.g., permission denied, or non-empty directory).
	if err := os.Remove(safePath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove existing item at path '%s': %w", path, err)
	}

	err = os.WriteFile(safePath, []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": fmt.Sprintf("file '%s' created successfully", path)}, nil
}

func deleteFile(path string) (any, error) {
	safePath, err := config.GetSafePath(path)
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
	safePath, err := config.GetSafePath(path)
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
	safeSourcePath, err := config.GetSafePath(sourcePath)
	if err != nil {
		return nil, err
	}
	safeDestinationPath, err := config.GetSafePath(destinationPath)
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
	safeSourcePath, err := config.GetSafePath(sourcePath)
	if err != nil {
		return nil, err
	}
	safeDestinationPath, err := config.GetSafePath(destinationPath)
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
	safePath, err := config.GetSafePath(path)
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
	baseDir, err := config.GetSafePath("")
	if err != nil {
		return nil, err
	}

	// get the absolute path of the search directory
	searchRoot, err := config.GetSafePath(path)
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
	safePath, err := config.GetSafePath(path)
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

// uploadImage prepares an image file to be sent to a live session.
func uploadImage(path string) (any, error) {
	// Use config.GetSafePath to ensure the file is within the configured workspace.
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return nil, err
	}

	// Read the file content.
	data, err := os.ReadFile(safePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read image file '%s': %w", path, err)
	}

	// Determine MIME type from file extension.
	mimeType := mime.TypeByExtension(filepath.Ext(safePath))
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, fmt.Errorf("file '%s' is not a supported image type (MIME: %s)", path, mimeType)
	}

	// Prepare the image content to be sent by the caller (executeToolCalls).
	// This follows the same pattern as verifyObjectDetection, promoting consistency.
	parts := []*genai.Part{
		genai.NewPartFromText(fmt.Sprintf("The user has uploaded the image '%s'. Please analyze it.", path)),
		genai.NewPartFromBytes(data, mimeType),
	}
	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	log.Printf("Successfully prepared image '%s' to be sent to live session.", path)
	result := map[string]any{
		"status":       fmt.Sprintf("Image '%s' prepared for analysis.", path),
		"send_content": content, // Special key for LiveAI to handle
	}
	return result, nil
}

func typeText(text string) (any, error) {
	log.Printf("Typing text: %s", text)
	wayland.Type(text)
	return map[string]any{
		"action": "type_text",
		"length": len(text),
		"result": "success",
	}, nil
}

func keyAction(keyCodes []int) (any, error) {
	keyNames := make([]string, len(keyCodes))
	for i, code := range keyCodes {
		// Check if the code is within the bounds of our slice and has a valid entry.
		if code >= 0 && code < len(keyCodeToName) && keyCodeToName[code] != "" {
			keyNames[i] = keyCodeToName[code]
		} else {
			// If the key code is not in our map, use its numeric value.
			keyNames[i] = fmt.Sprintf("CODE(%d)", code)
		}
	}
	keyNamesStr := strings.Join(keyNames, " + ")
	log.Printf("Performing key press for: %s", keyNamesStr)

	// Press the keys.
	wayland.KeyAction(keyCodes, wayland.BTN_PRESSED)
	// A short delay is crucial for the OS to register the key press before the release,
	// especially for combinations like Ctrl+C.
	time.Sleep(50 * time.Millisecond)
	// Release the keys.
	wayland.KeyAction(keyCodes, wayland.BTN_RELEASED)
	return map[string]any{
		"action": "key_action",
		"keys":   keyNames,
		"result": "success",
	}, nil
}

func getFunctionTools() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        "listFiles",
				Description: "FILE SYSTEM: List files and directories in a given path relative to the workspace. Use '.' for the current directory.",
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
				Description: "FILE SYSTEM: Read the entire content of a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to read."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "createFile",
				Description: "FILE SYSTEM: Create or overwrite a file in the workspace with specified content.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to create."}, "content": {Type: genai.TypeString, Description: "The content to write to the file."}}, Required: []string{"path", "content"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "deleteFile",
				Description: "FILE SYSTEM: Delete a file from the workspace.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to delete."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "makeDirectory",
				Description: "FILE SYSTEM: Create a new directory at the specified path within the workspace. It can create parent directories if they don't exist.",
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path for the new directory."}},
					Required:   []string{"path"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "moveFile",
				Description: "FILE SYSTEM: Move or rename a file or directory within the workspace.",
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
				Description: "FILE SYSTEM: Copy a file from a source path to a destination path within the workspace.",
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
				Description: "FILE SYSTEM: Get detailed information about a file or directory.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file or directory."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "searchFiles",
				Description: "FILE SYSTEM: Search for files recursively in a directory by a name pattern (glob).",
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
				Description: "FILE SYSTEM: Append content to the end of an existing file. If the file does not exist, it will be created.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to append to."}, "content": {Type: genai.TypeString, Description: "The content to append."}}, Required: []string{"path", "content"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "uploadImage",
				Description: "FILE SYSTEM: For analyzing a screenshot just taken, use the `detectObjects` tool directly. Upload an image file from the workspace to the session context. Use this tool when the user explicitly asks to analyze a specific file by its name.",
				Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the image file to upload."}}, Required: []string{"path"}},
				Behavior:    genai.BehaviorBlocking,
			},
			{
				Name:        "typeText",
				Description: "DESKTOP AUTOMATION: Types the given string of text using the virtual keyboard. Use this for interacting with the user interface, like filling out forms or typing commands. Each line MUST be finished with a newline character. Characters Tab and Backspace are supported.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"text": {Type: genai.TypeString, Description: "The text to be typed."},
					},
					Required: []string{"text"},
				},
				Behavior: genai.BehaviorBlocking,
			},
			{
				Name:        "keyAction",
				Description: "DESKTOP AUTOMATION: Simulates a key press (press and release) for one or more keys simultaneously (e.g., for shortcuts like Ctrl+C). Use this for keyboard shortcuts to control applications. Key codes should be from the linux/input-event-codes.h header.",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"key_codes": {
							Type:        genai.TypeArray,
							Description: "A list of integer key codes to be pressed simultaneously.",
							Items:       &genai.Schema{Type: genai.TypeInteger},
						},
					},
					Required: []string{"key_codes"},
				},
				Behavior: genai.BehaviorBlocking,
			},
		},
	}
}
