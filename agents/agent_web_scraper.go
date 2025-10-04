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
	"sync"
	"time"

	"github.com/asaskevich/EventBus"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
)

const AgentWebScraperName = "webScraperAgent"

type Tabulate struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type WebScraperAgent struct {
	*Agent
	bus *EventBus.Bus
}

// userBrowser manages a single, persistent connection to a browser instance.
type userBrowser struct {
	context      context.Context
	cancel       context.CancelFunc
	isRemote     bool
	isConnected  bool
	wipe         bool
	selectedTabs map[target.ID]Tabulate
	mu           sync.Mutex
}

var UserBrowser = &userBrowser{}

func init() {
	RegisterFactory(AgentWebScraperName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) Callable {
		// This agent is redundant for Post AI with native URL understanding.
		if !config.C.PostAI {
			return NewWebScraperAgent(ctx, client, toolset, bus)
		}
		return nil
	})
}

// NewWebScraperAgent creates a specialized agent for scraping and analyzing web pages.
func NewWebScraperAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool, bus *EventBus.Bus) *WebScraperAgent {
	// This agent is designed for asynchronous, non-blocking operation in LiveAI mode,
	// which requires the event bus.
	if bus == nil {
		return nil
	}
	systemInstruction := `You are a web page analysis and interaction expert with vision capabilities.
Your goal is to extract as much meaningful information as possible from the provided web page URL, and to interact with pages as requested by the user.
Analyze both the text content and the visual layout/images on the page to generate a comprehensive and detailed report.
Describe important visual elements like images, charts, and the overall page structure in your analysis.

To do this, you have access to the following tools. Use them strategically:
- **analyseWebPage**: Use this for a fast, comprehensive text-based analysis of a webpage. It's good for summarizing content and understanding the page's purpose.
- **getRawHTML**: Use this when you need to inspect the raw source code of a page, for example, to find CSS files or specific meta tags.
- **getRenderedContent**: Use this for modern, JavaScript-heavy websites where content is loaded dynamically. It provides the final HTML after all scripts have run.
- **getRenderedScreenshot**: This is your most powerful tool for visual analysis. When a user asks about the **layout, style, colors, or visual appearance** of a page, you MUST use this tool to get a screenshot. This will allow you to "see" the page and answer questions about its design accurately.
- **downloadWebFile**: Use this to download a file from a URL directly into the workspace. It's like using the 'wget' command.
- **clickElement**: Use this to click on an element on the current page. You need to provide a CSS selector for the element.
- **fillFormField**: Use this to fill a form field. You need to provide a CSS selector for the field and the value to fill.
- **submitForm**: Use this to submit a form. You need to provide a CSS selector for the form.
- **getElementValue**: Use this to get the value of an element. You need to provide a CSS selector for the element.`

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

	openPageInTabFunc := genai.FunctionDeclaration{
		Name:        "openPageInTab",
		Description: "WEB BROWSER: Opens a new tab in the user's existing browser window and navigates to the specified URL.",
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

	openPageInBrowserFunc := genai.FunctionDeclaration{
		Name:        "openPageInBrowser",
		Description: "WEB BROWSER: Opens a new browser window and navigates to the specified URL. Use this when you need to start a fresh browsing session.",
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
		Behavior: genai.BehaviorBlocking,
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

	readActivePageFunc := genai.FunctionDeclaration{
		Name:        "readActiveTabPage",
		Description: "WEB BROWSER: Reads the title and main content (body HTML) of the currently active tab in the user's browser.",
		Parameters:  &genai.Schema{Type: genai.TypeObject}, // No parameters
		Behavior:    genai.BehaviorBlocking,
	}

	clickElementFunc := genai.FunctionDeclaration{
		Name:        "clickElement",
		Description: "WEB BROWSER: Clicks on an element on the current page matching the given CSS selector.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"selector": {
					Type:        genai.TypeString,
					Description: "The CSS selector of the element to click.",
				},
			},
			Required: []string{"selector"},
		},
		Behavior: genai.BehaviorBlocking,
	}

	fillFormFieldFunc := genai.FunctionDeclaration{
		Name:        "fillFormField",
		Description: "WEB BROWSER: Fills a form field on the current page matching the given CSS selector with the provided value.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"selector": {
					Type:        genai.TypeString,
					Description: "The CSS selector of the form field to fill.",
				},
				"value": {
					Type:        genai.TypeString,
					Description: "The value to fill the form field with.",
				},
			},
			Required: []string{"selector", "value"},
		},
		Behavior: genai.BehaviorBlocking,
	}

	submitFormFunc := genai.FunctionDeclaration{
		Name:        "submitForm",
		Description: "WEB BROWSER: Submits a form on the current page matching the given CSS selector.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"selector": {
					Type:        genai.TypeString,
					Description: "The CSS selector of the form to submit.",
				},
			},
			Required: []string{"selector"},
		},
		Behavior: genai.BehaviorBlocking,
	}

	getElementValueFunc := genai.FunctionDeclaration{
		Name:        "getElementValue",
		Description: "WEB BROWSER: Gets the value of an element on the current page matching the given CSS selector.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"selector": {
					Type:        genai.TypeString,
					Description: "The CSS selector of the element to get the value from.",
				},
			},
			Required: []string{"selector"},
		},
		Behavior: genai.BehaviorBlocking,
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations,
		&analyseFunc, &getRawHTMLFunc, &getRenderedContentFunc,
		&getRenderedScreenshotFunc, &openPageInTabFunc, &downloadWebFileFunc,
		&readActivePageFunc, &openPageInBrowserFunc, &clickElementFunc, &fillFormFieldFunc, &submitFormFunc, &getElementValueFunc,
	)

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
	case "openPageInTab":
		return a.handleOpenPageInTabTool(call)
	case "openPageInBrowser":
		return a.handleOpenPageInBrowserTool(call)
	case "readActiveTabPage":
		return a.handleReadActiveTabPage(call)
	case "clickElement":
		return a.handleClickElementTool(call)
	case "fillFormField":
		return a.handleFillFormFieldTool(call)
	case "submitForm":
		return a.handleSubmitFormTool(call)
	case "getElementValue":
		return a.handleGetElementValueTool(call)
	default:
		return a.Agent.Handle(call)
	}
}

func (a *WebScraperAgent) handleGetElementValueTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	selector, ok := call.Args["selector"].(string)
	if !ok || selector == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'selector' argument is required and must be a non-empty string"))
	}

	UserBrowser.Connect()
	if !UserBrowser.isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("getting element value requires a running browser with remote debugging enabled"))
	}

	tabs, err := UserBrowser.Tabs()
	if err != nil || len(tabs) == 0 {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("could not get active tab to get element value: %w", err))
	}

	activeTabID := tabs[0].TargetID
	taskCtx := UserBrowser.SelectTab(activeTabID)
	taskCtx, cancel := context.WithTimeout(taskCtx, 1*time.Second)
	defer cancel()

	value, err := UserBrowser.GetValue(taskCtx, selector)
	if err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to get value from element with selector '%s': %w", selector, err))
	}

	a.Printf("Successfully got value from element with selector: '%s'", selector)
	result := map[string]any{"value": value}

	return a.CreateFunctionResponse(call, result, nil)
}

func (a *WebScraperAgent) handleSubmitFormTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	selector, ok := call.Args["selector"].(string)
	if !ok || selector == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'selector' argument is required and must be a non-empty string"))
	}

	UserBrowser.Connect()
	if !UserBrowser.isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("submitting forms requires a running browser with remote debugging enabled"))
	}

	tabs, err := UserBrowser.Tabs()
	if err != nil || len(tabs) == 0 {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("could not get active tab to submit form: %w", err))
	}

	activeTabID := tabs[0].TargetID
	taskCtx := UserBrowser.SelectTab(activeTabID)
	taskCtx, cancel := context.WithTimeout(taskCtx, 1*time.Second)
	defer cancel()

	if err := UserBrowser.Submit(taskCtx, selector); err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to submit form with selector '%s': %w", selector, err))
	}

	a.Printf("Successfully submitted form with selector: '%s'", selector)
	result := map[string]any{"status": "form submitted successfully"}

	return a.CreateFunctionResponse(call, result, nil)
}

func (a *WebScraperAgent) handleFillFormFieldTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	selector, ok := call.Args["selector"].(string)
	if !ok || selector == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'selector' argument is required and must be a non-empty string"))
	}
	value, ok := call.Args["value"].(string)
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'value' argument is required"))
	}

	UserBrowser.Connect()
	if !UserBrowser.isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("filling form fields requires a running browser with remote debugging enabled"))
	}

	tabs, err := UserBrowser.Tabs()
	if err != nil || len(tabs) == 0 {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("could not get active tab to fill form field: %w", err))
	}

	activeTabID := tabs[0].TargetID
	taskCtx := UserBrowser.SelectTab(activeTabID)
	taskCtx, cancel := context.WithTimeout(taskCtx, 1*time.Second)
	defer cancel()

	if err := UserBrowser.FillFormField(taskCtx, selector, value); err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to fill form field with selector '%s': %w", selector, err))
	}

	a.Printf("Successfully filled form field with selector: '%s'", selector)
	result := map[string]any{"status": "form field filled successfully"}

	return a.CreateFunctionResponse(call, result, nil)
}

func (a *WebScraperAgent) handleClickElementTool(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	selector, ok := call.Args["selector"].(string)
	if !ok || selector == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("'selector' argument is required and must be a non-empty string"))
	}

	UserBrowser.Connect()
	if !UserBrowser.isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("clicking elements requires a running browser with remote debugging enabled"))
	}

	tabs, err := UserBrowser.Tabs()
	if err != nil || len(tabs) == 0 {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("could not get active tab to click element: %w", err))
	}

	activeTabID := tabs[0].TargetID
	taskCtx := UserBrowser.SelectTab(activeTabID)
	taskCtx, cancel := context.WithTimeout(taskCtx, 1*time.Second)
	defer cancel()

	if err := UserBrowser.ClickElement(taskCtx, selector); err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to click element with selector '%s': %w", selector, err))
	}

	a.Printf("Successfully clicked element with selector: '%s'", selector)
	result := map[string]any{"status": "element clicked successfully"}

	return a.CreateFunctionResponse(call, result, nil)
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

		UserBrowser.Connect()

		if rawURL != "" {
			decodedURL, err := url.QueryUnescape(rawURL)
			if err != nil {
				a.Printf("WARNING: could not decode web page URL '%s', using it as is. Error: %v", rawURL, err)
				decodedURL = rawURL
			}
			a.Printf("Starting background rendering for web page URL: %s", decodedURL)

			newTabID, err := UserBrowser.CreateTab(decodedURL)
			if err != nil {
				processErr = fmt.Errorf("failed to create new tab: %w", err)
			} else {
				defer UserBrowser.DropTab(newTabID)
				taskCtx := UserBrowser.SelectTab(newTabID)
				processErr = chromedp.Run(taskCtx,
					chromedp.Sleep(2*time.Second), // Wait for JS to execute.
					chromedp.OuterHTML("html", &htmlContent),
				)
			}
		} else {
			if UserBrowser.isRemote {
				a.Printf("Starting background rendering for current active tab.")
				tabs, err := UserBrowser.Tabs()
				if err != nil || len(tabs) == 0 {
					processErr = fmt.Errorf("could not get active tab: %w", err)
				} else {
					activeTabCtx := UserBrowser.SelectTab(tabs[0].TargetID)
					processErr = chromedp.Run(activeTabCtx, chromedp.OuterHTML("html", &htmlContent))
				}
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
		UserBrowser.Connect()

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

			newTabID, err := UserBrowser.CreateTab(decodedURL)
			if err != nil {
				processErr = fmt.Errorf("failed to create new tab for screenshot: %w", err)
			} else {
				defer UserBrowser.DropTab(newTabID)
				screenshotCtx := UserBrowser.SelectTab(newTabID)
				processErr = chromedp.Run(screenshotCtx,
					chromedp.WaitVisible(`body`, chromedp.ByQuery),
					chromedp.Sleep(2*time.Second),
					chromedp.FullScreenshot(&screenshotBuf, 0),
				)
			}
		} else {
			if UserBrowser.isRemote {
				// --- Behavior without URL (Remote): Screenshot the currently active tab ---
				pageDescription = "your current screen"
				a.Printf("Starting background screenshot capture of the current active tab.")
				tabs, err := UserBrowser.Tabs()
				if err != nil || len(tabs) == 0 {
					processErr = fmt.Errorf("could not get active tab: %w", err)
				} else {
					activeTabCtx := UserBrowser.SelectTab(tabs[0].TargetID)
					processErr = chromedp.Run(activeTabCtx,
						chromedp.FullScreenshot(&screenshotBuf, 0),
					)
				}
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

func (a *WebScraperAgent) handleOpenPageInTabTool(call *genai.FunctionCall) *genai.FunctionResponse {
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

	UserBrowser.Connect()

	if !UserBrowser.isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("the 'openPageInTab' tool requires a running browser with remote debugging enabled"))
	}

	// Run the navigation in a goroutine so it doesn't block the main thread.
	go func() {
		if _, err := UserBrowser.CreateTab(decodedURL); err != nil {
			a.Printf("Error creating new tab: %v", err)
		}
	}()

	// We don't cancel the context here, allowing the tab to remain open.
	status := fmt.Sprintf("A new tab should now be opening with the URL: %s", decodedURL)
	result := map[string]any{"status": status}
	return a.CreateFunctionResponse(call, result, nil)
}

func (a *WebScraperAgent) handleOpenPageInBrowserTool(call *genai.FunctionCall) *genai.FunctionResponse {
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

	if err := UserBrowser.OpenBrowser(decodedURL); err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to open new browser window: %w", err))
	}

	status := fmt.Sprintf("A new browser window should now be opening with the URL: %s", decodedURL)
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

func (a *WebScraperAgent) handleReadActiveTabPage(call *genai.FunctionCall) *genai.FunctionResponse {
	a.Printf(PrintTemplate, call.Name, call.Args)

	UserBrowser.Connect()
	if !UserBrowser.isRemote {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("reading the active tab requires a running browser with remote debugging enabled"))
	}

	tabs, err := UserBrowser.Tabs()
	if err != nil || len(tabs) == 0 {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("could not get active tab to read page: %w", err))
	}

	// The first tab in the list is typically the active one.
	activeTabID := tabs[0].TargetID
	taskCtx := UserBrowser.SelectTab(activeTabID)

	var title, content string
	if err := chromedp.Run(taskCtx,
		chromedp.Title(&title),
		chromedp.OuterHTML("body", &content),
	); err != nil {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("failed to get active tab title and content: %w", err))
	}

	a.Printf("Successfully read active tab title: '%s' and content.", title)
	result := map[string]any{"title": title, "html_content": content}

	return a.CreateFunctionResponse(call, result, nil)
}

// ========================== WEB BROWSER IMPLEMENTATION ================================================================
// http://localhost:9222/json/version

// User requested wipe on close
func (b *userBrowser) Wipe() {
	b.wipe = true
}

// isRemoteRunning checks if a browser is listening on the remote debugging port.
func (b *userBrowser) isRemoteRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:9222", 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Connect establishes the connection to the browser. It's designed to be called once.
func (b *userBrowser) Connect() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.isConnected {
		return
	}

	// 1. Try to connect to the remote debugging port.
	if b.isRemoteRunning() {
		// Connection successful, use the remote browser.
		log.Println("Remote browser detected. Creating allocator...")
		b.context, b.cancel = chromedp.NewRemoteAllocator(context.Background(), "http://localhost:9222")
		b.isRemote = true
	} else {
		// 2. If connection failed, fall back to a new headless browser.
		log.Println("Remote browser not detected. Launching new headless browser.")
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("no-sandbox", true),
		)
		b.context, b.cancel = chromedp.NewExecAllocator(context.Background(), opts...)
		b.isRemote = false
	}

	b.isConnected = true
	b.selectedTabs = make(map[target.ID]Tabulate)
}

func (b *userBrowser) OpenBrowser(url string) error {
	// Check for already opened browser
	if b.isRemoteRunning() {
		// Check connection status before acquiring the main lock.
		b.Connect() // do nothing if connected.
		log.Println("Browser is already connected, opening a new tab.")
		_, err := b.CreateTab(url)
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// The user-data-dir can cause startup errors if a SingletonLock file is left
	// over from a previous crashed instance. We remove it to ensure a clean start.
	userDataDir := "/home/awdf/.config/google-chrome-debug"
	if err := os.Remove(filepath.Join(userDataDir, "SingletonLock")); err != nil && !os.IsNotExist(err) {
		log.Printf("WARNING: could not remove stale SingletonLock: %v", err)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", false),
		chromedp.Flag("start-maximized", true),
		chromedp.Flag("remote-debugging-port", "9222"),
		chromedp.Flag("user-data-dir", userDataDir),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)

	// Create a new context for the browser instance itself.
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)

	// Launch the browser and navigate to the initial URL.
	if err := chromedp.Run(taskCtx, chromedp.Navigate(url)); err != nil {
		cancelTask()
		cancelAlloc()
		return fmt.Errorf("failed to launch browser and navigate: %w", err)
	}

	// Now that the browser is running, set the state.
	b.context = taskCtx
	b.cancel = cancelTask
	b.isConnected = true
	b.isRemote = true // This mode effectively makes it a remote-controllable browser.
	b.selectedTabs = make(map[target.ID]Tabulate)

	log.Printf("Successfully opened a new browser window and navigated to %s.", url)
	return nil
}

// Close terminates the browser connection.
func (b *userBrowser) Close() {
	if !b.isConnected {
		return
	}

	if b.wipe && len(b.selectedTabs) > 0 {
		// Cancel all remaining selected tab contexts
		for id := range b.selectedTabs {
			b.DropTab(id)
		}
	}

	// Close browser context
	if b.cancel != nil {
		b.cancel()
	}

	// Reset the state completely to prevent using a stale context on the next run.
	b.isConnected = false
	b.context = nil
	b.cancel = nil
	b.selectedTabs = nil
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
		if ti.Type == "page" {
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

	// If a context for this tab doesn't exist, create and store it.
	if _, ok := b.selectedTabs[tabID]; !ok {
		taskCtx, cancel := chromedp.NewContext(b.context, chromedp.WithTargetID(tabID))
		b.selectedTabs[tabID] = Tabulate{ctx: taskCtx, cancel: cancel}
	}

	return b.selectedTabs[tabID].ctx
}

// CreateTab creates a new tab, registers it, and returns its context and ID.
func (b *userBrowser) CreateTab(url string) (target.ID, error) {
	// If the browser isn't connected, open it. OpenBrowser will create the first tab.
	if !b.IsConnected() {
		err := b.OpenBrowser(url)
		return "", err // OpenBrowser doesn't return a tab ID, so we return an empty one.
	}

	tabs, err := b.Tabs()
	if err != nil {
		return "", err
	}

	taskCtx, _ := chromedp.NewContext(b.context, chromedp.WithTargetID(tabs[0].TargetID))

	// Create dirty tab with broken context
	var dirtyID target.ID
	err = chromedp.Run(taskCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			// We get dirty ID not able to proceed by chromedp
			dirtyID, err = target.CreateTarget(url).WithForTab(true).Do(ctx)
			return err
		}),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create new tab: %w", err)
	}
	if dirtyID == "" {
		return "", fmt.Errorf("create new tab returned empty target ID")
	}

	// Create a set of old tab IDs for efficient lookup.
	oldTabIDs := make(map[target.ID]struct{}, len(tabs))
	for _, tab := range tabs {
		oldTabIDs[tab.TargetID] = struct{}{}
	}

	// Get updated tabs with valid IDs
	newTabs, err := b.Tabs()
	if err != nil {
		return "", err
	}

	var newlyCreatedTab *target.Info
	// Find the new tab valid ID in the updated list.
	for _, tab := range newTabs {
		if _, exists := oldTabIDs[tab.TargetID]; !exists {
			newlyCreatedTab = tab
			break // Found it
		}
	}

	if newlyCreatedTab == nil {
		return "", fmt.Errorf("could not find newly created tab in the browser's tab list")
	}

	// Introduce a small delay to ensure the browser has fully processed the
	// new tab creation and is ready to receive commands for it.
	time.Sleep(500 * time.Millisecond)

	// Create and store the definitive context for the new tab.
	b.SelectTab(newlyCreatedTab.TargetID)
	return newlyCreatedTab.TargetID, nil
}

// DropTab closes a specific tab by its ID.
func (b *userBrowser) DropTab(tabID target.ID) {
	if !b.isConnected {
		return
	}

	// Normal close, not robust, but properly clear context
	b.mu.Lock()
	defer b.mu.Unlock()

	if tab, ok := b.selectedTabs[tabID]; ok {
		tab.cancel()
		delete(b.selectedTabs, tabID)
	}

	// Check if tab is closed, as cancel() fails on some pages
	tabIDs, err := b.Tabs()
	if err == nil {
		for tab := range tabIDs {
			if tabIDs[tab].TargetID == tabID {
				return
			}
		}
	}

	// Emergency close, max robust but error prone.
	// To close a target, we need a context. We can use the main allocator context
	// to create a temporary one just for this action.
	taskCtx, cancel := chromedp.NewContext(b.context, chromedp.WithTargetID(tabID))
	defer cancel()

	err = chromedp.Run(taskCtx, chromedp.ActionFunc(
		func(ctx context.Context) error {
			return target.CloseTarget(tabID).Do(ctx)
		},
	))
	if err != nil {
		config.DebugPrintf("failed to close tab with ID %s: %v", tabID, err)
	}
}

// GetValue retrieves the value of an element matching the selector.
func (b *userBrowser) GetValue(ctx context.Context, selector string) (string, error) {
	var value string
	if err := chromedp.Run(ctx, chromedp.Value(selector, &value, chromedp.ByQuery)); err != nil {
		return "", err
	}
	return value, nil
}

// Submit triggers a submit action on a form element.
func (b *userBrowser) Submit(ctx context.Context, selector string) error {
	return chromedp.Run(ctx, chromedp.Submit(selector, chromedp.ByQuery))
}

// FillFormField enters text into a form field.
func (b *userBrowser) FillFormField(ctx context.Context, selector, value string) error {
	return chromedp.Run(ctx, chromedp.SendKeys(selector, value, chromedp.ByQuery))
}

// ClickElement simulates a mouse click on an element.
func (b *userBrowser) ClickElement(ctx context.Context, selector string) error {
	return chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery))
}
