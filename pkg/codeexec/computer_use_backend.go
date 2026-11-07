package codeexec

import (
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// UIElement is one interactable element of a capture, bounds are x y w h in logical pixels
type UIElement struct {
	Index        int
	Role         string
	Label        string
	Bounds       [4]int
	App          string
	PID          int
	WindowID     int
	Attributes   map[string]any
	ElementToken *string
}

// CaptureResult carries the screenshot, the element mapping or both depending on the capture mode
type CaptureResult struct {
	Mode          string
	Width         int
	Height        int
	PNGB64        *string
	Elements      []UIElement
	App           string
	WindowTitle   string
	PNGBytesLen   int
	ImageMimeType *string
	Note          string
	AXMaxElements int
}

// ActionResult carries transport success plus the structured driver verdict of one action
type ActionResult struct {
	OK           bool
	Action       string
	Message      string
	Capture      *CaptureResult
	Meta         *pyjson.Object
	Verified     *bool
	Effect       *string
	Escalation   *pyjson.Object
	Path         *string
	Degraded     *bool
	DeliveryMode *string
	Code         *string
}

// ClickArgs are the keyword arguments of a click, Button and ClickCount carry the caller's values
type ClickArgs struct {
	Element      *int
	X, Y         *int
	Button       string
	ClickCount   int
	Modifiers    []string
	DeliveryMode *string
	BringToFront bool
}

// DragArgs are the keyword arguments of a drag between two elements or two points
type DragArgs struct {
	FromElement  *int
	ToElement    *int
	FromXY       *[2]int
	ToXY         *[2]int
	Button       string
	Modifiers    []string
	DeliveryMode *string
	BringToFront bool
}

// ScrollArgs are the keyword arguments of a scroll, Amount is wheel ticks
type ScrollArgs struct {
	Direction    string
	Amount       int
	Element      *int
	X, Y         *int
	Modifiers    []string
	DeliveryMode *string
	BringToFront bool
}

// DeliveryArgs are the delivery keyword arguments of type_text and key
type DeliveryArgs struct {
	DeliveryMode *string
	BringToFront bool
}

// ComputerUseBackend is the synchronous driver surface every computer use implementation provides
type ComputerUseBackend interface {
	Start() error
	Stop() error
	IsAvailable() (bool, error)
	Capture(mode string, app *string, pid, windowID *int) (*CaptureResult, error)
	Click(a ClickArgs) (*ActionResult, error)
	Drag(a DragArgs) (*ActionResult, error)
	Scroll(a ScrollArgs) (*ActionResult, error)
	TypeText(text string, d DeliveryArgs) (*ActionResult, error)
	Key(keys string, d DeliveryArgs) (*ActionResult, error)
	ListApps() ([]any, error)
	ListWindows() ([]*pyjson.Object, error)
	FocusApp(app string, raiseWindow bool) (*ActionResult, error)
	SetValue(value string, element *int) (*ActionResult, error)
	Wait(seconds float64) (*ActionResult, error)
}

// BackendDefaults supplies the non abstract list_windows and wait methods of the abstract backend
type BackendDefaults struct{}

// Reports no windows for backends that predate window discovery
func (BackendDefaults) ListWindows() ([]*pyjson.Object, error) { return []*pyjson.Object{}, nil }

// Sleeps the requested seconds clamped to zero through thirty and reports the wait
func (BackendDefaults) Wait(seconds float64) (*ActionResult, error) {
	time.Sleep(time.Duration(max(0.0, min(seconds, 30.0)) * float64(time.Second)))
	return &ActionResult{OK: true, Action: "wait", Message: fmt.Sprintf("waited %.2fs", seconds), Meta: pyjson.NewObject()}, nil
}

// ComputerUseProvider is the factory a computer use provider plugin registers for one backend name
type ComputerUseProvider interface {
	Name() string
	DisplayName() string
	GetSetupSchema() (map[string]any, error)
	CreateBackend(permissionMode string) (ComputerUseBackend, error)
	IsAvailable() (bool, error)
	Doctor() (code int, ok bool, err error)
}

// ProviderDefaults supplies the identity, picker row, availability and doctor defaults of a provider
type ProviderDefaults struct {
	ID    string
	Label string
}

// Stable config key value of the provider
func (p ProviderDefaults) Name() string { return p.ID }

// Human readable label, the name when no label is set
func (p ProviderDefaults) DisplayName() string {
	if p.Label != "" {
		return p.Label
	}
	return p.ID
}

// Picker row with the display name and no badge, tag or env vars
func (p ProviderDefaults) GetSetupSchema() (map[string]any, error) {
	return map[string]any{"name": p.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}, nil
}

// Usable on this host by default
func (ProviderDefaults) IsAvailable() (bool, error) { return true, nil }

// Backend ships no doctor by default
func (ProviderDefaults) Doctor() (int, bool, error) { return 0, false, nil }
