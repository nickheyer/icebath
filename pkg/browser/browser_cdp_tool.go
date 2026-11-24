package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const cdpDocsURL = "https://chromedevtools.github.io/devtools-protocol/"

const cdpDescription = "Send a raw Chrome DevTools Protocol (CDP) command. Escape hatch for browser operations the other " +
	"browser tools don't cover.\n\n" +
	"**Requires a reachable CDP endpoint.** Available when the user has run '/browser connect' to attach " +
	"to a running Chrome, Brave, Chromium, or Edge browser, or when 'browser.cdp_url' is set in " +
	"config.yaml. Not currently wired up for cloud backends (Browserbase, Browser Use, Firecrawl) — " +
	"those expose CDP per session but live-session routing is a follow-up. If the tool is in your toolset " +
	"at all, a CDP endpoint is already reachable.\n\n" +
	"**CDP method reference:** " + cdpDocsURL + " — use an available documentation lookup or extraction " +
	"tool on a method's URL (e.g. '/tot/Page/#method-handleJavaScriptDialog') to look up parameters and " +
	"return shape.\n\n" +
	"**Common patterns:**\n" +
	"- List tabs: method='Target.getTargets', params={}\n" +
	"- Handle a native JS dialog: method='Page.handleJavaScriptDialog', " +
	"params={'accept': true, 'promptText': ''}, target_id=<tabId>\n" +
	"- Get all cookies: method='Network.getAllCookies', params={}\n" +
	"- Eval in a specific tab: method='Runtime.evaluate', params={'expression': '...', 'returnByValue': true}, " +
	"target_id=<tabId>\n" +
	"- Set viewport for a tab: method='Emulation.setDeviceMetricsOverride', " +
	"params={'width': 1280, 'height': 720, 'deviceScaleFactor': 1, 'mobile': false}, target_id=<tabId>\n\n" +
	"**Usage rules:**\n" +
	"- Browser-level methods (Target.*, Browser.*, Storage.*): omit target_id and frame_id.\n" +
	"- Page-level methods (Page.*, Runtime.*, DOM.*, Emulation.*, Network.* scoped to a tab): pass " +
	"target_id from Target.getTargets.\n" +
	"- **Cross-origin iframe scope** (Runtime.evaluate inside an OOPIF, Page.* targeting a frame target, " +
	"etc.): pass a frame_id from Page.getFrameTree. This routes through the CDP " +
	"supervisor's live connection — the only reliable way on Browserbase where stateless CDP calls hit " +
	"signed-URL expiry.\n" +
	"- Each stateless call (without frame_id) is independent — sessions and event subscriptions do not " +
	"persist between calls. For stateful workflows, prefer the dedicated browser tools or use frame_id " +
	"routing."

// Static browser_cdp schema
func BrowserCDPSchema() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"name", "browser_cdp",
		"description", cdpDescription,
		"parameters", jsonutil.ObjectOf(
			"type", "object",
			"properties", jsonutil.ObjectOf(
				"method", jsonutil.ObjectOf("type", "string", "description", "CDP method name, e.g. 'Target.getTargets', 'Runtime.evaluate', 'Page.handleJavaScriptDialog'."),
				"params", jsonutil.ObjectOf("type", "object", "properties", jsonutil.ObjectOf(), "additionalProperties", true,
					"description", "Method-specific parameters as a JSON object. Omit or pass {} for methods that take no parameters."),
				"target_id", jsonutil.ObjectOf("type", "string", "description", "Optional. Target/tab ID from Target.getTargets result (each entry's 'targetId'). Use for "+
					"page-level methods at the top-level tab scope. Mutually exclusive with frame_id."),
				"frame_id", jsonutil.ObjectOf("type", "string", "description", "Optional. Out-of-process iframe (OOPIF) frame id from Page.getFrameTree. "+
					"When set, routes the call through the CDP supervisor's live session for "+
					"that iframe. Essential for Runtime.evaluate inside cross-origin iframes, especially on "+
					"Browserbase where fresh per-call CDP connections can't keep up with signed URL rotation. For "+
					"same-origin iframes, use parent contentWindow/contentDocument from Runtime.evaluate at the "+
					"top-level page instead."),
				"timeout", jsonutil.ObjectOf("type", "number", "default", 30, "description", "Timeout in seconds (default 30, max 300)."),
			),
			"required", []any{"method"},
		),
	)
}

// Inspection and tab management methods that never read page body, cookies, DOM or storage
var cdpPrivatePageAllowedMethods = map[string]bool{
	"Browser.getVersion": true, "Target.getTargets": true, "Target.attachToTarget": true, "Target.detachFromTarget": true,
	"Target.createTarget": true, "Target.activateTarget": true, "Target.closeTarget": true,
	"Page.navigate": true, "Page.reload": true, "Page.stopLoading": true,
}

// Result paths that always carry opaque base64
var cdpAlwaysBinaryPaths = map[string][][]string{
	"Page.captureScreenshot": {{"data"}}, "Page.printToPDF": {{"data"}},
	"Network.streamResourceContent": {{"bufferedData"}}, "HeadlessExperimental.beginFrame": {{"screenshotData"}},
	"CacheStorage.requestCachedResponse": {{"response", "body"}},
}

// Result paths that carry opaque base64 only beside a base64Encoded true sibling
var cdpFlaggedBinaryPaths = map[string][][]string{
	"Network.getResponseBody": {{"body"}}, "Fetch.getResponseBody": {{"body"}},
	"IO.read": {{"data"}}, "Network.getRequestPostData": {{"postData"}},
}

const (
	redactWalkMaxDepth    = 320
	redactWalkDepthMarker = "[redacted: nesting limit]"
)

func pathLeaf(paths [][]string, key string) bool {
	for _, p := range paths {
		if len(p) == 1 && p[0] == key {
			return true
		}
	}
	return false
}

func pathDescend(paths [][]string, key string) [][]string {
	var out [][]string
	for _, p := range paths {
		if len(p) > 1 && p[0] == key {
			out = append(out, p[1:])
		}
	}
	return out
}

// Browser originated CDP result text redacted, declared binary paths left byte identical
func redactCDPOutput(env *tools.Env, value any, always, flagged [][]string, depth int) any {
	if depth >= redactWalkMaxDepth {
		return redactWalkDepthMarker
	}
	switch v := value.(type) {
	case string:
		return tools.RedactSensitiveTextWith(env, v, tools.RedactOptions{Force: true})
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactCDPOutput(env, item, nil, nil, depth+1)
		}
		return out
	}
	obj, ok := jsonutil.AsObject(value)
	if !ok {
		return value
	}
	flag, _ := obj.Get("base64Encoded")
	base64Flagged := flag == true
	out := jsonutil.NewObject()
	for _, key := range obj.Keys() {
		item := obj.Value(key)
		opaque := pathLeaf(always, key) || (pathLeaf(flagged, key) && base64Flagged)
		outKey := tools.RedactSensitiveTextWith(env, key, tools.RedactOptions{Force: true})
		if s, isStr := item.(string); isStr && opaque {
			out.Set(outKey, s)
			continue
		}
		out.Set(outKey, redactCDPOutput(env, item, pathDescend(always, key), pathDescend(flagged, key), depth+1))
	}
	return out
}

// Seams the tool reads its endpoint, transport and guard facts through
var (
	resolveCDPEndpoint = func(env *tools.Env) string {
		return strings.TrimSpace(cfgOf(env).cdpOverride(context.Background()))
	}
	cdpEvalSSRFGuardActive  = evalSSRFGuardActive
	currentPagePrivateURL   = cdpCurrentPagePrivateURL
	cdpCallFn               = cdpCall
	browserCDPViaSupervisor = cdpViaSupervisor
)

// Error envelope with the bounded message first and extra keys in call order
func toolErr(message string, extra ...any) string {
	out := jsonutil.ObjectOf("error", tools.BoundErrorText(message))
	for i := 0; i+1 < len(extra); i += 2 {
		out.Set(extra[i].(string), extra[i+1])
	}
	return dumpsUnicode(out)
}

// json.dumps with ensure_ascii off
func dumpsUnicode(v any) string {
	text, err := jsonutil.Encode(v, jsonutil.Spaced())
	if err != nil {
		return tools.ErrorResult(err)
	}
	return text
}

func cdpBlocked(message, method string) string {
	return toolErr(message, "method", method, "cdp_docs", cdpDocsURL)
}

// Dict get on params, AttributeError for a non dict value as Python raises
func paramsGet(params any, key string) any {
	if params == nil {
		return nil
	}
	o, ok := jsonutil.AsObject(params)
	if !ok {
		panic(pyError("AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", anyutil.Kind(params))))
	}
	v, _ := o.Get(key)
	return v
}

func paramStr(params any, key string) string {
	v := paramsGet(params, key)
	if !anyutil.IsTrue(v) {
		return ""
	}
	return anyutil.String(v)
}

type cdpParamGuard struct {
	probe    func(params any, blocked func(string) bool) string
	template string
}

func expressionPrivateTarget(params any, blocked func(string) bool) string {
	return expressionTargetsPrivateURL(paramStr(params, "expression"), blocked)
}

// Blocked url literal of Page.navigate params, empty when allowed
func navigatePrivateTarget(params any, blocked func(string) bool) string {
	target := strings.TrimSpace(paramStr(params, "url"))
	if target != "" && blocked(target) {
		return target
	}
	return ""
}

// A blank tab opens freely, any other url is held to the Page.navigate rule
func newTabPrivateTarget(params any, blocked func(string) bool) string {
	if strings.ToLower(strings.TrimSpace(paramStr(params, "url"))) == "about:blank" {
		return ""
	}
	return navigatePrivateTarget(params, blocked)
}

var cdpMethodParamGuards = map[string]cdpParamGuard{
	"Page.navigate":       {navigatePrivateTarget, "Blocked: CDP Page.navigate target is a private or internal address ({})."},
	"Target.createTarget": {newTabPrivateTarget, "Blocked: CDP Target.createTarget url is a private or internal address ({})."},
	"Runtime.evaluate":    {expressionPrivateTarget, "Blocked: CDP Runtime.evaluate expression targets a private or internal address ({})."},
}

const pageURLUnverified = "<unverified>"

var originlessPagePrefixes = []string{"about:", "chrome:", "chrome-untrusted:", "chrome-search:", "devtools:", "edge:", "brave:"}

// Private page url a non allow listed call would reach, the unverified marker when targets cannot be read
func cdpCurrentPagePrivateURL(env *tools.Env, endpoint string, targetID any) string {
	if endpoint == "" {
		return pageURLUnverified
	}
	result, err := cdpCallFn(context.Background(), endpoint, "Target.getTargets", jsonutil.NewObject(), nil, 5.0)
	if err != nil {
		cfgOf(env).logger().Debug(fmt.Sprintf("browser_cdp: page-URL probe failed: %s", err.Error()))
		return pageURLUnverified
	}
	infosVal := anyGet(result, "targetInfos")
	infos, _ := infosVal.([]any)
	var acted any
	if anyutil.IsTrue(targetID) {
		for _, t := range infos {
			if anyutil.Equal(anyGet(t, "targetId"), targetID) {
				acted = t
				break
			}
		}
	}
	u := objStr(acted, "url")
	if u != "" && urlBlocked(env, u) {
		return u
	}
	if u != "" && anyGet(acted, "type") == "page" {
		return ""
	}
	for _, t := range infos {
		if anyGet(t, "type") != "page" {
			continue
		}
		pageURL := objStr(t, "url")
		if pageURL == "" || hasAnyPrefix(pageURL, originlessPagePrefixes) {
			continue
		}
		if urlBlocked(env, pageURL) {
			return pageURL
		}
	}
	return ""
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Methods whose url params name hosts without fetching them
var cdpNonFetchingMethods = map[string]bool{
	"Network.setBlockedURLs": true, "Network.setRequestInterception": true, "Fetch.enable": true,
	"Page.handleJavaScriptDialog": true,
	"Network.setCookie":           true, "Network.setCookies": true, "Network.deleteCookies": true, "Storage.setCookies": true,
}

// Every url a CDP call's params name, url values whole plus each http and scheme relative literal at any depth
func paramURLLiterals(value any, key string) []string {
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
			out = append(out, paramURLLiterals(item, key)...)
		}
		return out
	}
	o, ok := jsonutil.AsObject(value)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range o.Keys() {
		out = append(out, paramURLLiterals(o.Value(k), k)...)
	}
	return out
}

// Distinct urls a call's params could make the browser fetch
func fetchedParamURLs(method string, params any) []string {
	if strings.HasPrefix(method, "Input.") || cdpNonFetchingMethods[method] {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, u := range paramURLLiterals(params, "") {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// SSRF and private page guard of a raw CDP call, the refusal text or empty when allowed
func browserCDPPrivateGuard(env *tools.Env, method string, params any, endpoint string, targetID any) string {
	guardActive := cdpEvalSSRFGuardActive(env)
	guard, hasGuard := cdpMethodParamGuards[method]
	blocked := func(u string) bool { return urlBlocked(env, u) }
	if guardActive && hasGuard {
		if literal := guard.probe(params, blocked); literal != "" {
			return cdpBlocked(strings.Replace(guard.template, "{}", literal, 1), method)
		}
	}
	literals := fetchedParamURLs(method, params)
	for _, u := range literals {
		if literalTargetsMetadata(env, u) {
			return cdpBlocked(fmt.Sprintf("Blocked: CDP %s targets a cloud metadata endpoint (%s).", method, u), method)
		}
	}
	if !guardActive {
		return ""
	}
	if !hasGuard {
		for _, u := range literals {
			low := strings.ToLower(u)
			if (strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")) && urlBlocked(env, u) {
				return cdpBlocked(fmt.Sprintf("Blocked: CDP %s params target a private or internal address (%s).", method, u), method)
			}
		}
	}
	if !cdpPrivatePageAllowedMethods[method] {
		blockedURL := currentPagePrivateURL(env, endpoint, targetID)
		if blockedURL == pageURLUnverified {
			return cdpBlocked(fmt.Sprintf("Blocked: could not read the browser's targets to verify that raw CDP method "+
				"%s cannot reach a private or internal page.", strconv.Quote(method)), method)
		}
		if blockedURL != "" {
			return cdpBlocked(fmt.Sprintf("Blocked: page URL targets a private or internal address (%s). "+
				"Raw CDP method %s could expose private page content or state.", blockedURL, strconv.Quote(method)), method)
		}
	}
	return ""
}

var wsCloseExplanations = map[int]string{
	1000: "OK", 1001: "going away", 1002: "protocol error", 1003: "unsupported data",
	1005: "no status received [internal]", 1006: "connection closed abnormally [internal]",
	1007: "invalid frame payload data", 1008: "policy violation", 1009: "message too big",
	1010: "mandatory extension", 1011: "internal error", 1012: "service restart",
	1013: "try again later", 1014: "bad gateway", 1015: "TLS handshake failure [internal]",
}

// Close frame text as the websockets library renders it
func wsCloseText(code int, reason string) string {
	explanation := wsCloseExplanations[code]
	switch {
	case code >= 3000 && code < 4000:
		explanation = "registered"
	case code >= 4000 && code < 5000:
		explanation = "private use"
	case explanation == "":
		explanation = "unknown"
	}
	out := fmt.Sprintf("%d (%s)", code, explanation)
	if reason != "" {
		out += " " + reason
	}
	return out
}

var webSocketExceptionClasses = map[string]bool{
	"ConnectionClosedOK": true, "ConnectionClosedError": true, "InvalidStatus": true, "InvalidHandshake": true,
	"InvalidMessage": true, "InvalidURI": true, "ProtocolError": true, "PayloadTooBig": true,
}

func isWebSocketException(err error) bool {
	var ce *cloudError
	return errors.As(err, &ce) && webSocketExceptionClasses[ce.Class]
}

// Transport error mapped to the exception class and text the websockets client raises
func pyWSError(err error, rawURL string) error {
	if err == nil {
		return nil
	}
	var ce *cloudError
	if errors.As(err, &ce) {
		return err
	}
	var closed *ErrWSClosed
	if errors.As(err, &closed) {
		class := "ConnectionClosedError"
		if closed.Code == 1000 || closed.Code == 1001 {
			class = "ConnectionClosedOK"
		}
		text := wsCloseText(closed.Code, closed.Reason)
		return pyError(class, "received "+text+"; then sent "+text)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) {
		return pyError("ConnectionClosedError", "no close frame received or sent")
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return timeoutError("timed out during opening handshake")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return pyError("gaierror", "host name could not be resolved")
	}
	var opErr *net.OpError
	var errno syscall.Errno
	if errors.As(err, &opErr) && opErr.Op == "dial" && errors.As(err, &errno) {
		host, port := "", 0
		if addr, ok := opErr.Addr.(*net.TCPAddr); ok {
			host, port = addr.IP.String(), addr.Port
		}
		return pyError(errorutil.Kind(err), fmt.Sprintf("could not connect to %s port %d: %s", host, port, errno.Error()))
	}
	text := err.Error()
	if rest, ok := strings.CutPrefix(text, "websocket handshake rejected: "); ok {
		status, _, _ := strings.Cut(rest, " ")
		return pyError("InvalidStatus", "server rejected WebSocket connection: HTTP "+status)
	}
	if strings.HasPrefix(text, "websocket handshake") {
		return pyError("InvalidHandshake", text)
	}
	if strings.HasPrefix(text, "websocket url") {
		return pyError("InvalidURI", rawURL+" isn't a valid URI")
	}
	if errorutil.IsSystem(err) {
		return pyError(errorutil.Kind(err), err.Error())
	}
	return err
}

// One CDP call on a fresh socket, attaching flattened to targetID first when given
func cdpCall(ctx context.Context, wsURL, method string, params any, targetID any, timeout float64) (*jsonutil.Object, error) {
	limit := seconds(timeout)
	dialCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ws, err := DialWS(dialCtx, wsURL, WSOptions{HandshakeTimeout: limit})
	if err != nil {
		return nil, pyWSError(err, wsURL)
	}
	defer func() { _ = ws.Close(1000, "") }()
	nextID := int64(1)
	send := func(req *jsonutil.Object, what string) (*jsonutil.Object, error) {
		callID := nextID
		nextID++
		payload := jsonutil.ObjectOf("id", callID)
		payload.Merge(req)
		text, err := jsonutil.Encode(payload, jsonutil.Spaced(), jsonutil.ASCII())
		if err != nil {
			return nil, err
		}
		if err := ws.WriteText(ctx, []byte(text)); err != nil {
			return nil, pyWSError(err, wsURL)
		}
		deadline := time.Now().Add(limit)
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, timeoutError("Timed out " + what)
			}
			readCtx, stop := context.WithTimeout(ctx, remaining)
			_, raw, err := ws.ReadMessage(readCtx)
			stop()
			if err != nil {
				if readCtx.Err() != nil && ctx.Err() == nil {
					return nil, timeoutError("")
				}
				return nil, pyWSError(err, wsURL)
			}
			decoded, err := jsonutil.Decode(raw)
			if err != nil {
				return nil, pyError("JSONDecodeError", err.Error())
			}
			msg, ok := decoded.(*jsonutil.Object)
			if !ok {
				return nil, pyError("AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", anyutil.Kind(decoded)))
			}
			if id, _ := msg.Get("id"); anyutil.Equal(id, callID) {
				return msg, nil
			}
		}
	}
	callParams := params
	if !anyutil.IsTrue(callParams) {
		callParams = jsonutil.NewObject()
	}
	req := jsonutil.ObjectOf("method", method, "params", callParams)
	if anyutil.IsTrue(targetID) {
		msg, err := send(jsonutil.ObjectOf("method", "Target.attachToTarget", "params", jsonutil.ObjectOf("targetId", targetID, "flatten", true)),
			"attaching to target "+anyutil.String(targetID))
		if err != nil {
			return nil, err
		}
		if errVal, has := msg.Get("error"); has {
			return nil, runtimeError("Target.attachToTarget failed: " + anyutil.String(errVal))
		}
		sessionID := anyGet(objChild(msg, "result"), "sessionId")
		if !anyutil.IsTrue(sessionID) {
			return nil, runtimeError("Target.attachToTarget did not return a sessionId")
		}
		req.Set("sessionId", sessionID)
	}
	msg, err := send(req, "waiting for response to "+method)
	if err != nil {
		return nil, err
	}
	if errVal, has := msg.Get("error"); has {
		return nil, runtimeError("CDP error: " + anyutil.String(errVal))
	}
	if result, has := msg.Get("result"); has {
		if o, ok := jsonutil.AsObject(result); ok {
			return o, nil
		}
		return nil, pyError("AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", anyutil.Kind(result)))
	}
	return jsonutil.NewObject(), nil
}

// Float seconds of a timeout argument, a TypeError for a value Python cannot add to an int
func timeoutSeconds(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case float64:
		return t, nil
	case *big.Int:
		f, _ := new(big.Float).SetInt(t).Float64()
		return f, nil
	}
	return 0, pyError("TypeError", fmt.Sprintf("can only concatenate %s (not \"int\") to %s", anyutil.Kind(v), anyutil.Kind(v)))
}

// CDP call routed through the supervisor's live session of an out of process frame
func cdpViaSupervisor(env *tools.Env, taskID, frameID, method string, params any, timeout any) string {
	sup := supervisors.get(taskID)
	if sup == nil {
		return toolErr(fmt.Sprintf("No CDP supervisor is attached for task=%s. Call browser_exec or "+
			"/browser connect first so the supervisor can attach; Page.getFrameTree lists the "+
			"frame ids you can pass here.", strconv.Quote(taskID)))
	}
	tree := sup.snapshot().FrameTree
	var frameInfo *jsonutil.Object
	candidates := []any{anyGet(tree, "top")}
	if children, ok := anyGet(tree, "children").([]any); ok {
		candidates = append(candidates, children...)
	}
	for _, f := range candidates {
		if o, ok := jsonutil.AsObject(f); ok && anyutil.IsTrue(f) && anyGet(o, "frame_id") == frameID {
			frameInfo = o
			break
		}
	}
	if frameInfo == nil {
		sup.stateMu.Lock()
		raw, ok := sup.frames.get(frameID)
		sup.stateMu.Unlock()
		if ok {
			frameInfo = raw.toDict()
		}
	}
	if frameInfo == nil {
		return toolErr(fmt.Sprintf("frame_id %s not found in supervisor state. "+
			"Call Page.getFrameTree to see the current frames.", strconv.Quote(frameID)))
	}
	childSID := objStr(frameInfo, "session_id")
	if childSID == "" {
		return toolErr(fmt.Sprintf("frame_id %s is not an out-of-process iframe (no dedicated CDP session). "+
			"For same-origin iframes, use `browser_cdp(method='Runtime.evaluate', params={'expression': "+
			"\"document.querySelector('iframe').contentDocument.title\"})` at the top-level page instead.", strconv.Quote(frameID)))
	}
	if !sup.currentLoop().isRunning() {
		return toolErr("CDP supervisor loop is not running. Try reconnecting with /browser connect.")
	}
	callParams := params
	if !anyutil.IsTrue(callParams) {
		callParams = jsonutil.NewObject()
	}
	wait, err := timeoutSeconds(timeout)
	var reply any
	if err == nil {
		reply, err = sup.schedule(func() (any, error) { return sup.cdpCall(method, callParams, childSID, wait) }, wait+2, false)
		if errors.Is(err, errLoopUnavailable) {
			return toolErr("CDP call via supervisor failed: loop unavailable", "cdp_docs", cdpDocsURL)
		}
	}
	if err != nil {
		return toolErr(fmt.Sprintf("CDP call via supervisor failed: %v", err), "cdp_docs", cdpDocsURL)
	}
	result, has := reply.(*jsonutil.Object).Get("result")
	if !has {
		result = jsonutil.NewObject()
	}
	return dumpsUnicode(jsonutil.ObjectOf("success", true, "method", method, "frame_id", frameID, "session_id", childSID, "result", result))
}

// Raw CDP command over a fresh socket or a frame's supervisor session, both behind the private page guard
func browserCDP(ctx context.Context, env *tools.Env, method, params, targetID, frameID, timeout any, taskID string) string {
	effectiveTaskID := taskID
	if effectiveTaskID == "" {
		effectiveTaskID = "default"
	}
	methodName, _ := method.(string)
	if anyutil.IsTrue(frameID) {
		endpoint := resolveCDPEndpoint(env)
		guardParams := params
		if !anyutil.IsTrue(guardParams) {
			guardParams = jsonutil.NewObject()
		}
		if blocked := browserCDPPrivateGuard(env, methodName, guardParams, endpoint, frameID); blocked != "" {
			return blocked
		}
		return browserCDPViaSupervisor(env, effectiveTaskID, anyutil.String(frameID), methodName, params, timeout)
	}
	if methodName == "" {
		return toolErr("'method' is required (e.g. 'Target.getTargets')", "cdp_docs", cdpDocsURL)
	}
	endpoint := resolveCDPEndpoint(env)
	if endpoint == "" {
		return toolErr("No CDP endpoint is available. Run '/browser connect' to attach to a running Chrome, "+
			"Brave, Chromium, or Edge browser, or set 'browser.cdp_url' in config.yaml.", "cdp_docs", cdpDocsURL)
	}
	if !strings.HasPrefix(endpoint, "ws://") && !strings.HasPrefix(endpoint, "wss://") {
		return toolErr(fmt.Sprintf("CDP endpoint is not a WebSocket URL: %s. Expected ws://... or wss://... — "+
			"the /browser connect resolver should have rewritten this. Check that a Chromium-family "+
			"browser is actually listening on the debug port.", strconv.Quote(endpoint)))
	}
	callParams := params
	if !anyutil.IsTrue(callParams) {
		callParams = jsonutil.NewObject()
	}
	if _, ok := jsonutil.AsObject(callParams); !ok {
		return toolErr("'params' must be an object/dict, got " + anyutil.Kind(callParams))
	}
	if blocked := browserCDPPrivateGuard(env, methodName, callParams, endpoint, targetID); blocked != "" {
		return blocked
	}
	safeTimeout := 30.0
	if anyutil.IsTrue(timeout) {
		if f, ok := floatArg(timeout); ok {
			safeTimeout = f
		}
	}
	if 300.0 < safeTimeout {
		safeTimeout = 300.0
	}
	if !(safeTimeout > 1.0) {
		safeTimeout = 1.0
	}
	result, err := cdpCallFn(ctx, endpoint, methodName, callParams, targetID, safeTimeout)
	if err != nil {
		var ce *cloudError
		class := ""
		if errors.As(err, &ce) {
			class = ce.Class
		}
		switch {
		case class == "TimeoutError" || errors.Is(err, context.DeadlineExceeded):
			return toolErr(fmt.Sprintf("CDP call timed out after %ss: %s", strconv.FormatFloat(safeTimeout, 'f', -1, 64), err.Error()), "method", methodName)
		case class == "RuntimeError":
			return toolErr(err.Error(), "method", methodName)
		case isWebSocketException(err):
			return toolErr(fmt.Sprintf("WebSocket error talking to CDP at %s: %s. The browser may have "+
				"disconnected — try '/browser connect' again.", endpoint, err.Error()), "method", methodName)
		default:
			cfgOf(env).logger().Error("browser_cdp unexpected error", "error", err)
			return toolErr("Unexpected error: "+err.Error(), "method", methodName)
		}
	}
	payload := jsonutil.ObjectOf("success", true, "method", methodName,
		"result", redactCDPOutput(env, result, cdpAlwaysBinaryPaths[methodName], cdpFlaggedBinaryPaths[methodName], 0))
	if anyutil.IsTrue(targetID) {
		payload.Set("target_id", targetID)
	}
	return dumpsUnicode(payload)
}

// float() of a tool argument, false where Python raises TypeError or ValueError
func floatArg(v any) (float64, bool) {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(t, "_", "")), 64)
		return f, err == nil
	case nil:
		return 0, false
	}
	f, err := timeoutSeconds(v)
	return f, err == nil
}

// Offered only with a static CDP url, without network traffic
func browserCDPCheck(env *tools.Env) bool { return cfgOf(env).cdpOverrideRaw() != "" }

// Raw CDP command with the timeout defaulting to 30 seconds and params kept in the order the model wrote them
func handleBrowserCDP(ctx context.Context, call *tools.Call) string {
	args := call.Args
	method, ok := args["method"]
	if !ok {
		method = ""
	}
	timeout, ok := args["timeout"]
	if !ok {
		timeout = 30.0
	}
	params := args["params"]
	if call.RawArgs != "" {
		if raw, err := jsonutil.Decode(call.RawArgs); err == nil {
			if v := anyGet(raw, "params"); v != nil {
				params = v
			}
		}
	}
	return browserCDP(ctx, call.Env, method, params, args["target_id"], args["frame_id"], timeout, call.TaskID)
}

func registerBrowserCDPTool() {
	tools.Register(tools.Def{Name: "browser_cdp", Toolset: "browser-cdp", Schema: BrowserCDPSchema(), Handler: handleBrowserCDP,
		Check: browserCDPCheck, Emoji: "\U0001F9EA"})
}
