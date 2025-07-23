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
	Trace    bool
	LogFile  string
	AI       AIConfig
	VAD      VADConfig
	Recorder RecorderConfig
	Display  DisplayConfig
	Pipeline PipelineConfig
}

// AIConfig holds settings related to the AI model.
type AIConfig struct {
	Model                    string
	ModelLive                string
	ModelLiveTTS             string
	TranscriptionPrompt      string
	ModelTTS                 string
	Voice                    string
	APIKey                   string
	VoicePrompt              string
	SystemPrompt             string
	Thinking                 int32
	Thoughts                 bool
	EnableTools              bool
	CacheDir                 string
	CacheSystemPrompt        string
	EnableCache              bool
	VoiceHistory             bool
	VoiceEnabled             bool
	WorkspaceDir             string
	Transcript               bool
	Retry                    RetryConfig
	ContextWindowCompression ContextWindowCompressionConfig
	SessionResumption        SessionResumptionConfig
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

// RetryConfig holds settings for API call retries.
type RetryConfig struct {
	MaxRetries     int
	InitialDelayMs int
	MaxDelayMs     int
}

// ContextWindowCompressionConfig holds settings for live session context window compression.
type ContextWindowCompressionConfig struct {
	Enabled       bool
	TriggerTokens int64
	TargetTokens  int64
}

// SessionResumptionConfig holds settings for live session resumption.
type SessionResumptionConfig struct {
	Enabled bool
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
	defaultConfig.Trace = false
	defaultConfig.LogFile = "app.log"
	defaultConfig.AI.Model = "gemini-2.5-flash"
	defaultConfig.AI.ModelTTS = "gemini-2.5-flash-preview-tts"
	defaultConfig.AI.ModelLive = "gemini-live-2.5-flash-preview"
	defaultConfig.AI.ModelLiveTTS = "gemini-2.5-flash-preview-native-audio-dialog"
	defaultConfig.AI.Voice = "Kore"
	defaultConfig.AI.TranscriptionPrompt = "Please provide a verbatim transcript of the audio."
	defaultConfig.AI.APIKey = "${GOOGLE_API_KEY}"
	defaultConfig.AI.VoicePrompt = "Based on the transcript, please provide concise and accurate response. Respond in the same language as the transcript."
	defaultConfig.AI.SystemPrompt = "You are a helpful assistant. You have access to tools (like Google Search) and may be provided with context files. Your instructions are: 1. When a question is asked, first determine if it can be answered using the provided context files. 2. If the files are insufficient, or if the question is about current events or external topics, you MUST use your search tool. 3. Synthesize a comprehensive answer from all available information."
	defaultConfig.AI.Thinking = -1
	defaultConfig.AI.Thoughts = false
	defaultConfig.AI.EnableTools = true
	defaultConfig.AI.CacheDir = "cache"
	defaultConfig.AI.CacheSystemPrompt = "The following files are provided as context:"
	defaultConfig.AI.EnableCache = false
	defaultConfig.AI.VoiceHistory = true
	defaultConfig.AI.VoiceEnabled = false
	defaultConfig.AI.WorkspaceDir = "~/gemini_workspace" // The directory for file system tools in live mode. Supports tilde expansion.
	defaultConfig.AI.Transcript = false
	defaultConfig.AI.ContextWindowCompression.Enabled = true
	defaultConfig.AI.ContextWindowCompression.TriggerTokens = 12000
	defaultConfig.AI.ContextWindowCompression.TargetTokens = 8000
	defaultConfig.AI.SessionResumption.Enabled = true
	defaultConfig.AI.Retry.MaxRetries = 3
	defaultConfig.AI.Retry.InitialDelayMs = 1000
	defaultConfig.AI.Retry.MaxDelayMs = 10000
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

func IsDebug() bool {
	return C.Debug || C.Trace
}

func DebugPrintf(format string, args ...interface{}) {
	if IsDebug() {
		log.Printf(format, args...)
	}
}

func DebugPrintln(args ...interface{}) {
	if IsDebug() {
		log.Println(args...)
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
