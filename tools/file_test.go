package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFile_Read(t *testing.T) {
	f := NewFile()
	testFilePath := "test_read.txt"
	testContent := "Hello, Gophers!"

	// Create a temporary file for testing
	err := os.WriteFile(testFilePath, []byte(testContent), 0o644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove(testFilePath) // Clean up after the test

	// Test successful read
	content, err := f.Read(testFilePath)
	if err != nil {
		t.Errorf("Read failed: %v", err)
	}
	if content != testContent {
		t.Errorf("Read content mismatch: got %q, want %q", content, testContent)
	}

	// Test reading a non-existent file
	_, err = f.Read("non_existent_file.txt")
	if err == nil {
		t.Error("Read did not return an error for a non-existent file")
	}
}

func TestFile_Create(t *testing.T) {
	f := NewFile()
	testDirPath := "test_dir" // Use a directory name
	testFilePath := filepath.Join(testDirPath, "test_create.txt")
	testContent := "Content to create"

	defer os.RemoveAll(testDirPath) // Clean up directory

	// Test successful creation
	err := f.Create(testFilePath, testContent)
	if err != nil {
		t.Errorf("Create failed: %v", err)
	}

	// Verify content
	content, err := os.ReadFile(testFilePath)
	if err != nil {
		t.Fatalf("Failed to read created file: %v", err)
	}
	if string(content) != testContent {
		t.Errorf("Created file content mismatch: got %q, want %q", string(content), testContent)
	}

	// Test overwriting
	newContent := "New content"
	err = f.Create(testFilePath, newContent)
	if err != nil {
		t.Errorf("Overwrite failed: %v", err)
	}
	content, err = os.ReadFile(testFilePath)
	if err != nil {
		t.Fatalf("Failed to read overwritten file: %v", err)
	}
	if string(content) != newContent {
		t.Errorf("Overwritten file content mismatch: got %q, want %q", string(content), newContent)
	}
}

func TestFile_Delete(t *testing.T) {
	f := NewFile()

	// Test deleting a file
	testFilePath := "test_delete_file.txt"
	err := os.WriteFile(testFilePath, []byte(""), 0o644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	err = f.Delete(testFilePath)
	if err != nil {
		t.Errorf("Delete file failed: %v", err)
	}
	if _, err := os.Stat(testFilePath); !os.IsNotExist(err) {
		t.Error("File was not deleted")
	}

	// Test deleting a directory
	testDirPath := "test_delete_dir"
	err = os.Mkdir(testDirPath, 0o755)
	if err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}
	err = f.Delete(testDirPath)
	if err != nil {
		t.Errorf("Delete directory failed: %v", err)
	}
	if _, err := os.Stat(testDirPath); !os.IsNotExist(err) {
		t.Error("Directory was not deleted")
	}

	// Test deleting a non-existent path (should not return error)
	err = f.Delete("non_existent_path")
	if err != nil {
		t.Errorf("Delete on non-existent path returned an error: %v", err)
	}
}

func TestFile_List(t *testing.T) {
	f := NewFile()
	testDirPath := "test_list_dir"
	_ = os.MkdirAll(testDirPath, 0o755)
	defer os.RemoveAll(testDirPath)

	// Create some files and a subdirectory
	_ = os.WriteFile(filepath.Join(testDirPath, "file1.txt"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(testDirPath, "file2.txt"), []byte(""), 0o644)
	_ = os.Mkdir(filepath.Join(testDirPath, "subdir"), 0o755)

	files, err := f.List(testDirPath)
	if err != nil {
		t.Errorf("List failed: %v", err)
	}

	if len(files) != 3 {
		t.Fatalf("Expected 3 entries, got %d", len(files))
	}

	// Check if expected files/dirs are present
	foundNames := make(map[string]bool)
	for _, fileInfo := range files {
		foundNames[fileInfo["name"].(string)] = true
	}

	if !foundNames["file1.txt"] || !foundNames["file2.txt"] || !foundNames["subdir"] {
		t.Error("Did not find all expected entries in list")
	}
}

func TestFile_MkdirAll(t *testing.T) {
	f := NewFile()
	
	// Create a temporary directory for the test
	tempRoot := t.TempDir()
	testDirPath := filepath.Join(tempRoot, "a", "b", "c")

	err := f.MkdirAll(testDirPath)
	if err != nil {
		t.Errorf("MkdirAll failed: %v", err)
	}

	if _, err := os.Stat(testDirPath); os.IsNotExist(err) {
		t.Error("Directory was not created")
	}

	// Test creating an existing directory (should not return error)
	err = f.MkdirAll(testDirPath)
	if err != nil {
		t.Errorf("MkdirAll on existing directory returned an error: %v", err)
	}
}

func TestFile_Move(t *testing.T) {
	f := NewFile()
	sourcePath := "test_move_source.txt"
	destinationPath := "test_move_dest.txt"
	testContent := "Move me!"

	_ = os.WriteFile(sourcePath, []byte(testContent), 0o644)
	defer os.Remove(destinationPath) // Clean up destination
	defer os.Remove(sourcePath)      // In case move fails

	err := f.Move(sourcePath, destinationPath)
	if err != nil {
		t.Errorf("Move failed: %v", err)
	}

	// Verify source no longer exists
	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Error("Source file still exists after move")
	}

	// Verify destination exists and has content
	content, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatalf("Failed to read destination file: %v", err)
	}
	if string(content) != testContent {
		t.Errorf("Moved file content mismatch: got %q, want %q", string(content), testContent)
	}
}

func TestFile_Copy(t *testing.T) {
	f := NewFile()
	sourcePath := "test_copy_source.txt"
	destinationPath := "test_copy_dest.txt"
	testContent := "Copy me!"

	_ = os.WriteFile(sourcePath, []byte(testContent), 0o644)
	defer os.Remove(sourcePath)
	defer os.Remove(destinationPath)

	_, err := f.Copy(sourcePath, destinationPath)
	if err != nil {
		t.Errorf("Copy failed: %v", err)
	}

	// Verify source still exists
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		t.Error("Source file does not exist after copy")
	}

	// Verify destination exists and has content
	content, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatalf("Failed to read destination file: %v", err)
	}
	if string(content) != testContent {
		t.Errorf("Copied file content mismatch: got %q, want %q", string(content), testContent)
	}
}

func TestFile_Info(t *testing.T) {
	f := NewFile()
	testFilePath := "test_info.txt"
	_ = os.WriteFile(testFilePath, []byte("some content"), 0o644)
	defer os.Remove(testFilePath)

	info, err := f.Info(testFilePath)
	if err != nil {
		t.Errorf("Info failed: %v", err)
	}

	if info["name"] != testFilePath {
		t.Errorf("Expected name %q, got %q", testFilePath, info["name"])
	}
	if info["isDir"].(bool) {
		t.Error("Expected isDir to be false, got true")
	}
	if info["size"].(int64) <= 0 {
		t.Error("Expected size to be greater than 0")
	}
	if info["modTime"] == "" {
		t.Error("Expected modTime to be set")
	}
	if info["perms"] == "" {
		t.Error("Expected perms to be set")
	}

	// Test non-existent file
	_, err = f.Info("non_existent_info.txt")
	if err == nil {
		t.Error("Info did not return an error for non-existent file")
	}
}

func TestFile_Search(t *testing.T) {
	f := NewFile()
	testRoot := "test_search_root"
	_ = os.MkdirAll(filepath.Join(testRoot, "subdir1"), 0o755)
	_ = os.MkdirAll(filepath.Join(testRoot, "subdir2"), 0o755)
	defer os.RemoveAll(testRoot)

	// Create test files
	_ = os.WriteFile(filepath.Join(testRoot, "fileA.txt"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(testRoot, "subdir1", "fileB.go"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(testRoot, "subdir2", "image.png"), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(testRoot, "another.txt"), []byte(""), 0o644)

	tests := []struct {
		name          string
		pattern       string
		expectedFiles []string
	}{
		{"find all .txt", "*.txt", []string{"fileA.txt", "another.txt"}},
		{"find all .go", "*.go", []string{"subdir1/fileB.go"}},
		{"find all files", "*", []string{"fileA.txt", "another.txt", "subdir1/fileB.go", "subdir2/image.png"}},
		{"find non-existent", "*.json", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := f.Search(testRoot, tt.pattern)
			if err != nil {
				t.Errorf("Search failed for pattern %q: %v", tt.pattern, err)
			}
			// Sort for consistent comparison
			sort.Strings(found)
			sort.Strings(tt.expectedFiles)

			if tt.name == "find non-existent" {
				assert.Empty(t, found, "Expected no files to be found for pattern %q", tt.pattern)
			} else if !reflect.DeepEqual(found, tt.expectedFiles) {
				t.Errorf("Search mismatch for pattern %q: got %v, want %v", tt.pattern, found, tt.expectedFiles)
			}
		})
	}
}

func TestFile_Append(t *testing.T) {
	f := NewFile()

	t.Run("append to file without newline", func(t *testing.T) {
		testFilePath := filepath.Join(t.TempDir(), "append_no_newline.txt")
		initialContent := "Initial line."
		appendContent := "Appended line."
		require.NoError(t, os.WriteFile(testFilePath, []byte(initialContent), 0o644))

		err := f.Append(testFilePath, appendContent)
		require.NoError(t, err)

		content, err := os.ReadFile(testFilePath)
		require.NoError(t, err)

		expectedContent := "Initial line.Appended line."
		assert.Equal(t, expectedContent, string(content))
	})

	t.Run("append to file with newline", func(t *testing.T) {
		testFilePath := filepath.Join(t.TempDir(), "append_with_newline.txt")
		initialContent := "Initial line."
		appendContent := "Appended line."
		require.NoError(t, os.WriteFile(testFilePath, []byte(initialContent), 0o644))

		err := f.Append(testFilePath, appendContent)
		require.NoError(t, err)

		content, err := os.ReadFile(testFilePath)
		require.NoError(t, err)

		expectedContent := "Initial line.Appended line."
		assert.Equal(t, expectedContent, string(content))
	})

	t.Run("append to new file", func(t *testing.T) {
		newFilePath := filepath.Join(t.TempDir(), "append_new.txt")
		err := f.Append(newFilePath, "First line.")
		require.NoError(t, err)

		content, err := os.ReadFile(newFilePath)
		require.NoError(t, err)
		assert.Equal(t, "First line.", string(content))
	})
}
