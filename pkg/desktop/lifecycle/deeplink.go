package lifecycle

import (
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Protocol and Schemes return the registered deep link scheme and the accepted inbound schemes.
func Protocol(devServer bool) string {
	if devServer {
		return "icebath-dev"
	}
	return "icebath"
}

// Schemes lists the inbound schemes, the icebath names first and the hermes names beside them
func Schemes(devServer bool) []string {
	if devServer {
		return []string{"icebath-dev", "hermes-dev", "icebath", "hermes"}
	}
	return []string{"icebath", "hermes"}
}

// DeepLink is the hermes:deep-link payload.
type DeepLink struct {
	Kind   string            `json:"kind"`
	Name   string            `json:"name"`
	Params map[string]string `json:"params"`
}

// MainWindow is what deep link delivery does to the main window.
type MainWindow interface {
	IsDestroyed() bool
	IsMinimized() bool
	IsVisible() bool
	IsFullScreen() bool
	IsFocused() bool
	Restore()
	Show()
	ShowInactive()
	Focus()
	SetFullScreen(bool)
	Activate()
	Send(event string, payload any)
}

// DeepLinks routes inbound deep links and update requests to the renderer once it is ready.
type DeepLinks struct {
	mu            sync.Mutex
	schemes       []string
	protocol      string
	main          func() MainWindow
	log           func(string)
	ready         bool
	pending       *DeepLink
	pendingUpdate bool
}

// NewDeepLinks routes links of schemes to the window main returns
func NewDeepLinks(devServer bool, main func() MainWindow, log func(string)) *DeepLinks {
	return &DeepLinks{schemes: Schemes(devServer), protocol: Protocol(devServer), main: main, log: log}
}

// ExtractDeepLink returns the first argv entry starting with an accepted scheme
func (d *DeepLinks) ExtractDeepLink(argv []string) string {
	for _, a := range argv {
		for _, s := range d.schemes {
			if strings.HasPrefix(a, s+"://") {
				return a
			}
		}
	}
	return ""
}

// HasClosePreviewFlag reports the --close-preview relaunch flag
func HasClosePreviewFlag(argv []string) bool { return slices.Contains(argv, "--close-preview") }

// Handle parses raw and delivers it, queueing until the renderer signals readiness
func (d *DeepLinks) Handle(raw string) {
	if raw == "" {
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		d.log("[deeplink] ignoring malformed url: " + raw)
		return
	}
	scheme := strings.ToLower(parsed.Scheme)
	if !slices.Contains(d.schemes, scheme) {
		d.log("[deeplink] ignoring scheme " + scheme + " (expected " + strings.Join(d.schemes, " or ") + ")")
		return
	}
	name, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil {
		d.log("[deeplink] ignoring malformed url: " + raw)
		return
	}
	payload := DeepLink{Kind: parsed.Hostname(), Name: name, Params: map[string]string{}}
	for k, vs := range parsed.Query() {
		payload.Params[k] = vs[len(vs)-1]
	}
	if payload.Kind == "copilot-key" && payload.Name != "start" {
		d.log("[deeplink] ignoring copilot-key path: " + payload.Name)
		return
	}
	main := d.main()
	if payload.Kind == "close-preview" {
		if main != nil && !main.IsDestroyed() {
			if main.IsMinimized() {
				main.Restore()
			}
			if !main.IsVisible() {
				main.Show()
			}
			main.Focus()
			if main.IsFullScreen() {
				main.SetFullScreen(false)
			}
			main.Send("close-preview-requested", nil)
		}
		return
	}
	d.mu.Lock()
	if !d.ready || main == nil || main.IsDestroyed() {
		d.pending = &payload
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	main.Activate()
	main.Send("deep-link", payload)
	d.log("[deeplink] delivered " + payload.Kind + "/" + payload.Name)
}

// OpenUpdates asks the renderer to open the updates view, queued until it is ready
func (d *DeepLinks) OpenUpdates() {
	main := d.main()
	d.mu.Lock()
	if !d.ready || main == nil || main.IsDestroyed() {
		d.pendingUpdate = true
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	main.Send("open-updates", nil)
	if !main.IsVisible() {
		main.ShowInactive()
	}
	if !main.IsFocused() {
		main.Focus()
	}
}

// ReadyResult is the deep-link-ready answer.
type ReadyResult struct {
	OK bool `json:"ok"`
}

// Ready marks the renderer ready and flushes a queued update request and deep link
func (d *DeepLinks) Ready() ReadyResult {
	d.mu.Lock()
	d.ready = true
	update := d.pendingUpdate
	d.pendingUpdate = false
	queued := d.pending
	d.pending = nil
	d.mu.Unlock()
	if update {
		d.OpenUpdates()
	}
	if queued != nil {
		u := d.protocol + "://" + queued.Kind + "/" + url.PathEscape(queued.Name)
		if len(queued.Params) > 0 {
			q := url.Values{}
			for k, v := range queued.Params {
				q.Set(k, v)
			}
			u += "?" + q.Encode()
		}
		d.Handle(u)
	}
	return ReadyResult{OK: true}
}

// ProtocolRegistrar registers the app as the handler of a URL scheme.
type ProtocolRegistrar interface {
	SetAsDefaultProtocolClient(protocol string, execPath string, args []string) error
}

// RegisterDeepLinkProtocol registers protocol unless the skip variable is set, passing the entry script in dev runs
func RegisterDeepLinkProtocol(r ProtocolRegistrar, protocol string, env func(string) string, defaultApp bool, argv []string, execPath string, resolve func(string) string, log func(string)) {
	if env("ICEBATH_DESKTOP_SKIP_PROTOCOL_REGISTRATION") == "1" {
		log("[deeplink] " + protocol + ":// registration skipped (ICEBATH_DESKTOP_SKIP_PROTOCOL_REGISTRATION)")
		return
	}
	var err error
	if defaultApp && len(argv) >= 2 {
		err = r.SetAsDefaultProtocolClient(protocol, execPath, []string{resolve(argv[1])})
	} else {
		err = r.SetAsDefaultProtocolClient(protocol, "", nil)
	}
	if err != nil {
		log("[deeplink] protocol registration failed: " + err.Error())
		return
	}
	log("[deeplink] registered " + protocol + ":// handler")
}
