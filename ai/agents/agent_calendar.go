package agents

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/asaskevich/EventBus"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/google"
)

func init() {
	RegisterFactory(AgentCalendarName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		return NewCalendarAgent(ctx, client, toolset, bus)
	})
}

// CalendarAgent handles interactions with the Google Calendar API.
type CalendarAgent struct {
	*Agent
	service *calendar.Service
}

// NewCalendarAgent creates and initializes the Calendar agent.
func NewCalendarAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *CalendarAgent {
	if !config.C.Google.Enabled {
		log.Println("WARNING: Could not create Calendar agent, Google integration is disabled.")
		return nil
	}

	if config.C.Google.CredentialsFile == "" || config.C.Google.TokenFile == "" {
		log.Printf("google credentials file or token file path is not configured")
		return nil
	}

	gClient, err := google.GetClient(ctx)
	if err != nil {
		log.Printf("unable to get Google OAuth2 client for Calendar: %v", err)
		return nil
	}

	calendarService, err := calendar.NewService(ctx, option.WithHTTPClient(gClient))
	if err != nil {
		log.Printf("unable to retrieve Calendar client: %v", err)
		return nil
	}

	baseAgent := NewAgent(ctx, client, AgentConfig{
		Name: AgentCalendarName,
	})

	functions := []*genai.FunctionDeclaration{
		{
			Name:        "listEvents",
			Description: "CALENDAR: Lists events from the user's primary Google Calendar within a specified time range.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"time_min": {
						Type:        genai.TypeString,
						Description: "The start of the time range for events, in RFC3339 format (e.g., '2024-08-12T00:00:00Z'). Defaults to the current time if not specified.",
					},
					"time_max": {
						Type:        genai.TypeString,
						Description: "The end of the time range for events, in RFC3339 format (e.g., '2024-08-19T00:00:00Z'). Required.",
					},
					"max_results": {
						Type:        genai.TypeInteger,
						Description: "The maximum number of events to return. Defaults to 10.",
					},
				},
				Required: []string{"time_max"},
			},
		},
		{
			Name:        "createEvent",
			Description: "CALENDAR: Creates a new event on the user's primary Google Calendar.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"summary": {
						Type:        genai.TypeString,
						Description: "The title or summary of the event.",
					},
					"start_time": {
						Type:        genai.TypeString,
						Description: "The start time of the event in RFC3339 format.",
					},
					"end_time": {
						Type:        genai.TypeString,
						Description: "The end time of the event in RFC3339 format.",
					},
					"description": {
						Type:        genai.TypeString,
						Description: "A detailed description for the event.",
					},
					"location": {
						Type:        genai.TypeString,
						Description: "The location of the event.",
					},
					"attendees": {
						Type:        genai.TypeArray,
						Description: "A list of email addresses of attendees to invite.",
						Items:       &genai.Schema{Type: genai.TypeString},
					},
				},
				Required: []string{"summary", "start_time", "end_time"},
			},
		},
		{
			Name:        "deleteEvent",
			Description: "CALENDAR: Deletes an event from the user's primary Google Calendar using its ID.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"event_id": {
						Type:        genai.TypeString,
						Description: "The ID of the event to delete, obtained from 'listEvents'.",
					},
				},
				Required: []string{"event_id"},
			},
		},
		{
			Name:        "updateEvent",
			Description: "CALENDAR: Updates an existing event. Only the provided fields will be changed.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"event_id": {
						Type:        genai.TypeString,
						Description: "The ID of the event to update.",
					},
					"summary": {
						Type:        genai.TypeString,
						Description: "The new title for the event.",
					},
					"start_time": {
						Type:        genai.TypeString,
						Description: "The new start time in RFC3339 format.",
					},
					"end_time": {
						Type:        genai.TypeString,
						Description: "The new end time in RFC3339 format.",
					},
					"description": {
						Type:        genai.TypeString,
						Description: "The new description for the event.",
					},
					"location": {
						Type:        genai.TypeString,
						Description: "The new location for the event.",
					},
					"attendees": {
						Type:        genai.TypeArray,
						Description: "A new list of attendee email addresses. This will replace the existing list.",
						Items:       &genai.Schema{Type: genai.TypeString},
					},
				},
				Required: []string{"event_id"},
			},
		},
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, functions...)

	calendarAgent := &CalendarAgent{
		Agent:   baseAgent,
		service: calendarService,
	}

	calendarAgent.Println("Initialized successfully.")
	return calendarAgent
}

// WarmUp for CalendarAgent does nothing as it doesn't make direct model calls.
func (a *CalendarAgent) WarmUp() time.Duration {
	return 0
}

// AttendeeInfo holds details for a single event attendee, including their response.
type AttendeeInfo struct {
	Email          string `json:"email"`
	DisplayName    string `json:"display_name,omitempty"`
	ResponseStatus string `json:"response_status"`
}

// EventSummary contains the essential details of a calendar event.
type EventSummary struct {
	ID          string         `json:"id"`
	Summary     string         `json:"summary"`
	StartTime   string         `json:"start_time"`
	EndTime     string         `json:"end_time"`
	Location    string         `json:"location,omitempty"`
	Description string         `json:"description,omitempty"`
	Attendees   []AttendeeInfo `json:"attendees,omitempty"`
	Link        string         `json:"link"`
}

// ListEvents retrieves a list of events from the primary calendar.
func (a *CalendarAgent) ListEvents(timeMin, timeMax string, maxResults int64) ([]EventSummary, error) {
	if a.service == nil {
		return nil, fmt.Errorf("calendar agent not initialized")
	}

	if timeMin == "" {
		timeMin = time.Now().Format(time.RFC3339)
	}

	events, err := a.service.Events.List("primary").
		ShowDeleted(false).
		SingleEvents(true).
		TimeMin(timeMin).
		TimeMax(timeMax).
		MaxResults(maxResults).
		OrderBy("startTime").
		Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve next ten of the user's events: %w", err)
	}

	var summaries []EventSummary
	if len(events.Items) == 0 {
		return summaries, nil
	}

	for _, item := range events.Items {
		summary := EventSummary{
			ID:          item.Id,
			Summary:     item.Summary,
			Location:    item.Location,
			Description: item.Description,
			Link:        item.HtmlLink,
		}
		// Handle attendees by extracting their email, display name, and response status.
		if len(item.Attendees) > 0 {
			for _, attendee := range item.Attendees {
				summary.Attendees = append(summary.Attendees, AttendeeInfo{
					Email:          attendee.Email,
					DisplayName:    attendee.DisplayName,
					ResponseStatus: attendee.ResponseStatus,
				})
			}
		}
		// Handle all-day events vs. timed events
		if item.Start.DateTime != "" {
			summary.StartTime = item.Start.DateTime
		} else {
			summary.StartTime = item.Start.Date
		}
		if item.End.DateTime != "" {
			summary.EndTime = item.End.DateTime
		} else {
			summary.EndTime = item.End.Date
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// CreateEvent adds a new event to the primary calendar.
func (a *CalendarAgent) CreateEvent(summary, location, description, startTime, endTime string, attendees []string) (*EventSummary, error) {
	if a.service == nil {
		return nil, fmt.Errorf("calendar agent not initialized")
	}

	event := &calendar.Event{
		Summary:     summary,
		Location:    location,
		Description: description,
		Start: &calendar.EventDateTime{
			DateTime: startTime,
			TimeZone: config.C.AI.Timezone,
		},
		End: &calendar.EventDateTime{
			DateTime: endTime,
			TimeZone: config.C.AI.Timezone,
		},
	}

	if len(attendees) > 0 {
		var eventAttendees []*calendar.EventAttendee
		for _, email := range attendees {
			eventAttendees = append(eventAttendees, &calendar.EventAttendee{Email: email})
		}
		event.Attendees = eventAttendees
	}

	createdEvent, err := a.service.Events.Insert("primary", event).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to create event: %w", err)
	}

	return &EventSummary{
		ID:      createdEvent.Id,
		Summary: createdEvent.Summary,
		Link:    createdEvent.HtmlLink,
	}, nil
}

// DeleteEvent removes an event from the primary calendar.
func (a *CalendarAgent) DeleteEvent(eventID string) error {
	if a.service == nil {
		return fmt.Errorf("calendar agent not initialized")
	}
	return a.service.Events.Delete("primary", eventID).Do()
}

// UpdateEvent modifies an existing event.
func (a *CalendarAgent) UpdateEvent(eventID string, updates map[string]interface{}) (*EventSummary, error) {
	if a.service == nil {
		return nil, fmt.Errorf("calendar agent not initialized")
	}

	// First, get the existing event to update.
	event, err := a.service.Events.Get("primary", eventID).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve event '%s' for update: %w", eventID, err)
	}

	// Apply updates
	if summary, ok := updates["summary"].(string); ok {
		event.Summary = summary
	}
	if location, ok := updates["location"].(string); ok {
		event.Location = location
	}
	if description, ok := updates["description"].(string); ok {
		event.Description = description
	}
	if startTime, ok := updates["start_time"].(string); ok {
		event.Start.DateTime = startTime
	}
	if endTime, ok := updates["end_time"].(string); ok {
		event.End.DateTime = endTime
	}
	if attendees, ok := updates["attendees"].([]string); ok {
		var eventAttendees []*calendar.EventAttendee
		for _, email := range attendees {
			eventAttendees = append(eventAttendees, &calendar.EventAttendee{Email: email})
		}
		event.Attendees = eventAttendees
	}

	updatedEvent, err := a.service.Events.Update("primary", eventID, event).Do()
	if err != nil {
		return nil, fmt.Errorf("unable to update event: %w", err)
	}

	return &EventSummary{
		ID:      updatedEvent.Id,
		Summary: updatedEvent.Summary,
		Link:    updatedEvent.HtmlLink,
	}, nil
}

// Handle dispatches tool calls to the appropriate handler.
func (a *CalendarAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "listEvents":
		return a.handleListEvents(call)
	case "createEvent":
		return a.handleCreateEvent(call)
	case "deleteEvent":
		return a.handleDeleteEvent(call)
	case "updateEvent":
		return a.handleUpdateEvent(call)
	default:
		return nil
	}
}

func (a *CalendarAgent) handleListEvents(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	timeMin, _ := call.Args["time_min"].(string)
	timeMax, ok := call.Args["time_max"].(string)
	if !ok || timeMax == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'time_max' argument is required"))
	}
	maxResultsFloat, _ := call.Args["max_results"].(float64)
	maxResults := int64(maxResultsFloat)
	if maxResults <= 0 {
		maxResults = 10
	}

	events, err := a.ListEvents(timeMin, timeMax, maxResults)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"events": events}, nil)
}

func (a *CalendarAgent) handleCreateEvent(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	summary, sumOK := call.Args["summary"].(string)
	startTime, startOK := call.Args["start_time"].(string)
	endTime, endOK := call.Args["end_time"].(string)
	if !sumOK || !startOK || !endOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'summary', 'start_time', and 'end_time' are required"))
	}

	location, _ := call.Args["location"].(string)
	description, _ := call.Args["description"].(string)

	var attendees []string
	if att, ok := call.Args["attendees"].([]interface{}); ok {
		for _, v := range att {
			if email, ok := v.(string); ok {
				attendees = append(attendees, email)
			}
		}
	}

	event, err := a.CreateEvent(summary, location, description, startTime, endTime, attendees)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"created_event": event}, nil)
}

func (a *CalendarAgent) handleDeleteEvent(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	eventID, ok := call.Args["event_id"].(string)
	if !ok || eventID == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'event_id' is required"))
	}

	err := a.DeleteEvent(eventID)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"status": "event deleted successfully"}, nil)
}

func (a *CalendarAgent) handleUpdateEvent(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	eventID, ok := call.Args["event_id"].(string)
	if !ok || eventID == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'event_id' is required"))
	}

	updates := make(map[string]interface{})
	if summary, ok := call.Args["summary"].(string); ok && summary != "" {
		updates["summary"] = summary
	}
	if location, ok := call.Args["location"].(string); ok && location != "" {
		updates["location"] = location
	}
	if description, ok := call.Args["description"].(string); ok && description != "" {
		updates["description"] = description
	}
	if startTime, ok := call.Args["start_time"].(string); ok && startTime != "" {
		updates["start_time"] = startTime
	}
	if endTime, ok := call.Args["end_time"].(string); ok && endTime != "" {
		updates["end_time"] = endTime
	}
	if att, ok := call.Args["attendees"].([]interface{}); ok {
		var attendees []string
		for _, v := range att {
			if email, ok := v.(string); ok {
				attendees = append(attendees, email)
			}
		}
		if len(attendees) > 0 {
			updates["attendees"] = attendees
		}
	}

	if len(updates) == 0 {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("at least one field to update must be provided"))
	}

	event, err := a.UpdateEvent(eventID, updates)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}
	return a.CreateFunctionResponse(call, map[string]any{"updated_event": event}, nil)
}
