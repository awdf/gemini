package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gemini/agents"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	fmt.Println("----------TESTS STARTED----------")
	// Close any existing browser before running tests to ensure a clean state.
	agents.UserBrowser.Close()

	if err := agents.UserBrowser.OpenBrowser("https://www.example.com/"); err != nil {
		fmt.Printf("FATAL: Could not open browser for tests: %v\n", err)
		os.Exit(1)
	}
	agents.UserBrowser.Wipe()
	code := m.Run()
	agents.UserBrowser.Close()
	fmt.Println("----------TESTS DONE----------")
	os.Exit(code)
}

// TestWebScraperAgent_CreateTabAndNavigate tests creating a new tab and navigating.
func TestWebScraperAgent_CreateTabAndNavigate(t *testing.T) {
	if !agents.UserBrowser.IsConnected() {
		t.Error("The remote browser interaction tests as no remote browser is running or connected.")
	}

	newTabID, err := agents.UserBrowser.CreateTab("https://www.google.com")
	if err != nil {
		t.Fatal(err.Error())
	}
	if newTabID == "" {
		t.Fatal("CreateTab returned an empty tab ID.")
	}
	fmt.Printf("Created new tab with ID: %s\n", newTabID)

	newTabCtx := agents.UserBrowser.SelectTab(newTabID)
	var title string
	// Poll the document title until it contains "Google", waiting up to 5 seconds.
	// This is more reliable than a fixed sleep or just waiting for the body,
	// as the title is set by JavaScript after the initial load.
	err = chromedp.Run(newTabCtx, chromedp.Poll(`document.title.includes("Google")`, nil, chromedp.WithPollingTimeout(5*time.Second)))
	if err != nil {
		t.Fatalf("Failed to get title: %v", err)
	}
	err = chromedp.Run(newTabCtx, chromedp.Title(&title))
	if !strings.Contains(title, "Google") {
		t.Errorf("Expected title to contain 'Google', but got '%s'", title)
	}

	t.Logf("Successfully navigated to Google and read title: '%s'", title)
}

// TestWebScraperAgent_ReadActiveTabTitle tests reading the active tab title using a remote browser.
func TestWebScraperAgent_ReadActiveTabTitle(t *testing.T) {
	if !agents.UserBrowser.IsConnected() {
		t.Error("The remote browser interaction tests as no remote browser is running or connected.")
	}

	tabs, err := agents.UserBrowser.Tabs()
	if err != nil {
		t.Fatalf("Failed to get browser tabs: %v", err)
	}

	var exampleTab *target.Info
	for _, tab := range tabs {
		if strings.Contains(tab.URL, "example.com") {
			exampleTab = tab
			break
		}
	}

	if exampleTab == nil {
		t.Fatal("Could not find tab with example.com")
	}

	taskCtx := agents.UserBrowser.SelectTab(exampleTab.TargetID)

	var title string
	if err := chromedp.Run(taskCtx, chromedp.Title(&title)); err != nil {
		t.Fatalf("Failed to get active tab title: %v", err)
	}

	t.Logf("Successfully read active tab title: '%s'", title)

	if title == "" {
		t.Error("Expected active tab title to be non-empty, but it was empty.")
	}

	if strings.EqualFold(title, "about:blank") {
		t.Errorf("Expected active tab title not to be 'about:blank', but it was.")
	}

	assert.Equal(t, "Example Domain", title)
}

// TestWebScraperAgent_ReadActiveTabContent tests reading the active tab content using a remote browser.
func TestWebScraperAgent_ReadActiveTabContent(t *testing.T) {
	if !agents.UserBrowser.IsConnected() {
		t.Error("The remote browser interaction tests as no remote browser is running or connected.")
	}

	tabs, err := agents.UserBrowser.Tabs()
	if err != nil {
		t.Fatalf("Failed to get browser tabs: %v", err)
	}

	var exampleTab *target.Info
	for _, tab := range tabs {
		if strings.Contains(tab.URL, "example.com") {
			exampleTab = tab
			break
		}
	}

	if exampleTab == nil {
		t.Fatal("Could not find tab with example.com")
	}

	taskCtx := agents.UserBrowser.SelectTab(exampleTab.TargetID)

	var content string
	if err := chromedp.Run(taskCtx, chromedp.OuterHTML("body", &content)); err != nil {
		t.Fatalf("Failed to get active tab content: %v", err)
	}

	if content == "" {
		t.Error("Expected active tab to have non-empty body content, but it was empty.")
	}
	t.Log("Successfully read non-empty body content from the active tab.")
}

func TestWebScraperAgent_TestFormInteractions(t *testing.T) {
	if !agents.UserBrowser.IsConnected() {
		t.Skip("Skipping browser interaction tests as no remote browser is running or connected.")
	}

	// Create a simple HTTP server to serve a test page.
	server := setupTestServer()
	defer server.Close()

	// Navigate to the test page.
	newTabID, err := agents.UserBrowser.CreateTab(server.URL)
	if err != nil {
		t.Fatalf("Failed to create tab: %v", err)
	}
	ctx := agents.UserBrowser.SelectTab(newTabID)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Test FillFormField and GetValue
	err = agents.UserBrowser.FillFormField(ctx, "#test-input", "hello world")
	if err != nil {
		t.Fatalf("Failed to fill form field: %v", err)
	}

	value, err := agents.UserBrowser.GetValue(ctx, "#test-input")
	if err != nil {
		t.Fatalf("Failed to get value: %v", err)
	}
	assert.Equal(t, "hello world", value, "The input value should be 'hello world'")

	// Test ClickElement
	err = agents.UserBrowser.ClickElement(ctx, "#test-button")
	if err != nil {
		t.Fatalf("Failed to click element: %v", err)
	}

	// After clicking, a new element should be present.
	var clickResult string
	err = chromedp.Run(ctx, chromedp.Text("#click-result", &clickResult, chromedp.ByQuery))
	if err != nil {
		t.Fatalf("Failed to get click result: %v", err)
	}
	assert.Equal(t, "Button clicked!", clickResult, "The click result text should be 'Button clicked!'")

	// Test Submit
	err = agents.UserBrowser.Submit(ctx, "#test-form")
	if err != nil {
		t.Fatalf("Failed to submit form: %v", err)
	}

	// After submitting, the URL should change.
	var url string
	err = chromedp.Run(ctx, chromedp.Location(&url))
	if err != nil {
		t.Fatalf("Failed to get URL: %v", err)
	}
	assert.Contains(t, url, "?input=hello+world", "The URL should contain the submitted form data")
}

func setupTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintln(w, "<!DOCTYPE html><html><body><form id=\"test-form\" action=\"/\" method=\"get\"><input id=\"test-input\" name=\"input\" type=\"text\"><input id=\"test-submit\" type=\"submit\" value=\"Submit\"></form><button id=\"test-button\">Click me</button><div id=\"click-result\"></div><script>document.getElementById('test-button').addEventListener('click', function() { document.getElementById('click-result').textContent = 'Button clicked!'; });</script></body></html>")
	}))
}
