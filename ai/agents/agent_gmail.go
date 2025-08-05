package agents

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/google"
)

func init() {
	RegisterFactory(AgentGmailName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		// NewGmailAgent can return nil, which is a valid nil interface value.
		return NewGmailAgent(ctx, client, toolset)
	})
}

// GmailAgent handles interactions with the Gmail API.
type GmailAgent struct {
	*Agent
	service   *gmail.Service
	userEmail string // To store the user's email address for the 'From' header.
}

// NewGmailAgent creates and initializes the Gmail agent.
// It handles the OAuth2 flow to get an authenticated client.
func NewGmailAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *GmailAgent {
	if !config.C.Google.Enabled {
		log.Println("WARNING: Could not create Gmail agent, Gmail tools is disabled.")
		return nil
	}

	if config.C.Google.CredentialsFile == "" || config.C.Google.TokenFile == "" {
		log.Printf("google credentials file or token file path is not configured")
		return nil
	}

	scopes := []string{
		gmail.GmailReadonlyScope,
		gmail.GmailSendScope,
		// Add more scopes here if needed in the future, e.g., compose, send
	}

	gClient, err := google.GetClient(ctx, scopes)
	if err != nil {
		log.Printf("unable to get Google OAuth2 client: %v", err)
		return nil
	}

	gmailService, err := gmail.NewService(ctx, option.WithHTTPClient(gClient))
	if err != nil {
		log.Printf("unable to retrieve Gmail client: %v", err)
		return nil
	}

	// Get user's email address to use in the 'From' header when sending.
	profile, err := gmailService.Users.GetProfile("me").Do()
	if err != nil {
		log.Printf("unable to retrieve user's Gmail profile: %v", err)
		return nil
	}
	if profile.EmailAddress == "" {
		log.Printf("could not determine user's email address from profile")
		return nil
	}

	// Create a base agent. It won't use the model directly, but embedding it makes it a valid Callable.
	baseAgent := NewAgent(ctx, client, AgentConfig{
		Name: AgentGmailName,
	})

	functions := []*genai.FunctionDeclaration{
		{
			Name:        "listEmails",
			Description: "GMAIL: Lists emails from the user's Gmail account. Can be filtered with a query.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"query": {
						Type:        genai.TypeString,
						Description: "A standard Gmail search query (e.g., 'from:hello@example.com is:unread'). Optional.",
					},
					"max_results": {
						Type:        genai.TypeInteger,
						Description: "The maximum number of emails to return. Defaults to 10 if not specified.",
					},
				},
			},
			Behavior: genai.BehaviorBlocking,
		},
		{
			Name:        "readEmail",
			Description: "GMAIL: Reads the full content of a specific email using its message ID.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"message_id": {
						Type:        genai.TypeString,
						Description: "The ID of the message to read, obtained from 'listEmails'.",
					},
				},
				Required: []string{"message_id"},
			},
			Behavior: genai.BehaviorBlocking,
		},
		{
			Name:        "sendEmail",
			Description: "GMAIL: Sends an email from the user's Gmail account.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"to": {
						Type:        genai.TypeString,
						Description: "The recipient's email address.",
					},
					"subject": {
						Type:        genai.TypeString,
						Description: "The subject of the email.",
					},
					"body": {
						Type:        genai.TypeString,
						Description: "The plain text body of the email.",
					},
				},
				Required: []string{"to", "subject", "body"},
			},
			Behavior: genai.BehaviorBlocking,
		},
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, functions...)

	gmailAgent := &GmailAgent{
		Agent:     baseAgent,
		service:   gmailService,
		userEmail: profile.EmailAddress,
	}

	gmailAgent.Printf("Initialized successfully for user: %s", gmailAgent.userEmail)
	return gmailAgent
}

// WarmUp for GmailAgent does nothing as it doesn't make direct model calls.
// It exists to satisfy the Callable interface.
func (a *GmailAgent) WarmUp() time.Duration {
	return 0
}

// EmailSummary contains the essential details of an email.
type EmailSummary struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Snippet string `json:"snippet"`
	Date    string `json:"date"`
}

// ListEmails retrieves a list of emails matching a query.
func (a *GmailAgent) ListEmails(query string, maxResults int64) ([]EmailSummary, error) {
	if a.service == nil {
		return nil, fmt.Errorf("gmail agent not initialized")
	}

	listCall := a.service.Users.Messages.List("me")
	if query != "" {
		listCall.Q(query)
	}
	if maxResults > 0 {
		listCall.MaxResults(maxResults)
	}

	msgs, err := listCall.Do()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve messages: %w", err)
	}

	var summaries []EmailSummary
	if len(msgs.Messages) == 0 {
		return summaries, nil
	}

	for _, msg := range msgs.Messages {
		fullMessage, err := a.service.Users.Messages.Get("me", msg.Id).Format("metadata").Do()
		if err != nil {
			a.Printf("WARNING: unable to get full message for ID %s: %v", msg.Id, err)
			continue
		}
		summary := EmailSummary{ID: fullMessage.Id, Snippet: fullMessage.Snippet}
		for _, h := range fullMessage.Payload.Headers {
			switch h.Name {
			case "Subject":
				summary.Subject = h.Value
			case "From":
				addr, err := mail.ParseAddress(h.Value)
				if err == nil {
					if addr.Name != "" {
						summary.From = fmt.Sprintf("%s (%s)", addr.Name, addr.Address)
					} else {
						summary.From = addr.Address
					}
				} else {
					summary.From = h.Value // Fallback on parse error
				}
			case "To":
				addrs, err := mail.ParseAddressList(h.Value)
				if err == nil {
					var toParts []string
					for _, addr := range addrs {
						if addr.Name != "" {
							toParts = append(toParts, fmt.Sprintf("%s (%s)", addr.Name, addr.Address))
						} else {
							toParts = append(toParts, addr.Address)
						}
					}
					summary.To = strings.Join(toParts, ", ")
				} else {
					summary.To = h.Value // Fallback on parse error
				}
			case "Date":
				// Parse the date string from the email header.
				parsedTime, err := mail.ParseDate(h.Value)
				if err == nil {
					// If successful, convert it to the user's configured local timezone.
					loc, locErr := time.LoadLocation(config.C.AI.Timezone)
					if locErr != nil {
						loc = time.UTC // Fallback to UTC on error
					}
					summary.Date = parsedTime.In(loc).Format(config.TimeFormat)
				} else {
					summary.Date = h.Value // On parse error, use the raw date string.
				}
			}
		}
		summaries = append(summaries, summary)
	}

	return summaries, nil
}

// findBodyPart recursively searches for a part with a specific MIME type.
func (a *GmailAgent) findBodyPart(part *gmail.MessagePart, mimeType string) string {
	if part == nil {
		return ""
	}

	var bodyBuilder strings.Builder

	// If the current part matches the desired MIME type, decode and append its body.
	if part.MimeType == mimeType && part.Body != nil && part.Body.Data != "" {
		data, err := base64.URLEncoding.DecodeString(part.Body.Data)
		if err == nil {
			bodyBuilder.WriteString(string(data))
		}
	}

	// If the part is multipart, recurse into its sub-parts.
	if strings.HasPrefix(part.MimeType, "multipart/") {
		for _, subPart := range part.Parts {
			// Append the result of the recursive call.
			bodyBuilder.WriteString(a.findBodyPart(subPart, mimeType))
		}
	}

	return bodyBuilder.String()
}

// ReadEmail retrieves the full content of a specific email.
func (a *GmailAgent) ReadEmail(messageID string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("gmail agent not initialized")
	}

	msg, err := a.service.Users.Messages.Get("me", messageID).Format("full").Do() // Use "full" to get all parts
	if err != nil {
		return "", fmt.Errorf("unable to retrieve message %s: %w", messageID, err)
	}

	if msg.Payload == nil {
		return "[Email has no content]", nil
	}

	// 1. Prioritize finding the 'text/plain' part.
	body := a.findBodyPart(msg.Payload, "text/plain")
	if body != "" {
		return body, nil
	}

	// 2. If no 'text/plain', fall back to 'text/html'. The AI can often parse this.
	body = a.findBodyPart(msg.Payload, "text/html")
	if body != "" {
		return body, nil
	}

	// 3. As a last resort for very simple emails, check the top-level body directly.
	if msg.Payload.Body != nil && msg.Payload.Body.Data != "" {
		data, err := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
		if err == nil {
			return string(data), nil
		}
	}

	return "[Could not decode email body]", nil
}

// SendEmail sends an email on behalf of the user.
func (a *GmailAgent) SendEmail(to, subject, body string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("gmail agent not initialized")
	}

	// Construct the email message headers in RFC 2822 format.
	messageStr := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		a.userEmail, to, subject, body)

	// Base64-encode the message for the Gmail API.
	rawMessage := base64.URLEncoding.EncodeToString([]byte(messageStr))

	message := &gmail.Message{
		Raw: rawMessage,
	}

	sentMsg, err := a.service.Users.Messages.Send("me", message).Do()
	if err != nil {
		return "", fmt.Errorf("failed to send email: %w", err)
	}

	return fmt.Sprintf("Email sent successfully. Message ID: %s", sentMsg.Id), nil
}

func (a *GmailAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "sendEmail":
		return a.handleGmailSendEmailTool(call)
	case "listEmails":
		return a.handleGmailListEmailsTool(call)
	case "readEmail":
		return a.handleGmailReadEmailTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *GmailAgent) handleGmailSendEmailTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	if a == nil {
		err = fmt.Errorf("gmail agent not initialized or enabled")
	} else {
		to, toOK := call.Args["to"].(string)
		subject, subjectOK := call.Args["subject"].(string)
		body, bodyOK := call.Args["body"].(string)
		if !toOK || !subjectOK || !bodyOK {
			err = fmt.Errorf("'to', 'subject', and 'body' arguments are required and must be strings")
		} else {
			status, sendErr := a.SendEmail(to, subject, body)
			if sendErr != nil {
				err = fmt.Errorf("failed to send email: %w", sendErr)
			} else {
				a.Printf("Successfully sent email to: '%s'", to)
				result = map[string]any{"status": status}
			}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}

func (a *GmailAgent) handleGmailListEmailsTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	if a == nil {
		err = fmt.Errorf("gmail agent not initialized or enabled")
	} else {
		query, _ := call.Args["query"].(string)
		maxResultsFloat, _ := call.Args["max_results"].(float64)
		maxResults := int64(maxResultsFloat)
		if maxResults <= 0 {
			maxResults = 10 // Default value
		}

		emails, listErr := a.ListEmails(query, maxResults)
		if listErr != nil {
			err = fmt.Errorf("failed to list emails: %w", listErr)
		} else {
			a.Printf("Successfully listed %d emails for query: '%s'", len(emails), query)
			result = map[string]any{"emails": emails}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}

func (a *GmailAgent) handleGmailReadEmailTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error

	if a == nil {
		err = fmt.Errorf("gmail agent not initialized or enabled")
	} else {
		messageID, ok := call.Args["message_id"].(string)
		if !ok || messageID == "" {
			err = fmt.Errorf("'message_id' argument is required and must be a non-empty string")
		} else {
			content, readErr := a.ReadEmail(messageID)
			if readErr != nil {
				err = fmt.Errorf("failed to read email with ID '%s': %w", messageID, readErr)
			} else {
				a.Printf("Successfully read email with ID: '%s'", messageID)
				result = map[string]any{"content": content}
			}
		}
	}

	return a.CreateFunctionResponse(call, result, err)
}
