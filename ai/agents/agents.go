package agents

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
	"gemini/inout"
)

// Agent names used as keys in the agent map.
const (
	AgentObjectDetectionName = "objectDetection"
	AgentFileName            = "FileAgent"
	AgentPdfReaderName       = "pdfReaderAgent"
	AgentRtfReaderName       = "rtfReaderAgent"
	AgentYoutubeName         = "youtubeAgent"
	AgentWebScraperName      = "webScraperAgent"
	AgentDesktopName         = "desktopAgent"
	AgentGmailName           = "gmailAgent"
)

type Callable interface {
	ModelName() string
	RPM() int
	WarmUp() time.Duration
	Process(prompt string, parts ...*genai.Part) (string, error)
	Handle(call *genai.FunctionCall) *genai.FunctionResponse
}

// AgentFactory is a function type for creating new agents.
type AgentFactory func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable

// Agent is a specialized, self-contained AI processor for specific tasks.
// It operates without the main application's event bus or pipeline, making it
// suitable for on-demand, synchronous processing like in tool calls.
type Agent struct {
	name              string
	ctx               context.Context
	client            *genai.Client
	modelName         string
	rpm               int
	systemInstruction *genai.Content
	responseSchema    *genai.Schema
	temperature       *float32
	tools             []*genai.Tool
}

// AgentConfig defines the configuration for an Agent.
type AgentConfig struct {
	Name                string
	Model               string
	RPM                 int
	SystemInstruction   string
	Temperature         *float32
	EnableGoogleSearch  bool
	EnableURLContext    bool
	EnableCodeExecution bool
	ResponseSchema      *genai.Schema
}

// AgentRegistry holds all created agent instances, keyed by their name.
var AgentRegistry = make(map[string]Callable)

// agentFactories holds the constructor functions for all available agents.
var agentFactories = make(map[string]AgentFactory)

var (
	// modelRequestTimestamps tracks the timestamps of recent warm-up calls for rate limiting.
	modelRequestTimestamps = make(map[string][]time.Time)
	rateLimitMu            sync.Mutex
)

var (
	warmedUpModels = make(map[string]bool)
	warmUpMu       sync.Mutex
)

// NewToolSet creates a new, empty toolset.
// Agents will register their function declarations with this toolset during their initialization.
func NewToolSet() *genai.Tool {
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{},
	}
}

// RegisterFactory is called by agents in their init() function to register themselves.
func RegisterFactory(name string, factory AgentFactory) {
	if _, exists := agentFactories[name]; exists {
		log.Fatalf("agent factory for '%s' is already registered", name)
	}
	log.Printf("Registering agent factory: %s", name)
	agentFactories[name] = factory
}

// Registerate acts as a factory and registry for agents. It centralizes the
// creation, registration, and post-initialization (e.g., WarmUp) of all agents.
func Registerate(ctx context.Context, client *genai.Client, toolset *genai.Tool, agentName string) {
	factory, ok := agentFactories[agentName]
	if !ok {
		log.Printf("WARNING: Attempted to register an unknown agent: '%s'", agentName)
		return
	}

	agent := factory(ctx, client, toolset)
	if agent == nil {
		// The factory can return nil if the agent is disabled (e.g., GmailAgent)
		return
	}

	// --- Rate Limiting Logic ---
	// This logic is executed before the warm-up call to ensure we respect API limits.
	rpm := agent.RPM()

	if rpm > 0 {
		rateLimitMu.Lock()
		modelName := agent.ModelName()
		now := time.Now()
		// Clean up timestamps older than one minute.
		timestamps := modelRequestTimestamps[modelName]
		validTimestamps := []time.Time{}
		for _, ts := range timestamps {
			if now.Sub(ts) < time.Minute {
				validTimestamps = append(validTimestamps, ts)
			}
		}
		modelRequestTimestamps[modelName] = validTimestamps

		// If we've hit the limit, wait until the oldest request is more than a minute old.
		if len(validTimestamps) >= rpm {
			oldestTimestamp := validTimestamps[0]
			timeToWait := time.Minute - now.Sub(oldestTimestamp) + (1 * time.Second) // Add a 1s buffer.
			log.Printf("RPM limit of %d for model '%s' reached. Waiting for %v...", rpm, modelName, timeToWait)
			rateLimitMu.Unlock() // Unlock while sleeping to not block other agents.
			time.Sleep(timeToWait)
			rateLimitMu.Lock() // Re-lock before proceeding.
		}
		rateLimitMu.Unlock()
	}

	// --- Warm-up and Registration ---
	duration := agent.WarmUp()
	if duration > 0 {
		// If the warm-up actually made an API call, record its timestamp for rate limiting.
		rateLimitMu.Lock()
		modelRequestTimestamps[agent.ModelName()] = append(modelRequestTimestamps[agent.ModelName()], time.Now())
		rateLimitMu.Unlock()
	}
	AgentRegistry[agentName] = agent
}

// NewAgent creates a new AI agent with a specific configuration.
func NewAgent(ctx context.Context, client *genai.Client, agentConfig AgentConfig) *Agent {
	agent := Agent{
		name:           agentConfig.Name,
		ctx:            ctx,
		client:         client,
		modelName:      agentConfig.Model,
		rpm:            agentConfig.RPM,
		responseSchema: agentConfig.ResponseSchema,
		temperature:    agentConfig.Temperature,
	}

	agent.Printf("Creating new agent with model: %s", agent.modelName)

	if agentConfig.SystemInstruction != "" {
		agent.systemInstruction = genai.NewContentFromParts([]*genai.Part{genai.NewPartFromText(agentConfig.SystemInstruction)}, "")
	}

	// Determine if any tools are enabled by checking the specific configuration flags.
	toolsEnabled := agentConfig.EnableGoogleSearch || agentConfig.EnableURLContext || agentConfig.EnableCodeExecution
	if toolsEnabled {
		agent.Println("Tool use is enabled.")
		var tools []*genai.Tool

		// Standard tools can be combined into a single tool definition.
		if agentConfig.EnableGoogleSearch || agentConfig.EnableURLContext {
			standardTool := &genai.Tool{}
			if agentConfig.EnableGoogleSearch {
				agent.Println("GoogleSearch tool enabled")
				standardTool.GoogleSearch = &genai.GoogleSearch{}
			}
			if agentConfig.EnableURLContext {
				agent.Println("URLContext tool enabled")
				standardTool.URLContext = &genai.URLContext{}
			}
			tools = append(tools, standardTool)
		}

		if agentConfig.EnableCodeExecution {
			tools = append(tools, &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}})
			agent.Println("Code execution tool enabled")
		}

		if len(tools) > 0 {
			agent.tools = tools
		}
	}

	return &agent
}

// ModelName returns the name of the model this agent is configured to use.
func (a *Agent) ModelName() string {
	return a.modelName
}

// RPM returns the configured requests per minute for the agent's model.
func (a *Agent) RPM() int {
	return a.rpm
}

// Process sends a prompt (with optional other parts like images or URIs) to the agent's model and returns the text response.
// It's a synchronous, one-shot call.
func (a *Agent) Process(prompt string, otherParts ...*genai.Part) (string, error) {
	a.Printf("Processing prompt: '%s'", prompt)
	if len(otherParts) > 0 {
		var partDescriptions []string
		for _, p := range otherParts {
			if p.InlineData != nil {
				partDescriptions = append(partDescriptions, fmt.Sprintf("InlineData(MIME: %s, Size: %d)", p.InlineData.MIMEType, len(p.InlineData.Data)))
			} else if p.FileData != nil {
				partDescriptions = append(partDescriptions, fmt.Sprintf("FileURI(%s)", p.FileData.FileURI))
			}
		}
		a.Printf("Processing with %d additional parts: %s", len(otherParts), strings.Join(partDescriptions, ", "))
	}

	startTime := time.Now()

	parts := []*genai.Part{genai.NewPartFromText(prompt)}
	parts = append(parts, otherParts...)

	userContent := genai.NewContentFromParts(parts, genai.RoleUser)
	conversation := []*genai.Content{userContent}

	genConfig := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: helpers.Ptr(int32(0))},
	}

	if len(a.tools) == 0 && a.responseSchema != nil {
		// Only set the response MIME type if we are NOT using tools, as they are incompatible.
		// The presence of a ResponseSchema is sufficient to get JSON output when using tools.
		genConfig.ResponseMIMEType = "application/json"
	}

	if a.temperature != nil {
		genConfig.Temperature = a.temperature
	} else {
		genConfig.Temperature = helpers.Ptr(float32(0.5)) // Default temperature if not specified
	}
	if a.systemInstruction != nil {
		genConfig.SystemInstruction = a.systemInstruction
	}
	if len(a.tools) > 0 {
		genConfig.Tools = a.tools
	}
	if a.responseSchema != nil {
		genConfig.ResponseSchema = a.responseSchema
	}

	resp, err := a.client.Models.GenerateContent(a.ctx, a.modelName, conversation, genConfig)
	if err != nil {
		a.Printf("ERROR: Content generation failed: %v", err)
		return "", fmt.Errorf("[%s] content generation failed: %w", a.name, err)
	}

	duration := time.Since(startTime)
	a.Printf("Processing successful in %v. Response length: %d", duration, len(resp.Text()))
	return resp.Text(), nil
}

// WarmUp sends a simple, low-cost prompt to the agent's model to reduce
// the "cold start" latency on the first real request. It runs in a goroutine
// to avoid blocking the application's startup sequence. It returns the duration
// of the API call, or 0 if no call was made.
func (a *Agent) WarmUp() time.Duration {
	if !(config.C.AI.EnableFunctionCalling && config.C.AI.AgentWarmUp) {
		return 0
	}

	warmUpMu.Lock()
	if warmedUpModels[a.modelName] {
		a.Printf("Model '%s' already warmed up, skipping for agent '%s'.", a.modelName, a.name)
		warmUpMu.Unlock()
		return 0
	}
	warmedUpModels[a.modelName] = true
	warmUpMu.Unlock()

	a.Printf("Warming up model '%s'...", a.modelName)
	startTime := time.Now()
	_, err := a.Process("ping")
	duration := time.Since(startTime)

	if err != nil {
		a.Printf("WARNING: Warm-up call failed: %v", err)
		return 0 // Return 0 on failure so it doesn't count against rate limits.
	}
	return duration
}

// Handle is the base implementation for the Callable interface. It returns nil,
// indicating that the base agent does not handle any specific tool calls by itself.
func (a *Agent) Handle(_ *genai.FunctionCall) *genai.FunctionResponse {
	return nil
}

// CreateFunctionResponse is a method to standardize the creation of FunctionResponse objects for an agent.
func (a *Agent) CreateFunctionResponse(call *genai.FunctionCall, result any, err error) *genai.FunctionResponse {
	if err != nil {
		a.Printf("ERROR executing tool call '%s': %v", call.Name, err)
		result = map[string]any{"error": err.Error()}
	}

	inout.LogToolResult(call.Name, result)

	responseMap, ok := result.(map[string]any)
	if !ok {
		a.Printf("NOTICE: tool call result for '%s' is not a map[string]any, wrapping it. Type: %T", call.Name, result)
		responseMap = map[string]any{"output": result}
	}

	return &genai.FunctionResponse{
		ID:         call.ID,
		Name:       call.Name,
		Response:   responseMap,
		Scheduling: genai.FunctionResponseSchedulingWhenIdle,
	}
}

func (a *Agent) Printf(format string, args ...any) {
	log.Printf(fmt.Sprintf("[%s] %s", a.name, format), args...)
}

func (a *Agent) Println(args ...any) {
	// Prepend agent name to the arguments for log.Println
	allArgs := append([]any{fmt.Sprintf("[%s]", a.name)}, args...)
	log.Println(allArgs...)
}
