package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
	oauth2api "google.golang.org/api/oauth2/v2"
	"google.golang.org/api/option"

	"gemini/config"
)

var (
	googleClientOnce sync.Once
	googleClient     *http.Client
	googleClientErr  error
)

// getAllScopes defines all possible Google API scopes the application might need across all agents.
// This ensures a single token is requested with all necessary permissions.
func getAllScopes() []string {
	return []string{
		gmail.GmailReadonlyScope,
		gmail.GmailSendScope,
		calendar.CalendarEventsScope,
		oauth2api.UserinfoEmailScope, // Required to validate the token and get user email.
	}
}

// GetClient uses a singleton pattern to create and return a single, shared http.Client
// for all Google API interactions. It requests all necessary scopes upfront.
func GetClient(ctx context.Context) (*http.Client, error) {
	googleClientOnce.Do(func() {
		googleClient, googleClientErr = createGoogleClient(ctx)
	})
	return googleClient, googleClientErr
}

// createGoogleClient contains the logic to perform the OAuth2 flow.
// It's called only once by the GetClient singleton.
func createGoogleClient(ctx context.Context) (*http.Client, error) {
	credentialsFile := config.C.Google.CredentialsFile
	tokenFile := config.C.Google.TokenFile

	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read client secret file (%s): %w", credentialsFile, err)
	}

	// For desktop apps, the redirect URI must match what is configured in the Google Cloud Console.
	oauthConfig, err := google.ConfigFromJSON(b, getAllScopes()...)
	if err != nil {
		return nil, fmt.Errorf("unable to parse client secret file to config: %w", err)
	}
	oauthConfig.RedirectURL = "http://localhost:8080/oauth2callback"

	// First, try to get a validated client from a saved token.
	client, err := getClientFromToken(ctx, oauthConfig, tokenFile)
	if err == nil {
		return client, nil // Success!
	}

	// If getting client from token failed (e.g., no token, or token was stale),
	// start the interactive auth flow to get a new one.
	log.Printf("Could not use existing token (%v), performing new OAuth 2.0 authorization flow...", err)
	return getClientFromWeb(ctx, oauthConfig, tokenFile)
}

// getClientFromToken attempts to create an http.Client from a saved token file.
// It validates the token by making a simple API call. If the token is stale/revoked,
// it deletes the token file and returns an error to trigger re-authentication.
func getClientFromToken(ctx context.Context, config *oauth2.Config, tokenFile string) (*http.Client, error) {
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read token from file: %w", err)
	}

	client := config.Client(ctx, tok)

	// Validate the token by making a simple, low-scope API call.
	oauth2Service, err := oauth2api.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("failed to create oauth2 service for validation: %w", err)
	}

	_, err = oauth2Service.Userinfo.Get().Do()
	if err != nil {
		// If the error indicates an invalid grant, the token is stale or has been revoked.
		if strings.Contains(err.Error(), "invalid_grant") {
			log.Println("Stale or revoked token detected. Deleting token file and re-authenticating.")
			_ = os.Remove(tokenFile) // Attempt to remove, ignore error if it fails.
			return nil, fmt.Errorf("token is stale or revoked: %w", err)
		}
		// For other errors, we might still be able to proceed, but log it as a warning.
		log.Printf("Warning: token validation call failed, but proceeding anyway: %v", err)
	}

	log.Println("Successfully validated and using existing token from file.")
	return client, nil
}

// getClientFromWeb performs the interactive OAuth 2.0 flow to get a new token from the user.
func getClientFromWeb(ctx context.Context, config *oauth2.Config, tokenFile string) (*http.Client, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Printf("Go to the following link in your browser to authorize the application: \n%v\n", authURL)

	codeCh := make(chan string)
	server := &http.Server{Addr: ":8080"}

	// Temporarily replace the default ServeMux to handle only our callback.
	originalMux := http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
	defer func() { http.DefaultServeMux = originalMux }()

	http.HandleFunc("/oauth2callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "No code in request", http.StatusBadRequest)
			log.Println("No code in OAuth callback request.")
			close(codeCh)
			return
		}
		fmt.Fprintf(w, "Authorization code received! You can close this tab.")
		codeCh <- code
		go func() {
			if err := server.Shutdown(context.Background()); err != nil {
				log.Printf("Error shutting down OAuth server: %v", err)
			}
		}()
	})

	fmt.Printf("Listening for OAuth callback on http://localhost:8080/oauth2callback\n")
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("OAuth callback server failed: %v", err)
		}
	}()

	authCode, ok := <-codeCh
	if !ok {
		return nil, fmt.Errorf("authorization flow was canceled or failed")
	}

	tok, err := config.Exchange(ctx, authCode)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve token from web: %w", err)
	}

	saveToken(tokenFile, tok)
	return config.Client(ctx, tok), nil
}

// tokenFromFile retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	return tok, json.NewDecoder(f).Decode(tok)
}

// saveToken saves a token to a file.
func saveToken(file string, token *oauth2.Token) {
	log.Printf("Saving credential file to: %s\n", file)
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("WARNING: Unable to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(token)
}
