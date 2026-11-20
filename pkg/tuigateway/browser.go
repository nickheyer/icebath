package tuigateway

type BrowserAction string

func (BrowserAction) Values() []string { return []string{"status", "connect", "disconnect"} }

// Browser action, the CDP url a connect attaches to and the session its progress lines go to
type BrowserManageParams struct {
	Action    BrowserAction `json:"action,omitzero"`
	URL       *string       `json:"url,omitzero"`
	SessionID string        `json:"session_id,omitzero"`
	Profile   *string       `json:"profile,omitzero"`
}

// Whether the tools reach a browser, its CDP url and the lines a connect narrated
type BrowserManageResult struct {
	Connected bool     `json:"connected"`
	URL       string   `json:"url,omitzero"`
	Messages  []string `json:"messages,omitzero"`
}

type BrowserProgressLevel string

func (BrowserProgressLevel) Values() []string { return []string{"info", "warn", "error"} }

// One line of a browser connect or launch
type BrowserProgressPayload struct {
	Message string               `json:"message"`
	Level   BrowserProgressLevel `json:"level"`
}

// Inspects, attaches to or drops the CDP browser the tools use
var BrowserManage = NewMethod[BrowserManageParams, BrowserManageResult]("browser.manage")

// Progress line of a browser connect
var BrowserProgressEvent = NewEvent[BrowserProgressPayload]("browser.progress")
