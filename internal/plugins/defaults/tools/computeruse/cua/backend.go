package cua

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	cuconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	cupackages "github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/packages"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One interactable element of a capture, its bounds x, y, width and height in the screen's own pixels
type Element struct {
	Index    int
	Role     string
	Label    string
	Bounds   [4]int
	App      string
	PID      int
	WindowID int
	// Capability the driver minted for the element in this snapshot, empty when it mints none
	Token string
}

// A screenshot, the element map, or both, as the mode asked
type Capture struct {
	Mode   string
	Width  int
	Height int
	// Base64 screenshot and its type, empty when there is none
	Image string
	Mime  string
	// Bytes of the screenshot
	Bytes    int
	Elements []Element
	App      string
	Title    string
	Note     string
	// Cap of the accessibility walk, zero for none
	AXMax int
}

// What an action came to: whether the driver took it, and its verdict of what happened
type Action struct {
	OK         bool
	Action     string
	Message    string
	Meta       map[string]any
	Verified   *bool
	Effect     *string
	Escalation map[string]any
	Path       *string
	Degraded   *bool
	Delivery   *string
	Code       *string
}

// How input is delivered: in the background, or with the window fronted, the persistent bring_to_front first when asked
type Delivery struct {
	Mode  string
	Front bool
}

type Click struct {
	Element, X, Y *int
	Button        string
	Count         int
	Modifiers     []string
	Delivery
}

type Drag struct {
	From, To     *int
	FromXY, ToXY *[2]int
	Button       string
	Modifiers    []string
	Delivery
}

type Scroll struct {
	Direction     string
	Amount        int
	Element, X, Y *int
	Modifiers     []string
	Delivery
}

// A session of the driver one icebath session drives the desktop through, sticking to the window its last capture or focus picked
type Backend struct {
	driver  *Driver
	config  *config.Config
	Mode    string
	daemon  *daemon
	session *session
	// Window input goes to, and the app it belongs to
	pid, window *int
	app         string
	// Element tokens of the last snapshot by index
	tokens map[int]string
	// Label the driver knows this run by
	label string
}

// Backend in the permission mode, with a daemon of its own outside the standard one
func (d *Driver) Backend(cfg *config.Config, mode string) (*Backend, error) {
	if mode != cuconfig.Standard && mode != cuconfig.Bounded && mode != cuconfig.Unrestricted {
		return nil, fmt.Errorf("unsupported cua-driver permission mode: %s", mode)
	}
	b := &Backend{driver: d, config: cfg, Mode: mode, label: "icebath-" + strs.Random(6)}
	if mode != cuconfig.Standard {
		dm, err := newDaemon(d, cfg, mode, cfg.GetString(cuconfig.CapabilityManifest))
		if err != nil {
			return nil, err
		}
		b.daemon = dm
	}
	b.session = newSession(d, cfg, b.daemon)
	b.session.reset = b.clear
	b.clear()
	return b, nil
}

// App input goes to, empty before a capture or focus picked one
func (b *Backend) App() string { return b.app }

// Exact window input goes to, nil before one was picked
func (b *Backend) Target() (pid, window *int) { return b.pid, b.window }

// Checks the contract, starts the private daemon and the session, then declares the run's label and applies the profile's settings
func (b *Backend) Start(ctx context.Context) error {
	sandboxed, err := b.driver.Sandboxed(ctx, b.config)
	if err != nil {
		return err
	}
	// The driver inside the terminal backend's sandbox comes with its image, so only one of this machine is readied and checked
	if sandboxed == nil {
		override := b.driver.Override() != ""
		if !override {
			if _, err := b.driver.packages.Ensure(ctx, b.config, cupackages.CuaDriver, packages.Ensuring{}); err != nil {
				return err
			}
		}
		c, err := b.driver.Contract(ctx, b.config, "")
		if err != nil {
			return err
		}
		if !c.Ready {
			hint := "Run `icebath computer-use install` to repair it."
			if override {
				hint = "Update the binary selected by " + cuconfig.DriverCommandEnv + " or remove that override."
			}
			return fmt.Errorf("cua-driver is not ready: %s. %s", cmp.Or(c.Reason, "runtime contract is incomplete"), hint)
		}
	}
	if b.daemon != nil {
		if err := b.daemon.start(ctx); err != nil {
			return errors.Join(err, b.daemon.stop())
		}
	}
	if err := b.session.start(ctx); err != nil {
		if b.daemon != nil {
			return errors.Join(err, b.daemon.stop())
		}
		return err
	}
	b.quietly(ctx, "start_session failed (continuing anonymous)", "start_session", map[string]any{"session": b.label})
	if b.session.started.Load() {
		if edge := b.config.GetInt(cuconfig.MaxImageDimension); edge > 0 {
			b.quietly(ctx, "set_config(max_image_dimension) failed", "set_config", map[string]any{"max_image_dimension": edge, "session": b.label})
		}
		if noOverlay(b.config) {
			b.quietly(ctx, "set_agent_cursor_enabled failed", "set_agent_cursor_enabled", map[string]any{"enabled": false, "cursor_id": b.label,
				"session": b.label})
		}
	}
	return nil
}

// Ends the driver session, then closes it and stops the private daemon even when a step fails
func (b *Backend) Stop() error {
	if b.session.started.Load() {
		b.quietly(context.Background(), "end_session failed (continuing teardown)", "end_session", map[string]any{"session": b.label})
	}
	b.session.stop()
	if b.daemon != nil {
		return b.daemon.stop()
	}
	return nil
}

// A call whose failure is only logged
func (b *Backend) quietly(ctx context.Context, what, name string, args map[string]any) {
	out, err := b.session.call(ctx, name, args, 30*time.Second)
	if err != nil {
		slog.Debug(fmt.Sprintf("cua-driver %s: %v", what, err))
	} else if out.IsError && name != "start_session" && name != "end_session" {
		slog.Debug(fmt.Sprintf("cua-driver %s: %s", what, out.text()))
	}
}

// Forgets the window input goes to and the tokens of its snapshot, so a failed lookup can't misroute input
func (b *Backend) clear() {
	b.pid, b.window, b.app = nil, nil, ""
	b.tokens = map[int]string{}
}

func (b *Backend) target(w map[string]any) {
	pid, _ := whole(w["pid"])
	window, _ := whole(w["window_id"])
	b.pid, b.window = &pid, &window
	b.tokens = map[int]string{}
}

func refuse(action, message string) *Action {
	return &Action{Action: action, Message: message, Meta: map[string]any{}}
}

func refuseCoded(action, message, code, delivery string) *Action {
	a := refuse(action, message)
	a.Code = &code
	if delivery != "" {
		a.Delivery = &delivery
	}
	return a
}

// One driver action, the element's token attached where the driver takes it, the session label put in, the verdict read off the reply
func (b *Backend) action(ctx context.Context, name string, args map[string]any, label bool) *Action {
	if index, ok := args["element_index"].(int); ok {
		if token := b.tokens[index]; token != "" && (b.session.takes(name, "element_token") || b.session.supports("accessibility.element_tokens", name)) {
			args["element_token"] = token
		}
	}
	if _, has := args["session"]; label && !has {
		args["session"] = b.label
	}
	out, err := b.session.call(ctx, name, args, 30*time.Second)
	if err != nil {
		slog.Error(fmt.Sprintf("cua-driver %s call failed", name), "error", err)
		return refuse(name, "cua-driver error: "+err.Error())
	}
	message := ""
	data, _ := out.Data.(map[string]any)
	if text, ok := out.Data.(string); ok {
		message = text
	} else if data != nil {
		message = show(cmp.Or(data["message"], any("")))
	}
	if message == "" && out.Structured != nil {
		message = show(cmp.Or(out.Structured["message"], any("")))
	}
	meta := map[string]any{}
	maps.Copy(meta, data)
	maps.Copy(meta, out.Structured)
	var delivery *string
	if mode, ok := args["delivery_mode"].(string); ok {
		delivery = &mode
	}
	return verdict(name, !out.IsError, message, meta, out.Structured, delivery)
}

// Action lifting the driver's verdict off its structured reply, the flattened copy second
func verdict(name string, ok bool, message string, meta, structured map[string]any, delivery *string) *Action {
	raw := func(key string) any {
		if v, has := structured[key]; has {
			return v
		}
		return meta[key]
	}
	boolean := func(v any) *bool {
		if b, ok := v.(bool); ok {
			return &b
		}
		return nil
	}
	text := func(v any) *string {
		if s, ok := v.(string); ok {
			return &s
		}
		return nil
	}
	code := raw("code")
	if s, _ := code.(string); s == "" {
		code = raw("reason_code")
	}
	escalation, _ := raw("escalation").(map[string]any)
	return &Action{OK: ok, Action: name, Message: message, Meta: meta, Verified: boolean(raw("verified")), Effect: text(raw("effect")),
		Escalation: escalation, Path: text(raw("path")), Degraded: boolean(raw("degraded")), Delivery: delivery, Code: text(code)}
}

// Brings the window to the front so foreground input lands on it
func (b *Backend) bringToFront(ctx context.Context, pid int, window *int) *Action {
	args := map[string]any{"pid": pid}
	if window != nil {
		args["window_id"] = *window
	}
	return b.action(ctx, "bring_to_front", args, false)
}

// Whole-screen and desktop-shell intents of the app argument
var (
	fullScreen      = []string{"screen", "fullscreen", "full screen", "all"}
	desktopWindows  = []string{"progman", "workerw", "program manager", "shell_traywnd", "taskbar", "finder", "desktop", "dock"}
	desktopBackdrop = []string{"progman", "workerw", "program manager", "finder", "desktop"}
	windowTitle     = regexp.MustCompile(`AXWindow[\s\p{Zs}]+"([^"]+)"`)
	appPid          = regexp.MustCompile(`(.+?)[\s\p{Zs}]+\(pid[\s\p{Zs}]+([0-9]+)\)`)
)

const (
	noDesktopWindow = "<no desktop/shell window found for app=%s; cua-driver captures one window at a time and exposes no whole-virtual-desktop " +
		"or per-monitor capture. Call list_apps / capture(app='<AppName>') to target a specific window instead. On Windows the taskbar is " +
		"'Shell_TrayWnd' and the desktop is 'Progman'.>"
	noAppMatch = "<no on-screen window matched app=%s; call list_apps to see available app names or bundle IDs (macOS reports localized names, e.g. " +
		"'計算機' instead of 'Calculator'; some Linux/Qt apps only resolve via list_apps metadata)>"
	noDesktopImage = "<get_desktop_state returned no image; the driver may predate the desktop capture lane — try capture(app='<AppName>') for a " +
		"specific window>"
	fullScreenNote = "full-screen capture has no interactable elements; to act on what you see, call capture(app='<AppName>') for that app's " +
		"clickable element list, or capture(app='desktop') for the desktop shell (wallpaper icons / taskbar) with elements"
	noTarget         = "No active window — call capture() first."
	noFront          = "The connected cua-driver does not advertise the standalone bring_to_front tool."
	noForegroundMode = "The connected cua-driver action schema does not accept delivery_mode, so foreground delivery is unavailable. Use another " +
		"verified rung without assuming the reported package version describes the live schema."
)

// Text of a row's field, the default when it lacks the field
func field(w map[string]any, key, def string) string {
	if v, has := w[key]; has {
		return show(v)
	}
	return def
}

// The authorization daemon's own window, whatever its app name's spacing
func selfWindow(w map[string]any) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(field(w, "app_name", ""))) {
		if !unicode.IsSpace(r) && r != '_' && r != '-' {
			b.WriteRune(r)
		}
	}
	return b.String() == "cuadriver"
}

// Best window of a front first list: on screen, not the driver's own, on Linux no shell helper and the X11 active window when the z order says
// nothing
func (b *Backend) pick(windows []map[string]any, requested, exact bool) map[string]any {
	var pool []map[string]any
	for _, w := range windows {
		if w["off_screen"] != true {
			pool = append(pool, w)
		}
	}
	if !exact {
		if external := slices.DeleteFunc(slices.Clone(pool), selfWindow); len(external) > 0 {
			pool = external
		}
	}
	if !exact && !requested && b.driver.Linux(b.config) {
		if real := slices.DeleteFunc(slices.Clone(pool), func(w map[string]any) bool { return !realWindow(w) }); len(real) > 0 {
			pool = real
		}
		if len(pool) > 0 && zUninformative(pool) {
			if active, ok := b.activeWindow(); ok {
				for _, w := range pool {
					if id, ok := w["window_id"].(int); ok && int64(id) == active {
						return w
					}
				}
			}
		}
	}
	if len(pool) > 0 {
		return pool[0]
	}
	return windows[0]
}

// X11 active window of the display the driver acts on, read inside the terminal backend's sandbox when the screen lives there
func (b *Backend) activeWindow() (int64, bool) {
	ctx := context.Background()
	argv, env := []string{"xprop", "-root", "_NET_ACTIVE_WINDOW"}, b.driver.Env(b.config)
	if b.driver.Desktop.InSandbox(b.config) {
		command, err := b.driver.Desktop.Command(ctx, b.config, argv, nil, "run xprop", false)
		if err != nil || command == nil {
			return 0, false
		}
		argv, env = command, map[string]string{"PATH": os.Getenv("PATH")}
	} else if !linux || env["DISPLAY"] == "" {
		return 0, false
	}
	out, err := run(ctx, argv, 5*time.Second, env, nil)
	if err != nil || out.code != 0 {
		return 0, false
	}
	return xpropActive(out.stdout)
}

// Whether the graphical session is locked as loginctl says, unknown off Linux or when it can't tell
func locked() (bool, bool) {
	if !linux {
		return false, false
	}
	out, err := run(context.Background(), []string{"loginctl", "list-sessions", "--no-legend"}, 2*time.Second, nil, nil)
	if err != nil {
		return false, false
	}
	var seats []string
	for _, line := range strings.Split(out.stdout, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.Contains(line, "seat") {
			seats = append(seats, f[0])
		}
	}
	if out.code != 0 || len(seats) == 0 {
		return false, false
	}
	for _, s := range seats {
		shown, err := run(context.Background(), []string{"loginctl", "show-session", s, "-p", "LockedHint"}, 2*time.Second, nil, nil)
		if err != nil {
			return false, false
		}
		if strings.Contains(shown.stdout, "LockedHint=no") {
			return false, true
		}
	}
	return true, true
}

// One line saying why window discovery found nothing
func emptyDiscovery() string {
	if lock, known := locked(); known && lock {
		return "the desktop session is LOCKED (loginctl LockedHint=yes) — unlock the screen; a locked compositor hides windows and freezes app renderers"
	}
	if linux && os.Getenv("DISPLAY") == "" {
		return "no DISPLAY is set — X11/XWayland is not reachable from this process"
	}
	if darwin {
		return "window discovery returned no windows; on macOS this usually means no shareable display (headless Mac or panel asleep) — wake the " +
			"display or attach a monitor/HDMI dummy, then run `icebath computer-use doctor`"
	}
	return "window discovery returned no windows; run `icebath computer-use doctor` (display reachability, AX capability)"
}

// Forgets the target when a step of a capture failed
func (b *Backend) disarm(err error) error {
	if err != nil {
		b.clear()
	}
	return err
}

func (b *Backend) failedCapture(mode, message string) *Capture {
	b.clear()
	return &Capture{Mode: mode, Title: message}
}

// A capture step whose failure, of the transport or of the driver, disarms the target
func (b *Backend) captureCall(ctx context.Context, name string, args map[string]any) (*envelope, error) {
	out, err := b.session.call(ctx, name, args, 30*time.Second)
	if err != nil {
		return nil, b.disarm(err)
	}
	if out.IsError {
		message := "cua-driver " + name + " failed"
		if text, ok := out.Data.(string); ok && text != "" {
			message += ": " + text
		}
		return nil, b.disarm(errors.New(message))
	}
	return out, nil
}

// Fetches through the CLI once after an empty MCP result, nil when that failed too
func (b *Backend) refetch(ctx context.Context, name string, args map[string]any, timeout time.Duration, what, warning string) *envelope {
	slog.Warn(warning)
	out, err := b.session.viaCLI(ctx, name, args, timeout)
	if err != nil {
		slog.Error(fmt.Sprintf("cua-driver CLI re-fetch for %s failed: %v", what, err))
		return nil
	}
	if !out.IsError {
		return out
	}
	if name == "list_windows" {
		slog.Error("cua-driver CLI re-fetch for list_windows returned an error")
	}
	b.clear()
	return nil
}

func (b *Backend) fetch(ctx context.Context, name string, args map[string]any, timeout time.Duration, what string, empty func(*envelope) bool,
	warning string) (*envelope, error) {
	out, err := b.captureCall(ctx, name, args)
	if err != nil {
		return nil, err
	}
	if empty(out) {
		if again := b.refetch(ctx, name, args, timeout, what, warning); again != nil && !empty(again) {
			return again, nil
		}
	}
	return out, nil
}

// Rows of list_windows, front first by z order, ties in the driver's order
func sortedWindows(e *envelope) []map[string]any {
	rows := ingest(windowsOf(e))
	slices.SortStableFunc(rows, func(a, b map[string]any) int { return cmpFloat(zValue(b["z_index"]), zValue(a["z_index"])) })
	return rows
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Windows on screen, front first, fetched through the CLI when MCP gives none
func (b *Backend) ListWindows(ctx context.Context) ([]map[string]any, error) {
	out, err := b.fetch(ctx, "list_windows", map[string]any{"on_screen_only": true, "session": b.label}, 20*time.Second, "list_windows",
		func(e *envelope) bool { return len(sortedWindows(e)) == 0 }, "cua-driver list_windows returned no windows over MCP; re-fetching via CLI transport")
	if err != nil {
		return nil, err
	}
	return sortedWindows(out), nil
}

// Windows of an app by its exact name, then a list_apps name, bundle id or pid, then a part of a name, then the titles of nameless windows
func (b *Backend) match(ctx context.Context, windows []map[string]any, app string) []map[string]any {
	wanted := strings.ToLower(strings.TrimSpace(app))
	name := func(w map[string]any) string { return strings.ToLower(field(w, "app_name", "")) }
	var exact []map[string]any
	for _, w := range windows {
		if wanted != "" && wanted == strings.TrimSpace(name(w)) {
			exact = append(exact, w)
		}
	}
	if wanted == "" || len(exact) > 0 {
		return exact
	}
	apps, err := b.ListApps(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use list_apps fallback failed for '%s': %v", app, err))
	}
	exactPids, partialPids := map[int]bool{}, map[int]bool{}
	for _, raw := range apps {
		a, _ := raw.(map[string]any)
		pid, ok := positive(a["pid"])
		if !ok || a["running"] == false {
			continue
		}
		aliases := map[string]bool{}
		for _, k := range []string{"bundle_id", "bundleId", "name", "app_name", "display_name"} {
			if v, ok := a[k].(string); ok && strings.TrimSpace(v) != "" {
				aliases[strings.ToLower(strings.TrimSpace(v))] = true
			}
		}
		if aliases[wanted] {
			exactPids[pid] = true
			continue
		}
		for alias := range aliases {
			if strings.Contains(alias, wanted) {
				partialPids[pid] = true
				break
			}
		}
	}
	in := func(w map[string]any, set map[int]bool) bool {
		pid, ok := w["pid"].(int)
		return ok && set[pid]
	}
	for _, tier := range []func(map[string]any) bool{
		func(w map[string]any) bool { return in(w, exactPids) },
		func(w map[string]any) bool { return strings.Contains(name(w), wanted) },
		func(w map[string]any) bool { return in(w, partialPids) },
		func(w map[string]any) bool {
			return strings.TrimSpace(name(w)) == "" && strings.Contains(strings.ToLower(field(w, "title", "")), wanted)
		},
	} {
		var matched []map[string]any
		for _, w := range windows {
			if tier(w) {
				matched = append(matched, w)
			}
		}
		if len(matched) > 0 {
			return matched
		}
	}
	return nil
}

func isDesktop(w map[string]any, names []string) bool {
	hay := strings.ToLower(field(w, "app_name", "") + " " + field(w, "title", ""))
	return slices.ContainsFunc(names, func(n string) bool { return strings.Contains(hay, n) })
}

// Windows a capture may take, or the empty capture saying why there are none
func (b *Backend) candidates(ctx context.Context, mode, app string, pid, window *int) ([]map[string]any, *Capture, error) {
	if pid != nil || window != nil {
		if pid == nil || window == nil {
			return nil, b.failedCapture(mode, "<capture targeting requires both pid and window_id>"), nil
		}
		if *pid <= 0 || *window <= 0 {
			return nil, b.failedCapture(mode, "<capture targeting requires positive integer pid and window_id>"), nil
		}
		return []map[string]any{{"app_name": app, "pid": *pid, "window_id": *window, "off_screen": false, "title": "", "z_index": 0.0}}, nil, nil
	}
	windows, err := b.ListWindows(ctx)
	if err != nil {
		return nil, nil, b.disarm(err)
	}
	if len(windows) == 0 {
		return nil, b.failedCapture(mode, emptyDiscovery()), nil
	}
	if app == "" {
		return windows, nil, nil
	}
	if strings.ToLower(strings.TrimSpace(app)) == "desktop" {
		var desktop []map[string]any
		for _, w := range windows {
			if isDesktop(w, desktopWindows) {
				desktop = append(desktop, w)
			}
		}
		slices.SortStableFunc(desktop, func(x, y map[string]any) int {
			switch xb, yb := isDesktop(x, desktopBackdrop), isDesktop(y, desktopBackdrop); {
			case xb && !yb:
				return -1
			case yb && !xb:
				return 1
			}
			return 0
		})
		if len(desktop) == 0 {
			return nil, b.failedCapture(mode, fmt.Sprintf(noDesktopWindow, "'"+app+"'")), nil
		}
		return desktop, nil, nil
	}
	if matched := b.match(ctx, windows, app); len(matched) > 0 {
		return matched, nil, nil
	}
	return nil, b.failedCapture(mode, fmt.Sprintf(noAppMatch, "'"+app+"'")), nil
}

// get_window_state arguments for the target with the profile's cap on the accessibility walk
func (b *Backend) stateArgs() map[string]any {
	args := map[string]any{"pid": deref(b.pid), "window_id": deref(b.window), "session": b.label}
	if limit := max(b.config.GetInt(cuconfig.AXMaxElements), 0); limit != 0 {
		args["max_elements"] = limit
	}
	return args
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// Tree and window title of a get_window_state reply
func treeOf(e *envelope) (string, string) {
	data, _ := e.Data.(string)
	_, tree, _ := strings.Cut(data, "\n")
	if m := windowTitle.FindStringSubmatch(tree); m != nil {
		return tree, m[1]
	}
	return tree, ""
}

func emptyState(e *envelope) bool {
	tree, _ := treeOf(e)
	elements, _ := e.Structured["elements"].([]any)
	shot, _ := e.Structured["screenshot_png_b64"].(string)
	return len(e.Images) == 0 && len(elements) == 0 && shot == "" && strings.TrimSpace(tree) == ""
}

// Pixels only: the screenshot tool, then get_window_state, then the CLI
func (b *Backend) vision(ctx context.Context) (string, string, string, error) {
	image, mime, title := "", "", ""
	if b.session.has("screenshot") || !b.session.discovered() {
		out, err := b.captureCall(ctx, "screenshot", map[string]any{"window_id": deref(b.window), "format": "jpeg", "quality": 85, "session": b.label})
		if err != nil {
			return "", "", "", err
		}
		image, mime = imageOf(out)
	}
	if image == "" {
		out, err := b.captureCall(ctx, "get_window_state", b.stateArgs())
		if err != nil {
			return "", "", "", err
		}
		image, mime = imageOf(out)
		_, title = treeOf(out)
	}
	if image == "" {
		if out := b.refetch(ctx, "get_window_state", b.stateArgs(), 30*time.Second, "vision screenshot",
			fmt.Sprintf("cua-driver vision capture returned no image over MCP (window_id=%s); re-fetching via CLI transport", show(deref(b.window)))); out != nil &&
			len(out.Images) > 0 {
			image, mime = out.Images[0], "image/png"
		}
	}
	return image, mime, title, nil
}

// Accessibility tree and screenshot of the target, its element tokens kept for the input that follows
func (b *Backend) state(ctx context.Context) (string, string, []Element, string, error) {
	out, err := b.fetch(ctx, "get_window_state", b.stateArgs(), 30*time.Second, "get_window_state", emptyState,
		fmt.Sprintf("cua-driver get_window_state returned an empty result over MCP (pid=%s window_id=%s); re-fetching via CLI transport",
			show(deref(b.pid)), show(deref(b.window))))
	if err != nil {
		return "", "", nil, "", err
	}
	tree, title := treeOf(out)
	elements := []Element{}
	if listed, ok := out.Structured["elements"].([]any); ok && len(listed) > 0 {
		elements = structuredElements(listed)
	} else if tree != "" {
		elements = treeElements(tree)
	}
	b.tokens = map[int]string{}
	for _, e := range elements {
		if e.Token != "" {
			b.tokens[e.Index] = e.Token
		}
	}
	image, mime := imageOf(out)
	return image, mime, elements, title, nil
}

// Captures the front window, the app's, or an exact window, in som, ax or vision mode, making it the target of input
func (b *Backend) Capture(ctx context.Context, mode, app string, pid, window *int) (*Capture, error) {
	if pid != nil && *pid <= 0 {
		pid = nil
	}
	if window != nil && *window <= 0 {
		window = nil
	}
	exact := pid != nil || window != nil
	if !exact && slices.Contains(fullScreen, strings.ToLower(strings.TrimSpace(app))) {
		return b.fullScreen(ctx, mode)
	}
	windows, failed, err := b.candidates(ctx, mode, app, pid, window)
	if err != nil || failed != nil {
		return failed, err
	}
	target := b.pick(windows, app != "", exact)
	b.target(target)
	name, _ := target["app_name"].(string)
	if app != "" || b.app == "" {
		b.app = cmp.Or(name, app)
	}
	var image, mime, title string
	elements := []Element{}
	if mode == "vision" {
		image, mime, title, err = b.vision(ctx)
	} else {
		image, mime, elements, title, err = b.state(ctx)
	}
	if err != nil {
		return nil, err
	}
	c := &Capture{Mode: mode, Image: image, Mime: mime, Elements: elements, App: name, Title: title}
	if image != "" {
		c.Bytes, c.Width, c.Height = metrics(image, 0, 0)
	}
	if mode != "vision" {
		c.AXMax, _ = b.stateArgs()["max_elements"].(int)
	}
	return c, nil
}

// Composited whole screen through get_desktop_state, the driver's capture scope switched to the desktop and back
func (b *Backend) fullScreen(ctx context.Context, mode string) (*Capture, error) {
	b.clear()
	previous := ""
	if out, err := b.session.call(ctx, "get_config", map[string]any{"session": b.label}, 10*time.Second); err != nil {
		slog.Debug(fmt.Sprintf("cua-driver get_config before full-screen capture failed: %v", err))
	} else if scope, ok := out.Structured["capture_scope"].(string); ok {
		previous = scope
	}
	scope := func(value string) error {
		_, err := b.session.call(ctx, "set_config", map[string]any{"key": "capture_scope", "value": value, "session": b.label}, 10*time.Second)
		return err
	}
	out, err := func() (*envelope, error) {
		defer func() {
			if previous != "" && previous != "desktop" {
				if err := scope(previous); err != nil {
					slog.Debug(fmt.Sprintf("cua-driver restore capture_scope failed: %v", err))
				}
			}
		}()
		if previous != "desktop" {
			if err := scope("desktop"); err != nil {
				return nil, err
			}
		}
		return b.captureCall(ctx, "get_desktop_state", map[string]any{"session": b.label})
	}()
	if err != nil {
		return nil, err
	}
	image, mime := imageOf(out)
	if image == "" {
		return b.failedCapture(mode, noDesktopImage), nil
	}
	dim := func(keys ...string) int {
		for _, k := range keys {
			if n, ok := whole(out.Structured[k]); ok && n != 0 {
				return n
			}
		}
		return 0
	}
	c := &Capture{Mode: "vision", Image: image, Mime: mime, Elements: []Element{}, App: "screen", Title: "Full screen (composited)", Note: fullScreenNote}
	c.Bytes, c.Width, c.Height = metrics(image, dim("screenshot_width", "screen_width"), dim("screenshot_height", "screen_height"))
	return c, nil
}

// Running apps: the structured list, the shapes older drivers reply in, those of the windows, then the text listing
func (b *Backend) ListApps(ctx context.Context) ([]any, error) {
	out, err := b.session.call(ctx, "list_apps", map[string]any{"session": b.label}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	data, _ := out.Data.(map[string]any)
	for _, candidate := range []any{out.Structured["apps"], out.Data, data["apps"], appsOf(windowsOf(out))} {
		if apps, ok := candidate.([]any); ok && len(apps) > 0 {
			return apps, nil
		}
	}
	text, _ := out.Data.(string)
	apps := []any{}
	for _, line := range strings.Split(text, "\n") {
		if m := appPid.FindStringSubmatch(line); m != nil {
			pid, _ := strconv.Atoi(m[2])
			apps = append(apps, map[string]any{"name": strings.TrimSpace(m[1]), "pid": pid})
		}
	}
	return apps, nil
}

// Makes the app's window the target, raising it only through the separately approved bring_to_front
func (b *Backend) FocusApp(ctx context.Context, app string, raise bool) (*Action, error) {
	windows, err := b.ListWindows(ctx)
	if err != nil {
		return nil, b.disarm(err)
	}
	matched := b.match(ctx, windows, app)
	if len(matched) == 0 {
		b.clear()
		return refuse("focus_app", fmt.Sprintf("No on-screen window found for app '%s'.", app)), nil
	}
	b.target(matched[0])
	name, _ := matched[0]["app_name"].(string)
	b.app = cmp.Or(name, app)
	if !raise {
		return &Action{OK: true, Action: "focus_app", Meta: map[string]any{}, Message: fmt.Sprintf("Targeted %s (pid %s, window %s) without raising window.",
			name, show(deref(b.pid)), show(deref(b.window)))}, nil
	}
	if !b.session.has("bring_to_front") {
		return refuseCoded("focus_app", noFront, "bring_to_front_unsupported", ""), nil
	}
	focused := b.bringToFront(ctx, *b.pid, b.window)
	if focused.OK {
		focused.Action = "focus_app"
		focused.Meta["target_selected"] = true
	}
	return focused, nil
}

// Base arguments of an input action on the target, or the refusal when there is none
func (b *Backend) inputArgs(action string, window bool) (*Action, map[string]any) {
	if b.pid == nil || window && b.window == nil {
		return refuse(action, noTarget), nil
	}
	args := map[string]any{"pid": *b.pid}
	if window {
		args["window_id"] = *b.window
	}
	return nil, args
}

// Addressing of an input action: the first form given, with the window; refused when none is and missing says so
func (b *Backend) address(tool string, args map[string]any, forms []struct {
	what  string
	extra func() map[string]any
}, missing string) *Action {
	for _, form := range forms {
		if form.extra == nil {
			continue
		}
		if b.window == nil {
			return refuse(tool, "No active window_id for "+form.what+".")
		}
		maps.Copy(args, form.extra())
		args["window_id"] = *b.window
		return nil
	}
	if missing != "" {
		return refuse(tool, missing)
	}
	return nil
}

type form = struct {
	what  string
	extra func() map[string]any
}

func fixed(m map[string]any) func() map[string]any { return func() map[string]any { return m } }

// One rung of delivery: foreground where the live schema takes it, bring_to_front run first when asked
func (b *Backend) deliver(ctx context.Context, action string, args map[string]any, d Delivery) *Action {
	switch d.Mode {
	case "", "background":
	case "foreground":
		if !b.session.takes(action, "delivery_mode") {
			return refuseCoded(action, noForegroundMode, "foreground_unsupported", "foreground")
		}
		args["delivery_mode"] = "foreground"
	default:
		return refuseCoded(action, fmt.Sprintf("unknown delivery_mode '%s' — use background|foreground.", d.Mode), "bad_delivery_mode", "")
	}
	if d.Front {
		switch {
		case d.Mode != "foreground":
			return refuseCoded(action, "bring_to_front requires delivery_mode='foreground'.", "bring_to_front_requires_foreground", "")
		case !b.session.has("bring_to_front"):
			return refuseCoded(action, noFront, "bring_to_front_unsupported", "foreground")
		case b.pid == nil || b.window == nil:
			return refuseCoded(action, "Capture an exact target before requesting persistent foreground focus.", "bring_to_front_target_required",
				"foreground")
		}
		if focused := b.bringToFront(ctx, *b.pid, b.window); !focused.OK {
			return focused
		}
	}
	result := b.action(ctx, action, args, true)
	if d.Front {
		result.Meta["foreground_focus"] = map[string]any{"invoked": true, "tool": "bring_to_front"}
	}
	return result
}

// Click or double click by element index or by point, with the button checked against the driver's choices
func (b *Backend) Click(ctx context.Context, c Click) *Action {
	refusal, args := b.inputArgs("click", false)
	if refusal != nil {
		return refusal
	}
	button := strings.ToLower(cmp.Or(c.Button, "left"))
	if button != "left" && button != "right" && button != "middle" {
		return refuse("click", fmt.Sprintf("unknown button '%s' — expected left, right, middle.", c.Button))
	}
	tool := "click"
	if c.Count == 2 {
		tool = "double_click"
	}
	args["button"] = button
	forms := []form{{what: "element_index click"}, {what: "coordinate click"}}
	if c.Element != nil {
		forms[0].extra = fixed(map[string]any{"element_index": *c.Element})
	}
	if c.X != nil && c.Y != nil {
		forms[1].extra = fixed(map[string]any{"x": *c.X, "y": *c.Y})
	}
	refusal = b.address(tool, args, forms, "click requires element= or x/y.")
	if len(c.Modifiers) > 0 {
		args["modifier"] = c.Modifiers
	}
	if refusal != nil {
		return refusal
	}
	return b.deliver(ctx, tool, args, c.Delivery)
}

// Drag between two elements or two points
func (b *Backend) Drag(ctx context.Context, d Drag) *Action {
	refusal, args := b.inputArgs("drag", false)
	if refusal != nil {
		return refusal
	}
	forms := []form{{what: "element-based drag"}, {what: "coordinate drag"}}
	if d.From != nil && d.To != nil {
		forms[0].extra = fixed(map[string]any{"from_element": *d.From, "to_element": *d.To})
	}
	if d.FromXY != nil && d.ToXY != nil {
		forms[1].extra = fixed(map[string]any{"from_x": d.FromXY[0], "from_y": d.FromXY[1], "to_x": d.ToXY[0], "to_y": d.ToXY[1]})
	}
	if refusal := b.address("drag", args, forms, "drag requires from_element/to_element or from_coordinate/to_coordinate."); refusal != nil {
		return refusal
	}
	return b.deliver(ctx, "drag", args, d.Delivery)
}

// Scroll by wheel ticks, one to fifty, at an element or a point when the driver takes points
func (b *Backend) Scroll(ctx context.Context, s Scroll) *Action {
	refusal, args := b.inputArgs("scroll", false)
	if refusal != nil {
		return refusal
	}
	args["direction"], args["amount"] = s.Direction, max(1, min(50, s.Amount))
	forms := []form{{what: "element scroll"}, {what: "coordinate scroll"}}
	if s.Element != nil && b.window != nil {
		forms[0].extra = fixed(map[string]any{"element_index": *s.Element})
	}
	if s.X != nil && s.Y != nil {
		x, y := *s.X, *s.Y
		forms[1].extra = func() map[string]any {
			if b.session.supports("input.scroll.coordinates", "scroll") {
				return map[string]any{"x": x, "y": y}
			}
			return map[string]any{}
		}
	}
	if refusal := b.address("scroll", args, forms, ""); refusal != nil {
		return refusal
	}
	return b.deliver(ctx, "scroll", args, s.Delivery)
}

// Types the text into the target window
func (b *Backend) Type(ctx context.Context, text string, d Delivery) *Action {
	refusal, args := b.inputArgs("type_text", true)
	if refusal != nil {
		return refusal
	}
	args["text"] = text
	return b.deliver(ctx, "type_text", args, d)
}

// Presses a key, or a combination through hotkey
func (b *Backend) Key(ctx context.Context, keys string, d Delivery) *Action {
	refusal, args := b.inputArgs("key", true)
	if refusal != nil {
		return refusal
	}
	key, modifiers := keyCombo(keys)
	if key == "" {
		return refuse("key", fmt.Sprintf("Could not parse key from '%s'.", keys))
	}
	if len(modifiers) > 0 {
		args["keys"] = append(modifiers, key)
		return b.deliver(ctx, "hotkey", args, d)
	}
	args["key"] = key
	return b.deliver(ctx, "press_key", args, d)
}

// Sets the value of an element, a popup's option included
func (b *Backend) SetValue(ctx context.Context, value string, element *int) *Action {
	refusal, args := b.inputArgs("set_value", true)
	if refusal != nil {
		return refusal
	}
	if element == nil {
		return refuse("set_value", "set_value requires element= (element index).")
	}
	args["element_index"], args["value"] = *element, value
	return b.action(ctx, "set_value", args, true)
}

// Waits the seconds asked, nought to thirty
func (b *Backend) Wait(ctx context.Context, seconds float64) *Action {
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(max(0, min(seconds, 30)) * float64(time.Second))):
	}
	return &Action{OK: true, Action: "wait", Message: fmt.Sprintf("waited %.2fs", seconds), Meta: map[string]any{}}
}
