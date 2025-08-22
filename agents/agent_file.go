package agents

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
	"gemini/tools"
)

const (
	AgentFileName = "fileAgent"
	pathError     = "'path' argument is required"
)

func init() {
	RegisterFactory(AgentFileName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewFileAgent(ctx, client, toolset, bus)
	})
}

type FileAgent struct {
	*Agent
	fileTool *tools.File
}

// NewFileAgent creates a specialized agent for reading local files.
func NewFileAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, _ *EventBus.Bus) *FileAgent {
	// Define the function declarations for all file system tools.
	functions := []*genai.FunctionDeclaration{
		{
			Name:        "listFiles",
			Description: "FILE SYSTEM: List files and directories in a given path relative to the workspace. Use '.' for the current directory.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"path": {Type: genai.TypeString, Description: "The directory path to list. Defaults to the workspace root if empty."},
				},
			},
		},
		{
			Name:        "readFile",
			Description: "FILE SYSTEM: Read the entire content of a file from the workspace.",
			Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to read."}}, Required: []string{"path"}},
		},
		{
			Name:        "createFile",
			Description: "FILE SYSTEM: Create or overwrite a file in the workspace with specified content.",
			Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to create."}, "content": {Type: genai.TypeString, Description: "The content to write to the file."}}, Required: []string{"path", "content"}},
		},
		{
			Name:        "deleteFile",
			Description: "FILE SYSTEM: Delete a file or an empty directory from the workspace.",
			Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file or directory to delete."}}, Required: []string{"path"}},
		},
		{
			Name:        "makeDirectory",
			Description: "FILE SYSTEM: Create a new directory at the specified path within the workspace. It can create parent directories if they don't exist.",
			Parameters: &genai.Schema{
				Type:       genai.TypeObject,
				Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path for the new directory."}},
				Required:   []string{"path"},
			},
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
		},
		{
			Name:        "getFileInfo",
			Description: "FILE SYSTEM: Get detailed information about a file or directory.",
			Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file or directory."}}, Required: []string{"path"}},
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
		},
		{
			Name:        "appendToFile",
			Description: "FILE SYSTEM: Append content to the end of an existing file. If the file does not exist, it will be created.",
			Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the file to append to."}, "content": {Type: genai.TypeString, Description: "The content to append."}}, Required: []string{"path", "content"}},
		},
		{
			Name:        "uploadImage",
			Description: "FILE SYSTEM: For analyzing a screenshot just taken, use the `detectObjects` tool directly. Upload an image file from the workspace to the session context. Use this tool when the user explicitly asks to analyze a specific file by its name.",
			Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "The path of the image file to upload."}}, Required: []string{"path"}},
			Behavior:    genai.BehaviorBlocking,
		},
	}

	// Append the function to the shared toolset.
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, functions...)

	safePathToWorkspace := helpers.Check(config.GetSafePath(config.C.AI.WorkspaceDir))
	agentInstructions := fmt.Sprintf(`You have access to a file system toolset.
All file operations are restricted to the "%s" directory. 
All paths provided to tools like "listFiles","readFile", "createFile", etc., must be relative to this workspace.`,
		safePathToWorkspace)

	agentConfig := AgentConfig{
		Name:              AgentFileName,
		AgentInstructions: agentInstructions,
		// This agent only executes tools, it does not generate creative responses,
		// so a response schema is not needed.
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	fileAgent := &FileAgent{
		Agent:    baseAgent,
		fileTool: tools.NewFile(),
	}

	return fileAgent
}

// WarmUp for FileAgent does nothing as it only executes local tools.
func (a *FileAgent) WarmUp() time.Duration {
	return 0
}

func (a *FileAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "readFile":
		return a.handleReadFileTool(call)
	case "createFile":
		return a.handleCreateFileTool(call)
	case "deleteFile":
		return a.handleDeleteFileTool(call)
	case "listFiles":
		return a.handleListFilesTool(call)
	case "makeDirectory":
		return a.handleMakeDirectoryTool(call)
	case "moveFile":
		return a.handleMoveFileTool(call)
	case "copyFile":
		return a.handleCopyFileTool(call)
	case "getFileInfo":
		return a.handleGetFileInfoTool(call)
	case "searchFiles":
		return a.handleSearchFilesTool(call)
	case "appendToFile":
		return a.handleAppendToFileTool(call)
	case "uploadImage":
		return a.handleUploadImageTool(call)
	default:
		// If this agent doesn't handle the tool, return nil to allow other agents to try.
		return nil
	}
}

func (a *FileAgent) handleReadFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf(pathError))
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	content, err := a.fileTool.Read(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"content": content}, nil)
}

func (a *FileAgent) handleCreateFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, pathOK := call.Args["path"].(string)
	content, contentOK := call.Args["content"].(string)
	if !pathOK || !contentOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' and 'content' arguments are required"))
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	err = a.fileTool.Create(safePath, content)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "file created successfully"}, nil)
}

func (a *FileAgent) handleDeleteFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, ok := call.Args["path"].(string)
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf(pathError))
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	err = a.fileTool.Delete(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "deleted successfully"}, nil)
}

func (a *FileAgent) handleListFilesTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, _ := call.Args["path"].(string)
	if path == "" {
		path = "." // Default to current directory
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	files, err := a.fileTool.List(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"files": files}, nil)
}

func (a *FileAgent) handleMakeDirectoryTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, ok := call.Args["path"].(string)
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf(pathError))
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	err = a.fileTool.MkdirAll(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "directory created successfully"}, nil)
}

func (a *FileAgent) handleMoveFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	source, sourceOK := call.Args["source_path"].(string)
	dest, destOK := call.Args["destination_path"].(string)
	if !sourceOK || !destOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'source_path' and 'destination_path' arguments are required"))
	}
	safeSource, err := config.GetSafePath(source)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	safeDest, err := config.GetSafePath(dest)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	err = a.fileTool.Move(safeSource, safeDest)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "moved successfully"}, nil)
}

func (a *FileAgent) handleCopyFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	source, sourceOK := call.Args["source_path"].(string)
	dest, destOK := call.Args["destination_path"].(string)
	if !sourceOK || !destOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'source_path' and 'destination_path' arguments are required"))
	}
	safeSource, err := config.GetSafePath(source)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	safeDest, err := config.GetSafePath(dest)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	bytesCopied, err := a.fileTool.Copy(safeSource, safeDest)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "copied successfully", "bytesCopied": bytesCopied}, nil)
}

func (a *FileAgent) handleGetFileInfoTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, ok := call.Args["path"].(string)
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf(pathError))
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	info, err := a.fileTool.Info(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, info, nil)
}

func (a *FileAgent) handleSearchFilesTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	pattern, ok := call.Args["pattern"].(string)
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'pattern' argument is required"))
	}
	path, _ := call.Args["path"].(string)
	if path == "" {
		path = "." // Default to current directory
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	files, err := a.fileTool.Search(safePath, pattern)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"files": files}, nil)
}

func (a *FileAgent) handleAppendToFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, pathOK := call.Args["path"].(string)
	content, contentOK := call.Args["content"].(string)
	if !pathOK || !contentOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' and 'content' arguments are required"))
	}
	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	err = a.fileTool.Append(safePath, content)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "content appended successfully"}, nil)
}

func (a *FileAgent) handleUploadImageTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf(pathError))
	}

	safePath, err := config.GetSafePath(path)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	data, err := os.ReadFile(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to read image file '%s': %w", path, err))
	}

	mimeType := mime.TypeByExtension(filepath.Ext(safePath))
	if !strings.HasPrefix(mimeType, "image/") {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("file '%s' is not a supported image type (MIME: %s)", path, mimeType))
	}

	parts := []*genai.Part{
		genai.NewPartFromText(fmt.Sprintf("The user has uploaded the image '%s'. Please analyze it.", path)),
		genai.NewPartFromBytes(data, mimeType),
	}
	turn := genai.NewContentFromParts(parts, genai.RoleUser)
	content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

	result := map[string]any{"status": "Image prepared for analysis.", "send_content": content}
	return a.CreateFunctionResponse(call, result, nil)
}
