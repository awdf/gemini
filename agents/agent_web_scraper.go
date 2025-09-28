package agents

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

const AgentWebScraperName = "webScraperAgent"

func init() {
	RegisterFactory(AgentWebScraperName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		// This agent is redundant for Post AI with native URL understanding.
		if !config.C.PostAI {
			return NewWebScraperAgent(ctx, client, toolset, bus)
		}
		return nil
	})
}

// GetExecutorAllocator attempts to connect to a running browser instance and returns an allocator context.
// If it fails, it falls back to creating a new headless browser instance.
func GetExecutorAllocator(parent context.Context) (context.Context, context.CancelFunc, bool) {
	// 1. Try to connect to the remote debugging port.
	conn, err := net.DialTimeout("tcp", "localhost:9222", 1*time.Second)
	if err == nil {
		// Connection successful, use the remote browser.
		conn.Close()
		log.Println("Remote browser detected. Creating allocator...")

		allocatorContext, cancelAllocator := chromedp.NewRemoteAllocator(parent, "http://localhost:9222")

		return allocatorContext, cancelAllocator, true // isRemote is true
	}

	// 2. If connection failed, fall back to a new headless browser.
	log.Println("Remote browser not detected. Launching new headless browser.")
	allocatorCtx, cancel := chromedp.NewExecAllocator(parent, chromedp.DefaultExecAllocatorOptions[:]...)
	ctx, cancelTimeout := context.WithTimeout(allocatorCtx, 30*time.Second)

	return ctx, func() {
		cancelTimeout()
		cancel()
	}, false // isRemote is false. The context is still an allocator context.
}

// NewTab creates a new tab in the browser associated with the allocator context.
// It returns a new context for that tab and its cancel function. This is the
// correct way to create a new tab in an existing window.
func NewTab(allocatorContext context.Context) (context.Context, context.CancelFunc, error) {
	var tabID target.ID
	// Step 1: Create a new target (tab) and capture its ID.

	// To run actions, we need a task context. We create a temporary one from the allocator
	// just to be able to execute the CreateTarget command.
	tempCtx, cancelTemp := chromedp.NewContext(allocatorContext)
	defer cancelTemp()

	err := chromedp.Run(tempCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			tabID, err = target.CreateTarget("about:blank").WithNewWindow(false).Do(ctx)
			return err
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create new target: %w", err)
	}
	if tabID == "" {
		return nil, nil, fmt.Errorf("did not receive a valid target ID for the new tab")
	}

	// Step 2: Create a new context specifically for the new tab using its ID.
	// This must be done with the original allocator context.
	newTabCtx, cancel := chromedp.NewContext(allocatorContext, chromedp.WithTargetID(tabID))

	return newTabCtx, cancel, nil
}

type WebScraperAgent struct {
	*Agent
	bus *EventBus.Bus
}

// NewWebScraperAgent creates a specialized agent for scraping and analyzing web pages.
func NewWebScraperAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *WebScraperAgent {
	// This agent is designed for asynchronous, non-blocking operation in LiveAI mode,
	// which requires the event bus.
	if bus == nil {
		return nil
	}
	systemInstruction := `You are a web page analysis expert with vision capabilities.
Your goal is to extract as much meaningful information as possible from the provided web page URL.
Analyze both the text content and the visual layout/images on the page to generate a comprehensive and detailed report.
Describe important visual elements like images, charts, and the overall page structure in your analysis.

To do this, you have access to the following tools. Use them strategically:
- **analyseWebPage**: Use this for a fast, comprehensive text-based analysis of a webpage. It's good for summarizing content and understanding the page's purpose.
- **getRawHTML**: Use this when you need to inspect the raw source code of a page, for example, to find CSS files or specific meta tags.
- **getRenderedContent**: Use this for modern, JavaScript-heavy websites where content is loaded dynamically. It provides the final HTML after all scripts have run.
- **getRenderedScreenshot**: This is your most powerful tool for visual analysis. When a user asks about the **layout, style, colors, or visual appearance** of a page, you MUST use this tool to get a screenshot. This will allow you to "see" the page and answer questions about its design accurately.
- **downloadWebFile**: Use this to download a file from a URL directly into the workspace. It's like using the 'wget' command.`

	scheme := genai.Schema{
		Type:        genai.TypeObject,
		Description: "A comprehensive analysis or summary of the web page.",
		Properties: map[string]*genai.Schema{
			"result": {Type: genai.TypeString, Description: "A detailed report of the web page content, formatted as a single Markdown string."},
		},
		Required: []string{"result"},
	}

	analyseFunc := genai.FunctionDeclaration{
		Name:        "analyseWebPage",
		Description: "WEB BROWSER: Scrapes and provides a comprehensive analysis of the content of a web page URL.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The full, URL-encoded URL of the web page to analyze.",
				},
			},
			Required: []string{"url"},
		},
		Behavior: genai.BehaviorNonBlocking,
	}

	getRawHTMLFunc := genai.FunctionDeclaration{
		Name:        "getRawHTML",
		Description: "WEB BROWSER: Fetches the raw HTML content of a web page URL without any processing or analysis.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The full, URL-encoded URL of the web page to fetch.",
				},
			},
			Required: []string{"url"},
		},
		Behavior: genai.BehaviorBlocking,
	}

	getRenderedContentFunc := genai.FunctionDeclaration{
		Name:        "getRenderedContent",
		Description: "WEB BROWSER: Fetches the fully rendered HTML content of a web page after JavaScript execution. If a URL is provided, it navigates to it. If no URL is provided, it reads the content of the currently active tab.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "Optional. The full, URL-encoded URL of the web page to render. If omitted, reads the current active tab.",
				},
			},
		},
		Behavior: genai.BehaviorNonBlocking,
	}

	getRenderedScreenshotFunc := genai.FunctionDeclaration{
		Name:        "getRenderedScreenshot",
		Description: "WEB BROWSER: Captures a screenshot of the web page in the user's currently active browser tab, or navigates to a new URL and captures it. The screenshot is then uploaded to the session context for visual analysis. Use this to 'see' the web page.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "Optional. The full, URL-encoded URL of a web page to capture. If omitted, captures the current active tab.",
				},
			},
		},
		Behavior: genai.BehaviorNonBlocking,
	}

	openPageFunc := genai.FunctionDeclaration{
		Name:        "openPage",
		Description: "WEB BROWSER: Opens a new tab in the user's browser and navigates to the specified URL.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The full, URL-encoded URL of the web page to open.",
				},
			},
			Required: []string{"url"},
		},
		Behavior: genai.BehaviorBlocking, // Blocking, as the action is immediate.
	}

	downloadWebFileFunc := genai.FunctionDeclaration{
		Name:        "downloadWebFile",
		Description: "WEB BROWSER: Downloads a file from a given URL and saves it to the workspace. Similar to the 'wget' command.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The full, URL-encoded URL of the file to download.",
				},
				"filename": {
					Type:        genai.TypeString,
					Description: "Optional. The name to save the file as in the workspace. If not provided, the name will be derived from the URL.",
				},
			},
			Required: []string{"url"},
		},
		Behavior: genai.BehaviorBlocking,
	}
	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, &analyseFunc, &getRawHTMLFunc, &getRenderedContentFunc, &getRenderedScreenshotFunc, &openPageFunc, &downloadWebFileFunc)

	agentConfig := AgentConfig{
		Name:              AgentWebScraperName,
		Model:             config.C.AI.Model,
		RPM:               config.C.AI.ModelRPM,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.2)),
		EnableURLContext:  true,
		ResponseSchema:    &scheme,
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	webScraperAgent := &WebScraperAgent{
		Agent: baseAgent,
		bus:   bus,
	}

	return webScraperAgent
}

func (a *WebScraperAgent) WarmUp() time.Duration {
	return a.Agent.WarmUp()
}

func (a *WebScraperAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "analyseWebPage":
		return a.handleAnalyseWebPageTool(call)
	case "getRawHTML":
		return a.handleGetRawHTMLTool(call)
	case "getRenderedContent":
		return a.handleGetRenderedContentTool(call)
	case "getRenderedScreenshot":
		return a.handleGetRenderedScreenshotTool(call)
	case "downloadWebFile":
		return a.handleDownloadWebFileTool(call)
	case "openPage":
		return a.handleOpenPageTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *WebScraperAgent) handleAnalyseWebPageTool(call *genai.FunctionCall) *genai.FunctionResponse {
	// 1. Parse arguments
	rawURL, urlOK := call.Args["url"].(string)
	if !urlOK || rawURL == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'url' argument is required and must be a non-empty string"))
	}

	// Decode the URL in case it's URL-encoded by the model.
	decodedURL, err := url.QueryUnescape(rawURL)
	if err != nil {
		// If decoding fails, it might not have been encoded. Use the raw URL but log a warning.
		a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
		decodedURL = rawURL
	}

	// Start the long-running analysis in a goroutine.
	go func() {
		a.Printf("Starting background analysis for web page URL: %s", decodedURL)
		// 2. Process with the agent. The agent is configured with URLContext,
		// so we just pass the URL in the prompt. The model will use its tool.
		prompt := fmt.Sprintf("Please analyze the provided web page and generate a comprehensive report based on your instructions. URL: %s", decodedURL)
		resultText, processErr := a.Process(prompt)

		var finalResponse *genai.FunctionResponse
		if processErr != nil {
			a.Printf("ERROR: Web page processing failed: %v", processErr)
			finalResponse = a.CreateFunctionResponse(call, nil, fmt.Errorf("web page processing failed: %w", processErr))
		} else {
			a.Printf("Web page analysis successful for url: '%s'", decodedURL)
			finalResponse = a.CreateFunctionResponse(call, map[string]any{"result": resultText}, nil)
		}
		(*a.bus).Publish(config.AgentTopic, finalResponse)
	}()

	// Immediately return the initial response to acknowledge the request.
	a.Printf("Acknowledging web page analysis request. Will report back when complete.")
	return a.CreateFunctionResponse(call, map[string]any{"status": "Web page analysis started."}, nil, true)
}

func (a *WebScraperAgent) handleGetRawHTMLTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	// 1. Parse arguments
	rawURL, urlOK := call.Args["url"].(string)
	if !urlOK || rawURL == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'url' argument is required and must be a non-empty string"))
	}

	// 2. Decode URL
	decodedURL, err := url.QueryUnescape(rawURL)
	if err != nil {
		a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
		decodedURL = rawURL
	}

	// 3. Fetch content
	resp, err := http.Get(decodedURL)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to fetch URL %s: %w", decodedURL, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to fetch URL %s: status code %d", decodedURL, resp.StatusCode))
	}

	// 4. Read body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to read response body from %s: %w", decodedURL, err))
	}

	// 5. Return response
	return a.CreateFunctionResponse(call, map[string]any{"html_content": string(body)}, nil)
}

func (a *WebScraperAgent) handleGetRenderedContentTool(call *genai.FunctionCall) *genai.FunctionResponse {
	// 1. Parse arguments
	rawURL, _ := call.Args["url"].(string) // URL is now optional

	go func() {
		var processErr error
		var htmlContent string

		// 3. Use chromedp to get rendered HTML
		allocatorCtx, cancel, isRemote := GetExecutorAllocator(context.Background())
		defer cancel()

		if rawURL != "" {
			decodedURL, err := url.QueryUnescape(rawURL)
			if err != nil {
				a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
				decodedURL = rawURL
			}
			a.Printf("Starting background rendering for web page URL: %s", decodedURL)

			taskCtx, cancelTask, err := NewTab(allocatorCtx)
			if err != nil {
				processErr = fmt.Errorf("failed to create new tab: %w", err)
			} else {
				defer cancelTask()
				processErr = chromedp.Run(taskCtx,
					chromedp.Navigate(decodedURL),
					chromedp.Sleep(2*time.Second), // Wait for JS to execute.
					chromedp.OuterHTML("html", &htmlContent),
				)
			}
		} else {
			if isRemote {
				a.Printf("Starting background rendering for current active tab.")
				activeTabCtx, cancelActiveTab := chromedp.NewContext(allocatorCtx)
				defer cancelActiveTab()
				processErr = chromedp.Run(activeTabCtx, chromedp.OuterHTML("html", &htmlContent))
			} else {
				processErr = fmt.Errorf("a URL is required to get rendered content in headless mode")
			}
		}

		var finalResponse *genai.FunctionResponse
		if processErr != nil {
			finalResponse = a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to get rendered content: %w", processErr))
		} else {
			finalResponse = a.CreateFunctionResponse(call, map[string]any{"rendered_html_content": htmlContent}, nil)
		}
		(*a.bus).Publish(config.AgentTopic, finalResponse)
	}()

	// Immediately return the initial response to acknowledge the request.
	a.Printf("Acknowledging web page rendering request. Will report back when complete.")
	return a.CreateFunctionResponse(call, map[string]any{"status": "Web page rendering started."}, nil, true)
}

func (a *WebScraperAgent) handleGetRenderedScreenshotTool(call *genai.FunctionCall) *genai.FunctionResponse {
	// 1. Parse arguments
	rawURL, _ := call.Args["url"].(string) // URL is now optional

	go func() {
		allocatorCtx, cancel, isRemote := GetExecutorAllocator(context.Background())
		defer cancel()

		var screenshotBuf []byte
		var processErr error
		var pageDescription string

		if rawURL != "" {
			// --- Behavior with URL: Navigate and screenshot ---
			decodedURL, err := url.QueryUnescape(rawURL)
			if err != nil {
				a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
				decodedURL = rawURL
			}
			pageDescription = fmt.Sprintf("the page at %s", decodedURL)
			a.Printf("Starting background screenshot capture for web page URL: %s", decodedURL)

			screenshotCtx, cancelTab, err := NewTab(allocatorCtx)
			if err != nil {
				processErr = fmt.Errorf("failed to create new tab for screenshot: %w", err)
			} else {
				defer cancelTab()
				processErr = chromedp.Run(screenshotCtx,
					chromedp.Navigate(decodedURL),
					// Wait for a common element to be visible, with a fallback sleep.
					chromedp.WaitVisible(`body`, chromedp.ByQuery),
					// Add a short, explicit delay after the element is visible to allow for rendering.
					chromedp.Sleep(2*time.Second),
					chromedp.FullScreenshot(&screenshotBuf, 0),
				)
			}
		} else {
			if isRemote {
				// --- Behavior without URL (Remote): Screenshot the currently active tab ---
				pageDescription = "your current screen"
				a.Printf("Starting background screenshot capture of the current active tab.")
				// To screenshot the active tab, we create a context that attaches to it.
				activeTabCtx, cancelActiveTab := chromedp.NewContext(allocatorCtx)
				defer cancelActiveTab()
				processErr = chromedp.Run(activeTabCtx,
					chromedp.FullScreenshot(&screenshotBuf, 0),
				)
			} else {
				// --- Behavior without URL (Headless): Not supported ---
				processErr = fmt.Errorf("a URL is required to take a screenshot in headless mode")
			}
		}

		var finalResponse *genai.FunctionResponse
		if processErr != nil {
			finalResponse = a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to get screenshot for %s: %w", pageDescription, processErr), false)
		} else {
			// 4. Prepare content for the model
			parts := []*genai.Part{
				genai.NewPartFromText(fmt.Sprintf("Here is the screenshot of %s that you requested for visual analysis.", pageDescription)),
				genai.NewPartFromBytes(screenshotBuf, config.MIMEImage), // config.MIMEImage is "image/png"
			}
			turn := genai.NewContentFromParts(parts, genai.RoleUser)
			content := genai.LiveClientContentInput{Turns: []*genai.Content{turn}}

			a.Println("Successfully prepared screenshot to be sent to live session.")
			result := map[string]any{"status": "Screenshot captured and prepared for analysis.", "send_content": content}
			finalResponse = a.CreateFunctionResponse(call, result, nil, false) // Final response, willContinue is false.
		}
		(*a.bus).Publish(config.AgentTopic, finalResponse)
	}()

	// Immediately return the initial response to acknowledge the request.
	a.Printf("Acknowledging web page screenshot request. Will report back when complete.")
	return a.CreateFunctionResponse(call, map[string]any{"status": "Web page screenshot capture started."}, nil, true)
}

func (a *WebScraperAgent) handleOpenPageTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	// 1. Parse arguments
	rawURL, urlOK := call.Args["url"].(string)
	if !urlOK || rawURL == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'url' argument is required and must be a non-empty string"))
	}

	decodedURL, err := url.QueryUnescape(rawURL)
	if err != nil {
		a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
		decodedURL = rawURL
	}

	// 2. Connect to the browser and open the page in a new tab.
	// We don't defer the cancel function here because we want the tab to stay open.
	allocatorCtx, _, isRemote := GetExecutorAllocator(context.Background())

	if !isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("the 'openPage' tool requires a running browser with remote debugging enabled"))
	}

	// Run the navigation in a goroutine so it doesn't block the main thread.
	go func() {
		// Create a new tab using the correct method.
		taskCtx, _, err := NewTab(allocatorCtx)
		if err != nil {
			a.Printf("Error creating new tab: %v", err)
			return
		}
		if err := chromedp.Run(taskCtx, chromedp.Navigate(decodedURL)); err != nil {
			a.Printf("Error navigating to %s: %v", decodedURL, err)
		}
		// We intentionally do not call the cancel function for taskCtx,
		// because we want the tab to remain open for the user.
	}()

	// We don't cancel the context here, allowing the tab to remain open.
	status := fmt.Sprintf("A new tab should now be opening with the URL: %s", decodedURL)
	result := map[string]any{"status": status}
	return a.CreateFunctionResponse(call, result, nil)
}

func (a *WebScraperAgent) handleDownloadWebFileTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	// 1. Parse arguments
	rawURL, urlOK := call.Args["url"].(string)
	if !urlOK || rawURL == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'url' argument is required and must be a non-empty string"))
	}
	filename, _ := call.Args["filename"].(string) // Optional

	// 2. Decode URL
	decodedURL, err := url.QueryUnescape(rawURL)
	if err != nil {
		a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
		decodedURL = rawURL
	}

	// 3. Fetch content
	resp, err := http.Get(decodedURL)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to fetch URL %s: %w", decodedURL, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to fetch URL %s: status code %d", decodedURL, resp.StatusCode))
	}

	// 4. Determine filename
	if filename == "" {
		// If no filename is provided, get it from the URL path.
		parsedURL, err := url.Parse(decodedURL)
		if err != nil {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to parse URL for filename: %w", err))
		}
		filename = filepath.Base(parsedURL.Path)
		if filename == "." || filename == "/" {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("could not determine filename from URL: %s", decodedURL))
		}
	}

	// 5. Get safe path and save file
	safePath, err := config.GetSafePath(filename)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, err)
	}

	file, err := os.Create(safePath)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to create file %s: %w", safePath, err))
	}
	defer file.Close()

	bytesCopied, err := io.Copy(file, resp.Body)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to write to file %s: %w", safePath, err))
	}

	// 6. Return response
	a.Printf("Successfully downloaded %d bytes to %s", bytesCopied, safePath)
	result := map[string]any{
		"status":      "file downloaded successfully",
		"path":        filename, // Return relative path
		"bytes_saved": bytesCopied,
	}
	return a.CreateFunctionResponse(call, result, nil)
}
