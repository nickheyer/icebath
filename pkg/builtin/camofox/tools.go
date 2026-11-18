package camofox

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	visionTask      = "camofox_vision"
	scrollRepeat    = 5
	evalUnsupported = "JavaScript evaluation is not supported by this Camofox server. " +
		"Use browser_snapshot or browser_vision to inspect page state."
	tabGoneError = "The browser tab is gone (the Camofox server reaped it or restarted). " +
		"Call browser_navigate to open the page again."
	tabChangedError = "The browser tab was replaced (the Camofox server reaped or restarted it); now on %s. " +
		"Element refs from earlier snapshots no longer apply: call browser_snapshot, then retry."
	cookieMaxBytes = 5 * 1024 * 1024
	cookieMaxCount = 500
)

// Cookie fields the server accepts, in the order they are sent
var cookieAllowedFields = []string{"name", "value", "domain", "path", "expires", "httpOnly", "secure", "sameSite"}

var (
	imgAltRE = pyre.MustCompile(`img\s+"([^"]*)"`)
	imgURLRE = pyre.MustCompile(`/url:\s*(\S+)`)
)

// Body of one tab tool run against the session, its result text or the error it raised
type tabBody func(ss *session) (string, error)

// Snapshot reply of the session's tab
func (s *scope) snapshotData(ss *session) (any, error) {
	return s.get(tabPath(ss, "snapshot"), userParams(ss), commandTimeoutDefault)
}

// Redacted snapshot cut at browser.snapshot_threshold with its refsCount
func (s *scope) fetchSnapshot(ss *session) (string, any, error) {
	data, err := s.snapshotData(ss)
	if err != nil {
		return "", nil, err
	}
	raw, err := get(data, "snapshot", "")
	if err != nil {
		return "", nil, err
	}
	redacted := s.redactAny(raw)
	threshold, err := s.snapshotThreshold()
	if err != nil {
		return "", nil, err
	}
	snapshot, ok := redacted.(string)
	if !ok {
		return "", nil, &pyError{"TypeError", "object of type 'NoneType' has no len()"}
	}
	if int64(pystr.Len(snapshot)) > threshold {
		snapshot = s.truncateSnapshot(snapshot, threshold)
	}
	refs, err := get(data, "refsCount", int64(0))
	if err != nil {
		return "", nil, err
	}
	return snapshot, refs, nil
}

// Opens the URL in the task's tab, a 404 or 410 for the cached tab recreating it
func (s *scope) navigateTab(taskID, browserURL string) (*session, any, error) {
	ss := s.getSession(taskID)
	if ss.hasTab() {
		data, err := s.post(tabPath(ss, "navigate"), pyjson.ObjectOf("userId", ss.userID, "url", browserURL), 60)
		if err == nil {
			return ss, data, nil
		}
		he, ok := err.(*httpError)
		if !ok || (he.status != 404 && he.status != 410) {
			return nil, nil, err
		}
		toolsLog().Warn(fmt.Sprintf("Camofox tab %s returned %d — tab is gone. Creating a fresh tab.", pyrepr.Str(ss.tab()), he.status))
		ss.setTab(nil)
	}
	ss, err := s.ensureTab(taskID, browserURL)
	if err != nil {
		return nil, nil, err
	}
	return ss, pyjson.ObjectOf("ok", true, "url", browserURL), nil
}

// Whether an error says the cached tab is gone
func tabGone(err error) bool { return httpStatusIn(err, 404, 410) }

// Runs body on the session, a read retried on the live tab after a resync and an input action refused
func (s *scope) runOnTab(ss *session, body tabBody, readOnly bool) (string, error) {
	tracked := ss.tab()
	out, err := body(ss)
	if err == nil {
		return out, nil
	}
	if !tabGone(err) {
		return "", err
	}
	if !s.resyncTab(ss) {
		return fail(tabGoneError), nil
	}
	if pyjson.Equal(ss.tab(), tracked) {
		return "", err
	}
	toolsLog().Info(fmt.Sprintf("Camofox tab %s was gone; continuing in live tab %s", pyrepr.Str(tracked), pyrepr.Str(ss.tab())))
	if !readOnly {
		url := s.tabURL(ss)
		place := "another page"
		if pyrepr.Truthy(url) {
			place = pyrepr.Str(url)
		}
		return fail(fmt.Sprintf(tabChangedError, place)), nil
	}
	return body(ss)
}

// URL GET /tabs lists for the session's tab, empty on any failure
func (s *scope) tabURL(ss *session) any {
	var out any = ""
	err := try(func() error {
		tabs, err := s.listTabs(ss, 5)
		if err != nil {
			return err
		}
		list, _ := tabs.([]any)
		for _, tab := range list {
			if !isDict(tab) {
				continue
			}
			if id, _ := get(tab, "tabId", nil); pyjson.Equal(id, ss.tab()) {
				out, _ = get(tab, "url", "")
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return ""
	}
	return out
}

// Runs body on the task's existing tab, the navigate first error when none, every failure as a tool error
func (s *scope) withTab(taskID string, body tabBody, readOnly bool) string {
	var out string
	err := try(func() error {
		ss := s.getSession(taskID)
		if !ss.hasTab() {
			out = fail(noSessionError)
			return nil
		}
		var err error
		out, err = s.runOnTab(ss, body, readOnly)
		return err
	})
	if err != nil {
		return fail(err.Error())
	}
	return out
}

// POSTs the body to the tab action and builds the tool result from the reply
func (s *scope) tabAction(taskID, suffix string, body *pyjson.Object, result func(data any) (*pyjson.Object, error)) string {
	return s.withTab(taskID, func(ss *session) (string, error) {
		payload := pyjson.ObjectOf("userId", ss.userID)
		for _, k := range body.Keys {
			payload.Set(k, body.Vals[k])
		}
		data, err := s.post(tabPath(ss, suffix), payload, commandTimeoutDefault)
		if err != nil {
			return "", err
		}
		r, err := result(data)
		if err != nil {
			return "", err
		}
		return dumps(r), nil
	}, false)
}

// Page title from the navigate reply, else from this task's own tab in GET /tabs, empty when the lookup fails
func (s *scope) navigateTitle(ss *session, data any) (string, error) {
	responseTitle, err := get(data, "title", nil)
	if err != nil {
		return "", err
	}
	if t, ok := responseTitle.(string); ok && t != "" {
		return t, nil
	}
	title := ""
	if err := try(func() error {
		reply, err := s.get("/tabs", userParams(ss), commandTimeoutDefault)
		if err != nil {
			return err
		}
		tabs, err := get(reply, "tabs", []any{})
		if err != nil {
			return err
		}
		list, _ := tabs.([]any)
		for _, tab := range list {
			if !isDict(tab) {
				continue
			}
			if id, _ := get(tab, "tabId", nil); pyjson.Equal(id, ss.tab()) {
				t, _ := get(tab, "title", nil)
				title, _ = t.(string)
				return nil
			}
		}
		return nil
	}); err != nil {
		toolsLog().Debug(fmt.Sprintf("Camofox title lookup failed for tab %s: %s", pyrepr.Str(ss.tab()), err.Error()))
	}
	return title, nil
}

// Navigates to the URL, the result carrying the title and a compact snapshot
func (s *scope) browserNavigate(rawURL, taskID string) string {
	target, policyError := s.navigationPolicyError(rawURL)
	if policyError != nil {
		return dumps(policyError)
	}
	var out string
	err := try(func() error {
		browserURL, rewrite, err := s.rewriteLoopbackURL(target)
		if err != nil {
			return err
		}
		ss, data, err := s.navigateTab(taskID, browserURL)
		if err != nil {
			return err
		}
		landed, err := get(data, "url", browserURL)
		if err != nil {
			return err
		}
		title, err := s.navigateTitle(ss, data)
		if err != nil {
			return err
		}
		result := pyjson.ObjectOf("success", true, "url", landed, "title", title)
		if rewrite != nil {
			from, _ := rewrite.Get("from")
			to, _ := rewrite.Get("to")
			result.Set("requested_url", target)
			result.Set("url_rewrite", rewrite)
			result.Set("warning", "Rewrote loopback URL for Docker-hosted Camofox: "+pyrepr.Str(from)+" -> "+pyrepr.Str(to))
		}
		if vnc := s.vncURL(); vnc != "" {
			result.Set("vnc_url", vnc)
			result.Set("vnc_hint", "Browser is visible via VNC. Share this link with the user so they can watch the browser live.")
		}
		var snapshot string
		var refs any
		if try(func() error {
			var err error
			snapshot, refs, err = s.fetchSnapshot(ss)
			return err
		}) == nil {
			result.Set("snapshot", snapshot)
			result.Set("element_count", refs)
		}
		out = dumps(result)
		return nil
	})
	switch {
	case err == nil:
		return out
	case isHTTPError(err):
		return fail("Navigation failed: " + err.Error())
	case isConnectionError(err):
		return dumps(pyjson.ObjectOf("success", false, "error", s.connectionErrorMessage()))
	}
	return fail(err.Error())
}

// Whether an error is a requests HTTPError
func isHTTPError(err error) bool {
	_, ok := err.(*httpError)
	return ok
}

// Snapshot of the task's tab
func (s *scope) browserSnapshot(taskID string) string {
	return s.withTab(taskID, func(ss *session) (string, error) {
		snapshot, refs, err := s.fetchSnapshot(ss)
		if err != nil {
			return "", err
		}
		return dumps(pyjson.ObjectOf("success", true, "snapshot", snapshot, "element_count", refs)), nil
	}, true)
}

// Clicks the ref, the tool convention @ prefix stripped for Camofox
func (s *scope) browserClick(ref, taskID string) string {
	cleanRef := strings.TrimLeft(ref, "@")
	return s.tabAction(taskID, "click", pyjson.ObjectOf("ref", cleanRef), func(data any) (*pyjson.Object, error) {
		landed, err := get(data, "url", "")
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("success", true, "clicked", cleanRef, "url", landed), nil
	})
}

// Types the raw text into the ref, the result masking secret looking text
func (s *scope) browserType(ref, text, taskID string) string {
	var out string
	err := try(func() error {
		ss := s.getSession(taskID)
		if !ss.hasTab() {
			out = fail(noSessionError)
			return nil
		}
		cleanRef := strings.TrimLeft(ref, "@")
		body := func(sess *session) (string, error) {
			if _, err := s.post(tabPath(sess, "type"), pyjson.ObjectOf("userId", sess.userID, "ref", cleanRef, "text", text), commandTimeoutDefault); err != nil {
				return "", err
			}
			return dumps(s.redactTypedText(pyjson.ObjectOf("success", true, "typed", s.redactText(text), "element", cleanRef), text)), nil
		}
		res, err := s.runOnTab(ss, body, false)
		if err != nil {
			return err
		}
		out = s.redactTypedText(res, text).(string)
		return nil
	})
	if err != nil {
		return fail(s.redactTypedText(err.Error(), text).(string))
	}
	return out
}

// Scrolls five fixed REST steps, a failed step being the answer
func (s *scope) browserScroll(direction, taskID string) string {
	if direction != "up" && direction != "down" {
		return dumps(pyjson.ObjectOf("success", false, "error", fmt.Sprintf("Invalid direction '%s'. Use 'up' or 'down'.", direction)))
	}
	result := ""
	for range scrollRepeat {
		result = s.tabAction(taskID, "scroll", pyjson.ObjectOf("direction", direction), func(any) (*pyjson.Object, error) {
			return pyjson.ObjectOf("success", true, "scrolled", direction), nil
		})
		parsed, _ := pyjson.Loads(result)
		if ok, _ := get(parsed, "success", nil); !pyrepr.Truthy(ok) {
			break
		}
	}
	return result
}

// Goes back in the tab's history
func (s *scope) browserBack(taskID string) string {
	return s.tabAction(taskID, "back", pyjson.NewObject(), func(data any) (*pyjson.Object, error) {
		landed, err := get(data, "url", "")
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("success", true, "url", landed), nil
	})
}

// Presses a key on the tab
func (s *scope) browserPress(key, taskID string) string {
	return s.tabAction(taskID, "press", pyjson.ObjectOf("key", key), func(any) (*pyjson.Object, error) {
		return pyjson.ObjectOf("success", true, "pressed", key), nil
	})
}

// Images of an accessibility snapshot, each img alt with the URL on the following /url line
func parseSnapshotImages(snapshot any) ([]any, error) {
	text, ok := snapshot.(string)
	if !ok {
		return nil, &pyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'split'", pyrepr.TypeName(snapshot))}
	}
	images := []any{}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		stripped := pystr.Strip(line)
		if !strings.HasPrefix(stripped, "- img ") && !strings.HasPrefix(stripped, "img ") {
			continue
		}
		alt, src := "", ""
		if m, _ := imgAltRE.FindStringMatch(stripped); m != nil {
			alt = m.GroupByNumber(1).String()
		}
		if i+1 < len(lines) {
			if m, _ := imgURLRE.FindStringMatch(pystr.Strip(lines[i+1])); m != nil {
				src = m.GroupByNumber(1).String()
			}
		}
		if alt != "" || src != "" {
			images = append(images, pyjson.ObjectOf("src", src, "alt", alt))
		}
	}
	return images, nil
}

// Images on the tab's page, redacted like the snapshot
func (s *scope) browserGetImages(taskID string) string {
	return s.withTab(taskID, func(ss *session) (string, error) {
		data, err := s.snapshotData(ss)
		if err != nil {
			return "", err
		}
		snapshot, err := get(data, "snapshot", "")
		if err != nil {
			return "", err
		}
		parsed, err := parseSnapshotImages(snapshot)
		if err != nil {
			return "", err
		}
		images := s.redactOutput(parsed, 0).([]any)
		return dumps(pyjson.ObjectOf("success", true, "images", images, "count", len(images))), nil
	}, true)
}

// JS value Camofox returned, a JSON string parsed into structured data
func parseEvalValue(raw any) any {
	if text, ok := raw.(string); ok {
		if parsed, err := pyjson.Loads(text); err == nil {
			return parsed
		}
	}
	return raw
}

// Evaluates on the existing tab only, a server without /evaluate named as such
func (s *scope) evaluate(expression, taskID string) string {
	var out string
	err := try(func() error {
		ss := s.getSession(taskID)
		if !ss.hasTab() {
			out = fail(noSessionError)
			return nil
		}
		body := func(sess *session) (string, error) {
			resp, err := s.post(tabPath(sess, "evaluate"), pyjson.ObjectOf("expression", expression, "userId", sess.userID), commandTimeoutDefault)
			if err != nil {
				return "", err
			}
			raw := resp
			if isDict(resp) {
				raw, _ = get(resp, "result", nil)
			}
			parsed := parseEvalValue(raw)
			return dumps(pyjson.ObjectOf("success", true, "result", s.redactOutput(parsed, 0), "result_type", pyrepr.TypeName(parsed))), nil
		}
		var err error
		out, err = s.runOnTab(ss, body, false)
		return err
	})
	switch {
	case err == nil:
		return out
	case httpStatusIn(err, 404, 405, 501):
		return dumps(pyjson.ObjectOf("success", false, "error", evalUnsupported))
	}
	return fail(err.Error())
}

// Evaluates the expression in the page, without one an empty console result naming the missing capture
func (s *scope) browserConsole(expression *string, taskID string) string {
	if expression != nil {
		if refusal := s.evalPolicyError(*expression); refusal != "" {
			return dumps(pyjson.ObjectOf("success", false, "error", refusal))
		}
		return s.evaluate(*expression, taskID)
	}
	return dumps(pyjson.ObjectOf("success", true, "console_messages", []any{}, "js_errors", []any{}, "total_messages", 0,
		"total_errors", 0, "note", "Console log capture is not available with the Camofox backend. "+
			"Use browser_snapshot or browser_vision to inspect page state."))
}

// auxiliary.vision timeout and temperature, 120 and 0.1 on any config error
func (s *scope) visionSettings() (float64, float64) {
	raw, err := s.rawConfig()
	if err != nil {
		return 120, 0.1
	}
	block := cfgGet(raw, map[string]any{}, "auxiliary", "vision")
	if !pyrepr.Truthy(block) {
		block = map[string]any{}
	}
	cfg := config.AsMap(block)
	if cfg == nil {
		return 120, 0.1
	}
	timeoutVal, ok := cfg["timeout"]
	if !ok {
		timeoutVal = 120
	}
	timeout, err := pyint.Float(timeoutVal)
	if err != nil {
		return 120, 0.1
	}
	temperatureVal, ok := cfg["temperature"]
	if !ok {
		temperatureVal = 0.1
	}
	temperature, err := pyint.Float(temperatureVal)
	if err != nil {
		return 120, 0.1
	}
	return timeout, temperature
}

// OSError text and class of a failed filesystem call
func osError(err error) error {
	return &pyError{pypath.OSErrorClass(err), pypath.OSErrorText(err)}
}

// PNG bytes written under the profile browser_screenshots directory, the path returned
func (s *scope) saveScreenshot(content []byte) (string, error) {
	dir := pypath.PathJoin(s.home, "browser_screenshots")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", osError(err)
	}
	path := pypath.PathJoin(dir, "browser_screenshot_"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8]+".png")
	if err := os.WriteFile(path, content, 0o666); err != nil {
		return "", osError(err)
	}
	return path, nil
}

// Screenshots the tab and answers the question with the auxiliary vision model on the camofox_vision task
func (s *scope) browserVision(question, taskID string, llm *plugins.PluginLlm) string {
	return s.withTab(taskID, func(ss *session) (string, error) {
		resp, err := s.getRaw(tabPath(ss, "screenshot"), userParams(ss), commandTimeoutDefault)
		if err != nil {
			return "", err
		}
		screenshotPath, err := s.saveScreenshot(resp.Body)
		if err != nil {
			return "", err
		}
		imgB64 := base64.StdEncoding.EncodeToString(resp.Body)
		timeout, temperature := s.visionSettings()
		messages := []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "Analyze this browser screenshot and answer: " + question},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + imgB64}},
		}}}
		task, purpose := visionTask, "browser_vision"
		var result *plugins.PluginLlmCompleteResult
		if err := try(func() error {
			if llm == nil {
				return &pyError{"AttributeError", "'NoneType' object has no attribute 'complete'"}
			}
			var err error
			result, err = llm.Complete(s.ctx, messages, plugins.PluginLlmOptions{Task: &task, Purpose: &purpose,
				Temperature: &temperature, Timeout: &timeout})
			return err
		}); err != nil {
			toolsLog().Warn(fmt.Sprintf("browser_vision analysis failed: %v", err))
			return dumps(pyjson.ObjectOf("success", false, "error", "Error during vision analysis: "+err.Error(),
				"screenshot_path", screenshotPath,
				"note", "Screenshot was captured but vision analysis failed. You can still share it via MEDIA:<path>.")), nil
		}
		analysis := pystr.Strip(result.Text)
		if analysis == "" {
			analysis = "Vision analysis returned no content."
		}
		return dumps(pyjson.ObjectOf("success", true, "analysis", s.redactText(analysis), "screenshot_path", screenshotPath)), nil
	}, true)
}

// Netscape cookie file text as Playwright cookie objects, BOM, HttpOnly prefix and tab bearing values handled
func netscapeParse(text string) []*pyjson.Object {
	var cookies []*pyjson.Object
	for _, rawLine := range pystr.Splitlines(strings.TrimLeft(text, "\uFEFF"), false) {
		line := pystr.Strip(rawLine)
		if line == "" || (strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_")) {
			continue
		}
		httpOnly := strings.HasPrefix(line, "#HttpOnly_")
		if httpOnly {
			line = line[len("#HttpOnly_"):]
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		expires, err := pyint.Of(parts[4])
		if err != nil {
			continue
		}
		cookies = append(cookies, pyjson.ObjectOf("name", parts[5], "value", strings.Join(parts[6:], "\t"), "domain", parts[0],
			"path", parts[2], "expires", pyint.Value(expires), "httpOnly", httpOnly, "secure", pystr.Upper(parts[3]) == "TRUE"))
	}
	return cookies
}

// browser.camofox.cookies_dir with the tilde expanded, ~/.camofox/cookies by default
func (s *scope) cookiesDir() string {
	raw := pystr.Strip(pyOrStr(s.camofoxConfig()["cookies_dir"]))
	if raw == "" {
		raw = "~/.camofox/cookies"
	}
	return pypath.Normpath(pypath.Expanduser(raw))
}

// Text of a file decoded as strict UTF-8 with universal newlines
func readUTF8Text(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", osError(err)
	}
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return "", &pyError{"UnicodeDecodeError", pystr.UTF8DecodeError(data, i).Error()}
		}
		i += size
	}
	return strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), nil
}

// Cookies of a file that must resolve inside the cookies dir, capped at 5 MB, filtered by the domain suffix
func (s *scope) readCookieFile(cookiesPath string, domainSuffix *string) ([]*pyjson.Object, error) {
	base, err := pypath.Realpath(s.cookiesDir(), pypath.Lax)
	if err != nil {
		return nil, osError(err)
	}
	outside := &pyError{"ValueError", "cookies_path must be a relative path within the cookies directory"}
	if pypath.IsAbs(cookiesPath) {
		return nil, outside
	}
	resolved, err := pypath.Realpath(pypath.Join(base, cookiesPath), pypath.Lax)
	if err != nil {
		return nil, osError(err)
	}
	if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
		return nil, outside
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return nil, &pyError{"FileNotFoundError", "Cookie file not found: " + cookiesPath}
	}
	if size := st.Size(); size > cookieMaxBytes {
		return nil, &pyError{"ValueError", fmt.Sprintf("Cookie file too large (max %d bytes, got %d)", cookieMaxBytes, size)}
	}
	text, err := readUTF8Text(resolved)
	if err != nil {
		return nil, err
	}
	cookies := netscapeParse(text)
	if domainSuffix != nil && *domainSuffix != "" {
		var kept []*pyjson.Object
		for _, c := range cookies {
			if domain, _ := c.Get("domain"); strings.HasSuffix(pyrepr.Str(domain), *domainSuffix) {
				kept = append(kept, c)
			}
		}
		cookies = kept
	}
	out := make([]*pyjson.Object, 0, len(cookies))
	for _, c := range cookies {
		sanitized := pyjson.NewObject()
		for _, k := range cookieAllowedFields {
			if v, ok := c.Get(k); ok {
				sanitized.Set(k, v)
			}
		}
		out = append(out, sanitized)
	}
	return out, nil
}

// Imports a Netscape cookies.txt into the task's Camofox user through POST /sessions/user/cookies
func (s *scope) browserImportCookies(cookiesPath string, domainSuffix *string, taskID string) string {
	if s.secret("CAMOFOX_API_KEY") == "" {
		return fail("CAMOFOX_API_KEY is not set. Cookie import is disabled unless you set " +
			"CAMOFOX_API_KEY on both the Camofox server and Icebath.")
	}
	cookies, err := s.readCookieFile(cookiesPath, domainSuffix)
	if err != nil {
		return fail(err.Error())
	}
	if len(cookies) == 0 {
		return fail("No cookies found in file (after optional domain filter).")
	}
	if len(cookies) > cookieMaxCount {
		return fail(fmt.Sprintf("Too many cookies (%d). Maximum is %d per request.", len(cookies), cookieMaxCount))
	}
	ss := s.getSession(taskID)
	list := make([]any, len(cookies))
	for i, c := range cookies {
		list[i] = c
	}
	data, err := s.post("/sessions/"+ss.userID+"/cookies", pyjson.ObjectOf("cookies", list), commandTimeoutDefault)
	if err != nil {
		if he, ok := err.(*httpError); ok {
			var detail any = ""
			if body, jerr := decodeJSON(he.resp.Body); jerr == nil {
				if !pyrepr.Truthy(body) {
					body = pyjson.NewObject()
				}
				d, gerr := get(body, "error", "")
				if gerr != nil {
					panic(gerr)
				}
				detail = d
			}
			shown := pyOrStr(detail)
			if shown == "" {
				shown = he.Error()
			}
			return fail(fmt.Sprintf("Camofox cookie import failed (HTTP %d): %s", he.status, shown))
		}
		if isConnectionError(err) {
			return fail(s.connectionErrorMessage())
		}
		panic(err)
	}
	var count any = len(cookies)
	if isDict(data) {
		count, _ = get(data, "count", len(cookies))
	}
	toolsLog().Info(fmt.Sprintf("camofox cookies imported task=%s user=%s count=%s", logTask(taskID), ss.userID, pyrepr.Str(count)))
	return dumps(pyjson.ObjectOf("success", true, "imported", count, "user_id", ss.userID))
}

// Cookie import needs the server URL and the bearer token the server's cookie route checks
func checkCookieImportAvailable(env *tools.Env) bool {
	s := newScope(context.Background(), env)
	return s.camofoxURL() != "" && s.secret("CAMOFOX_API_KEY") != ""
}

// Tools are offered once CAMOFOX_URL is set, no network probe at schema build
func checkCamofoxConfigured(env *tools.Env) bool {
	return newScope(context.Background(), env).camofoxURL() != ""
}

// browser.headed of the profile, where the per turn cleanup is skipped
func (s *scope) headedMode() bool { return truthy(s.browserCfg("headed", nil)) }

// End of session cleanup, managed identities keeping their server profile and ephemeral ones deleted
func (s *scope) cleanupTask(taskID string) {
	if s.softCleanup(taskID) {
		return
	}
	if warning := s.closeSession(taskID); warning != "" {
		toolsLog().Debug(fmt.Sprintf("Camofox close for task %s: %s", logTask(taskID), warning))
	}
}

// str of an argument or the default when absent
func argStr(args map[string]any, key, def string) string {
	v, ok := args[key]
	if !ok {
		return def
	}
	return pyrepr.Str(v)
}

// str of an optional argument, nil when absent or null
func argOptStr(args map[string]any, key string) *string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	text := pyrepr.Str(v)
	return &text
}

// Registry handler running one tool body in the call's profile scope
func handler(run func(s *scope, args map[string]any, taskID string) string) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		return run(newScope(ctx, call.Env), call.Args, call.TaskID)
	}
}

var (
	handleNavigate = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserNavigate(argStr(a, "url", ""), task)
	})
	handleSnapshot = handler(func(s *scope, _ map[string]any, task string) string { return s.browserSnapshot(task) })
	handleClick    = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserClick(argStr(a, "ref", ""), task)
	})
	handleType = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserType(argStr(a, "ref", ""), argStr(a, "text", ""), task)
	})
	handleScroll = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserScroll(argStr(a, "direction", "down"), task)
	})
	handleBack  = handler(func(s *scope, _ map[string]any, task string) string { return s.browserBack(task) })
	handlePress = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserPress(argStr(a, "key", ""), task)
	})
	handleGetImages = handler(func(s *scope, _ map[string]any, task string) string { return s.browserGetImages(task) })
	handleConsole   = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserConsole(argOptStr(a, "expression"), task)
	})
	handleImportCookies = handler(func(s *scope, a map[string]any, task string) string {
		return s.browserImportCookies(argStr(a, "cookies_path", ""), argOptStr(a, "domain_suffix"), task)
	})
)

// browser_vision handler reading the plugin LLM facade at call time
func visionHandler(llm func() *plugins.PluginLlm) tools.Handler {
	return handler(func(s *scope, a map[string]any, task string) string {
		return s.browserVision(argStr(a, "question", ""), task, llm())
	})
}

// Logger of the reference camofox_tools module
func toolsLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_plugins.camofox.camofox_tools")
}
