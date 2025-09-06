package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	t.Run("creates default config if not exists", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, DefaultConfigFileName)

		// Ensure file doesn't exist
		_, err := os.Stat(configPath)
		require.True(t, os.IsNotExist(err), "config file should not exist before Load")

		// Set GEMINI_PATH to satisfy the new requirement in Load()
		t.Setenv("GEMINI_PATH", tempDir)

		Load(configPath)

		// Check if file was created
		_, err = os.Stat(configPath)
		require.NoError(t, err, "Load should create a default config file")

		// Check some default values
		assert.Equal(t, "gemini-2.5-flash", C.AI.Model)
		assert.Equal(t, false, C.Debug)
		assert.Equal(t, 100, C.Display.BarWidth)
	})

	t.Run("loads existing config", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, DefaultConfigFileName)

		// Create a custom config file
		content := `
Debug = true
Trace = true
LogFile = "test.log"
[AI]
Model = "test-model"
EnableCache = true
`
		err := os.WriteFile(configPath, []byte(content), 0o644)
		require.NoError(t, err)

		// Set GEMINI_PATH
		t.Setenv("GEMINI_PATH", tempDir)

		Load(configPath)

		assert.Equal(t, true, C.Debug)
		assert.Equal(t, true, C.Trace)
		assert.Equal(t, "test.log", C.LogFile)
		assert.Equal(t, "test-model", C.AI.Model)
		assert.Equal(t, true, C.AI.EnableCache)
	})

	t.Run("expands environment variables", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, DefaultConfigFileName)
		apiKey := "test-api-key-from-env"

		t.Setenv("TEST_API_KEY", apiKey)

		content := `
[AI]
APIKey = "${TEST_API_KEY}"
`
		err := os.WriteFile(configPath, []byte(content), 0o644)
		require.NoError(t, err)

		// Set GEMINI_PATH
		t.Setenv("GEMINI_PATH", tempDir)

		Load(configPath)

		assert.Equal(t, apiKey, C.AI.APIKey)
	})
}

func TestVADConfig_WarmUpDuration(t *testing.T) {
	tests := []struct {
		name         string
		warmupString string
		expected     time.Duration
	}{
		{"valid duration", "5s", 5 * time.Second},
		{"empty duration", "", 0},
		{"invalid duration", "invalid", 1 * time.Second}, // Should fall back to default
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vadConfig := &VADConfig{WarmupDuration: tt.warmupString}
			assert.Equal(t, tt.expected, vadConfig.WarmUpDuration())
		})
	}
}

func TestGetConfigPath(t *testing.T) {
	// Set a temporary project root for tests
	originalProjectRoot := ProjectRoot
	tempProjectRoot := t.TempDir()
	ProjectRoot = tempProjectRoot
	defer func() { ProjectRoot = originalProjectRoot }()

	absCliPath := "/from/cli/" + DefaultConfigFileName
	relCliPath := "profiles/dev.toml"
	defaultPath := filepath.Join(tempProjectRoot, DefaultConfigFileName)

	t.Run("uses absolute cliPath when set", func(t *testing.T) {
		path := GetConfigPath(absCliPath)
		assert.Equal(t, absCliPath, path)
	})

	t.Run("uses default path when cliPath is empty", func(t *testing.T) {
		path := GetConfigPath("")
		assert.Equal(t, defaultPath, path)
	})

	t.Run("uses default path when cliPath is the default value", func(t *testing.T) {
		path := GetConfigPath(DefaultConfigFileName)
		assert.Equal(t, defaultPath, path)
	})

	t.Run("uses default path when cliPath is empty", func(t *testing.T) {
		path := GetConfigPath("")
		assert.Equal(t, defaultPath, path)
	})

	t.Run("resolves relative cliPath against project root if it exists there", func(t *testing.T) {
		// Create the dummy profile file relative to the temp project root
		profileDir := filepath.Join(tempProjectRoot, "profiles")
		require.NoError(t, os.Mkdir(profileDir, 0o755))
		profilePath := filepath.Join(profileDir, "dev.toml")
		require.NoError(t, os.WriteFile(profilePath, []byte(""), 0o644))

		path := GetConfigPath(relCliPath)
		assert.Equal(t, profilePath, path)
	})

	t.Run("uses relative cliPath from CWD if it exists there", func(t *testing.T) {
		// Create a temporary CWD for this test
		tempCwd := t.TempDir()
		originalCwd, _ := os.Getwd()
		require.NoError(t, os.Chdir(tempCwd))
		defer os.Chdir(originalCwd)

		// Create the dummy profile file relative to the temp project root
		profileDir := filepath.Join(tempProjectRoot, "profiles")
		require.NoError(t, os.Mkdir(profileDir, 0o755))
		profilePath := filepath.Join(profileDir, "dev.toml")
		require.NoError(t, os.WriteFile(profilePath, []byte(""), 0o644))

		// Create a file with the same relative path in the CWD
		cwdProfileDir := filepath.Join(tempCwd, "profiles")
		require.NoError(t, os.Mkdir(cwdProfileDir, 0o755))
		cwdProfilePath := filepath.Join(cwdProfileDir, "dev.toml")
		require.NoError(t, os.WriteFile(cwdProfilePath, []byte(""), 0o644))

		path := GetConfigPath(relCliPath)
		assert.Equal(t, relCliPath, path, "should prefer path relative to CWD over ProjectRoot")
	})
}
