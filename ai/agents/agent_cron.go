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

// CronTriggerEvent is the event payload published when a cron job fires.
type CronTriggerEvent struct {
	ID      string
	Message string
}

func init() {
	RegisterFactory(AgentCronName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewCronAgent(ctx, client, toolset, bus)
	})
}

// CronEvent holds information about a scheduled event.
type CronEvent struct {
	ID      string       `json:"id"`
	Pattern string       `json:"pattern"` // Cron pattern or duration string
	Message string       `json:"message"`
	IsCron  bool         `json:"is_cron"`
	EntryID cron.EntryID `json:"-"` // Internal cron job ID, only if IsCron is true
	Timer   *time.Timer  `json:"-"` // Timer for one-time events, only if IsCron is false
}

// CronAgent handles scheduling and triggering time-based events.
type CronAgent struct {
	*Agent
	cronScheduler *cron.Cron
	events        map[string]*CronEvent
	bus           *EventBus.Bus
	mu            sync.Mutex
}

// NewCronAgent creates a new CronAgent.
func NewCronAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *CronAgent {
	if bus == nil {
		// This agent is useless without the event bus.
		return nil
	}

	agentConfig := AgentConfig{
		Name: AgentCronName,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	// Load timezone from config to ensure cron jobs fire at the correct local time.
	loc, err := time.LoadLocation(config.C.AI.Timezone)
	if err != nil {
		log.Printf("WARNING: [cronAgent] Invalid timezone '%s' in config, falling back to UTC. Error: %v", config.C.AI.Timezone, err)
		loc = time.UTC
	}

	agent := &CronAgent{
		Agent:         baseAgent,
		cronScheduler: cron.New(cron.WithLocation(loc)),
		events:        make(map[string]*CronEvent),
		bus:           bus,
	}

	agent.cronScheduler.Start()
	agent.Printf("Initialized and cron scheduler started in timezone: %s.", loc.String())

	functions := []*genai.FunctionDeclaration{
		{
			Name:        "scheduleRecurringEvent",
			Description: "CRON: Schedules a recurring event or a reminder. After a successful call, you MUST confirm to the user that the event has been scheduled and state the pattern. This function returns immediately. The model will be proactively notified in a new turn when the event is due.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"pattern": {
						Type:        genai.TypeString,
						Description: "The cron pattern for the schedule (e.g., '0 9 * * MON' for 9 AM every Monday, or '@every 5m' for every 5 minutes).",
					},
					"message": {
						Type:        genai.TypeString,
						Description: "The reminder message for the event.",
					},
				},
				Required: []string{"pattern", "message"},
			},
		},
		{
			Name:        "scheduleOneTimeReminder",
			Description: "CRON: Schedules a single, non-recurring reminder for a future time. Use this for simple delays like 'in 5 minutes'. After a successful call, you MUST confirm to the user that the reminder has been set and state when it will trigger (e.g., 'in 5 minutes').",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"duration": {
						Type:        genai.TypeString,
						Description: "The duration from now to wait before sending the reminder, in a format like '5m', '1h30s', '2h'.",
					},
					"message": {
						Type:        genai.TypeString,
						Description: "The reminder message for the event.",
					},
				},
				Required: []string{"duration", "message"},
			},
		},
		{
			Name:        "listEvents",
			Description: "CRON: Lists all currently scheduled events.",
			Parameters:  &genai.Schema{Type: genai.TypeObject},
		},
		{
			Name:        "deleteEvent",
			Description: "CRON: Deletes a scheduled event by its ID.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"id": {
						Type:        genai.TypeString,
						Description: "The ID of the event to delete, obtained from 'listEvents'.",
					},
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
	case "scheduleRecurringEvent":
		return a.handleScheduleRecurringEvent(call)
	case "scheduleOneTimeReminder":
		return a.handleScheduleOneTimeReminder(call)
	case "listEvents":
		return a.handleListEvents(call)
	case "deleteEvent":
		return a.handleDeleteEvent(call)
	default:
		return nil
	}
}

func (a *CronAgent) handleScheduleRecurringEvent(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	pattern, patOK := call.Args["pattern"].(string)
	message, msgOK := call.Args["message"].(string)

	if !patOK || !msgOK || pattern == "" || message == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'pattern' and 'message' are required non-empty strings"))
	}

	eventID := uuid.New().String()
	event := &CronEvent{
		ID:      eventID,
		Pattern: pattern,
		Message: message,
		IsCron:  true,
	}

	entryID, err := a.cronScheduler.AddFunc(pattern, func() {
		// This function runs when the cron job fires.
		// We publish an event to the main bus.
		triggerEvent := CronTriggerEvent{
			ID:      event.ID,
			Message: event.Message,
		}
		a.Printf("Cron event triggered: %+v. Publishing to topic 'cron:trigger'", triggerEvent)
		(*a.bus).Publish("cron:trigger", triggerEvent)
	})
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid cron pattern '%s': %w", pattern, err))
	}

	event.EntryID = entryID
	a.events[eventID] = event

	a.Printf("Scheduled event '%s' with ID %s", message, eventID)
	result := map[string]any{
		"status":   "Event scheduled successfully.",
		"event_id": eventID,
	}
	return a.CreateFunctionResponse(call, result, nil)
}

func (a *CronAgent) handleScheduleOneTimeReminder(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	durationStr, durOK := call.Args["duration"].(string)
	message, msgOK := call.Args["message"].(string)

	if !durOK || !msgOK || durationStr == "" || message == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'duration' and 'message' are required non-empty strings"))
	}

	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid duration format '%s': %w", durationStr, err))
	}

	eventID := uuid.New().String()
	event := &CronEvent{
		ID:      eventID,
		Pattern: durationStr, // Store the original duration string
		Message: message,
		IsCron:  false,
	}

	timer := time.AfterFunc(duration, func() {
		// This function runs when the timer fires.
		triggerEvent := CronTriggerEvent{
			ID:      event.ID,
			Message: event.Message,
		}
		a.Printf("One-time event triggered: %+v. Publishing to topic 'cron:trigger'", triggerEvent)
		(*a.bus).Publish("cron:trigger", triggerEvent)

		// Clean up the event from the map after it has fired.
		a.mu.Lock()
		delete(a.events, event.ID)
		a.mu.Unlock()
	})

	event.Timer = timer
	a.events[eventID] = event

	a.Printf("Scheduled one-time event '%s' with ID %s", message, eventID)
	result := map[string]any{"status": "One-time event scheduled successfully.", "event_id": eventID}
	return a.CreateFunctionResponse(call, result, nil)
}

func (a *CronAgent) handleListEvents(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Create a copy of the events to return, without the internal EntryID.
	eventList := make([]CronEvent, 0, len(a.events))
	for _, event := range a.events {
		eventList = append(eventList, CronEvent{
			ID:      event.ID,
			Pattern: event.Pattern,
			Message: event.Message,
			IsCron:  event.IsCron,
		})
	}

	return a.CreateFunctionResponse(call, map[string]any{"events": eventList}, nil)
}

func (a *CronAgent) handleDeleteEvent(call *genai.FunctionCall) *genai.FunctionResponse {
	a.mu.Lock()
	defer a.mu.Unlock()

	eventID, ok := call.Args["id"].(string)
	if !ok || eventID == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'id' is a required non-empty string"))
	}

	event, exists := a.events[eventID]
	if !exists {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("event with ID '%s' not found", eventID))
	}

	if event.IsCron {
		a.cronScheduler.Remove(event.EntryID)
	} else if event.Timer != nil {
		event.Timer.Stop()
	}
	delete(a.events, eventID)

	a.Printf("Deleted scheduled event with ID %s", eventID)
	result := map[string]any{"status": "Event deleted successfully."}
	return a.CreateFunctionResponse(call, result, nil)
}
