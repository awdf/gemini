package agents

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/genai"

	"gemini/config"
	"gemini/helpers"
	"gemini/wayland"
)

func init() {
	RegisterFactory(AgentDesktopName, func(ctx context.Context, client *genai.Client, toolset *genai.Tool) Callable {
		return NewDesktopAgent(ctx, client, toolset)
	})
}

type DesktopAgent struct {
	*Agent
}

const maxKeyCode = 127 // The highest known key code is 126 (KEY_RIGHTMETA), so the array size is 127.

// keyCodeToName provides a constant-size array for a fast, index-based lookup of key codes.
// It uses an array literal with explicit keys to correctly map non-contiguous key codes
// from linux/input-event-codes.h to their string names. This is both efficient and readable.
var keyCodeToName = [maxKeyCode]string{
	1:   "KEY_ESC",
	2:   "KEY_1",
	3:   "KEY_2",
	4:   "KEY_3",
	5:   "KEY_4",
	6:   "KEY_5",
	7:   "KEY_6",
	8:   "KEY_7",
	9:   "KEY_8",
	10:  "KEY_9",
	11:  "KEY_0",
	12:  "KEY_MINUS",
	13:  "KEY_EQUAL",
	14:  "KEY_BACKSPACE",
	15:  "KEY_TAB",
	16:  "KEY_Q",
	17:  "KEY_W",
	18:  "KEY_E",
	19:  "KEY_R",
	20:  "KEY_T",
	21:  "KEY_Y",
	22:  "KEY_U",
	23:  "KEY_I",
	24:  "KEY_O",
	25:  "KEY_P",
	26:  "KEY_LEFTBRACE",
	27:  "KEY_RIGHTBRACE",
	28:  "KEY_ENTER",
	29:  "KEY_LEFTCTRL",
	30:  "KEY_A",
	31:  "KEY_S",
	32:  "KEY_D",
	33:  "KEY_F",
	34:  "KEY_G",
	35:  "KEY_H",
	36:  "KEY_J",
	37:  "KEY_K",
	38:  "KEY_L",
	39:  "KEY_SEMICOLON",
	40:  "KEY_APOSTROPHE",
	41:  "KEY_GRAVE",
	42:  "KEY_LEFTSHIFT",
	43:  "KEY_BACKSLASH",
	44:  "KEY_Z",
	45:  "KEY_X",
	46:  "KEY_C",
	47:  "KEY_V",
	48:  "KEY_B",
	49:  "KEY_N",
	50:  "KEY_M",
	51:  "KEY_COMMA",
	52:  "KEY_DOT",
	53:  "KEY_SLASH",
	54:  "KEY_RIGHTSHIFT",
	55:  "KEY_KPASTERISK",
	56:  "KEY_LEFTALT",
	57:  "KEY_SPACE",
	58:  "KEY_CAPSLOCK",
	59:  "KEY_F1",
	60:  "KEY_F2",
	61:  "KEY_F3",
	62:  "KEY_F4",
	63:  "KEY_F5",
	64:  "KEY_F6",
	65:  "KEY_F7",
	66:  "KEY_F8",
	67:  "KEY_F9",
	68:  "KEY_F10",
	69:  "KEY_NUMLOCK",
	70:  "KEY_SCROLLLOCK",
	71:  "KEY_KP7",
	72:  "KEY_KP8",
	73:  "KEY_KP9",
	74:  "KEY_KPMINUS",
	75:  "KEY_KP4",
	76:  "KEY_KP5",
	77:  "KEY_KP6",
	78:  "KEY_KPPLUS",
	79:  "KEY_KP1",
	80:  "KEY_KP2",
	81:  "KEY_KP3",
	82:  "KEY_KP0",
	83:  "KEY_KPDOT",
	87:  "KEY_F11",
	88:  "KEY_F12",
	96:  "KEY_KPENTER",
	97:  "KEY_RIGHTCTRL",
	98:  "KEY_KPSLASH",
	99:  "KEY_SYSRQ",
	100: "KEY_RIGHTALT",
	102: "KEY_HOME",
	103: "KEY_UP",
	104: "KEY_PAGEUP",
	105: "KEY_LEFT",
	106: "KEY_RIGHT",
	107: "KEY_END",
	108: "KEY_DOWN",
	109: "KEY_PAGEDOWN",
	110: "KEY_INSERT",
	111: "KEY_DELETE",
	125: "KEY_LEFTMETA",
	126: "KEY_RIGHTMETA",
}

// NewDesktopAgent creates a specialized agent for desktop automation tasks.
func NewDesktopAgent(ctx context.Context, client *genai.Client, toolset *genai.Tool) *DesktopAgent {
	systemInstruction := `You are a desktop automation expert. You can control the keyboard to type text and perform key combinations.`

	functions := []*genai.FunctionDeclaration{
		{
			Name:        "typeText",
			Description: "DESKTOP AUTOMATION: Types the given string of text using the virtual keyboard. Use this for interacting with the user interface, like filling out forms or typing commands. Each line MUST be finished with a newline character. Characters Tab and Backspace are supported.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"text": {Type: genai.TypeString, Description: "The text to be typed."},
				},
				Required: []string{"text"},
			},
			Behavior: genai.BehaviorBlocking,
		},
		{
			Name:        "keyAction",
			Description: "DESKTOP AUTOMATION: Simulates a key press (press and release) for one or more keys simultaneously (e.g., for shortcuts like Ctrl+C). Use this for keyboard shortcuts to control applications. Key codes should be from the linux/input-event-codes.h header.",
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"key_codes": {
						Type:        genai.TypeArray,
						Description: "A list of integer key codes to be pressed simultaneously.",
						Items:       &genai.Schema{Type: genai.TypeInteger},
					},
				},
				Required: []string{"key_codes"},
			},
			Behavior: genai.BehaviorBlocking,
		},
	}

	toolset.FunctionDeclarations = append(toolset.FunctionDeclarations, functions...)

	agentConfig := AgentConfig{
		Name:              AgentDesktopName,
		Model:             config.C.AI.Model,
		SystemInstruction: systemInstruction,
		Temperature:       helpers.Ptr(float32(0.0)),
	}
	baseAgent := NewAgent(ctx, client, agentConfig)

	desktopAgent := &DesktopAgent{Agent: baseAgent}
	return desktopAgent
}

func (a *DesktopAgent) WarmUp() {
	a.Agent.WarmUp()
}

func (a *DesktopAgent) Handle(call *genai.FunctionCall) *genai.FunctionResponse {
	switch call.Name {
	case "typeText":
		return a.handleTypeTextTool(call)
	case "keyAction":
		return a.handleKeyActionTool(call)
	default:
		return nil
	}
}

func (a *DesktopAgent) handleTypeTextTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	text, ok := call.Args["text"].(string)
	if !ok || text == "" {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("argument 'text' is required and must be a non-empty string"))
	}
	wayland.Type(text)
	result := map[string]any{"status": "text typed successfully"}
	return a.CreateFunctionResponse(call, result, nil)
}

func (a *DesktopAgent) handleKeyActionTool(call *genai.FunctionCall) *genai.FunctionResponse {
	log.Printf("Executing tool call: %s with args: %v", call.Name, call.Args)
	codesArg, ok := call.Args["key_codes"].([]interface{})
	if !ok {
		return a.CreateFunctionResponse(call, nil, fmt.Errorf("argument 'key_codes' (array of integers) is required"))
	}

	var keyCodes []int
	for _, v := range codesArg {
		if code, ok := v.(float64); ok {
			keyCodes = append(keyCodes, int(code))
		} else {
			return a.CreateFunctionResponse(call, nil, fmt.Errorf("invalid item in 'key_codes' array; all must be integers"))
		}
	}

	wayland.KeyAction(keyCodes, wayland.BTN_PRESSED)
	time.Sleep(50 * time.Millisecond)
	wayland.KeyAction(keyCodes, wayland.BTN_RELEASED)

	keyNames := make([]string, len(keyCodes))
	for i, code := range keyCodes {
		if code >= 0 && code < len(keyCodeToName) && keyCodeToName[code] != "" {
			keyNames[i] = keyCodeToName[code]
		} else {
			keyNames[i] = fmt.Sprintf("CODE(%d)", code)
		}
	}
	keyNamesStr := strings.Join(keyNames, " + ")
	log.Printf("Performing key press for: %s", keyNamesStr)

	result := map[string]any{"status": fmt.Sprintf("key action for %s performed", keyNamesStr)}
	return a.CreateFunctionResponse(call, result, nil)
}
