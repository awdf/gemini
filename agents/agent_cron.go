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

const AgentCronName = "cronAgent"

func init() {
	RegisterFactory(AgentCronName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewCronAgent(ctx, client, toolset, bus)
	})
}

// ScheduledEvent holds information about a scheduled event.
type ScheduledEvent struct {
	ID       string         `json:"id"` // ID of the event for model and accounting
	Pattern  string         `json:"pattern"`
	Prompt   string         `json:"prompt"`
	Context  map[string]any `json:"context,omitempty"`
	EntryID  cron.EntryID   `json:"-"` // ID of the cron entry
	CallID   string         `json:"-"` // ID of the original tool call
	CallName string         `json:"-"` // Name of the original tool call
}

// TriggeredEvent represents the data sent when a scheduled action fires.
type TriggeredEvent struct {
	ID      string         `json:"id"`
	Prompt  string         `json:"prompt"`
	Context map[string]any `json:"context,omitempty"`
}

// ResponsePayload defines the structured response for cron agent tool calls.
type ResponsePayload struct {
	Confirmation     string            `json:"confirmation,omitempty"`
	ScheduledEvent   *ScheduledEvent   `json:"scheduled_event,omitempty"`
	ScheduledActions []*ScheduledEvent `json:"scheduled_actions,omitempty"`
	TriggeredEvent   *TriggeredEvent   `json:"triggered_event,omitempty"`
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
	agentInstructions := `When using CRON tools ('scheduleDelayedAction', 'scheduleRecuringAction'):
1. A scheduled action is asynchronous. When you schedule an action, you will get an immediate confirmation with an 'id'.
2. The actual execution will happen later. When the scheduled time arrives, you will receive a new tool response containing a 'triggered_event' with the 'prompt' and 'context' you originally provided.
3. You MUST then execute the 'prompt' from the 'triggered_event'.
4. For recurring actions, you can use 'updateScheduledActionContext' to change the 'context' for the *next* execution. This is how you can maintain state, like a counter.

A cron 'pattern' is a string of 6 fields separated by spaces, representing:
1. Seconds (0-59)
2. Minutes (0-59)
3. Hours (0-23)
4. Day of month (1-31)
5. Month (1-12 or JAN-DEC)
6. Day of week (0-6 or SUN-SAT, where 0 is Sunday)
You can also use special characters: '*' to match any value, ',' to list values (e.g., '1,15'), '-' for ranges (e.g., '9-17'), and '/' for steps (e.g., '*/15'). The '?' character can be used in the day-of-month or day-of-week field to signify no specific value.`

	agentConfig := AgentConfig{
		Name:              AgentCronName,
		AgentInstructions: agentInstructions,
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
			Description: "CRON: Schedules an action to be performed once after a specified delay. The tool returns a confirmation and the scheduled_event object. When the delay is over, a tool response will be sent with a 'triggered_event' object containing the 'prompt' and 'context', which you must then execute.",
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
					"context": {
						Type:        genai.TypeObject,
						Description: "Optional. A JSON object to store state or context that will be returned when the action is triggered.",
					},
				},
				Required: []string{"delay", "action_prompt"},
			},
			Behavior: genai.BehaviorNonBlocking,
		},
		{
			Name:        "scheduleRecuringAction",
			Description: "CRON: Schedules an action to be performed on a recurring basis using a cron pattern. The tool returns a confirmation and the scheduled_event object. Each time the schedule is met, a tool response will be sent with a 'triggered_event' object containing the 'prompt' and 'context', which you must then execute.",
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
					"context": {
						Type:        genai.TypeObject,
						Description: "Optional. A JSON object to store state or context that will be returned when the action is triggered.",
					},
				},
				Required: []string{"pattern", "action_prompt"},
			},
			Behavior: genai.BehaviorNonBlocking,
		},
		{
			Name:        "listScheduledActions",
			Description: "CRON: Lists all currently scheduled actions, returning an object containing a 'scheduled_actions' array.",
			Parameters:  &genai.Schema{Type: genai.TypeObject},
		},
		{
			Name:        "deleteScheduledAction",
			Description: "CRON: Deletes a scheduled action by its 'id' and returns a confirmation message.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"id": {Type: genai.TypeString, Description: "The ID of the scheduled action to delete, obtained from 'listScheduledActions' or a 'schedule' call."},
				},
				Required: []string{"id"},
			},
		},
		{
			Name:        "updateScheduledActionContext",
			Description: "CRON: Updates the context data for an existing scheduled action and returns a confirmation message. This allows changing the state that will be passed to the next execution of the action.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"id": {
						Type:        genai.TypeString,
						Description: "The ID of the scheduled action to update, obtained from a previous 'schedule' call or 'listScheduledActions'.",
					},
					"context": {
						Type:        genai.TypeObject,
						Description: "A JSON object representing the new context to store. This will completely replace the old context.",
					},
				},
				Required: []string{"id", "context"},
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
	case "updateScheduledActionContext":
		return a.handleUpdateScheduledActionContext(call)
	default:
		return nil
	}
}

func (a *CronAgent) parseAndValidateScheduleArgs(args map[string]any) (cronSpec, actionPrompt string, context map[string]any, isRecurring bool, err error) {
	var delayOK, patternOK, promptOK bool
	delayStr, delayOK := args["delay"].(string)
	pattern, patternOK := args["pattern"].(string)
	actionPrompt, promptOK = args["action_prompt"].(string)
	context, _ = args["context"].(map[string]any) // It's optional, so we ignore the 'ok'

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
	cronSpec, actionPrompt, context, isRecurring, err := a.parseAndValidateScheduleArgs(call.Args)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	// Both delayed and recurring actions require the event bus for async responses.
	if a.bus == nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("scheduled actions are not supported in this mode"))
	}

	if cronSpec != "" {
		return a.scheduleCronExpression(call, cronSpec, actionPrompt, context, isRecurring)
	}

	// Should not be reached due to validation above, but as a fallback.
	return a.CreateFunctionResponse(call, nil, fmt.Errorf("internal error: no valid scheduling parameter found"))
}

func (a *CronAgent) scheduleCronExpression(call *genai.FunctionCall, cronSpec, actionPrompt string, context map[string]any, isRecurring bool) *genai.FunctionResponse {
	// Handle scheduled action
	a.mu.Lock()
	defer a.mu.Unlock()

	eventID := uuid.New().String()
	event := &ScheduledEvent{
		ID:       eventID,
		Pattern:  cronSpec,
		Prompt:   actionPrompt,
		Context:  context,
		CallID:   call.ID,
		CallName: call.Name,
	}

	entryID, err := a.cronScheduler.AddFunc(cronSpec, func() {
		a.Printf("Scheduled action triggered for event ID %s. Sending FunctionResponse to model.", event.ID)

		triggeredEvent := &TriggeredEvent{
			ID:      event.ID,
			Prompt:  event.Prompt,
			Context: event.Context,
		}
		response := a.CreateFunctionResponse(call, ResponsePayload{TriggeredEvent: triggeredEvent}, nil, isRecurring)
		(*a.bus).Publish(config.AgentTopic, response)
	})
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid cron pattern '%s': %w", cronSpec, err))
	}

	event.EntryID = entryID
	a.events[eventID] = event

	a.Printf("Scheduled new action '%s' with ID %s", actionPrompt, eventID)
	result := ResponsePayload{
		Confirmation:   "New action scheduled successfully. A response will be sent when the action fires.",
		ScheduledEvent: event,
	}
	return a.CreateFunctionResponse(call, result, nil, true)
}

func (a *CronAgent) handleScheduledActionsList(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	eventList := make([]*ScheduledEvent, 0, len(a.events))
	for _, event := range a.events {
		eventList = append(eventList, event)
	}

	result := ResponsePayload{ScheduledActions: eventList}
	return a.CreateFunctionResponse(call, result, nil)
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
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("scheduled action with event ID '%s' not found", eventID))
	}

	a.cronScheduler.Remove(event.EntryID)
	delete(a.events, eventID)

	// Send a final response to the model to close the tool call transaction.
	if a.bus != nil {
		a.Printf("Closing tool call transaction for deleted scheduled action ID %s (Call ID: %s)", eventID, event.CallID)
		finalPayload := ResponsePayload{Confirmation: "Scheduled action has been deleted and the task is now complete."}
		finalResponse := a.CreateFunctionResponse(
			&genai.FunctionCall{ID: event.CallID, Name: event.CallName},
			finalPayload,
			nil,
			false, // This is the final response for this tool call ID.
		)
		(*a.bus).Publish(config.AgentTopic, finalResponse)
	}

	a.Printf("Deleted scheduled action with ID %s", eventID)
	result := ResponsePayload{Confirmation: "Scheduled action deleted successfully."}
	return a.CreateFunctionResponse(call, result, nil)
}

func (a *CronAgent) handleUpdateScheduledActionContext(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	eventID, ok := call.Args["id"].(string)
	if !ok || eventID == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'id' is a required non-empty string"))
	}

	newContext, ok := call.Args["context"].(map[string]any)
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'context' is a required map[string]any object"))
	}

	event, exists := a.events[eventID]
	if !exists {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("scheduled action with ID '%s' not found", eventID))
	}

	event.Context = newContext
	a.Printf("Updated context for scheduled action with ID %s", eventID)

	result := ResponsePayload{Confirmation: "Context updated successfully."}
	return a.CreateFunctionResponse(call, result, nil)
}
