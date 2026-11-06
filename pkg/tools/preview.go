package tools

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
)

var (
	previewLocalHostRe = regexp.MustCompile(`(?i)^(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(:\d+)?(/|$)`)
	previewDomainRe    = regexp.MustCompile(`(?i)^[\w.-]+\.[a-z]{2,}(:\d+)?(/.*)?$`)
	pyIntLiteralRe     = regexp.MustCompile(`^[+-]?\d+$`)
)

// Coaxes a bare host or domain into a fetchable URL and leaves paths and schemes alone
func normalizePreviewTarget(raw string) string {
	v := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "`"))
	if v == "" || strings.Contains(v, "://") || hasAnyPrefix(v, []string{"/", "./", "../", "~", "file:"}) {
		return v
	}
	if previewLocalHostRe.MatchString(v) {
		return "http://" + v
	}
	if previewDomainRe.MatchString(v) {
		return "https://" + v
	}
	return v
}

// Filesystem path for local targets, empty for http(s) URLs
func previewLocalFSPath(target string) string {
	raw := strings.TrimSpace(target)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || strings.ToLower(parsed.Scheme) != "file" {
			return ""
		}
		path := parsed.Path
		if unescaped, err := url.PathUnescape(path); err == nil {
			path = unescaped
		}
		if parsed.Host != "" && parsed.Host != "localhost" {
			path = "//" + parsed.Host + path
		} else if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		return path
	}
	return expandTilde(raw)
}

// True only when the target is positively observed to be an existing directory
func previewIsExistingDirectory(target string) bool {
	path := previewLocalFSPath(target)
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Asks the desktop GUI to show the url in the preview pane beside the chat
func openPreviewTool(env *Env, rawURL, label string) string {
	target := normalizePreviewTarget(rawURL)
	if target == "" {
		return toolErrorJSON("url is required — a web URL (https://…), a localhost dev server, or a "+
			"file path to show in the preview pane.", nil)
	}
	if previewIsExistingDirectory(target) {
		return toolErrorJSON(fmt.Sprintf("directories are not previewable — pass a file path or a URL. %s is a directory.", target), nil)
	}
	label = strings.TrimSpace(label)
	return desktopEmitOrError(env, "preview.open", map[string]any{"url": target, "label": label},
		"Failed to open the preview pane: ", "The preview pane is only available in the Icebath desktop app.",
		pyObject("success", true, "url", target, "label", label))
}

// Asks the desktop GUI to close the preview pane, or the tab for the url
func previewClose(env *Env, rawURL string) string {
	target := normalizePreviewTarget(rawURL)
	closed := target
	if closed == "" {
		closed = "all"
	}
	return desktopEmitOrError(env, "preview.close", map[string]any{"url": target},
		"Failed to close the preview: ", "The preview pane is only available in the Icebath desktop app.",
		pyObject("success", true, "closed", closed))
}

// ReadPreviewPane reads the active preview tab through the gateway bridge, installed by the desktop gateway
var ReadPreviewPane func(env *Env, window map[string]int) (string, error)

// Returns the active preview tab's contents and metadata as a JSON string
func readPreviewTool(env *Env, start, count any) string {
	if ReadPreviewPane == nil {
		return toolErrorJSON("preview read must run inside a desktop session (no GUI callback here).", nil)
	}
	return readPane(env, ReadPreviewPane, []paneWindow{{"start", start, 0}, {"count", count, 1}}, [4]string{
		"read_preview is only available in the Icebath desktop app.",
		"start and count must be integers.",
		"Failed to read the preview pane: ",
		"No preview tab answered: no page is loaded (call open_preview first), " +
			"or the bridge timed out. If the pane IS open, the desktop app may be older " +
			"than this backend — update it and retry.",
	})
}

// Dispatches the desktop_preview action
func handlePreview(ctx context.Context, call *Call) string {
	switch strings.TrimSpace(argS(call.Args, "action")) {
	case "open":
		return openPreviewTool(call.Env, argS(call.Args, "url"), argS(call.Args, "label"))
	case "close":
		return previewClose(call.Env, argS(call.Args, "url"))
	case "read":
		return readPreviewTool(call.Env, call.Args["start"], call.Args["count"])
	}
	return toolErrorJSON("action must be one of: open, close, read.", nil)
}

var previewSchema = map[string]any{
	"name": "desktop_preview",
	"description": "Open, close, or read the preview pane beside the chat. open: show " +
		"a web URL (bare domains fine), a localhost dev server, or a file path " +
		"(HTML renders live) — opens for the current window only. close: dismiss " +
		"the whole pane, or one tab via url. read: what the pane currently shows " +
		"— returns {kind, url, title, text, start, end, total_chars}; a Browser " +
		"tab's text is the rendered page's visible text, paged with start/count " +
		"(char offsets); a file tab answers identity only (read the file with " +
		"read_file).",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []any{"open", "close", "read"}},
			"url":    map[string]any{"type": "string", "description": "open: the target. close: one tab (omit for the whole pane)."},
			"label":  map[string]any{"type": "string", "description": "open: optional tab label."},
			"start":  map[string]any{"type": "integer", "description": "read: 0-indexed char offset."},
			"count":  map[string]any{"type": "integer", "description": "read: chars to return (capped per read)."},
		},
		"required": []any{"action"},
	},
}

// PreviewAct sends one preview.act request to the renderer and returns its raw reply, installed by the desktop gateway
var PreviewAct func(env *Env, payload map[string]any) (string, error)

var (
	annotateActions = []string{"add", "hold", "remove", "clear"}
	annotateWire    = map[string]string{"add": "pin", "hold": "hold", "remove": "unpin", "clear": "unpin"}
)

const previewNoPageText = "No GUI window answered with a page: no preview tab is open. " +
	"Open a page with open_preview first. If the pane IS open, the desktop app " +
	"may be older than this backend — its bridge-unavailable error names that case."

// Puts one annotation up, takes one down, or clears them all
func annotatePreviewTool(env *Env, action string, ref, selector, label *string) string {
	if PreviewAct == nil {
		return toolErrorJSON("annotate_preview is only available in the Icebath desktop app.", nil)
	}
	verb := strings.ToLower(strings.TrimSpace(action))
	if verb == "" {
		verb = "add"
	}
	if !containsString(annotateActions, verb) {
		return toolErrorJSON(fmt.Sprintf("action must be one of: %s.", joinEnum(annotateActions)), nil)
	}
	if (verb == "add" || verb == "remove") && derefEmpty(ref) && derefEmpty(selector) {
		return toolErrorJSON(fmt.Sprintf("%s needs a ref from drive_preview action='elements' (e.g. 'btn-sign-in') or a CSS selector.", verb), nil)
	}
	targeted := verb != "clear" && verb != "hold"
	payload := map[string]any{"action": annotateWire[verb]}
	if targeted && ref != nil {
		payload["ref"] = *ref
	}
	if targeted && selector != nil {
		payload["selector"] = *selector
	}
	if label != nil {
		payload["text"] = *label
	}
	raw, err := PreviewAct(env, payload)
	if err != nil {
		return toolErrorJSON("Failed to annotate the in-app browser: "+err.Error(), nil)
	}
	if raw == "" {
		return toolErrorJSON(previewNoPageText, nil)
	}
	return desktopPassthroughJSON(raw)
}

// Reports whether an optional string is absent or empty
func derefEmpty(s *string) bool { return s == nil || *s == "" }

var annotatePreviewSchema = map[string]any{
	"name": "annotate_preview",
	"description": "Highlight preview-pane elements with lasting marks (drive_preview's fade). " +
		"Point at findings, flag what you're about to change, keep your place. Use the refs " +
		"from drive_preview action='elements'. add: outline one element " +
		"(optional short label — a word or two, drawn on the page). hold: " +
		"freeze the whole visible field, every element outlined and named. " +
		"remove/clear: take one/all down. Marks follow their element on " +
		"scroll; navigation clears them.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": enumValues(annotateActions), "description": "Defaults to 'add'."},
			"ref":      map[string]any{"type": "string", "description": "Ref from drive_preview elements."},
			"selector": map[string]any{"type": "string", "description": "CSS selector fallback. Prefer ref."},
			"label":    map[string]any{"type": "string", "description": "Optional caption, e.g. 'cheapest'."},
		},
		"required": []any{},
	},
}

var (
	driveActions     = []string{"elements", "click", "hover", "type", "scroll", "press", "strobe", "back", "forward", "reload"}
	driveScrollTo    = []string{"top", "bottom"}
	driveNeedsTarget = []string{"click", "hover", "type", "press"}
)

// Dispatches one preview interaction to the desktop renderer and returns its outcome
func handleDrivePreview(ctx context.Context, call *Call) string {
	args := call.Args
	if PreviewAct == nil {
		return toolErrorJSON("drive_preview is only available in the Icebath desktop app.", nil)
	}
	verb := strings.ToLower(strings.TrimSpace(argS(args, "action")))
	if !containsString(driveActions, verb) {
		return toolErrorJSON(fmt.Sprintf("action must be one of: %s.", joinEnum(driveActions)), nil)
	}
	ref, selector, text, key, to := optString(args, "ref"), optString(args, "selector"), optString(args, "text"), optString(args, "key"), optString(args, "to")
	if containsString(driveNeedsTarget, verb) && derefEmpty(ref) && derefEmpty(selector) {
		return toolErrorJSON(fmt.Sprintf("%s needs a ref from action='elements' (e.g. 'btn-sign-in') or a CSS selector.", verb), nil)
	}
	if verb == "type" && text == nil {
		return toolErrorJSON("type needs the text to enter.", nil)
	}
	if verb == "press" && derefEmpty(key) {
		return toolErrorJSON("press needs a key, e.g. 'Enter' or 'Escape'.", nil)
	}
	if to != nil && !containsString(driveScrollTo, *to) {
		return toolErrorJSON(fmt.Sprintf("to must be one of: %s.", joinEnum(driveScrollTo)), nil)
	}
	payload := map[string]any{"action": verb}
	for name, v := range map[string]*string{"ref": ref, "selector": selector, "text": text, "key": key, "to": to} {
		if v != nil {
			payload[name] = *v
		}
	}
	for _, name := range []string{"submit", "full", "allow_shortcut"} {
		if argHas(args, name) {
			payload[name] = args[name]
		}
	}
	for arg, wire := range map[string]string{"amount": "amount", "max": "max"} {
		if !argHas(args, arg) {
			continue
		}
		n, ok := pyIntValue(args[arg])
		if !ok {
			return toolErrorJSON("amount and max must be integers.", nil)
		}
		payload[wire] = n
	}
	raw, err := PreviewAct(call.Env, payload)
	if err != nil {
		return toolErrorJSON("Failed to act on the in-app browser: "+err.Error(), nil)
	}
	if raw == "" {
		return toolErrorJSON(previewNoPageText, nil)
	}
	return desktopPassthroughJSON(raw)
}

// Integer the way Python int() accepts a JSON value, truncating floats and parsing digit strings
func pyIntValue(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		var out int
		if trimmed := strings.TrimSpace(n); pyIntLiteralRe.MatchString(trimmed) {
			fmt.Sscanf(trimmed, "%d", &out)
			return out, true
		}
	}
	return 0, false
}

var drivePreviewSchema = map[string]any{
	"name": "drive_preview",
	"description": "Use the desktop preview-pane page: log in, fill forms, click through flows. " +
		"It is the page `desktop_preview` opens. ALWAYS " +
		"start with action='elements' — it inventories clickable/typable " +
		"things as refs ('btn-sign-in') with role/label/value; act by ref, " +
		"not guessed selectors. Refs survive re-renders and only die on " +
		"navigation (you'll be told they're stale — call elements again). " +
		"After the first full inventory, actions answer with a DELTA: " +
		"'added' in full, 'changed' as ref + moved fields, 'removed'/" +
		"'rebound' as ref lists ('rebound' needs nothing from you — the ref " +
		"already follows the rebuilt element). Anything unmentioned is " +
		"unchanged; do not re-read to check. Input is real (pointer travels, " +
		"hover menus open). Actions: elements, click, hover (park the " +
		"pointer — opens dropdowns before clicking in), type (submit=true " +
		"also presses Enter), scroll, press, strobe (visual flourish only — " +
		"one call runs a multi-second burst; never loop it), back/forward/" +
		"reload. Moves draw live and fade; annotate_preview leaves a lasting " +
		"mark. A printable press on body/html is refused unless allow_shortcut " +
		"is true. Page text only: desktop_preview action=read. Separate automated " +
		"browser: browser_* tools.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":         map[string]any{"type": "string", "enum": enumValues(driveActions), "description": "Start with 'elements'."},
			"ref":            map[string]any{"type": "string", "description": "Element ref from an earlier elements call."},
			"selector":       map[string]any{"type": "string", "description": "CSS selector fallback. Prefer ref."},
			"text":           map[string]any{"type": "string", "description": "type: the text."},
			"submit":         map[string]any{"type": "boolean", "description": "type: press Enter + submit the form after."},
			"key":            map[string]any{"type": "string", "description": "press: key name ('Enter', 'Escape', 'ArrowDown')."},
			"allow_shortcut": map[string]any{"type": "boolean", "description": "press: allow a printable key on body/html. Off by default."},
			"amount":         map[string]any{"type": "integer", "description": "scroll: pixels (negative = up; default ~one screen)."},
			"to":             map[string]any{"type": "string", "enum": enumValues(driveScrollTo), "description": "scroll: jump to top/bottom instead."},
			"max":            map[string]any{"type": "integer", "description": "elements: cap the inventory."},
			"full":           map[string]any{"type": "boolean", "description": "elements: full re-read instead of a delta. Rarely needed."},
		},
		"required": []any{"action"},
	},
}
