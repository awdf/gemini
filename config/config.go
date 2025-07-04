package config

import (
	"log"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// C holds the global application configuration.
var C Config

// Config defines the structure of the configuration file.
type Config struct {
	Debug    bool
	LogFile  string
	AI       AIConfig
	VAD      VADConfig
	Recorder RecorderConfig
	Display  DisplayConfig
	Pipeline PipelineConfig
}

// AIConfig holds settings related to the AI model.
type AIConfig struct {
	Model               string
	TranscriptionPrompt string
	ModelTTS            string
	Voice               string
	APIKey              string
	MainPrompt          string
	SystemPrompt        string
	Thinking            int32
	Thoughts            bool
	EnableTools         bool
	CacheDir            string
	CacheSystemPrompt   string
	EnableCache         bool
}

// VADConfig holds settings for the Voice Activity Detector.
type VADConfig struct {
	SilenceThreshold    float64
	HangoverDurationSec float64
	WarmupDuration      string `toml:"WarmupDuration"`
}

// RecorderConfig holds settings for the audio recorder.
type RecorderConfig struct {
	MinFileSizeBytes int64
}

// DisplayConfig holds settings for the terminal display.
type DisplayConfig struct {
	BarWidth         int
	UpdateIntervalMs int
}

// PipelineConfig holds settings for the GStreamer pipeline.
type PipelineConfig struct {
	BufferTimeUs int64
	Device       string
}

// Load reads the configuration from the specified file path.
// It supports expanding environment variables in the format ${VAR} or $VAR.
func Load(path string) {
	content, err := os.ReadFile(path)
	if err != nil {
		// If config file doesn't exist, create a default one.
		if os.IsNotExist(err) {
			log.Printf("Config file not found at %s, creating a default one.", path)
			createDefaultConfig(path)
			// Retry decoding after creating the file.
			content, err = os.ReadFile(path)
			if err != nil {
				log.Fatalf("Failed to read newly created config file: %v", err)
			}
		} else {
			log.Fatalf("Error reading config file %s: %v", path, err)
		}
	}

	expandedContent := os.ExpandEnv(string(content))
	if _, err := toml.Decode(expandedContent, &C); err != nil {
		log.Fatalf("Error decoding config from %s: %v", path, err)
	}
}

// createDefaultConfig creates a default config.toml file.
func createDefaultConfig(path string) {
	defaultConfig := C // Start with zero-value struct
	// Populate with default values
	defaultConfig.Debug = false
	defaultConfig.LogFile = "app.log"
	defaultConfig.AI.Model = "gemini-2.5-flash"
	defaultConfig.AI.ModelTTS = "gemini-2.5-flash-preview-tts"
	defaultConfig.AI.Voice = "Kore"
	defaultConfig.AI.TranscriptionPrompt = "Please provide a verbatim transcript of the audio."
	defaultConfig.AI.APIKey = "${GOOGLE_API_KEY}" // You can set this directly or use an environment variable.
	defaultConfig.AI.MainPrompt = "You are a helpful voice assistant. Based on the transcript, please provide a transcript and concise and accurate response. Respond in the same language as the transcript."
	defaultConfig.AI.SystemPrompt = ""
	defaultConfig.AI.Thinking = -1
	defaultConfig.AI.Thoughts = false
	defaultConfig.AI.EnableTools = true
	defaultConfig.AI.CacheDir = "cache"
	defaultConfig.AI.CacheSystemPrompt = "You are an expert in software development. The following files are provided as context for our conversation."
	defaultConfig.AI.EnableCache = false
	defaultConfig.VAD.SilenceThreshold = 0.02
	defaultConfig.VAD.HangoverDurationSec = 2.0
	defaultConfig.Recorder.MinFileSizeBytes = 600000
	defaultConfig.Display.BarWidth = 100
	defaultConfig.Display.UpdateIntervalMs = 50
	defaultConfig.Pipeline.BufferTimeUs = 500000

	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Failed to create default config file: %v", err)
	}
	defer f.Close()

	if err := toml.NewEncoder(f).Encode(defaultConfig); err != nil {
		log.Fatalf("Failed to write to default config file: %v", err)
	}
}

// HangoverDuration converts the configured seconds into a time.Duration.
func (v VADConfig) HangoverDuration() time.Duration {
	return time.Duration(v.HangoverDurationSec * float64(time.Second))
}

// UpdateInterval converts the configured milliseconds into a time.Duration.
func (d DisplayConfig) UpdateInterval() time.Duration {
	return time.Duration(d.UpdateIntervalMs) * time.Millisecond
}

// WarmupDuration parses the VAD.WarmupDuration string into a time.Duration.
func (v *VADConfig) WarmUpDuration() time.Duration {
	if v.WarmupDuration == "" {
		return 0 // No warm-up if not specified.
	}
	d, err := time.ParseDuration(v.WarmupDuration)
	if err != nil {
		log.Printf("Warning: could not parse VAD.WarmupDuration '%s', using default 1s. Error: %v", v.WarmupDuration, err)
		return time.Second
	}
	return d
}
