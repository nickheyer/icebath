package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools"
)

const cdpDocs = "https://chromedevtools.github.io/devtools-protocol/"

const cdpAbout = "Send a raw Chrome DevTools Protocol (CDP) command. Escape hatch for browser operations the other browser tools don't cover.\n\n" +
	"**Requires a reachable CDP endpoint.** Available when the user has run '/browser connect' to attach to a running Chrome, Brave, Chromium, or " +
	"Edge browser, or when 'browser.cdp_url' is set in config.yaml. Not currently wired up for cloud backends (Browserbase, Browser Use, " +
	"Firecrawl) — those expose CDP per session but live-session routing is a follow-up. If the tool is in your toolset at all, a CDP endpoint " +
	"is already reachable.\n\n" +
	"**CDP method reference:** " + cdpDocs + " — use an available documentation lookup or extraction tool on a method's URL (e.g. " +
	"'/tot/Page/#method-handleJavaScriptDialog') to look up parameters and return shape.\n\n" +
	"**Common patterns:**\n" +
	"- List tabs: method='Target.getTargets', params={}\n" +
	"- Handle a native JS dialog: method='Page.handleJavaScriptDialog', params={'accept': true, 'promptText': ''}, target_id=<tabId>\n" +
	"- Get all cookies: method='Network.getAllCookies', params={}\n" +
	"- Eval in a specific tab: method='Runtime.evaluate', params={'expression': '...', 'returnByValue': true}, target_id=<tabId>\n" +
	"- Set viewport for a tab: method='Emulation.setDeviceMetricsOverride', params={'width': 1280, 'height': 720, 'deviceScaleFactor': 1, " +
	"'mobile': false}, target_id=<tabId>\n\n" +
	"**Usage rules:**\n" +
	"- Browser-level methods (Target.*, Browser.*, Storage.*): omit target_id and frame_id.\n" +
	"- Page-level methods (Page.*, Runtime.*, DOM.*, Emulation.*, Network.* scoped to a tab): pass target_id from Target.getTargets.\n" +
	"- **Cross-origin iframe scope** (Runtime.evaluate inside an OOPIF, Page.* targeting a frame target, etc.): pass a frame_id from " +
	"Page.getFrameTree. This routes through the CDP supervisor's live connection — the only reliable way on Browserbase where stateless CDP " +
	"calls hit signed-URL expiry.\n" +
	"- Each stateless call (without frame_id) is independent — sessions and event subscriptions do not persist between calls. For stateful " +
	"workflows, prefer the dedicated browser tools or use frame_id routing."

func cdpSchema() map[string]any {
	return map[string]any{"description": cdpAbout, "parameters": map[string]any{"type": "object", "required": []string{"method"},
		"properties": map[string]any{
			"method": tools.Property("string", "CDP method name, e.g. 'Target.getTargets', 'Runtime.evaluate', 'Page.handleJavaScriptDialog'."),
			"params": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true,
				"description": "Method-specific parameters as a JSON object. Omit or pass {} for methods that take no parameters."},
			"target_id": tools.Property("string", "Optional. Target/tab ID from Target.getTargets result (each entry's 'targetId'). Use for "+
				"page-level methods at the top-level tab scope. Mutually exclusive with frame_id."),
			"frame_id": tools.Property("string", "Optional. Out-of-process iframe (OOPIF) frame id from Page.getFrameTree. When set, routes the "+
				"call through the CDP supervisor's live session for that iframe. Essential for Runtime.evaluate inside cross-origin iframes, "+
				"especially on Browserbase where fresh per-call CDP connections can't keep up with signed URL rotation. For same-origin iframes, "+
				"use parent contentWindow/contentDocument from Runtime.evaluate at the top-level page instead."),
			"timeout": map[string]any{"type": "number", "default": 30, "description": "Timeout in seconds (default 30, max 300)."},
		}}}
}

// Methods that inspect or manage tabs and never read a page's body, cookies, DOM or storage
var pageSafe = []string{"Browser.getVersion", "Target.getTargets", "Target.attachToTarget", "Target.detachFromTarget", "Target.createTarget",
	"Target.activateTarget", "Target.closeTarget", "Page.navigate", "Page.reload", "Page.stopLoading"}

// Result paths that always carry opaque base64, and those that do beside a base64Encoded flag
var (
	alwaysBinary = map[string][][]string{"Page.captureScreenshot": {{"data"}}, "Page.printToPDF": {{"data"}},
		"Network.streamResourceContent": {{"bufferedData"}}, "HeadlessExperimental.beginFrame": {{"screenshotData"}},
		"CacheStorage.requestCachedResponse": {{"response", "body"}}}
	flaggedBinary = map[string][][]string{"Network.getResponseBody": {{"body"}}, "Fetch.getResponseBody": {{"body"}}, "IO.read": {{"data"}},
		"Network.getRequestPostData": {{"postData"}}}
)

const (
	walkDepth  = 320
	walkMarker = "[redacted: nesting limit]"
)

func leaf(paths [][]string, key string) bool {
	return slices.ContainsFunc(paths, func(p []string) bool { return len(p) == 1 && p[0] == key })
}

func descend(paths [][]string, key string) [][]string {
	var out [][]string
	for _, p := range paths {
		if len(p) > 1 && p[0] == key {
			out = append(out, p[1:])
		}
	}
	return out
}

// Text of a result the browser sent with its secrets masked, the declared binary paths left byte for byte
func redactResult(value any, always, flagged [][]string, depth int) any {
	if depth >= walkDepth {
		return walkMarker
	}
	switch v := value.(type) {
	case string:
		return pageText(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactResult(item, nil, nil, depth+1)
		}
		return out
	case map[string]any:
		encoded := v["base64Encoded"] == true
		out := make(map[string]any, len(v))
		for key, item := range v {
			opaque := leaf(always, key) || leaf(flagged, key) && encoded
			if s, ok := item.(string); ok && opaque {
				out[pageText(key)] = s
				continue
			}
			out[pageText(key)] = redactResult(item, descend(always, key), descend(flagged, key), depth+1)
		}
		return out
	}
	return value
}

// Error result, the message first and the extra keys after it in order
func toolErr(message string, extra ...any) string {
	result := map[string]any{"error": message}
	order := []string{"error"}
	for i := 0; i+1 < len(extra); i += 2 {
		result[extra[i].(string)] = extra[i+1]
		order = append(order, extra[i].(string))
	}
	return orderedJSON(result, order...)
}

func cdpBlocked(message, method string) string {
	return toolErr(message, "method", method, "cdp_docs", cdpDocs)
}

func paramStr(params map[string]any, key string) string {
	v := params[key]
	if !config.Truthy(v) {
		return ""
	}
	return fmt.Sprint(v)
}

type paramGuard struct {
	probe    func(params map[string]any, blocked func(string) bool) string
	template string
}

// Refused URL a Page.navigate names, empty when allowed
func navigateTarget(params map[string]any, blocked func(string) bool) string {
	if target := strings.TrimSpace(paramStr(params, "url")); target != "" && blocked(target) {
		return target
	}
	return ""
}

var paramGuards = map[string]paramGuard{
	"Page.navigate": {navigateTarget, "Blocked: CDP Page.navigate target is a private or internal address (%s)."},
	"Target.createTarget": {func(params map[string]any, blocked func(string) bool) string {
		if strings.ToLower(strings.TrimSpace(paramStr(params, "url"))) == "about:blank" {
			return ""
		}
		return navigateTarget(params, blocked)
	}, "Blocked: CDP Target.createTarget url is a private or internal address (%s)."},
	"Runtime.evaluate": {func(params map[string]any, blocked func(string) bool) string {
		return scriptTargets(paramStr(params, "expression"), blocked)
	}, "Blocked: CDP Runtime.evaluate expression targets a private or internal address (%s)."},
}

const unverified = "<unverified>"

var originless = []string{"about:", "chrome:", "chrome-untrusted:", "chrome-search:", "devtools:", "edge:", "brave:"}

// Private page a call off the safe list would reach, unverified when the targets can't be read
func (r *registrant) privatePage(ctx context.Context, cfg *config.Config, endpoint, target string) string {
	if endpoint == "" {
		return unverified
	}
	raw, err := freshCall(ctx, endpoint, "Target.getTargets", map[string]any{}, "", 5*time.Second)
	if err != nil {
		slog.Debug(fmt.Sprintf("browser_cdp: page-URL probe failed: %s", err))
		return unverified
	}
	var listed struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	json.Unmarshal(raw, &listed)
	var acted *targetInfo
	if target != "" {
		for i := range listed.TargetInfos {
			if listed.TargetInfos[i].TargetID == target {
				acted = &listed.TargetInfos[i]
				break
			}
		}
	}
	if acted != nil && acted.URL != "" {
		if blocked(cfg, acted.URL) {
			return acted.URL
		}
		if acted.Type == "page" {
			return ""
		}
	}
	for _, t := range listed.TargetInfos {
		if t.Type != "page" || t.URL == "" || slices.ContainsFunc(originless, func(p string) bool { return strings.HasPrefix(t.URL, p) }) {
			continue
		}
		if blocked(cfg, t.URL) {
			return t.URL
		}
	}
	return ""
}

// Methods whose URL parameters name hosts without fetching them
var nonFetching = []string{"Network.setBlockedURLs", "Network.setRequestInterception", "Fetch.enable", "Page.handleJavaScriptDialog",
	"Network.setCookie", "Network.setCookies", "Network.deleteCookies", "Storage.setCookies"}

// Every URL the parameters name: url values whole, and each http or scheme relative literal at any depth
func paramLiterals(value any, key string) []string {
	switch v := value.(type) {
	case string:
		var out []string
		if key == "url" && strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
		return append(out, urlLiterals(v, true)...)
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, paramLiterals(item, key)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, k := range slices.Sorted(func(yield func(string) bool) {
			for k := range v {
				if !yield(k) {
					return
				}
			}
		}) {
			out = append(out, paramLiterals(v[k], k)...)
		}
		return out
	}
	return nil
}

// Distinct URLs a call could make the browser fetch
func fetched(method string, params map[string]any) []string {
	if strings.HasPrefix(method, "Input.") || slices.Contains(nonFetching, method) {
		return nil
	}
	var out []string
	for _, u := range paramLiterals(params, "") {
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// Refusal of a raw call that reaches cloud metadata, a private address or a private page, empty when allowed
func (r *registrant) cdpGuard(ctx context.Context, cfg *config.Config, method string, params map[string]any, endpoint, target string) string {
	active := r.guardActive(cfg)
	guard, guarded := paramGuards[method]
	refused := func(u string) bool { return blocked(cfg, u) }
	if active && guarded {
		if literal := guard.probe(params, refused); literal != "" {
			return cdpBlocked(fmt.Sprintf(guard.template, literal), method)
		}
	}
	literals := fetched(method, params)
	for _, u := range literals {
		if metadataLiteral(u) {
			return cdpBlocked(fmt.Sprintf("Blocked: CDP %s targets a cloud metadata endpoint (%s).", method, u), method)
		}
	}
	if !active {
		return ""
	}
	if !guarded {
		for _, u := range literals {
			low := strings.ToLower(u)
			if (strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")) && blocked(cfg, u) {
				return cdpBlocked(fmt.Sprintf("Blocked: CDP %s params target a private or internal address (%s).", method, u), method)
			}
		}
	}
	if !slices.Contains(pageSafe, method) {
		switch page := r.privatePage(ctx, cfg, endpoint, target); page {
		case "":
		case unverified:
			return cdpBlocked(fmt.Sprintf("Blocked: could not read the browser's targets to verify that raw CDP method %s cannot reach a private "+
				"or internal page.", strconv.Quote(method)), method)
		default:
			return cdpBlocked(fmt.Sprintf("Blocked: page URL targets a private or internal address (%s). Raw CDP method %s could expose private "+
				"page content or state.", page, strconv.Quote(method)), method)
		}
	}
	return ""
}

// A call on a fresh connection: attached flattened to the target first when given
func freshCall(ctx context.Context, endpoint, method string, params map[string]any, target string, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialCDP(ctx, endpoint, 0)
	if err != nil {
		return nil, err
	}
	defer conn.close()
	session := ""
	if target != "" {
		raw, err := conn.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, "")
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("Timed out attaching to target %s: %w", target, err)
			}
			return nil, fmt.Errorf("Target.attachToTarget failed: %w", err)
		}
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &attached) != nil || attached.SessionID == "" {
			return nil, errors.New("Target.attachToTarget did not return a sessionId")
		}
		session = attached.SessionID
	}
	raw, err := conn.call(ctx, method, params, session)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("Timed out waiting for response to %s: %w", method, err)
		}
		return nil, err
	}
	return raw, nil
}

// A call through the supervisor's live session of an out of process frame
func (r *registrant) viaSupervisor(task, frame, method string, params map[string]any, timeout time.Duration) string {
	s := r.supervisors.get(task)
	if s == nil {
		return toolErr(fmt.Sprintf("No CDP supervisor is attached for task=%s. Call browser_exec or /browser connect first so the supervisor can "+
			"attach; Page.getFrameTree lists the frame ids you can pass here.", strconv.Quote(task)))
	}
	tree := s.snapshot().Tree
	var info map[string]any
	candidates := []any{tree["top"]}
	if children, ok := tree["children"].([]any); ok {
		candidates = append(candidates, children...)
	}
	for _, f := range candidates {
		if o, ok := f.(map[string]any); ok && o["frame_id"] == frame {
			info = o
			break
		}
	}
	if info == nil {
		s.mu.Lock()
		f, ok := s.frames.get(frame)
		s.mu.Unlock()
		if ok {
			info = f.view()
		}
	}
	if info == nil {
		return toolErr(fmt.Sprintf("frame_id %s not found in supervisor state. Call Page.getFrameTree to see the current frames.", strconv.Quote(frame)))
	}
	session, _ := info["session_id"].(string)
	if session == "" {
		return toolErr(fmt.Sprintf("frame_id %s is not an out-of-process iframe (no dedicated CDP session). For same-origin iframes, use "+
			"`browser_cdp(method='Runtime.evaluate', params={'expression': \"document.querySelector('iframe').contentDocument.title\"})` at the "+
			"top-level page instead.", strconv.Quote(frame)))
	}
	conn := s.connection()
	if conn == nil || !s.healthy() {
		return toolErr("CDP supervisor loop is not running. Try reconnecting with /browser connect.")
	}
	raw, err := s.call(conn, method, params, session, timeout)
	if err != nil {
		return toolErr(fmt.Sprintf("CDP call via supervisor failed: %v", err), "cdp_docs", cdpDocs)
	}
	var result any = map[string]any{}
	if len(raw) > 0 {
		json.Unmarshal(raw, &result)
	}
	return orderedJSON(map[string]any{"success": true, "method": method, "frame_id": frame, "session_id": session, "result": result},
		"success", "method", "frame_id", "session_id", "result")
}

// Seconds a timeout argument names, false for one that names none
func secondsArg(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(t, "_", "")), 64)
		return f, err == nil
	}
	f, err := cast.ToFloat64E(v)
	return f, err == nil
}

// Raw command on a fresh connection, or on a frame's supervisor session, both behind the private page guard
func (r *registrant) cdp(ctx context.Context, call *tools.Call) string {
	cfg := call.Env.Config
	task := cmp.Or(call.SessionID, "default")
	method, _ := call.Args["method"].(string)
	target, _ := call.Args["target_id"].(string)
	frame, _ := call.Args["frame_id"].(string)
	timeoutArg, given := call.Args["timeout"]
	if !given {
		timeoutArg = 30.0
	}
	rawParams := call.Args["params"]
	params, isObject := rawParams.(map[string]any)
	if rawParams == nil || !config.Truthy(rawParams) {
		params, isObject = map[string]any{}, true
	}
	endpoint := strings.TrimSpace(r.override(ctx, cfg))
	if frame != "" {
		if !isObject {
			params = map[string]any{}
		}
		if refused := r.cdpGuard(ctx, cfg, method, params, endpoint, frame); refused != "" {
			return refused
		}
		wait, ok := secondsArg(timeoutArg)
		if !ok {
			return toolErr(fmt.Sprintf("CDP call via supervisor failed: timeout must be a number, got %T", timeoutArg), "cdp_docs", cdpDocs)
		}
		return r.viaSupervisor(task, frame, method, params, time.Duration(wait*float64(time.Second)))
	}
	if method == "" {
		return toolErr("'method' is required (e.g. 'Target.getTargets')", "cdp_docs", cdpDocs)
	}
	if endpoint == "" {
		return toolErr("No CDP endpoint is available. Run '/browser connect' to attach to a running Chrome, Brave, Chromium, or Edge browser, or "+
			"set 'browser.cdp_url' in config.yaml.", "cdp_docs", cdpDocs)
	}
	if !strings.HasPrefix(endpoint, "ws://") && !strings.HasPrefix(endpoint, "wss://") {
		return toolErr(fmt.Sprintf("CDP endpoint is not a WebSocket URL: %s. Expected ws://... or wss://... — the /browser connect resolver should "+
			"have rewritten this. Check that a Chromium-family browser is actually listening on the debug port.", strconv.Quote(redactEndpoint(endpoint))))
	}
	if !isObject {
		return toolErr(fmt.Sprintf("'params' must be an object/dict, got %T", rawParams))
	}
	if refused := r.cdpGuard(ctx, cfg, method, params, endpoint, target); refused != "" {
		return refused
	}
	limit := 30.0
	if config.Truthy(timeoutArg) {
		if f, ok := secondsArg(timeoutArg); ok {
			limit = f
		}
	}
	limit = max(min(limit, 300), 1)
	raw, err := freshCall(ctx, endpoint, method, params, target, time.Duration(limit*float64(time.Second)))
	if err != nil {
		var protocol *cdpError
		var socket *socketError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return toolErr(fmt.Sprintf("CDP call timed out after %ss: %s", strconv.FormatFloat(limit, 'f', -1, 64), err), "method", method)
		case errors.As(err, &protocol):
			return toolErr("CDP error: "+protocol.Error(), "method", method)
		case errors.As(err, &socket), errors.Is(err, errClosed):
			return toolErr(fmt.Sprintf("WebSocket error talking to CDP at %s: %s. The browser may have disconnected — try '/browser connect' again.",
				redactEndpoint(endpoint), redactEndpoint(err.Error())), "method", method)
		case strings.HasPrefix(err.Error(), "Target.attachToTarget"):
			return toolErr(err.Error(), "method", method)
		default:
			slog.Error("browser_cdp unexpected error", "error", err)
			return toolErr("Unexpected error: "+err.Error(), "method", method)
		}
	}
	var result any = map[string]any{}
	if len(raw) > 0 {
		if json.Unmarshal(raw, &result) != nil {
			result = map[string]any{}
		}
	}
	if _, ok := result.(map[string]any); !ok {
		return toolErr(fmt.Sprintf("Unexpected error: the browser answered %s with no object", method), "method", method)
	}
	payload := map[string]any{"success": true, "method": method, "result": redactResult(result, alwaysBinary[method], flaggedBinary[method], 0)}
	if target != "" {
		payload["target_id"] = target
	}
	return orderedJSON(payload, "success", "method", "result", "target_id")
}
