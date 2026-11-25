package codeexec

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Python str pattern whitespace class
const wsValue = `[\s\x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// Whole screen and desktop shell intents of the app argument
var (
	fullScreenSentinels   = map[string]bool{"screen": true, "fullscreen": true, "full screen": true, "all": true}
	desktopShellSentinels = map[string]bool{"desktop": true}
	desktopWindowNames    = []string{"progman", "workerw", "program manager", "shell_traywnd", "taskbar", "finder", "desktop", "dock"}
	desktopBackdropNames  = []string{"progman", "workerw", "program manager", "finder", "desktop"}
	axWindowTitleRE       = regexp.MustCompile(`AXWindow` + wsValue + `+"([^"]+)"`)
	textAppPIDRE          = regexp.MustCompile(`(.+?)` + wsValue + `+\(pid` + wsValue + `+([0-9]+)\)`)
)

const (
	noDesktopWindowMsg = "<no desktop/shell window found for app=%s; cua-driver captures one window at a time " +
		"and exposes no whole-virtual-desktop or per-monitor capture. Call list_apps / " +
		"capture(app='<AppName>') to target a specific window instead. On Windows the taskbar is " +
		"'Shell_TrayWnd' and the desktop is 'Progman'.>"
	noAppMatchMsg = "<no on-screen window matched app=%s; call list_apps to see available app names or bundle " +
		"IDs (macOS reports localized names, e.g. '計算機' instead of 'Calculator'; some Linux/Qt apps " +
		"only resolve via list_apps metadata)>"
	noDesktopImageMsg = "<get_desktop_state returned no image; the driver may predate the desktop capture lane — " +
		"try capture(app='<AppName>') for a specific window>"
	fullScreenNote = "full-screen capture has no interactable elements; to act on what you see, call " +
		"capture(app='<AppName>') for that app's clickable element list, or capture(app='desktop') for " +
		"the desktop shell (wallpaper icons / taskbar) with elements"
)

// Argv and env reading _NET_ACTIVE_WINDOW on the display the driver acts on, nil argv without an X11 display
func activeWindowProbe(env *tools.Env) ([]string, map[string]string, error) {
	xprop := []string{"xprop", "-root", "_NET_ACTIVE_WINDOW"}
	if BotDesktopPlacementWhere(env) == botDesktopTerminal {
		published := BotDesktopPublishedEnv(env)
		if published["DISPLAY"] == "" {
			return nil, nil, nil
		}
		sandbox, err := BotDesktopSandboxEnv(env, false)
		if err != nil || sandbox == nil {
			return nil, nil, err
		}
		command, args, err := BotDesktopSandboxCommand(sandbox, published, xprop)
		if err != nil {
			return nil, nil, err
		}
		return append([]string{command}, args...), map[string]string{"PATH": os.Getenv("PATH")}, nil
	}
	if cuPlatform != "linux" {
		return nil, nil, nil
	}
	child := cuaDriverChildEnv(env, nil)
	if child["DISPLAY"] == "" {
		return nil, nil, nil
	}
	return xprop, child, nil
}

// X11 active window id of the driver display, nil on any failure, swapped by tests
var linuxX11ActiveWindowID = func(env *tools.Env) *int64 {
	argv, childEnv, err := activeWindowProbe(env)
	if err != nil || argv == nil {
		return nil
	}
	proc, err := runQuiet(argv, 5, runOpts{Env: childEnv})
	if err != nil || proc.ExitCode != 0 {
		return nil
	}
	if id, ok := parseXpropNetActiveWindow(proc.Stdout); ok {
		return &id
	}
	return nil
}

// Whether the driver runs on Linux, always for a desktop inside the terminal sandbox
func driverOnLinux(env *tools.Env) bool {
	return cuPlatform == "linux" || BotDesktopPlacementWhere(env) == botDesktopTerminal
}

// Text of a row field, the default when the row lacks it
func cuStrField(w *jsonutil.Object, key, def string) string {
	v, has := w.Get(key)
	if !has {
		return def
	}
	return anyutil.String(v)
}

// True for the authorization daemon's own window under its normalized app name
func isCuaDriverSelfWindow(w *jsonutil.Object) bool {
	name := strings.ToLower(strings.TrimSpace(cuStrField(w, "app_name", "")))
	var b strings.Builder
	for _, r := range name {
		if !unicode.IsSpace(r) && r != '_' && r != '-' {
			b.WriteRune(r)
		}
	}
	return b.String() == "cuadriver"
}

// Best window of a frontmost first list, skipping self windows and on Linux shell helpers and preferring the X11 active window
func selectCaptureTarget(windows []*jsonutil.Object, appRequested, exactTarget, linux bool, activeWindow func() *int64) *jsonutil.Object {
	var pool []*jsonutil.Object
	for _, w := range windows {
		if cuGet(w, "off_screen") != true {
			pool = append(pool, w)
		}
	}
	if !exactTarget {
		var external []*jsonutil.Object
		for _, w := range pool {
			if !isCuaDriverSelfWindow(w) {
				external = append(external, w)
			}
		}
		if len(external) > 0 {
			pool = external
		}
	}
	if !exactTarget && !appRequested && linux {
		var real []*jsonutil.Object
		for _, w := range pool {
			if isRealAppWindow(w) {
				real = append(real, w)
			}
		}
		if len(real) > 0 {
			pool = real
		}
		if len(pool) > 0 && zIndexUninformative(pool) {
			if active := activeWindow(); active != nil {
				for _, w := range pool {
					if wid, ok := cuGet(w, "window_id").(int); ok && int64(wid) == *active {
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

// Numeric value of a z_index for ordering, true and false as 1 and 0, zero for anything that is not a number
func zValue(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	}
	if n, ok := anyutil.Int(v); ok {
		return float64(n)
	}
	return 0
}

// Normalised list_windows rows ordered by z_index descending, ties keeping driver order
func sortedWindows(out *jsonutil.Object) []*jsonutil.Object {
	rows := ingestWindows(windowsFromToolResult(out))
	sort.SliceStable(rows, func(i, j int) bool {
		return zValue(cuGet(rows[i], "z_index")) > zValue(cuGet(rows[j], "z_index"))
	})
	return rows
}

// Tree markdown and window title of a get_window_state result
func treeAndTitle(out *jsonutil.Object) (string, string) {
	data, _ := cuGet(out, "data").(string)
	_, tree := splitTreeText(data)
	if m := axWindowTitleRE.FindStringSubmatch(tree); m != nil {
		return tree, m[1]
	}
	return tree, ""
}

// structuredContent of a result as an object, empty when absent
func scObject(out *jsonutil.Object) *jsonutil.Object {
	if sc, ok := cuGet(out, "structuredContent").(*jsonutil.Object); ok && anyutil.IsTrue(sc) {
		return sc
	}
	return jsonutil.NewObject()
}

// A get_window_state result carrying neither a screenshot nor elements nor a parseable tree
func gwsIsEmpty(out *jsonutil.Object) bool {
	sc := scObject(out)
	tree, _ := treeAndTitle(out)
	return !(anyutil.IsTrue(cuGet(out, "images")) || anyutil.IsTrue(cuGet(sc, "elements")) ||
		anyutil.IsTrue(cuGet(sc, "screenshot_png_b64")) || strings.TrimSpace(tree) != "")
}

// Byte length and size of a capture, the sniffed size winning when the bytes carry a readable header
func pngMetrics(pngB64 string, width, height int) (int, int, int) {
	raw, err := base64.StdEncoding.DecodeString(pngB64)
	if err != nil {
		return len(pngB64) * 3 / 4, width, height
	}
	dw, dh := imageDimensionsFromBytes(raw)
	if dw != 0 && dh != 0 {
		return len(raw), dw, dh
	}
	return len(raw), width, height
}

// The row names a desktop shell window
func isDesktopWindow(w *jsonutil.Object, names []string) bool {
	hay := strings.ToLower(cuStrField(w, "app_name", "") + " " + cuStrField(w, "title", ""))
	for _, n := range names {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

// Forgets the sticky target when a capture stage step failed
func (b *CuaDriverBackend) disarm(err error) error {
	if err != nil {
		b.clearActiveTarget()
	}
	return err
}

// Empty capture after disarming the prior target
func (b *CuaDriverBackend) failedCapture(mode, message string) *CaptureResult {
	b.clearActiveTarget()
	return &CaptureResult{Mode: mode, WindowTitle: message}
}

// Capture stage tool call that disarms on transport or logical failure
func (b *CuaDriverBackend) callCaptureTool(name string, args *jsonutil.Object) (*jsonutil.Object, error) {
	out, err := b.session.callTool(name, args, 30)
	if err != nil {
		return nil, b.disarm(err)
	}
	if cuGet(out, "isError") == true {
		msg := "cua-driver " + name + " failed"
		if s, ok := cuGet(out, "data").(string); ok && s != "" {
			msg += ": " + s
		}
		return nil, b.disarm(errors.New(msg))
	}
	return out, nil
}

// One shot CLI transport call after an empty MCP result, nil on failure
func (b *CuaDriverBackend) cliRefetch(name string, args *jsonutil.Object, timeout float64, what, warning string) *jsonutil.Object {
	pkgLog(nil).Warn(warning)
	cliOut, err := b.session.callToolViaCLIFn(name, args, timeout)
	if err != nil {
		pkgLog(nil).Error(fmt.Sprintf("cua-driver CLI re-fetch for %s failed: %v", what, err))
		return nil
	}
	if cuGet(cliOut, "isError") != true {
		return cliOut
	}
	if name == "list_windows" {
		pkgLog(nil).Error("cua-driver CLI re-fetch for list_windows returned an error")
	}
	b.clearActiveTarget()
	return nil
}

// Capture tool call whose empty result is replaced by a non empty CLI refetch
func (b *CuaDriverBackend) fetchOrRefetch(name string, args *jsonutil.Object, timeout float64, what string, empty func(*jsonutil.Object) bool, warning string) (*jsonutil.Object, error) {
	out, err := b.callCaptureTool(name, args)
	if err != nil {
		return nil, err
	}
	if empty(out) {
		if cliOut := b.cliRefetch(name, args, timeout, what, warning); cliOut != nil && !empty(cliOut) {
			return cliOut, nil
		}
	}
	return out, nil
}

// Visible windows frontmost first, refetched over the CLI transport when MCP returns none
func (b *CuaDriverBackend) ListWindows() ([]*jsonutil.Object, error) {
	args := jsonutil.NewObject()
	args.Set("on_screen_only", true)
	args.Set("session", b.sessionID)
	out, err := b.fetchOrRefetch("list_windows", args, 20.0, "list_windows",
		func(o *jsonutil.Object) bool { return len(sortedWindows(o)) == 0 },
		"cua-driver list_windows returned no windows over MCP; re-fetching via CLI transport")
	if err != nil {
		return nil, err
	}
	return sortedWindows(out), nil
}

// Windows of app by exact name, then exact list_apps alias, then substrings, then nameless titles
func (b *CuaDriverBackend) matchWindowsForApp(windows []*jsonutil.Object, app string) []*jsonutil.Object {
	appLower := strings.ToLower(strings.TrimSpace(app))
	name := func(w *jsonutil.Object) string { return strings.ToLower(cuStrField(w, "app_name", "")) }
	var directExact []*jsonutil.Object
	for _, w := range windows {
		if appLower != "" && appLower == strings.TrimSpace(name(w)) {
			directExact = append(directExact, w)
		}
	}
	if appLower == "" || len(directExact) > 0 {
		return directExact
	}
	runningApps, err := b.ListApps()
	if err != nil {
		pkgLog(nil).Debug(fmt.Sprintf("computer_use list_apps fallback failed for %s: %v", errchain.Show(app), err))
		runningApps = nil
	}
	exactPIDs, partialPIDs := map[int]bool{}, map[int]bool{}
	for _, raw := range runningApps {
		a, ok := raw.(*jsonutil.Object)
		if !ok {
			continue
		}
		pid, ok := positiveInt(cuGet(a, "pid"))
		if !ok || cuGet(a, "running") == false {
			continue
		}
		aliases := map[string]bool{}
		for _, key := range []string{"bundle_id", "bundleId", "name", "app_name", "display_name"} {
			if v, ok := cuGet(a, key).(string); ok && strings.TrimSpace(v) != "" {
				aliases[strings.ToLower(strings.TrimSpace(v))] = true
			}
		}
		if aliases[appLower] {
			exactPIDs[pid] = true
			continue
		}
		for alias := range aliases {
			if strings.Contains(alias, appLower) {
				partialPIDs[pid] = true
				break
			}
		}
	}
	pidIn := func(w *jsonutil.Object, set map[int]bool) bool {
		pid, ok := cuGet(w, "pid").(int)
		return ok && set[pid]
	}
	tiers := []func(*jsonutil.Object) bool{
		func(w *jsonutil.Object) bool { return pidIn(w, exactPIDs) },
		func(w *jsonutil.Object) bool { return strings.Contains(name(w), appLower) },
		func(w *jsonutil.Object) bool { return pidIn(w, partialPIDs) },
		func(w *jsonutil.Object) bool {
			return strings.TrimSpace(name(w)) == "" && strings.Contains(strings.ToLower(cuStrField(w, "title", "")), appLower)
		},
	}
	for _, tier := range tiers {
		var matched []*jsonutil.Object
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

// Candidate windows of a capture, or the failed capture that explains why there are none
func (b *CuaDriverBackend) resolveCaptureWindows(mode string, app *string, pid, windowID *int) ([]*jsonutil.Object, *CaptureResult, error) {
	if pid != nil || windowID != nil {
		if pid == nil || windowID == nil {
			return nil, b.failedCapture(mode, "<capture targeting requires both pid and window_id>"), nil
		}
		tp, okP := positiveInt(*pid)
		tw, okW := positiveInt(*windowID)
		if !okP || !okW {
			return nil, b.failedCapture(mode, "<capture targeting requires positive integer pid and window_id>"), nil
		}
		row := jsonutil.NewObject()
		appName := ""
		if app != nil {
			appName = *app
		}
		row.Set("app_name", appName)
		row.Set("pid", tp)
		row.Set("window_id", tw)
		row.Set("off_screen", false)
		row.Set("title", "")
		row.Set("z_index", 0)
		return []*jsonutil.Object{row}, nil, nil
	}
	windows, err := b.ListWindows()
	if err != nil {
		return nil, nil, b.disarm(err)
	}
	if len(windows) == 0 {
		return nil, b.failedCapture(mode, emptyDiscoveryReason()), nil
	}
	if app == nil || *app == "" {
		return windows, nil, nil
	}
	if desktopShellSentinels[strings.ToLower(strings.TrimSpace(*app))] {
		var desktop []*jsonutil.Object
		for _, w := range windows {
			if isDesktopWindow(w, desktopWindowNames) {
				desktop = append(desktop, w)
			}
		}
		sort.SliceStable(desktop, func(i, j int) bool {
			return isDesktopWindow(desktop[i], desktopBackdropNames) && !isDesktopWindow(desktop[j], desktopBackdropNames)
		})
		if len(desktop) == 0 {
			return nil, b.failedCapture(mode, fmt.Sprintf(noDesktopWindowMsg, errchain.Show(*app))), nil
		}
		return desktop, nil, nil
	}
	if matched := b.matchWindowsForApp(windows, *app); len(matched) > 0 {
		return matched, nil, nil
	}
	return nil, b.failedCapture(mode, fmt.Sprintf(noAppMatchMsg, errchain.Show(*app))), nil
}

// get_window_state args bounding the driver's accessibility walk by the configured element cap
func (b *CuaDriverBackend) gwsArgs() (*jsonutil.Object, error) {
	args := jsonutil.NewObject()
	args.Set("pid", intPtrValue(b.activePID))
	args.Set("window_id", intPtrValue(b.activeWindowID))
	args.Set("session", b.sessionID)
	if capped := cuaConfiguredAXMaxElements(b.env); capped != 0 {
		args.Set("max_elements", capped)
	}
	return args, nil
}

// Value of an optional int, nil for None
func intPtrValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// Pixels only capture through screenshot, then get_window_state, then the CLI transport
func (b *CuaDriverBackend) captureVision() (*string, *string, []UIElement, string, error) {
	var png, mime *string
	title := ""
	if b.session.hasTool("screenshot") || !b.session.capabilitiesDiscovered() {
		args := jsonutil.NewObject()
		args.Set("window_id", intPtrValue(b.activeWindowID))
		args.Set("format", "jpeg")
		args.Set("quality", 85)
		args.Set("session", b.sessionID)
		out, err := b.callCaptureTool("screenshot", args)
		if err != nil {
			return nil, nil, nil, "", err
		}
		png, mime = imageFromToolResult(out)
	}
	if png == nil || *png == "" {
		args, err := b.gwsArgs()
		if err != nil {
			return nil, nil, nil, "", err
		}
		out, err := b.callCaptureTool("get_window_state", args)
		if err != nil {
			return nil, nil, nil, "", err
		}
		png, mime = imageFromToolResult(out)
		_, title = treeAndTitle(out)
	}
	if png == nil || *png == "" {
		args, err := b.gwsArgs()
		if err != nil {
			return nil, nil, nil, "", err
		}
		cliOut := b.cliRefetch("get_window_state", args, 30.0, "vision screenshot",
			fmt.Sprintf("cua-driver vision capture returned no image over MCP (window_id=%s); re-fetching via CLI transport",
				anyutil.String(intPtrValue(b.activeWindowID))))
		if cliOut == nil {
			cliOut = jsonutil.NewObject()
		}
		if images, _ := cuGet(cliOut, "images").([]any); len(images) > 0 {
			if s, ok := images[0].(string); ok {
				m := "image/png"
				png, mime = &s, &m
			}
		}
	}
	return png, mime, []UIElement{}, title, nil
}

// Accessibility tree plus screenshot of the sticky target with the snapshot tokens recorded
func (b *CuaDriverBackend) captureWindowState() (*string, *string, []UIElement, string, error) {
	args, err := b.gwsArgs()
	if err != nil {
		return nil, nil, nil, "", err
	}
	out, err := b.fetchOrRefetch("get_window_state", args, 30.0, "get_window_state", gwsIsEmpty,
		fmt.Sprintf("cua-driver get_window_state returned an empty result over MCP (pid=%s window_id=%s); re-fetching via CLI transport",
			anyutil.String(intPtrValue(b.activePID)), anyutil.String(intPtrValue(b.activeWindowID))))
	if err != nil {
		return nil, nil, nil, "", err
	}
	tree, title := treeAndTitle(out)
	var elements []UIElement
	if sc, ok := cuGet(scObject(out), "elements").([]any); ok && len(sc) > 0 {
		if elements, err = parseElementsFromStructured(sc); err != nil {
			return nil, nil, nil, "", err
		}
	} else if tree != "" {
		elements = parseElementsFromTree(tree)
	} else {
		elements = []UIElement{}
	}
	b.snapshotTokens = map[int]string{}
	for _, e := range elements {
		if e.ElementToken != nil && *e.ElementToken != "" {
			b.snapshotTokens[e.Index] = *e.ElementToken
		}
	}
	png, mime := imageFromToolResult(out)
	return png, mime, elements, title, nil
}

// Captures the frontmost window or an exact known target in som, ax or vision mode
func (b *CuaDriverBackend) Capture(mode string, app *string, pid, windowID *int) (*CaptureResult, error) {
	if pid != nil && isSchemaFillerID(*pid) {
		pid = nil
	}
	if windowID != nil && isSchemaFillerID(*windowID) {
		windowID = nil
	}
	exactTarget := pid != nil || windowID != nil
	if !exactTarget && app != nil && *app != "" && fullScreenSentinels[strings.ToLower(strings.TrimSpace(*app))] {
		return b.captureFullScreen(mode)
	}
	windows, failed, err := b.resolveCaptureWindows(mode, app, pid, windowID)
	if err != nil || failed != nil {
		return failed, err
	}
	appRequested := app != nil && *app != ""
	target := selectCaptureTarget(windows, appRequested, exactTarget, driverOnLinux(b.env), func() *int64 { return linuxX11ActiveWindowID(b.env) })
	b.setActiveTarget(target)
	appName, _ := cuGet(target, "app_name").(string)
	if appRequested || b.lastApp == nil || *b.lastApp == "" {
		last := appName
		if last == "" && app != nil {
			last = *app
		}
		b.lastApp = &last
	}
	var png, mime *string
	var elements []UIElement
	var title string
	if mode == "vision" {
		png, mime, elements, title, err = b.captureVision()
	} else {
		png, mime, elements, title, err = b.captureWindowState()
	}
	if err != nil {
		return nil, err
	}
	n, width, height := 0, 0, 0
	if png != nil && *png != "" {
		n, width, height = pngMetrics(*png, 0, 0)
	}
	axMax := 0
	if mode != "vision" {
		args, err := b.gwsArgs()
		if err != nil {
			return nil, err
		}
		if v, ok := cuGet(args, "max_elements").(int); ok {
			axMax = v
		}
	}
	return &CaptureResult{Mode: mode, Width: width, Height: height, PNGB64: png, Elements: elements, App: appName,
		WindowTitle: title, PNGBytesLen: n, ImageMimeType: mime, AXMaxElements: axMax}, nil
}

// Composited whole screen grab through get_desktop_state with capture_scope switched and restored
func (b *CuaDriverBackend) captureFullScreen(mode string) (*CaptureResult, error) {
	b.clearActiveTarget()
	var previous *string
	cfgArgs := jsonutil.NewObject()
	cfgArgs.Set("session", b.sessionID)
	if out, err := b.session.callTool("get_config", cfgArgs, 10.0); err != nil {
		pkgLog(nil).Debug(fmt.Sprintf("cua-driver get_config before full-screen capture failed: %v", err))
	} else if sc, ok := cuGet(out, "structuredContent").(*jsonutil.Object); ok {
		if s, ok := cuGet(sc, "capture_scope").(string); ok {
			previous = &s
		}
	}
	setScope := func(value string) error {
		args := jsonutil.NewObject()
		args.Set("key", "capture_scope")
		args.Set("value", value)
		args.Set("session", b.sessionID)
		_, err := b.session.callTool("set_config", args, 10.0)
		return err
	}
	out, err := func() (*jsonutil.Object, error) {
		defer func() {
			if previous != nil && *previous != "" && *previous != "desktop" {
				if err := setScope(*previous); err != nil {
					pkgLog(nil).Debug(fmt.Sprintf("cua-driver restore capture_scope failed: %v", err))
				}
			}
		}()
		if previous == nil || *previous != "desktop" {
			if err := setScope("desktop"); err != nil {
				return nil, err
			}
		}
		args := jsonutil.NewObject()
		args.Set("session", b.sessionID)
		return b.callCaptureTool("get_desktop_state", args)
	}()
	if err != nil {
		return nil, err
	}
	png, mime := imageFromToolResult(out)
	if png == nil || *png == "" {
		return b.failedCapture(mode, noDesktopImageMsg), nil
	}
	sc := scObject(out)
	dim := func(a, c string) (int, error) {
		v := cuGet(sc, a)
		if !anyutil.IsTrue(v) {
			v = cuGet(sc, c)
		}
		if !anyutil.IsTrue(v) {
			return 0, nil
		}
		n, err := anyutil.ToInt(v)
		return int(n), err
	}
	w, err := dim("screenshot_width", "screen_width")
	if err != nil {
		return nil, err
	}
	h, err := dim("screenshot_height", "screen_height")
	if err != nil {
		return nil, err
	}
	n, width, height := pngMetrics(*png, w, h)
	return &CaptureResult{Mode: "vision", Width: width, Height: height, PNGB64: png, Elements: []UIElement{}, App: "screen",
		WindowTitle: "Full screen (composited)", PNGBytesLen: n, ImageMimeType: mime, Note: fullScreenNote}, nil
}

// Running apps from structuredContent, the compatibility envelopes, the windows payload, then the text listing
func (b *CuaDriverBackend) ListApps() ([]any, error) {
	args := jsonutil.NewObject()
	args.Set("session", b.sessionID)
	out, err := b.session.callTool("list_apps", args, 30)
	if err != nil {
		return nil, err
	}
	structured, data := cuGet(out, "structuredContent"), cuGet(out, "data")
	candidates := []func() any{
		func() any {
			if s, ok := structured.(*jsonutil.Object); ok {
				return cuGet(s, "apps")
			}
			return nil
		},
		func() any { return data },
		func() any {
			if d, ok := data.(*jsonutil.Object); ok {
				return cuGet(d, "apps")
			}
			return nil
		},
		func() any { return cuGet(out, "apps") },
		func() any {
			apps := appsFromWindows(windowsFromToolResult(out))
			list := make([]any, len(apps))
			for i, a := range apps {
				list[i] = a
			}
			return list
		},
	}
	for _, c := range candidates {
		if apps, ok := c().([]any); ok && len(apps) > 0 {
			return apps, nil
		}
	}
	text, ok := data.(string)
	if !ok {
		return []any{}, nil
	}
	apps := []any{}
	for _, line := range textutil.Lines(text) {
		m := textAppPIDRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[2])
		app := jsonutil.NewObject()
		app.Set("name", strings.TrimSpace(m[1]))
		app.Set("pid", pid)
		apps = append(apps, app)
	}
	return apps, nil
}

// Selects the app's window as the sticky target, raising it only through the approved bring_to_front tool
func (b *CuaDriverBackend) FocusApp(app string, raiseWindow bool) (*ActionResult, error) {
	windows, err := b.ListWindows()
	if err != nil {
		return nil, b.disarm(err)
	}
	matched := b.matchWindowsForApp(windows, app)
	if len(matched) == 0 {
		b.clearActiveTarget()
		return refuseAction("focus_app", fmt.Sprintf("No on-screen window found for app '%s'.", app)), nil
	}
	target := matched[0]
	b.setActiveTarget(target)
	name, _ := cuGet(target, "app_name").(string)
	last := name
	if last == "" {
		last = app
	}
	b.lastApp = &last
	if !raiseWindow {
		return &ActionResult{OK: true, Action: "focus_app", Meta: jsonutil.NewObject(), Message: fmt.Sprintf(
			"Targeted %s (pid %s, window %s) without raising window.", name,
			anyutil.String(intPtrValue(b.activePID)), anyutil.String(intPtrValue(b.activeWindowID)))}, nil
	}
	if !b.session.hasTool("bring_to_front") {
		code := "bring_to_front_unsupported"
		r := refuseAction("focus_app", btfUnsupportedMsg)
		r.Code = &code
		return r, nil
	}
	focused := b.BringToFront(*b.activePID, b.activeWindowID)
	if focused.OK {
		focused.Action = "focus_app"
		focused.Meta.Set("target_selected", true)
	}
	return focused, nil
}
