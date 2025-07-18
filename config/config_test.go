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
		configPath := filepath.Join(tempDir, "config.toml")

		// Ensure file doesn't exist
		_, err := os.Stat(configPath)
		require.True(t, os.IsNotExist(err), "config file should not exist before Load")

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
		configPath := filepath.Join(tempDir, "config.toml")

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

		Load(configPath)

		assert.Equal(t, true, C.Debug)
		assert.Equal(t, true, C.Trace)
		assert.Equal(t, "test.log", C.LogFile)
		assert.Equal(t, "test-model", C.AI.Model)
		assert.Equal(t, true, C.AI.EnableCache)
	})

	t.Run("expands environment variables", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "config.toml")
		apiKey := "test-api-key-from-env"

		t.Setenv("TEST_API_KEY", apiKey)

		content := `
[AI]
APIKey = "${TEST_API_KEY}"
`
		err := os.WriteFile(configPath, []byte(content), 0o644)
		require.NoError(t, err)

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
