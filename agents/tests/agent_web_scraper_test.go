package tests

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// userBrowser manages a single, persistent connection to a browser instance for testing.
type userBrowser struct {
	context      context.Context
	cancel       context.CancelFunc
	isRemote     bool
	isConnected  bool
	wipe         bool
	selectedTabs map[target.ID]struct {
		ctx    context.Context
		cancel context.CancelFunc
	}
	mu sync.Mutex
}

// User requested wipe on close
func (b *userBrowser) Wipe() {
	b.wipe = true
}

// isRemoteRunning checks if a browser is listening on the remote debugging port.
func (b *userBrowser) isRemoteRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:9222", 1100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Open establishes the connection to the browser. It's designed to be called once.
func (b *userBrowser) Open() {
	if b.isConnected {
		return
	}

	b.isRemote = b.isRemoteRunning()

	b.context, b.cancel = chromedp.NewRemoteAllocator(context.Background(), "http://localhost:9222")
	if b.isRemote {
		b.isConnected = true
		b.selectedTabs = make(map[target.ID]struct {
			ctx    context.Context
			cancel context.CancelFunc
		})
	}
}

// Close terminates the browser connection.
func (b *userBrowser) Close() {
	if !b.isConnected {
		return
	}

	if b.wipe && len(b.selectedTabs) > 0 {
		b.mu.Lock()
		// Cancel all remaining selected tab contexts
		for id, tab := range b.selectedTabs {
			tab.cancel()
			delete(b.selectedTabs, id)
		}
		b.mu.Unlock()
	}

	if b.cancel != nil {
		b.cancel()
	}
	b.isConnected = false
}

// IsConnected returns the connection status of the browser.
func (b *userBrowser) IsConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.isConnected
}

// Tabs returns a list of all open tabs in the browser.
func (b *userBrowser) Tabs() ([]*target.Info, error) {
	if !b.isConnected {
		return nil, fmt.Errorf("browser is not connected")
	}

	// We need a task context to run the Targets command.
	// We can create a temporary one from the allocator context.
	taskCtx, cancel := chromedp.NewContext(b.context)
	defer cancel()

	allTargets, err := chromedp.Targets(taskCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to get browser targets: %w", err)
	}

	var pageTargets []*target.Info
	for _, ti := range allTargets {
		if ti.Type == "page" && (strings.HasPrefix(ti.URL, "http://") || strings.HasPrefix(ti.URL, "https://")) {
			pageTargets = append(pageTargets, ti)
		}
	}

	return pageTargets, nil
}

// SelectTab creates a new context for a specific tab ID.
func (b *userBrowser) SelectTab(tabID target.ID) context.Context {
	if !b.isConnected {
		// Return a canceled context if not connected.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// If a valid context for this tab already exists, return it.
	if tab, ok := b.selectedTabs[tabID]; ok && tab.ctx.Err() == nil {
		return tab.ctx
	}

	// Otherwise, create a new context.
	taskCtx, cancel := chromedp.NewContext(b.context, chromedp.WithTargetID(tabID))
	b.selectedTabs[tabID] = struct {
		ctx    context.Context
		cancel context.CancelFunc
	}{ctx: taskCtx, cancel: cancel}

	return taskCtx
}

// DropTab closes a specific tab by its ID.
func (b *userBrowser) DropTab(tabID target.ID) error {
	if !b.isConnected {
		return fmt.Errorf("browser is not connected")
	}

	b.mu.Lock()
	if tab, ok := b.selectedTabs[tabID]; ok {
		tab.cancel()
		delete(b.selectedTabs, tabID)
	}
	b.mu.Unlock()

	// To close a target, we need a context. We can use the main allocator context
	// to create a temporary one just for this action.
	taskCtx, cancel := chromedp.NewContext(b.context)
	defer cancel()

	if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error { return target.CloseTarget(tabID).Do(ctx) })); err != nil {
		return fmt.Errorf("failed to close tab with ID %s: %w", tabID, err)
	}
	return nil
}

var UserBrowser = &userBrowser{}

func TestMain(m *testing.M) {
	fmt.Println("----------TESTS STARTED----------")
	UserBrowser.Open()
	code := m.Run()
	UserBrowser.Close()
	fmt.Println("----------TESTS DONE----------")
	os.Exit(code)
}

// TestWebScraperAgent_ReadActiveTabTitle tests reading the active tab title using a remote browser.
func TestWebScraperAgent_ReadActiveTabTitle(t *testing.T) {
	if !UserBrowser.IsConnected() {
		t.Skip("Skipping remote browser interaction tests as no remote browser is running or connected.")
	}

	targets, err := UserBrowser.Tabs()
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

	taskCtx := UserBrowser.SelectTab(activeTabID)

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
}

// TestWebScraperAgent_ReadActiveTabContent tests reading the active tab content using a remote browser.
func TestWebScraperAgent_ReadActiveTabContent(t *testing.T) {
	time.Sleep(1 * time.Second)
	if !UserBrowser.IsConnected() {
		t.Skip("Skipping remote browser interaction tests as no remote browser is running or connected.")
	}

	targets, err := UserBrowser.Tabs()
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

	taskCtx := UserBrowser.SelectTab(activeTabID)

	var content string
	if err := chromedp.Run(taskCtx, chromedp.OuterHTML("body", &content)); err != nil {
		t.Fatalf("Failed to get active tab content: %v", err)
	}

	if content == "" {
		t.Error("Expected active tab to have non-empty body content, but it was empty.")
	}
	t.Log("Successfully read non-empty body content from the active tab.")
}
