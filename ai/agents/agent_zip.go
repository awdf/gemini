package agents

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/genai"

	"gemini/config"
)

const AgentZipName = "zipAgent"

func init() {
	RegisterFactory(AgentZipName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewZipAgent(ctx, client, toolset, bus)
	})
}

type ZipAgent struct {
	*Agent
}

// NewZipAgent creates a specialized agent for handling zip archives.
func NewZipAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, _ *EventBus.Bus) *ZipAgent {
	unzipFunc := &genai.FunctionDeclaration{
		Name:        "unzipFile",
		Description: "ZIP: Extracts a zip archive into a new directory within the workspace. Returns the list of extracted files.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the zip file to extract.",
				},
				"destination": {
					Type:        genai.TypeString,
					Description: "Optional. The directory name to extract files into. Defaults to the zip file's name without the extension.",
				},
			},
			Required: []string{"path"},
		},
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, unzipFunc)

	agentConfig := AgentConfig{
		Name: AgentZipName,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	zipAgent := &ZipAgent{
		Agent: baseAgent,
	}

	return zipAgent
}

func (a *ZipAgent) WarmUp() time.Duration {
	return 0
}

func (a *ZipAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	if call.Name == "unzipFile" {
		return a.handleUnzipFile(call)
	}
	return nil
}

func (a *ZipAgent) handleUnzipFile(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)
	path, ok := call.Args["path"].(string)
	if !ok || path == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'path' argument is required and must be a non-empty string"))
	}

	destination, _ := call.Args["destination"].(string)

	finalDest, extractedFiles, err := a.unzip(path, destination)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	return a.CreateFunctionResponse(call, map[string]any{
		"status":         "file extracted successfully",
		"extracted_path": finalDest,
		"files":          extractedFiles,
	}, nil)
}

// unzip extracts a zip archive to a destination directory and returns the final destination path.
func (a *ZipAgent) unzip(source, destination string) (string, []string, error) {
	safeSource, err := config.GetSafePath(source)
	if err != nil {
		return "", nil, err
	}

	if destination == "" {
		destination = strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	}

	safeDest, err := config.GetSafePath(destination)
	if err != nil {
		return "", nil, err
	}

	var extractedFiles []string

	r, err := zip.OpenReader(safeSource)
	if err != nil {
		return "", nil, err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(safeDest, f.Name)

		// Security check to prevent zip slip vulnerability.
		if !strings.HasPrefix(fpath, filepath.Clean(safeDest)+string(os.PathSeparator)) {
			return "", nil, fmt.Errorf("illegal file path: %s", fpath)
		}

		extractedFiles = append(extractedFiles, f.Name)

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return "", nil, err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return "", nil, err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return "", nil, err
		}

		_, err = io.Copy(outFile, rc)

		outFile.Close()
		rc.Close()

		if err != nil {
			return "", nil, err
		}
	}
	return destination, extractedFiles, nil
}
