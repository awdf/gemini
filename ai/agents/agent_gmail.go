package agents

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/google/uuid"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/google"
)

const AgentGmailName = "gmailAgent"

func init() {
	RegisterFactory(AgentGmailName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		// NewGmailAgent can return nil, which is a valid nil interface value.
		return NewGmailAgent(ctx, client, toolset, bus)
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
func NewGmailAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *GmailAgent {
	if !config.C.Google.Enabled {
		log.Println("WARNING: Could not create Gmail agent, Gmail tools is disabled.")
		return nil
	}

	if config.C.Google.CredentialsFile == "" || config.C.Google.TokenFile == "" {
		log.Printf("google credentials file or token file path is not configured")
		return nil
	}

	gClient, err := google.GetClient(ctx)
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
			Description: "GMAIL: Reads the full content of a specific email, including its body and a list of attachments, using its message ID.",
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
			Description: "GMAIL: Sends an email from the user's Gmail account. Can include an attachment from the workspace.",
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
					"attachment_path": {
						Type:        genai.TypeString,
						Description: "Optional. The path to a file in the workspace to attach to the email.",
					},
				},
				Required: []string{"to", "subject", "body"},
			},
			Behavior: genai.BehaviorBlocking,
		},
		{
			Name:        "downloadAttachment",
			Description: "GMAIL: Downloads a specific email attachment to the workspace directory.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"message_id": {
						Type:        genai.TypeString,
						Description: "The ID of the message containing the attachment.",
					},
					"filename": {
						Type:        genai.TypeString,
						Description: "The desired filename for the downloaded attachment, obtained from 'readEmail'.",
					},
				},
				Required: []string{"message_id", "filename"},
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

// AttachmentSummary holds metadata about an email attachment.
type AttachmentSummary struct {
	PartID   string `json:"part_id"`
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// EmailContent holds the body and attachment details of an email.
type EmailContent struct {
	Body        string              `json:"body"`
	Attachments []AttachmentSummary `json:"attachments"`
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

// extractParts recursively traverses the message parts to find the body and attachments.
func (a *GmailAgent) extractParts(part *gmail.MessagePart, content *EmailContent) {
	if part == nil {
		return
	}

	// If the part is multipart, recurse into its sub-parts.
	if strings.HasPrefix(part.MimeType, "multipart/") {
		for _, subPart := range part.Parts {
			a.extractParts(subPart, content)
		}
		return
	}

	// Check for text body parts.
	if part.MimeType == "text/plain" || part.MimeType == "text/html" {
		if part.Body != nil && part.Body.Data != "" {
			// Prioritize plain text. Only take HTML if plain text is not yet found.
			isPlainText := part.MimeType == "text/plain"
			if isPlainText || content.Body == "" {
				data, err := base64.URLEncoding.DecodeString(part.Body.Data)
				if err == nil {
					content.Body = string(data)
				}
			}
		}
		return
	}

	// Check for attachments. An attachment has a filename and is not an inline part.
	// Crucially, it must have a Body.AttachmentId to be downloadable with the Attachments.Get endpoint.
	if part.Filename != "" && part.Body != nil && part.Body.AttachmentId != "" {
		attachment := AttachmentSummary{
			// The PartID for the download tool is the AttachmentId from the body, not the PartId of the MIME part.
			PartID:   part.Body.AttachmentId,
			Filename: part.Filename,
			MIMEType: part.MimeType,
			Size:     part.Body.Size,
		}
		content.Attachments = append(content.Attachments, attachment)
	}
}

// ReadEmail retrieves the full content of a specific email, including body and attachments.
func (a *GmailAgent) ReadEmail(messageID string) (EmailContent, error) {
	content := EmailContent{}
	if a.service == nil {
		return content, fmt.Errorf("gmail agent not initialized")
	}

	msg, err := a.service.Users.Messages.Get("me", messageID).Format("full").Do()
	if err != nil {
		return content, fmt.Errorf("unable to retrieve message %s: %w", messageID, err)
	}

	if msg.Payload == nil {
		content.Body = "[Email has no content]"
		return content, nil
	}

	a.extractParts(msg.Payload, &content)

	// As a last resort for very simple emails that are not multipart.
	if content.Body == "" && msg.Payload.Body != nil && msg.Payload.Body.Data != "" {
		data, err := base64.URLEncoding.DecodeString(msg.Payload.Body.Data)
		if err == nil {
			content.Body = string(data)
		}
	}

	if content.Body == "" && len(content.Attachments) == 0 {
		content.Body = "[Could not decode email body or find attachments]"
	}

	return content, nil
}

// SendEmail sends an email on behalf of the user.
func (a *GmailAgent) SendEmail(to, subject, body, attachmentPath string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("gmail agent not initialized")
	}

	var messageBytes []byte
	if attachmentPath == "" {
		// No attachment, send a simple text email.
		// The message needs to be in RFC 2822 format.
		// We should specify Content-Type for clarity.
		messageBytes = []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
			a.userEmail, to, subject, body))
	} else {
		// Attachment present, construct a multipart message.
		safePath, err := config.GetSafePath(attachmentPath)
		if err != nil {
			return "", err // GetSafePath provides a good error message.
		}

		fileBytes, err := os.ReadFile(safePath)
		if err != nil {
			return "", fmt.Errorf("failed to read attachment file '%s': %w", attachmentPath, err)
		}

		mimeType := mime.TypeByExtension(filepath.Ext(safePath))
		if mimeType == "" {
			mimeType = "application/octet-stream" // Default MIME type
		}

		boundary := uuid.New().String()
		var mailBuilder strings.Builder

		// Headers for the multipart message
		mailBuilder.WriteString(fmt.Sprintf("From: %s\r\n", a.userEmail))
		mailBuilder.WriteString(fmt.Sprintf("To: %s\r\n", to))
		mailBuilder.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
		mailBuilder.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", boundary))

		// Text part
		mailBuilder.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		mailBuilder.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
		mailBuilder.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
		mailBuilder.WriteString(body + "\r\n")

		// Attachment part
		mailBuilder.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		mailBuilder.WriteString(fmt.Sprintf("Content-Type: %s\r\n", mimeType))
		mailBuilder.WriteString("Content-Transfer-Encoding: base64\r\n")
		mailBuilder.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", filepath.Base(safePath)))
		mailBuilder.WriteString(base64.StdEncoding.EncodeToString(fileBytes))
		mailBuilder.WriteString("\r\n")

		// Closing boundary
		mailBuilder.WriteString(fmt.Sprintf("--%s--", boundary))

		messageBytes = []byte(mailBuilder.String())
	}

	// Base64-encode the entire message for the Gmail API.
	rawMessage := base64.URLEncoding.EncodeToString(messageBytes)

	message := &gmail.Message{
		Raw: rawMessage,
	}

	sentMsg, err := a.service.Users.Messages.Send("me", message).Do()
	if err != nil {
		return "", fmt.Errorf("failed to send email: %w", err)
	}

	return fmt.Sprintf("Email sent successfully. Message ID: %s", sentMsg.Id), nil
}

// findAttachmentPart recursively searches for a message part that corresponds to an attachment with the given filename.
func (a *GmailAgent) findAttachmentPart(part *gmail.MessagePart, filename string) *gmail.MessagePart {
	if part == nil {
		return nil
	}

	// If the current part matches the filename and has an attachment ID, we've found it.
	if strings.EqualFold(part.Filename, filename) && part.Body != nil && part.Body.AttachmentId != "" {
		return part
	}

	// If the part is multipart, recurse into its sub-parts.
	if strings.HasPrefix(part.MimeType, "multipart/") {
		for _, subPart := range part.Parts {
			if foundPart := a.findAttachmentPart(subPart, filename); foundPart != nil {
				return foundPart
			}
		}
	}

	return nil
}

// DownloadAttachment retrieves a specific attachment and saves it to the workspace.
func (a *GmailAgent) DownloadAttachment(messageID, filename string) (string, error) {
	if a.service == nil {
		return "", fmt.Errorf("gmail agent not initialized")
	}

	// To download an attachment, we need its AttachmentID. We find this by re-fetching
	// the message and searching for the part with the matching filename. This is more
	// robust than relying on the model to pass the correct ID.
	msg, err := a.service.Users.Messages.Get("me", messageID).Format("full").Do()
	if err != nil {
		return "", fmt.Errorf("unable to retrieve message %s to find attachment: %w", messageID, err)
	}

	attachmentPart := a.findAttachmentPart(msg.Payload, filename)
	if attachmentPart == nil || attachmentPart.Body == nil || attachmentPart.Body.AttachmentId == "" {
		return "", fmt.Errorf("could not find an attachment named '%s' with a downloadable ID in message %s", filename, messageID)
	}

	attachmentID := attachmentPart.Body.AttachmentId

	// Get the safe path within the workspace.
	safePath, err := config.GetSafePath(filename)
	if err != nil {
		return "", err // The error from GetSafePath is already descriptive.
	}

	// Retrieve the attachment data from the Gmail API using the found AttachmentID.
	attachment, err := a.service.Users.Messages.Attachments.Get("me", messageID, attachmentID).Do()
	if err != nil {
		return "", fmt.Errorf("unable to retrieve attachment with ID %s: %w", attachmentID, err)
	}

	// The data is base64url encoded.
	decodedData, err := base64.URLEncoding.DecodeString(attachment.Data)
	if err != nil {
		return "", fmt.Errorf("failed to decode attachment data: %w", err)
	}

	// Write the decoded data to the file.
	if err := os.WriteFile(safePath, decodedData, 0o644); err != nil {
		return "", fmt.Errorf("failed to write attachment to file '%s': %w", safePath, err)
	}

	return fmt.Sprintf("Attachment '%s' downloaded successfully to workspace.", filename), nil
}

func (a *GmailAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "sendEmail":
		return a.handleGmailSendEmailTool(call)
	case "listEmails":
		return a.handleGmailListEmailsTool(call)
	case "readEmail":
		return a.handleGmailReadEmailTool(call)
	case "downloadAttachment":
		return a.handleDownloadAttachmentTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *GmailAgent) handleGmailSendEmailTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)

	var result any
	var err error
	to, toOK := call.Args["to"].(string)
	subject, subjectOK := call.Args["subject"].(string)
	body, bodyOK := call.Args["body"].(string)
	attachmentPath, _ := call.Args["attachment_path"].(string) // It's optional
	if !toOK || !subjectOK || !bodyOK {
		err = fmt.Errorf("'to', 'subject', and 'body' arguments are required and must be strings")
	} else {
		status, sendErr := a.SendEmail(to, subject, body, attachmentPath)
		if sendErr != nil {
			err = fmt.Errorf("failed to send email: %w", sendErr)
		} else {
			a.Printf("Successfully sent email to: '%s'", to)
			result = map[string]any{"status": status}
		}
	}
	return a.CreateFunctionResponse(call, result, err)
}

func (a *GmailAgent) handleDownloadAttachmentTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	var result any
	var err error
	messageID, msgOk := call.Args["message_id"].(string)
	filename, fileOk := call.Args["filename"].(string)
	if !msgOk || !fileOk || messageID == "" || filename == "" {
		err = fmt.Errorf("'message_id' and 'filename' arguments are required and must be non-empty strings")
	} else {
		status, downloadErr := a.DownloadAttachment(messageID, filename)
		if downloadErr != nil {
			err = fmt.Errorf("failed to download attachment: %w", downloadErr)
		} else {
			a.Printf("Successfully downloaded attachment: '%s'", filename)
			result = map[string]any{"status": status}
		}
	}
	return a.CreateFunctionResponse(call, result, err)
}

func (a *GmailAgent) handleGmailListEmailsTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	var result any
	var err error
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
	return a.CreateFunctionResponse(call, result, err)
}

func (a *GmailAgent) handleGmailReadEmailTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	var result any
	var err error
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
	return a.CreateFunctionResponse(call, result, err)
}
