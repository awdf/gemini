package tests

import (
	"fmt"
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
	agents.UserBrowser.Open()
	code := m.Run()
	agents.UserBrowser.Close()
	fmt.Println("----------TESTS DONE----------")
	os.Exit(code)
}

// TestWebScraperAgent_OpenNewTabAndNavigate tests creating a new tab and navigating.
func TestWebScraperAgent_OpenNewTabAndNavigate(t *testing.T) {
	if !agents.UserBrowser.IsConnected() {
		t.Skip("Skipping remote browser interaction tests as no remote browser is running or connected.")
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
		t.Skip("Skipping remote browser interaction tests as no remote browser is running or connected.")
	}

	targets, err := agents.UserBrowser.Tabs()
	if err != nil {
		t.Fatalf("Failed to get browser targets: %v", err)
	}

	activeTabID := target.ID("")
	if len(targets) > 0 {
		activeTabID = targets[0].TargetID
	}

	if activeTabID == "" {
		t.Fatal("Could not find an active, visible tab to read the title from.")
	}

	taskCtx := agents.UserBrowser.SelectTab(activeTabID)

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

	assert.Equal(t, "Google", title)
}

// TestWebScraperAgent_ReadActiveTabContent tests reading the active tab content using a remote browser.
func TestWebScraperAgent_ReadActiveTabContent(t *testing.T) {
	if !agents.UserBrowser.IsConnected() {
		t.Skip("Skipping remote browser interaction tests as no remote browser is running or connected.")
	}

	targets, err := agents.UserBrowser.Tabs()
	if err != nil {
		t.Fatalf("Failed to get browser targets: %v", err)
	}

	activeTabID := target.ID("")
	if len(targets) > 0 {
		activeTabID = targets[0].TargetID
	}

	if activeTabID == "" {
		t.Fatal("Could not find an active, visible tab to read content from.")
	}

	taskCtx := agents.UserBrowser.SelectTab(activeTabID)

	var content string
	if err := chromedp.Run(taskCtx, chromedp.OuterHTML("body", &content)); err != nil {
		t.Fatalf("Failed to get active tab content: %v", err)
	}

	if content == "" {
		t.Error("Expected active tab to have non-empty body content, but it was empty.")
	}
	t.Log("Successfully read non-empty body content from the active tab.")
}
