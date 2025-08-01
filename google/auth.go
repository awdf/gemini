package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"gemini/config"
)

// GetClient uses a previously saved token or performs a new OAuth 2.0 flow.
func GetClient(ctx context.Context, scopes []string) (*http.Client, error) {
	credentialsFile := config.C.Google.CredentialsFile
	tokenFile := config.C.Google.TokenFile

	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read client secret file (%s): %w", credentialsFile, err)
	}

	// For desktop apps, the redirect URI should be a loopback address.
	// This must match what is configured in the Google Cloud Console for the Desktop App client ID.
	config, err := google.ConfigFromJSON(b, scopes...)
	if err != nil {
		return nil, fmt.Errorf("unable to parse client secret file to config: %w", err)
	}
	config.RedirectURL = "http://localhost:8080/oauth2callback"

	tok, err := tokenFromFile(tokenFile)
	if err == nil {
		return config.Client(ctx, tok), nil
	}

	log.Println("Performing new OAuth 2.0 authorization flow for Google services...")
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Printf("Go to the following link in your browser to authorize the application: \n%v\n", authURL)

	codeCh := make(chan string)
	server := &http.Server{Addr: ":8080"}

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

	tok, err = config.Exchange(ctx, authCode)
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
