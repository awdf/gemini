package wayland

/*
#cgo LDFLAGS: -L/home/awdf/Workspace/cpp/wl-mouse-automation/builddir/ -lwl_automation -lwayland-client
#cgo CFLAGS: -I/home/awdf/Workspace/cpp/wl-mouse-automation/
#include <stdlib.h>
#include "wl_automation.h"

// Create a wrapper for the 'type' function to avoid a cgo name collision
// with the 'type' field in 'struct key'.
static inline void proxy_type(const char *str) {
	type(str);
}

*/
import "C"

import (
	"fmt"
	"image"
	"time"
	"unsafe"
)

// Valid for keyboard and joystick
const (
	BTN_PRESSED  = 1
	BTN_RELEASED = 0
	BTN_REPEATED = 2
)

// // Event types
const (
	JS_EVENT_BUTTON uint8 = 0x01
	JS_EVENT_AXIS   uint8 = 0x02
	JS_EVENT_INIT   uint8 = 0x80
)

// // XBOX 360 Event buttons numbers
const (
	JS_BUTTON_A           = 0
	JS_BUTTON_B           = 1
	JS_BUTTON_X           = 2
	JS_BUTTON_Y           = 3
	JS_BUTTON_LB          = 4
	JS_BUTTON_RB          = 5
	JS_BUTTON_BACK        = 6
	JS_BUTTON_START       = 7
	JS_BUTTON_GUIDE       = 8
	JS_BUTTON_LEFT_STICK  = 9
	JS_BUTTON_RIGHT_STICK = 10
	JS_BUTTON_DPAD_LEFT   = 11
	JS_BUTTON_DPAD_RIGHT  = 12
	JS_BUTTON_DPAD_UP     = 13
	JS_BUTTON_DPAD_DOWN   = 14
)

//// XBOX 360 Event axis numbers
/*
For left, right and pad axis:
 move right and top is negotive values
 move left and bottom is positive values
*/
const (
	JS_LEFT_AXIS_X  = 0
	JS_LEFT_AXIS_Y  = 1
	JS_RIGHT_AXIS_X = 3
	JS_RIGHT_AXIS_Y = 4
)

// PAD has axis as min/max values, cannot have intermediate values
const (
	JS_AXIS_DPAD_X = 6
	JS_AXIS_DPAD_Y = 7
)

// for triggers easy pushing starts from MIN 4 byte signed integer value -32767
// and full pressing ends with MAX 4 byte signed integer value 32767
const (
	JS_LEFT_TRIGGER  = 2
	JS_RIGHT_TRIGGER = 5
)

type JEvent struct {
	Time   uint32
	Value  int16
	Type   uint8
	Number uint8
}

func Init() error {
	if !C.init() {
		return fmt.Errorf("failed to initialize C library")
	}
	return nil
}

func Done() {
	C.done()
}

func SetOffset(value int) {
	C.setOffset(C.int(value))
}

func SetAccuracy(value int) {
	C.setAccuracy(C.int(value))
}

func MoveMouseRelative(dx, dy int) {
	C.mouseMoveRel(C.int(dx), C.int(dy))
}

func MoveMouseToPosition(x, y int) {
	C.mouseMoveAbs(C.int(x), C.int(y))
}

func MouseLeftClick(n int) {
	for i := 0; i < n; i++ {
		C.mouseLeft(C.int(BTN_PRESSED))
		time.Sleep(50 * time.Millisecond)
		C.mouseLeft(C.int(BTN_RELEASED))
		time.Sleep(50 * time.Millisecond)
	}
}

func MouseStep(dx, dy int) {
	C.mouseStep(C.int(dx), C.int(dy))
}

func MousePos(x, y *int) {
	var cx, cy C.int
	C.mousePos(&cx, &cy)

	*x = int(cx)
	*y = int(cy)
}

func TouchPadTap(x int, y int, pressure int) {
	C.touchpadTap(C.int(x), C.int(y), C.int(pressure))
}

func TouchPadRelease() {
	C.touchpadRelease()
}

// KeyEvent represents a keyboard event read from a device.
type KeyEvent struct {
	Type  int
	Code  int
	Value int
}

// ReadKey reads a single event from the physical keyboard. It is a blocking call
// and will wait until a key event is available.
func ReadKey() (*KeyEvent, error) {
	var cKey C.struct_key
	if C.kbdRead(&cKey) < 0 {
		return nil, fmt.Errorf("failed to read key event")
	}
	return &KeyEvent{
		Type:  int(cKey._type),
		Code:  int(cKey.code),
		Value: int(cKey.value),
	}, nil
}
func DisableJoystick() {
	C.disableJoystick()
}

func ReadJEvent() (*JEvent, error) {
	event := C.readJoystickEvent()
	if event == nil {
		return nil, fmt.Errorf("failed to read joystick event")
	}

	defer C.freeJoystickEvent(event)

	jsEvent := (*C.struct_js_event)(unsafe.Pointer(event))

	return &JEvent{
		Time:   uint32(jsEvent.time),
		Value:  int16(jsEvent.value),
		Type:   uint8(jsEvent._type),
		Number: uint8(jsEvent.number),
	}, nil
}

func JoystickVibrate(left, right, delay int) error {
	if C.vibrateJoystick(C.ushort(left), C.ushort(right), C.uint(delay)) == 0 {
		return fmt.Errorf("failed to vibrate joystick")
	}
	return nil
}

// Type sends a string of characters to be typed.
// Note: `type` is a keyword in Go, so the C function is accessed via `C._type`.
func Type(s string) {
	cStr := C.CString(s)
	defer C.free(unsafe.Pointer(cStr))
	C.proxy_type(cStr)
}

// KeyAction sends a key press, release, or repeat event for one or more keys simultaneously (e.g., for shortcuts like Ctrl+C).
// The key codes should be from the linux/input-event-codes.h header.
// The state should be one of BTN_PRESSED, BTN_RELEASED, or BTN_REPEATED.
func KeyAction(keyCodes []int, state int) {
	if len(keyCodes) == 0 {
		return
	}
	cKeyCodes := make([]C.int, len(keyCodes))
	for i, code := range keyCodes {
		cKeyCodes[i] = C.int(code)
	}
	C.keyAction(&cKeyCodes[0], C.int(len(keyCodes)), C.int(state))
}

// Calculate pointer move step based on event axis offset.
func CalcScaleOffset(scaleMax int, ev *JEvent) int {
	if ev.Value == 0 {
		return 0
	}
	if ev.Value > 0 {
		return int(float64(scaleMax) * float64(ev.Value) / 32767.0)
	}
	return int(float64(scaleMax) * float64(ev.Value) / 32768.0)
}

func CalcTrigerScaleOffset(scaleMax int, ev *JEvent) int {
	return scaleMax * (32767 + int(ev.Value)) / 65535
}

func GrabFrame() (image.Image, error) {
	// Works with active frame buffer device
	frame := C.mapScreen()
	defer C.unmapScreen(frame)

	if frame == nil {
		return nil, fmt.Errorf("failed to grab frame")
	}
	width := int(frame.width)
	height := int(frame.height)
	stride := int(frame.stride)
	data := C.GoBytes(unsafe.Pointer(frame.data), C.int(frame.size))
	img := &image.RGBA{
		Pix:    data,
		Stride: stride,
		Rect:   image.Rect(0, 0, width, height),
	}
	return img, nil
}
