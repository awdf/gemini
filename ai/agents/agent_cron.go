package agents

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"google.golang.org/genai"

	"gemini/config"
)

func init() {
	RegisterFactory(AgentCronName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewCronAgent(ctx, client, toolset, bus)
	})
}

// ScheduledEvent holds information about a scheduled event.
type ScheduledEvent struct {
	ID       string       `json:"id"`
	Pattern  string       `json:"pattern"`
	Prompt   string       `json:"prompt"`
	EntryID  cron.EntryID `json:"-"`
	CallID   string       `json:"-"` // ID of the original tool call
	CallName string       `json:"-"` // Name of the original tool call
}

// CronAgent handles scheduling and triggering time-based events.
type CronAgent struct {
	*Agent
	bus           *EventBus.Bus
	cronScheduler *cron.Cron
	events        map[string]*ScheduledEvent
	mu            sync.Mutex
}

// NewCronAgent creates a new CronAgent.
func NewCronAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *CronAgent {
	agentConfig := AgentConfig{
		Name: AgentCronName,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	loc, err := time.LoadLocation(config.C.AI.Timezone)
	if err != nil {
		log.Printf("WARNING: [cronAgent] Invalid timezone '%s' in config, falling back to UTC. Error: %v", config.C.AI.Timezone, err)
		loc = time.UTC
	}

	agent := &CronAgent{
		Agent:         baseAgent,
		bus:           bus,
		cronScheduler: cron.New(cron.WithLocation(loc), cron.WithSeconds()),
		events:        make(map[string]*ScheduledEvent),
	}

	agent.cronScheduler.Start()
	agent.Printf("Initialized with cron scheduler in timezone: %s.", loc.String())

	functions := []*genai.FunctionDeclaration{
		{
			Name:        "scheduleDelayedAction",
			Description: "CRON: Schedules an action to be performed once after a specified delay.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"delay": {
						Type:        genai.TypeString,
						Description: "For a one-time action, the duration to wait before performing the action (e.g., '5m', '1h30s').",
					},
					"action_prompt": {
						Type:        genai.TypeString,
						Description: "The prompt or command that the model should execute after the delay.",
					},
				},
				Required: []string{"delay", "action_prompt"},
			},
			Behavior: genai.BehaviorNonBlocking,
		},
		{
			Name:        "scheduleRecuringAction",
			Description: "CRON: Schedules an action to be performed on a recurring basis.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"pattern": {
						Type:        genai.TypeString,
						Description: "The cron pattern for the schedule (e.g., '0 9 * * MON', '@every 5m').",
					},
					"action_prompt": {
						Type:        genai.TypeString,
						Description: "The prompt or command that the model should execute on schedule.",
					},
				},
				Required: []string{"pattern", "action_prompt"},
			},
			Behavior: genai.BehaviorNonBlocking,
		},
		{
			Name:        "listScheduledActions",
			Description: "CRON: Lists all currently scheduled actions.",
			Parameters:  &genai.Schema{Type: genai.TypeObject},
		},
		{
			Name:        "deleteScheduledAction",
			Description: "CRON: Deletes a scheduled action by its ID.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"id": {Type: genai.TypeString, Description: "The ID of the scheduled action to delete, obtained from 'listScheduledActions'."},
				},
				Required: []string{"id"},
			},
		},
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, functions...)

	return agent
}

// WarmUp for CronAgent does nothing.
func (a *CronAgent) WarmUp() time.Duration {
	return 0
}

// Handle processes tool calls for the cron agent.
func (a *CronAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "scheduleDelayedAction":
		return a.handleScheduleAction(call)
	case "scheduleRecuringAction":
		return a.handleScheduleAction(call)
	case "listScheduledActions":
		return a.handleScheduledActionsList(call)
	case "deleteScheduledAction":
		return a.handleDeleteScheduledAction(call)
	default:
		return nil
	}
}

func (a *CronAgent) parseAndValidateScheduleArgs(args map[string]any) (cronSpec, actionPrompt string, isRecurring bool, err error) {
	var delayOK, patternOK, promptOK bool
	delayStr, delayOK := args["delay"].(string)
	pattern, patternOK := args["pattern"].(string)
	actionPrompt, promptOK = args["action_prompt"].(string)

	if !promptOK || actionPrompt == "" {
		err = fmt.Errorf("'action_prompt' is a required non-empty string")
		return
	}
	if (!delayOK || delayStr == "") && (!patternOK || pattern == "") {
		err = fmt.Errorf("one of 'delay' or 'pattern' must be provided")
		return
	}
	if (delayOK && delayStr != "") && (patternOK && pattern != "") {
		err = fmt.Errorf("only one of 'delay' or 'pattern' can be provided")
		return
	}

	if delayOK && delayStr != "" {
		isRecurring = false
		duration, parseErr := time.ParseDuration(delayStr)
		if parseErr != nil {
			err = fmt.Errorf("invalid delay format '%s': %w", delayStr, parseErr)
			return
		}
		t := time.Now().Add(duration)
		cronSpec = fmt.Sprintf("%d %d %d %d %d *", t.Second(), t.Minute(), t.Hour(), t.Day(), t.Month())
	}

	if patternOK && pattern != "" {
		isRecurring = true
		cronSpec = pattern
	}

	return
}

func (a *CronAgent) handleScheduleAction(call *genai.FunctionCall) *genai.FunctionResponse {
	// 1. Parse and validate arguments
	cronSpec, actionPrompt, isRecurring, err := a.parseAndValidateScheduleArgs(call.Args)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	// Both delayed and recurring actions require the event bus for async responses.
	if a.bus == nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("scheduled actions are not supported in this mode"))
	}

	if cronSpec != "" {
		return a.scheduleCronExpression(call, cronSpec, actionPrompt, isRecurring)
	}

	// Should not be reached due to validation above, but as a fallback.
	return a.CreateFunctionResponse(call, nil, fmt.Errorf("internal error: no valid scheduling parameter found"))
}

// Field name   | Mandatory? | Allowed values  | Allowed special characters
// ----------   | ---------- | --------------  | --------------------------
// Seconds      | Yes        | 0-59            | * / , -
// Minutes      | Yes        | 0-59            | * / , -
// Hours        | Yes        | 0-23            | * / , -
// Day of month | Yes        | 1-31            | * / , - ?
// Month        | Yes        | 1-12 or JAN-DEC | * / , -
// Day of week  | Yes        | 0-6 or SUN-SAT  | * / , - ?
func (a *CronAgent) scheduleCronExpression(call *genai.FunctionCall, cronSpec, actionPrompt string, isRecurring bool) *genai.FunctionResponse {
	// Handle scheduled action
	a.mu.Lock()
	defer a.mu.Unlock()

	eventID := uuid.New().String()
	event := &ScheduledEvent{
		ID:       eventID,
		Pattern:  cronSpec,
		Prompt:   actionPrompt,
		CallID:   call.ID,
		CallName: call.Name,
	}

	entryID, err := a.cronScheduler.AddFunc(cronSpec, func() {
		a.Printf("Scheduled action triggered for event ID %s. Sending FunctionResponse to model.", event.ID)
		response := a.CreateFunctionResponse(call, map[string]any{"status": event.Prompt}, nil, isRecurring)
		(*a.bus).Publish("agent:tool_response", response)
	})
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid cron pattern '%s': %w", cronSpec, err))
	}

	event.EntryID = entryID
	a.events[eventID] = event

	a.Printf("Scheduled new action '%s' with ID %s", actionPrompt, eventID)
	return a.CreateFunctionResponse(call, map[string]any{"status": "New action scheduled successfully.", "event_id": eventID}, nil, true)
}

func (a *CronAgent) handleScheduledActionsList(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	eventList := make([]ScheduledEvent, 0, len(a.events))
	for _, event := range a.events {
		eventList = append(eventList, *event)
	}

	return a.CreateFunctionResponse(call, map[string]any{"scheduled_actions": eventList}, nil)
}

func (a *CronAgent) handleDeleteScheduledAction(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	eventID, ok := call.Args["id"].(string)
	if !ok || eventID == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'id' is a required non-empty string"))
	}

	event, exists := a.events[eventID]
	if !exists {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("scheduled action with ID '%s' not found", eventID))
	}

	a.cronScheduler.Remove(event.EntryID)
	delete(a.events, eventID)

	// Send a final response to the model to close the tool call transaction.
	if a.bus != nil {
		a.Printf("Closing tool call transaction for deleted scheduled action ID %s (Call ID: %s)", eventID, event.CallID)
		finalResponse := a.CreateFunctionResponse(
			&genai.FunctionCall{ID: event.CallID, Name: event.CallName},
			map[string]any{"status": "Scheduled action has been deleted and the task is now complete."},
			nil,
			false, // This is the final response for this tool call ID.
		)
		(*a.bus).Publish("agent:tool_response", finalResponse)
	}

	a.Printf("Deleted scheduled action with ID %s", eventID)
	result := map[string]any{"status": "Scheduled action deleted successfully."}
	return a.CreateFunctionResponse(call, result, nil)
}
