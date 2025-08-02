package tools

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// File provides functionality to read files from the local filesystem.
type File struct {
	// In a real-world scenario, you might add configuration for base paths,
	// allowed directories, or file size limits for security.
}

// NewFile creates a new File tool.
func NewFile() *File {
	return &File{}
}

// Read reads the content of a file at the given path.
func (f *File) Read(path string) (string, error) {
	// Security: In a real application, you MUST validate the path to prevent directory traversal attacks.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file '%s': %w", path, err)
	}
	return string(data), nil
}

// Create creates or overwrites a file at the given path with the specified content.
// It also creates any necessary parent directories.
func (f *File) Create(path string, content string) error {
	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	// WriteFile truncates the file if it exists, effectively overwriting it.
	return os.WriteFile(path, []byte(content), 0o644)
}

// Delete removes a file or an empty directory at the given path.
func (f *File) Delete(path string) error {
	return os.Remove(path)
}

// List reads the contents of a directory at the given path.
func (f *File) List(path string) ([]map[string]any, error) {
	entries, err := os.ReadDir(path)
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
	return files, nil
}

// MkdirAll creates a directory at the given path, including any necessary parents.
func (f *File) MkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

// Move renames (moves) a file or directory from a source to a destination path.
func (f *File) Move(sourcePath, destinationPath string) error {
	return os.Rename(sourcePath, destinationPath)
}

// Copy copies a file from a source to a destination path.
func (f *File) Copy(sourcePath, destinationPath string) (int64, error) {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return 0, err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(destinationPath)
	if err != nil {
		return 0, err
	}
	defer destFile.Close()

	return io.Copy(destFile, sourceFile)
}

// Info gets detailed information about a file or directory.
func (f *File) Info(path string) (map[string]any, error) {
	info, err := os.Stat(path)
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

// Search recursively finds files matching a glob pattern within a given directory.
func (f *File) Search(root, pattern string) ([]string, error) {
	var foundFiles []string
	err := filepath.WalkDir(root, func(currentPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			// Match against the base name of the file.
			matched, err := filepath.Match(pattern, d.Name())
			if err != nil {
				return err // Malformed pattern
			}
			if matched {
				// Return the path relative to the original search root.
				relPath, err := filepath.Rel(root, currentPath)
				if err != nil {
					// This shouldn't happen if currentPath is inside root
					return err
				}
				// For consistency, use forward slashes, even on Windows.
				foundFiles = append(foundFiles, filepath.ToSlash(relPath))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return foundFiles, nil
}

// Append appends content to a file, creating it if it doesn't exist.
func (f *File) Append(path, content string) error {
	// Open the file with flags to append, create if not exists, and write-only.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	// Add a newline if the file is not empty and doesn't end with one.
	// This improves readability for appended content.
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		buf := make([]byte, 1)
		_, err := file.ReadAt(buf, info.Size()-1)
		if err != nil && err != io.EOF {
			return err
		}
		if string(buf) != "\n" {
			if _, err := file.WriteString("\n"); err != nil {
				return err
			}
		}
	}

	// Write the new content.
	if _, err := file.WriteString(content); err != nil {
		return err
	}

	return nil
}
