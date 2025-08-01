package ai

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

	"gemini/config"
	"gemini/google"
)

// GmailAgent handles interactions with the Gmail API.
type GmailAgent struct {
	service   *gmail.Service
	userEmail string // To store the user's email address for the 'From' header.
}

// agentGmail is the global instance of the GmailAgent.
var agentGmail *GmailAgent

// InitGmailAgent creates and initializes the global Gmail agent.
// It handles the OAuth2 flow to get an authenticated client.
func InitGmailAgent(ctx context.Context) error {
	if config.C.Google.CredentialsFile == "" || config.C.Google.TokenFile == "" {
		return fmt.Errorf("google credentials file or token file path is not configured")
	}

	scopes := []string{
		gmail.GmailReadonlyScope,
		gmail.GmailSendScope,
		// Add more scopes here if needed in the future, e.g., compose, send
	}

	client, err := google.GetClient(ctx, scopes)
	if err != nil {
		return fmt.Errorf("unable to get Google OAuth2 client: %w", err)
	}

	gmailService, err := gmail.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("unable to retrieve Gmail client: %w", err)
	}

	// Get user's email address to use in the 'From' header when sending.
	profile, err := gmailService.Users.GetProfile("me").Do()
	if err != nil {
		return fmt.Errorf("unable to retrieve user's Gmail profile: %w", err)
	}
	if profile.EmailAddress == "" {
		return fmt.Errorf("could not determine user's email address from profile")
	}

	agentGmail = &GmailAgent{service: gmailService, userEmail: profile.EmailAddress}
	log.Printf("Gmail Agent initialized successfully for user: %s", agentGmail.userEmail)
	return nil
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
			log.Printf("WARNING: unable to get full message for ID %s: %v", msg.Id, err)
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
func findBodyPart(part *gmail.MessagePart, mimeType string) string {
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
			bodyBuilder.WriteString(findBodyPart(subPart, mimeType))
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
	body := findBodyPart(msg.Payload, "text/plain")
	if body != "" {
		return body, nil
	}

	// 2. If no 'text/plain', fall back to 'text/html'. The AI can often parse this.
	body = findBodyPart(msg.Payload, "text/html")
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
