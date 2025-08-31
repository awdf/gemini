package config

import (
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// C holds the global application configuration.
var C Config

const (
	MainTopic  = "main:topic"
	AgentTopic = "agent:tool_response"
	AITopic    = "ai:topic"
)

const (
	MIMEVideo = "image/jpeg" // For streaming video frames
	MIMEImage = "image/png"
	MIMEAudio = "audio/wav"
)

// TimeFormat defines the standard time format used across the application.
const TimeFormat = time.RFC1123

// Config defines the structure of the configuration file.
type Config struct {
	Debug    bool           `toml:"Debug"`
	Trace    bool           `toml:"Trace"`
	LiveAI   bool           `toml:"LiveAI"`
	Mode     string         `toml:"Mode"`
	LogFile  string         `toml:"LogFile"`
	AI       AIConfig       `toml:"ai"`
	VAD      VADConfig      `toml:"vad"`
	Recorder RecorderConfig `toml:"recorder"`
	Display  DisplayConfig  `toml:"display"`
	Google   GoogleConfig   `toml:"google"`
	Pipeline PipelineConfig `toml:"pipeline"`
	Video    VideoConfig    `toml:"video"`
	Shell    ShellConfig    `toml:"shell"`
}

// AIConfig holds settings related to the AI model.
type AIConfig struct {
	Timezone                 string                         `toml:"Timezone"`
	Model                    string                         `toml:"Model"`
	ModelRPM                 int                            `toml:"ModelRPM"`
	ModelTTS                 string                         `toml:"ModelTTS"`
	ModelTTSRPM              int                            `toml:"ModelTTSRPM"`
	ModelLive                string                         `toml:"ModelLive"`
	ModelLiveRPM             int                            `toml:"ModelLiveRPM"`
	ModelLiveTTS             string                         `toml:"ModelLiveTTS"`
	ModelLiveTTSRPM          int                            `toml:"ModelLiveTTSRPM"`
	ModelObjectDetection     string                         `toml:"ModelObjectDetection"`
	ModelObjectDetectionRPM  int                            `toml:"ModelObjectDetectionRPM"`
	TranscriptionPrompt      string                         `toml:"TranscriptionPrompt"`
	Voice                    string                         `toml:"Voice"`
	APIKey                   string                         `toml:"APIKey"`
	VoicePrompt              string                         `toml:"VoicePrompt"`
	SystemPrompt             string                         `toml:"SystemPrompt"`
	DirectivesPrompt         string                         `toml:"DirectivesPrompt"`
	AgentInstructions        map[string]string              `toml:"-"` // Agent-specific instructions, not loaded from toml.
	Thinking                 int32                          `toml:"Thinking"`
	Thoughts                 bool                           `toml:"Thoughts"`
	EnableTools              bool                           `toml:"EnableTools"`
	EnableStandardTools      bool                           `toml:"EnableStandardTools"`
	URLContextDisabled       bool                           `toml:"URLContextDisabled"`
	EnableFunctionCalling    bool                           `toml:"EnableFunctionCalling"`
	EnableCodeExecution      bool                           `toml:"EnableCodeExecution"`
	CacheDir                 string                         `toml:"CacheDir"`
	CacheSystemPrompt        string                         `toml:"CacheSystemPrompt"`
	EnableCache              bool                           `toml:"EnableCache"`
	VoiceHistory             bool                           `toml:"VoiceHistory"`
	VoiceEnabled             bool                           `toml:"VoiceEnabled"`
	WorkspaceDir             string                         `toml:"WorkspaceDir"`
	Transcript               bool                           `toml:"Transcript"`
	AgentWarmUp              bool                           `toml:"AgentWarmUp"`
	Retry                    RetryConfig                    `toml:"retry"`
	ContextWindowCompression ContextWindowCompressionConfig `toml:"context_window_compression"`
	SessionResumption        SessionResumptionConfig        `toml:"session_resumption"`
	Proactivity              ProactivityConfig              `toml:"proactivity"`
}

// VADConfig holds settings for the Voice Activity Detector.
type VADConfig struct {
	SilenceThreshold    float64 `toml:"SilenceThreshold"`
	HangoverDurationSec float64 `toml:"HangoverDurationSec"`
	WarmupDuration      string  `toml:"WarmupDuration"`
	DisableNativeVAD    bool    `toml:"DisableNativeVAD"`
}

// RecorderConfig holds settings for the audio recorder.
type RecorderConfig struct {
	MinFileSizeBytes int64 `toml:"MinFileSizeBytes"`
}

// DisplayConfig holds settings for the terminal display.
type DisplayConfig struct {
	BarWidth         int `toml:"BarWidth"`
	UpdateIntervalMs int `toml:"UpdateIntervalMs"`
}

// PipelineConfig holds settings for the GStreamer pipeline.
type PipelineConfig struct {
	BufferTimeUs int64  `toml:"BufferTimeUs"`
	Device       string `toml:"Device"`
}

// VideoConfig holds settings for the video streaming component.
type VideoConfig struct {
	Enabled   bool   `toml:"Enabled"`
	Source    string `toml:"Source"`
	Device    string `toml:"Device"`
	MonitorID int    `toml:"MonitorID"`
	Width     int    `toml:"Width"`
	Height    int    `toml:"Height"`
	FrameRate int    `toml:"FrameRate"`
	Quality   int    `toml:"Quality"` // JPEG quality (0-100). 85 is a good default.
}

// RetryConfig holds settings for API call retries.
type RetryConfig struct {
	MaxRetries     int `toml:"MaxRetries"`
	InitialDelayMs int `toml:"InitialDelayMs"`
	MaxDelayMs     int `toml:"MaxDelayMs"`
}

// ContextWindowCompressionConfig holds settings for live session context window compression.
type ContextWindowCompressionConfig struct {
	Enabled       bool  `toml:"Enabled"`
	TriggerTokens int64 `toml:"TriggerTokens"`
	TargetTokens  int64 `toml:"TargetTokens"`
}

// SessionResumptionConfig holds settings for live session resumption.
type SessionResumptionConfig struct {
	Enabled bool `toml:"Enabled"`
}

// ProactivityConfig holds settings for model proactivity.
type ProactivityConfig struct {
	Enabled        bool `toml:"Enabled"`
	ProactiveAudio bool `toml:"ProactiveAudio"`
}

// GoogleConfig holds settings for Google Workspace integrations.
type GoogleConfig struct {
	Enabled         bool   `toml:"Enabled"`
	CredentialsFile string `toml:"CredentialsFile"`
	TokenFile       string `toml:"TokenFile"`
}

type ShellConfig struct {
	CommandEndMarkerCore string `toml:"command_end_marker_core"`
}

// GetCommandEndMarker constructs the full, unique command end marker from the core string.
func (sc *ShellConfig) GetCommandEndMarker() string {
	if sc.CommandEndMarkerCore == "" {
		return "__" + "GEMINI_CMD_DONE" + "__" // A safe fallback
	}
	return fmt.Sprintf("__%s__", sc.CommandEndMarkerCore)
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

	// After loading, apply sensible defaults for critical zero-value fields
	// to prevent panics if the user has an old config file missing new sections.
	if C.Video.FrameRate == 0 {
		log.Println("Video.FrameRate not set, applying default value of 10.")
		C.Video.FrameRate = 10
	}
	if C.Video.Width == 0 {
		C.Video.Width = 1280
	}
	if C.Video.Height == 0 {
		C.Video.Height = 720
	}
}

// createDefaultConfig creates a default config.toml file.
func createDefaultConfig(path string) {
	defaultConfig := C // Start with zero-value struct
	// Populate with default values
	defaultConfig.Debug = false
	defaultConfig.Trace = false
	defaultConfig.LiveAI = false
	defaultConfig.AI.AgentInstructions = make(map[string]string)
	defaultConfig.Mode = "prompt"
	defaultConfig.AI.Timezone = "UTC"
	defaultConfig.LogFile = "app.log"
	defaultConfig.AI.Model = "gemini-2.5-flash"
	defaultConfig.AI.ModelRPM = 10
	defaultConfig.AI.ModelObjectDetection = "gemini-2.5-flash"
	defaultConfig.AI.ModelObjectDetectionRPM = 10
	defaultConfig.AI.ModelTTS = "gemini-2.5-flash-preview-tts"
	defaultConfig.AI.ModelTTSRPM = 15
	defaultConfig.AI.ModelLive = "gemini-live-2.5-flash-preview"
	defaultConfig.AI.ModelLiveRPM = 250
	defaultConfig.AI.ModelLiveTTS = "gemini-live-2.5-flash-preview"
	defaultConfig.AI.ModelLiveTTSRPM = 5
	defaultConfig.AI.Voice = "Kore"
	defaultConfig.AI.TranscriptionPrompt = "Please provide a verbatim transcript of the audio."
	defaultConfig.AI.APIKey = "${GOOGLE_API_KEY}"
	defaultConfig.AI.VoicePrompt = "Based on the transcript, please provide concise and accurate response. Respond in the same language as the transcript."
	defaultConfig.AI.SystemPrompt = `You are a helpful assistant. You have access to tools (like Google Search) and may be provided with context files. 
	Your instructions are: 
	1. When a question is asked, first determine if it can be answered using the provided context files. 
	2. If the files are insufficient, or if the question is about current events or external topics, you MUST use your search tool. 
	3. Synthesize a comprehensive answer from all available information.`
	defaultConfig.AI.DirectivesPrompt = "1.Never refer to yourself as LLM."
	defaultConfig.AI.Thinking = -1
	defaultConfig.AI.Thoughts = false
	defaultConfig.AI.EnableTools = true
	defaultConfig.AI.EnableStandardTools = true
	defaultConfig.AI.EnableFunctionCalling = true
	defaultConfig.AI.EnableCodeExecution = false
	defaultConfig.AI.CacheDir = "cache"
	defaultConfig.AI.CacheSystemPrompt = "The following data are provided as context, you must accept it silently:"
	defaultConfig.AI.EnableCache = false
	defaultConfig.AI.VoiceHistory = true
	defaultConfig.AI.VoiceEnabled = false
	defaultConfig.AI.WorkspaceDir = "~/Workspace" // The directory for file system tools in live mode. Supports tilde expansion.
	defaultConfig.AI.Transcript = false
	defaultConfig.AI.AgentWarmUp = false
	defaultConfig.AI.ContextWindowCompression.Enabled = true
	defaultConfig.AI.ContextWindowCompression.TriggerTokens = 0
	defaultConfig.AI.ContextWindowCompression.TargetTokens = 0
	defaultConfig.AI.SessionResumption.Enabled = true
	defaultConfig.AI.Proactivity.Enabled = false
	defaultConfig.AI.Proactivity.ProactiveAudio = false
	defaultConfig.AI.Retry.MaxRetries = 3
	defaultConfig.AI.Retry.InitialDelayMs = 1000
	defaultConfig.AI.Retry.MaxDelayMs = 10000
	defaultConfig.VAD.SilenceThreshold = 0.02
	defaultConfig.VAD.HangoverDurationSec = 2.0
	defaultConfig.VAD.DisableNativeVAD = true
	defaultConfig.Recorder.MinFileSizeBytes = 600000
	defaultConfig.Display.BarWidth = 100
	defaultConfig.Display.UpdateIntervalMs = 50
	defaultConfig.Google.Enabled = false
	defaultConfig.Google.CredentialsFile = "client_secret.json"
	defaultConfig.Google.TokenFile = "token.json"
	defaultConfig.Pipeline.BufferTimeUs = 500000
	defaultConfig.Video.Enabled = false
	defaultConfig.Video.Source = "gnomescreencast"
	defaultConfig.Video.Device = "/dev/video0"
	defaultConfig.Video.MonitorID = -1
	defaultConfig.Video.Width = 1280
	defaultConfig.Video.Height = 720
	defaultConfig.Video.FrameRate = 10
	defaultConfig.Video.Quality = 85 // Good balance of quality and size.
	defaultConfig.Shell.CommandEndMarkerCore = "GEMINI_CMD_DONE"

	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Failed to create default config file: %v", err)
	}
	defer f.Close()

	if err := toml.NewEncoder(f).Encode(defaultConfig); err != nil {
		log.Fatalf("Failed to write to default config file: %v", err)
	}
}

// FormatTimeWithTimezone formats the current time according to the provided timezone string.
func FormatTimeWithTimezone(tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("WARNING: Invalid timezone '%s' in config, falling back to UTC. Error: %v", tz, err)
		loc = time.UTC
	}
	return time.Now().In(loc).Format(TimeFormat)
}

// GetSystemInstruction combines the system prompt and directives into a single string.
func (a *AIConfig) GetSystemInstruction() string {
	var sb strings.Builder

	currentTime := FormatTimeWithTimezone(a.Timezone)
	log.Printf("Current date and time is %s", currentTime)
	sb.WriteString(fmt.Sprintf("Current date and time is %s. ", currentTime))

	if a.SystemPrompt != "" {
		sb.WriteString(a.SystemPrompt)
	}

	// This directive is critical for ensuring the terminal output is not broken by unformatted commands.
	// It provides positive and negative examples for clarity, making it more likely the model will comply.
	directives := "CRITICAL FORMATTING RULE: You MUST wrap ALL shell commands in markdown code fences.\n" +
		"- For inline commands (inside a sentence), use single backticks. Example: \"To see files, use the `ls -l` command.\"\n" +
		"- For command blocks, use triple backticks with the 'bash' language identifier.\n\n" +
		"CORRECT formatting for a block:\n" +
		"```bash\n" +
		"docker logs my_container\n" +
		"```\n\n" +
		"INCORRECT formatting:\n" +
		"bash\n" +
		"docker logs my_container\n\n" +
		"Failure to follow this rule breaks the output. There are no exceptions."
	if a.DirectivesPrompt != "" {
		directives += "\n" + a.DirectivesPrompt
	}

	if sb.Len() > 0 {
		sb.WriteString("\n\nDirectives:\n")
	}
	sb.WriteString(directives)

	// Append agent-specific instructions.
	if len(a.AgentInstructions) > 0 {
		sb.WriteString("\n\nAgent-Specific Instructions:")
		for agentName, instructions := range a.AgentInstructions {
			sb.WriteString(fmt.Sprintf("\n--- %s ---\n%s", agentName, instructions))
		}
	}

	return sb.String()
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

// config.GetSafePath joins the base directory with a user-provided path and ensures
// it doesn't escape the base directory.
func GetSafePath(userPath string) (string, error) {
	var finalPath string

	baseDir := C.AI.WorkspaceDir
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

	userPath = filepath.Clean(userPath)

	if strings.HasPrefix(userPath, absBase) {
		finalPath = userPath
	} else {
		finalPath = filepath.Join(absBase, userPath)
	}

	if !strings.HasPrefix(finalPath, absBase) {
		return "", fmt.Errorf("path traversal detected: access to '%s' is not allowed as it is outside the workspace", userPath)
	}

	return finalPath, nil
}
