// The camofox browser tools: a self hosted camofox-browser server driven over its REST API, its tab the page browser_vault fills
package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/browser"
	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	camofoxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/config"
	sessionstore "github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	scrollRepeat    = 5
	evalUnsupported = "JavaScript evaluation is not supported by this Camofox server. Use browser_snapshot or browser_vision to inspect page state."
	tabGone         = "The browser tab is gone (the Camofox server reaped it or restarted). Call browser_navigate to open the page again."
	tabChanged      = "The browser tab was replaced (the Camofox server reaped or restarted it); now on %s. Element refs from earlier snapshots no " +
		"longer apply: call browser_snapshot, then retry."
	cookieMaxBytes = 5 << 20
	cookieMaxCount = 500
)

// Cookie fields the server takes, in the order they are sent
var cookieFields = []string{"name", "value", "domain", "path", "expires", "httpOnly", "secure", "sameSite"}

var (
	imgAlt = regexp2.MustCompile(`img\s+"([^"]*)"`, regexp2.RE2)
	imgURL = regexp2.MustCompile(`/url:\s*(\S+)`, regexp2.RE2)
)

type registrant struct {
	config *config.Config
	mu     sync.Mutex
	// Sessions by profile and task
	sessions map[slot]*session
	// VNC address each server advertised, nil for one that advertised none
	vncMu sync.Mutex
	vnc   map[string]*string
}

// The camofox host of the plugin: what `icebath camofox` reaches
type Camofox struct{ r *registrant }

func Register(registry *tools.Registry, stores *sessionstore.Registry, browsers *browser.Registry, cfg *config.Config) *Camofox {
	r := &registrant{config: cfg, sessions: map[slot]*session{}, vnc: map[string]*string{}}
	registry.Register(r)
	browsers.Register(browser.Declared{Holders: []browser.Page{{Name: "camofox", Title: "Camofox", Section: camofoxconfig.Key, Has: r.hasPage, Evaluate: r.evaluatePage}}})
	stores.OnRelease(func(ctx context.Context, session string) { r.release(ctx, session) })
	return &Camofox{r: r}
}

func (r *registrant) scope(ctx context.Context, cfg *config.Config, env *tools.Env) *scope {
	return &scope{r: r, ctx: ctx, cfg: cfg, env: env}
}

// Result as JSON with its keys in the order they mean most
func dumps(v map[string]any) string {
	keys := []string{"success", "error", "url", "title", "requested_url", "url_rewrite", "warning", "vnc_url", "vnc_hint", "snapshot", "element_count",
		"clicked", "typed", "element", "scrolled", "pressed", "images", "count", "result", "result_type", "console_messages", "js_errors",
		"total_messages", "total_errors", "note", "analysis", "screenshot_path", "imported", "user_id", "blocked_by_policy"}
	var b strings.Builder
	b.WriteByte('{')
	written := 0
	put := func(k string, val any) {
		if written > 0 {
			b.WriteString(", ")
		}
		written++
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteString(": ")
		b.WriteString(tools.JSON(val))
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if val, ok := v[k]; ok {
			put(k, val)
			seen[k] = true
		}
	}
	for k, val := range v {
		if !seen[k] {
			put(k, val)
		}
	}
	b.WriteByte('}')
	return b.String()
}

func fail(message string) string { return dumps(map[string]any{"error": message, "success": false}) }

// Body of one tab tool on the session, its result or the error it ran into
type tabBody func(ss *session) (string, error)

func (s *scope) snapshotData(ss *session) (any, error) {
	return s.get(tabPath(ss, "snapshot"), userParams(ss), commandTimeout)
}

// Masked snapshot, cut at browser.snapshot_threshold, and its count of refs
func (s *scope) snapshot(ss *session) (string, any, error) {
	data, err := s.snapshotData(ss)
	if err != nil {
		return "", nil, err
	}
	raw, err := field(data, "snapshot", "")
	if err != nil {
		return "", nil, err
	}
	snapshot, ok := redactAny(raw).(string)
	if !ok {
		return "", nil, errors.New("the snapshot reply carries no snapshot text")
	}
	if limit := s.threshold(); int64(utf8.RuneCountInString(snapshot)) > limit {
		snapshot = s.truncate(snapshot, limit)
	}
	refs, err := field(data, "refsCount", 0)
	if err != nil {
		return "", nil, err
	}
	return snapshot, refs, nil
}

// Opens the address in the task's tab, a 404 or 410 for the tab opening a fresh one
func (s *scope) navigateTab(task, target string) (*session, any, error) {
	ss := s.session(task)
	if ss.hasTab() {
		data, err := s.post(tabPath(ss, "navigate"), map[string]any{"userId": ss.userID, "url": target}, 60)
		if err == nil {
			return ss, data, nil
		}
		var he *httpError
		if !errors.As(err, &he) || he.status != 404 && he.status != 410 {
			return nil, nil, err
		}
		slog.Warn(fmt.Sprintf("Camofox tab %s returned %d — tab is gone. Creating a fresh tab.", text(ss.tab()), he.status))
		ss.setTab(nil)
	}
	ss, err := s.ensureTab(task, target)
	if err != nil {
		return nil, nil, err
	}
	return ss, map[string]any{"ok": true, "url": target}, nil
}

// Runs the body on the session: after a gone tab a read runs again on the live one, an input action is refused
func (s *scope) onTab(ss *session, body tabBody, readOnly bool) (string, error) {
	tracked := ss.tab()
	out, err := body(ss)
	if err == nil {
		return out, nil
	}
	if !statusIn(err, 404, 410) {
		return "", err
	}
	if !s.resync(ss) {
		return fail(tabGone), nil
	}
	if ss.tab() == tracked {
		return "", err
	}
	slog.Info(fmt.Sprintf("Camofox tab %s was gone; continuing in live tab %s", text(tracked), text(ss.tab())))
	if !readOnly {
		place := "another page"
		if u := s.tabURL(ss); u != "" {
			place = u
		}
		return fail(fmt.Sprintf(tabChanged, place)), nil
	}
	return body(ss)
}

// Address of the session's tab as /tabs lists it, empty on any failure
func (s *scope) tabURL(ss *session) string {
	tabs, err := s.listTabs(ss, 5)
	if err != nil {
		return ""
	}
	for _, tab := range tabs {
		if o, ok := tab.(map[string]any); ok && o["tabId"] == ss.tab() {
			return text(o["url"])
		}
	}
	return ""
}

// Recovers a panic of a tool body as its error
func try(work func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", p)
		}
	}()
	return work()
}

// Runs the body on the task's open tab, asking for a navigate first when there is none, every failure a tool error
func (s *scope) withTab(task string, body tabBody, readOnly bool) string {
	var out string
	err := try(func() error {
		ss := s.session(task)
		if !ss.hasTab() {
			out = fail(noSession)
			return nil
		}
		var err error
		out, err = s.onTab(ss, body, readOnly)
		return err
	})
	if err != nil {
		return fail(err.Error())
	}
	return out
}

// POSTs to the tab action and makes the result of the reply
func (s *scope) tabAction(task, suffix string, body map[string]any, result func(data any) (map[string]any, error)) string {
	return s.withTab(task, func(ss *session) (string, error) {
		payload := map[string]any{"userId": ss.userID}
		for k, v := range body {
			payload[k] = v
		}
		data, err := s.post(tabPath(ss, suffix), payload, commandTimeout)
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

// Title of the navigate reply, else of the task's tab in /tabs, empty when that fails
func (s *scope) navigateTitle(ss *session, data any) (string, error) {
	got, err := field(data, "title", nil)
	if err != nil {
		return "", err
	}
	if t, ok := got.(string); ok && t != "" {
		return t, nil
	}
	tabs, err := s.listTabs(ss, commandTimeout)
	if err != nil {
		slog.Debug(fmt.Sprintf("Camofox title lookup failed for tab %s: %s", text(ss.tab()), err))
		return "", nil
	}
	for _, tab := range tabs {
		if o, ok := tab.(map[string]any); ok && o["tabId"] == ss.tab() {
			t, _ := o["title"].(string)
			return t, nil
		}
	}
	return "", nil
}

func (s *scope) navigate(raw, task string) string {
	target, refused := s.navigationPolicy(raw)
	if refused != nil {
		return dumps(refused)
	}
	var out string
	err := try(func() error {
		at, rewrite, err := s.rewriteLoopback(target)
		if err != nil {
			return err
		}
		ss, data, err := s.navigateTab(task, at)
		if err != nil {
			return err
		}
		landed, err := field(data, "url", at)
		if err != nil {
			return err
		}
		title, err := s.navigateTitle(ss, data)
		if err != nil {
			return err
		}
		result := map[string]any{"success": true, "url": landed, "title": title}
		if rewrite != nil {
			result["requested_url"], result["url_rewrite"] = target, rewrite
			result["warning"] = fmt.Sprintf("Rewrote loopback URL for Docker-hosted Camofox: %s -> %s", rewrite["from"], rewrite["to"])
		}
		if vnc := s.vncURL(); vnc != "" {
			result["vnc_url"], result["vnc_hint"] = vnc, "Browser is visible via VNC. Share this link with the user so they can watch the browser live."
		}
		var snapshot string
		var refs any
		if try(func() error {
			var err error
			snapshot, refs, err = s.snapshot(ss)
			return err
		}) == nil {
			result["snapshot"], result["element_count"] = snapshot, refs
		}
		out = dumps(result)
		return nil
	})
	var he *httpError
	var ce *connectionError
	switch {
	case err == nil:
		return out
	case errors.As(err, &he):
		return fail("Navigation failed: " + err.Error())
	case errors.As(err, &ce):
		return dumps(map[string]any{"success": false, "error": s.connectionMessage()})
	}
	return fail(err.Error())
}

func (s *scope) snapshotTool(task string) string {
	return s.withTab(task, func(ss *session) (string, error) {
		snapshot, refs, err := s.snapshot(ss)
		if err != nil {
			return "", err
		}
		return dumps(map[string]any{"success": true, "snapshot": snapshot, "element_count": refs}), nil
	}, true)
}

func (s *scope) click(ref, task string) string {
	clean := strings.TrimLeft(ref, "@")
	return s.tabAction(task, "click", map[string]any{"ref": clean}, func(data any) (map[string]any, error) {
		landed, err := field(data, "url", "")
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "clicked": clean, "url": landed}, nil
	})
}

// Types the text into the ref, the result masking text that looks like a secret
func (s *scope) typeText(ref, typed, task string) string {
	var out string
	err := try(func() error {
		ss := s.session(task)
		if !ss.hasTab() {
			out = fail(noSession)
			return nil
		}
		clean := strings.TrimLeft(ref, "@")
		body := func(sess *session) (string, error) {
			if _, err := s.post(tabPath(sess, "type"), map[string]any{"userId": sess.userID, "ref": clean, "text": typed}, commandTimeout); err != nil {
				return "", err
			}
			return dumps(redactTyped(map[string]any{"success": true, "typed": redactText(typed), "element": clean}, typed).(map[string]any)), nil
		}
		res, err := s.onTab(ss, body, false)
		if err != nil {
			return err
		}
		out = redactTyped(res, typed).(string)
		return nil
	})
	if err != nil {
		return fail(redactTyped(err.Error(), typed).(string))
	}
	return out
}

// Scrolls five fixed steps, a failed step the answer
func (s *scope) scroll(direction, task string) string {
	if direction != "up" && direction != "down" {
		return dumps(map[string]any{"success": false, "error": fmt.Sprintf("Invalid direction '%s'. Use 'up' or 'down'.", direction)})
	}
	result := ""
	for range scrollRepeat {
		result = s.tabAction(task, "scroll", map[string]any{"direction": direction}, func(any) (map[string]any, error) {
			return map[string]any{"success": true, "scrolled": direction}, nil
		})
		var parsed map[string]any
		if json.Unmarshal([]byte(result), &parsed) != nil || parsed["success"] != true {
			break
		}
	}
	return result
}

func (s *scope) back(task string) string {
	return s.tabAction(task, "back", map[string]any{}, func(data any) (map[string]any, error) {
		landed, err := field(data, "url", "")
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": true, "url": landed}, nil
	})
}

func (s *scope) press(key, task string) string {
	return s.tabAction(task, "press", map[string]any{"key": key}, func(any) (map[string]any, error) {
		return map[string]any{"success": true, "pressed": key}, nil
	})
}

// Images of an accessibility snapshot: the alt of each img and the address on the /url line after it
func snapshotImages(snapshot any) ([]any, error) {
	body, ok := snapshot.(string)
	if !ok {
		return nil, fmt.Errorf("expected snapshot text, got %T", snapshot)
	}
	images := []any{}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		stripped := strings.TrimSpace(line)
		if !strings.HasPrefix(stripped, "- img ") && !strings.HasPrefix(stripped, "img ") {
			continue
		}
		alt, src := "", ""
		if m, _ := imgAlt.FindStringMatch(stripped); m != nil {
			alt = m.GroupByNumber(1).String()
		}
		if i+1 < len(lines) {
			if m, _ := imgURL.FindStringMatch(strings.TrimSpace(lines[i+1])); m != nil {
				src = m.GroupByNumber(1).String()
			}
		}
		if alt != "" || src != "" {
			images = append(images, map[string]any{"src": src, "alt": alt})
		}
	}
	return images, nil
}

func (s *scope) images(task string) string {
	return s.withTab(task, func(ss *session) (string, error) {
		data, err := s.snapshotData(ss)
		if err != nil {
			return "", err
		}
		snapshot, err := field(data, "snapshot", "")
		if err != nil {
			return "", err
		}
		parsed, err := snapshotImages(snapshot)
		if err != nil {
			return "", err
		}
		images := redactOutput(parsed, 0).([]any)
		return dumps(map[string]any{"success": true, "images": images, "count": len(images)}), nil
	}, true)
}

// Value of a script, JSON text read into what it holds
func evalValue(raw any) any {
	if s, ok := raw.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			return parsed
		}
	}
	return raw
}

// Name of a value's kind as scripts' results have always named it
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		if f := v.(float64); f == float64(int64(f)) {
			return "int"
		}
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Evaluates on the open tab only, a server without /evaluate named as such
func (s *scope) evaluate(expression, task string) string {
	var out string
	err := try(func() error {
		ss := s.session(task)
		if !ss.hasTab() {
			out = fail(noSession)
			return nil
		}
		body := func(sess *session) (string, error) {
			resp, err := s.post(tabPath(sess, "evaluate"), map[string]any{"expression": expression, "userId": sess.userID}, commandTimeout)
			if err != nil {
				return "", err
			}
			raw := resp
			if o, ok := resp.(map[string]any); ok {
				raw = o["result"]
			}
			parsed := evalValue(raw)
			return dumps(map[string]any{"success": true, "result": redactOutput(parsed, 0), "result_type": kind(parsed)}), nil
		}
		var err error
		out, err = s.onTab(ss, body, false)
		return err
	})
	switch {
	case err == nil:
		return out
	case statusIn(err, 404, 405, 501):
		return dumps(map[string]any{"success": false, "error": evalUnsupported})
	}
	return fail(err.Error())
}

func (s *scope) console(expression *string, task string) string {
	if expression != nil {
		if refusal := s.evalPolicy(*expression); refusal != "" {
			return dumps(map[string]any{"success": false, "error": refusal})
		}
		return s.evaluate(*expression, task)
	}
	return dumps(map[string]any{"success": true, "console_messages": []any{}, "js_errors": []any{}, "total_messages": 0, "total_errors": 0,
		"note": "Console log capture is not available with the Camofox backend. Use browser_snapshot or browser_vision to inspect page state."})
}

// Saves a capture among the profile's screenshots and answers its path
func (s *scope) saveShot(content []byte) (string, error) {
	dir := filepath.Join(s.cfg.ProfileDir(s.cfg.Profile()), "cache", "screenshots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 4)
	rand.Read(raw)
	path := filepath.Join(dir, "browser_screenshot_"+hex.EncodeToString(raw)+".png")
	return path, os.WriteFile(path, content, 0o600)
}

// Captures the tab and answers the question with the helper model of the camofox_vision task
func (s *scope) vision(question, task string) string {
	return s.withTab(task, func(ss *session) (string, error) {
		shot, err := s.request("GET", tabPath(ss, "screenshot"), commandTimeout, userParams(ss), nil, false)
		if err != nil {
			return "", err
		}
		path, err := s.saveShot(shot.body)
		if err != nil {
			return "", err
		}
		temperature := cast.ToFloat64(s.cfg.Get(auxiliary.Task{Name: "vision"}.Key("temperature")))
		analysis, err := func() (string, error) {
			if s.env == nil || s.env.Aux == nil {
				return "", errors.New("no helper model is available to analyze the screenshot")
			}
			return s.env.Aux.Complete(s.ctx, auxiliary.Request{Task: camofoxconfig.Task, Messages: auxiliary.Prompt("",
				"Analyze this browser screenshot and answer: "+question, &sessionsv1.Media{MimeType: "image/png", Data: shot.body}),
				Temperature: &temperature})
		}()
		if err != nil {
			slog.Warn(fmt.Sprintf("browser_vision analysis failed: %v", err))
			return dumps(map[string]any{"success": false, "error": "Error during vision analysis: " + err.Error(), "screenshot_path": path,
				"note": "Screenshot was captured but vision analysis failed. You can still share it via MEDIA:<path>."}), nil
		}
		analysis = strings.TrimSpace(analysis)
		if analysis == "" {
			analysis = "Vision analysis returned no content."
		}
		return dumps(map[string]any{"success": true, "analysis": redactText(analysis), "screenshot_path": path}), nil
	}, true)
}

// Cookies of a Netscape cookies.txt as the server takes them, a BOM, HttpOnly marks and values with tabs handled
func netscape(body string) []map[string]any {
	var cookies []map[string]any
	for _, raw := range strings.Split(strings.TrimLeft(body, "\uFEFF"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		httpOnly := strings.HasPrefix(line, "#HttpOnly_")
		line = strings.TrimPrefix(line, "#HttpOnly_")
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		expires, err := cast.ToInt64E(strings.TrimSpace(parts[4]))
		if err != nil {
			continue
		}
		cookies = append(cookies, map[string]any{"name": parts[5], "value": strings.Join(parts[6:], "\t"), "domain": parts[0], "path": parts[2],
			"expires": expires, "httpOnly": httpOnly, "secure": strings.ToUpper(parts[3]) == "TRUE"})
	}
	return cookies
}

// browser.camofox.cookies_dir, ~ expanded
func (s *scope) cookiesDir() string {
	raw := s.setting(camofoxconfig.CookiesDir)
	if raw == "" {
		raw = "~/.camofox/cookies"
	}
	return filepath.Clean(fsx.ExpandHome(raw))
}

// Path with every link in it followed, as far as the path exists
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// Cookies of a file inside the cookies folder, at most 5 MB, kept to the domain suffix
func (s *scope) cookieFile(path string, suffix *string) ([]map[string]any, error) {
	base, err := resolve(s.cookiesDir())
	if err != nil {
		return nil, err
	}
	outside := errors.New("cookies_path must be a relative path within the cookies directory")
	if filepath.IsAbs(path) {
		return nil, outside
	}
	resolved, err := resolve(filepath.Join(base, path))
	if err != nil {
		return nil, err
	}
	if !fsx.Within(resolved, base) {
		return nil, outside
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("Cookie file not found: " + path)
	}
	if st.Size() > cookieMaxBytes {
		return nil, fmt.Errorf("Cookie file too large (max %d bytes, got %d)", cookieMaxBytes, st.Size())
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("'utf-8' codec can't decode the cookie file: it is not UTF-8 text")
	}
	body := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	cookies := netscape(body)
	if suffix != nil && *suffix != "" {
		var kept []map[string]any
		for _, c := range cookies {
			if strings.HasSuffix(text(c["domain"]), *suffix) {
				kept = append(kept, c)
			}
		}
		cookies = kept
	}
	out := make([]map[string]any, 0, len(cookies))
	for _, c := range cookies {
		clean := map[string]any{}
		for _, k := range cookieFields {
			if v, ok := c[k]; ok {
				clean[k] = v
			}
		}
		out = append(out, clean)
	}
	return out, nil
}

// Imports a cookies.txt into the task's Camofox user
func (s *scope) importCookies(path string, suffix *string, task string) string {
	if s.setting(camofoxconfig.APIKey) == "" {
		return fail("CAMOFOX_API_KEY is not set. Cookie import is disabled unless you set CAMOFOX_API_KEY on both the Camofox server and Icebath.")
	}
	cookies, err := s.cookieFile(path, suffix)
	if err != nil {
		return fail(err.Error())
	}
	if len(cookies) == 0 {
		return fail("No cookies found in file (after optional domain filter).")
	}
	if len(cookies) > cookieMaxCount {
		return fail(fmt.Sprintf("Too many cookies (%d). Maximum is %d per request.", len(cookies), cookieMaxCount))
	}
	ss := s.session(task)
	list := make([]any, len(cookies))
	for i, c := range cookies {
		list[i] = c
	}
	data, err := s.post("/sessions/"+url.PathEscape(ss.userID)+"/cookies", map[string]any{"cookies": list}, commandTimeout)
	if err != nil {
		var he *httpError
		var ce *connectionError
		switch {
		case errors.As(err, &he):
			shown := ""
			if body, jerr := decode(he.body); jerr == nil {
				if o, ok := body.(map[string]any); ok {
					shown = text(o["error"])
				}
			}
			if shown == "" {
				shown = he.Error()
			}
			return fail(fmt.Sprintf("Camofox cookie import failed (HTTP %d): %s", he.status, shown))
		case errors.As(err, &ce):
			return fail(s.connectionMessage())
		}
		return fail(err.Error())
	}
	var count any = len(cookies)
	if o, ok := data.(map[string]any); ok {
		if v, has := o["count"]; has {
			count = v
		}
	}
	slog.Info(fmt.Sprintf("camofox cookies imported task=%s user=%s count=%v", task, ss.userID, count))
	return dumps(map[string]any{"success": true, "imported": count, "user_id": ss.userID})
}

// Whether the task holds a Camofox tab, never opening one
func (r *registrant) hasPage(cfg *config.Config, task string) bool {
	s := r.scope(context.Background(), cfg, nil)
	if s.url() == "" {
		return false
	}
	ss := s.tracked(task)
	return ss != nil && ss.hasTab()
}

var loopbacks = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Whether a secret may go to the server: HTTPS, or HTTP on loopback, with no userinfo, query or fragment
func secretTransport(raw string) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, err
	}
	pass, _ := u.User.Password()
	if u.User.Username() != "" || pass != "" || u.RawQuery != "" || u.Fragment != "" {
		return false, nil
	}
	return u.Scheme == "https" || u.Scheme == "http" && loopbacks[strings.ToLower(u.Hostname())], nil
}

// Runs a script on the task's tab, one that carries a secret sent only to loopback or HTTPS with no proxy or redirect between
func (r *registrant) evaluatePage(ctx context.Context, cfg *config.Config, task, expression string, secret bool) (any, error) {
	s := r.scope(ctx, cfg, nil)
	ss := s.tracked(task)
	if ss == nil || !ss.hasTab() {
		return nil, errors.New(noSession)
	}
	if secret {
		ok, err := secretTransport(s.url())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("vault fills need CAMOFOX_URL on loopback or HTTPS")
		}
	}
	got, err := s.request("POST", tabPath(ss, "evaluate"), commandTimeout, nil, map[string]any{"expression": expression, "userId": ss.userID}, true)
	if err != nil {
		return nil, err
	}
	resp, err := decode(got.body)
	if err != nil {
		return nil, err
	}
	obj, ok := resp.(map[string]any)
	result, has := obj["result"]
	if !ok || !has || config.Truthy(obj["error"]) {
		msg := ""
		if ok {
			msg = text(obj["error"])
		}
		if msg == "" {
			msg = "evaluate failed"
		}
		return nil, errors.New(msg)
	}
	return result, nil
}

// Ends the task's session after every turn, unless browser.headed keeps the window for someone watching
func (r *registrant) Ended(env *tools.Env, session string) {
	if session == "" || env.Config.GetBool(browserconfig.Headed) {
		return
	}
	r.scope(context.Background(), env.Config, env).cleanupTask(session)
}

// Ends the sessions of a conversation as it closes, under every profile one ran in
func (r *registrant) release(ctx context.Context, session string) {
	r.mu.Lock()
	var profiles []string
	for key := range r.sessions {
		if key.task == session {
			profiles = append(profiles, key.profile)
		}
	}
	r.mu.Unlock()
	for _, name := range profiles {
		if cfg, err := r.config.Of(name); err == nil {
			r.scope(ctx, cfg, nil).cleanupTask(session)
		}
	}
}

func stringParam(description string) map[string]any { return tools.Property("string", description) }

func schema(description string, properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"description": description, "parameters": map[string]any{"type": "object", "properties": properties, "required": required}}
}

func arg(args map[string]any, key, def string) string {
	v, ok := args[key]
	if !ok {
		return def
	}
	return text(v)
}

func optional(args map[string]any, key string) *string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	t := text(v)
	return &t
}

func (r *registrant) handler(run func(s *scope, args map[string]any, task string) string) tools.Handler {
	return func(ctx context.Context, call *tools.Call) string {
		return run(r.scope(ctx, call.Env.Config, call.Env), call.Args, call.SessionID)
	}
}

func (r *registrant) Tools() []tools.Def {
	configured := func(env *tools.Env) bool { return r.scope(context.Background(), env.Config, env).url() != "" }
	cookies := func(env *tools.Env) bool {
		s := r.scope(context.Background(), env.Config, env)
		return s.url() != "" && s.setting(camofoxconfig.APIKey) != ""
	}
	def := func(name, title, about, icon, subject string, effect tools.Effect, spec map[string]any, check func(*tools.Env) bool,
		run tools.Handler) tools.Def {
		return tools.Def{Name: name, Toolset: camofoxconfig.Toolset, Title: title, About: about, Group: "Browser", Icon: icon, Subject: subject,
			Schema: spec, Handler: run, Check: check, Lacks: lacks, Effect: effect, Fallible: true, Untrusted: true}
	}
	return []tools.Def{
		def("browser_navigate", "Open page", "Open a web page in the Camofox browser", tools.IconFetch, "url", tools.MayChange, schema("Navigate to a URL in the browser. Initializes the session "+
			"and loads the page. Must be called before other browser tools. For simple information retrieval, prefer a lightweight retrieval tool "+
			"when one is available (faster, cheaper). For plain-text endpoints — URLs ending in .md, .txt, .json, .yaml, .yml, .csv, .xml, "+
			"raw.githubusercontent.com, or any documented API endpoint — prefer an available text-extraction or terminal-fetch tool; the browser "+
			"stack is overkill and much slower for these. Use browser tools when you need to interact with a page (click, fill forms, dynamic "+
			"content). Returns a compact page snapshot with interactive elements and ref IDs — no need to call browser_snapshot separately after "+
			"navigating.", map[string]any{"url": stringParam("The URL to navigate to (e.g., 'https://example.com')")}, "url"), configured,
			r.handler(func(s *scope, a map[string]any, task string) string { return s.navigate(arg(a, "url", ""), task) })),
		def("browser_snapshot", "Read page", "Read the text and buttons of the open page", tools.IconRead, "", tools.ReadOnly, schema("Get a text-based snapshot of the current page's "+
			"accessibility tree. Returns interactive elements with ref IDs (like @e1, @e2) for browser_click and browser_type. Snapshots over 15000 "+
			"chars are truncated; when that happens the complete snapshot is saved to a file and the output includes its path so you can page "+
			"through the rest with read_file. Requires browser_navigate first. Note: browser_navigate already returns a snapshot — use this to "+
			"refresh after interactions that change the page.", map[string]any{}), configured,
			r.handler(func(s *scope, _ map[string]any, task string) string { return s.snapshotTool(task) })),
		def("browser_click", "Click", "Click a button or a link on the page", tools.IconWrite, "ref", tools.Mutates, schema("Click on an element identified by its ref ID from the snapshot "+
			"(e.g., '@e5'). The ref IDs are shown in square brackets in the snapshot output. Requires browser_navigate and browser_snapshot to be "+
			"called first.", map[string]any{"ref": stringParam("The element reference from the snapshot (e.g., '@e5', '@e12')")}, "ref"), configured,
			r.handler(func(s *scope, a map[string]any, task string) string { return s.click(arg(a, "ref", ""), task) })),
		def("browser_type", "Type", "Type text into a field on the page", tools.IconWrite, "ref", tools.Mutates, schema("Type text into an input field identified by its ref ID. Clears "+
			"the field first, then types the new text. Requires browser_navigate and browser_snapshot to be called first.", map[string]any{
			"ref":  stringParam("The element reference from the snapshot (e.g., '@e3')"),
			"text": stringParam("The text to type into the field"),
		}, "ref", "text"), configured,
			r.handler(func(s *scope, a map[string]any, task string) string {
				return s.typeText(arg(a, "ref", ""), arg(a, "text", ""), task)
			})),
		def("browser_scroll", "Scroll", "Scroll the page up or down", tools.IconRead, "direction", tools.MayChange, schema("Scroll the page in a direction. Use this to reveal "+
			"more content that may be below or above the current viewport. Requires browser_navigate to be called first.", map[string]any{
			"direction": map[string]any{"type": "string", "enum": []string{"up", "down"}, "description": "Direction to scroll"},
		}, "direction"), configured,
			r.handler(func(s *scope, a map[string]any, task string) string {
				return s.scroll(arg(a, "direction", "down"), task)
			})),
		def("browser_back", "Go back", "Go back to the page before", tools.IconFetch, "", tools.MayChange, schema("Navigate back to the previous page in browser history. Requires "+
			"browser_navigate to be called first.", map[string]any{}), configured,
			r.handler(func(s *scope, _ map[string]any, task string) string { return s.back(task) })),
		def("browser_press", "Press key", "Press a key on the page, such as Enter or Tab", tools.IconWrite, "key", tools.Mutates, schema("Press a keyboard key. Useful for submitting forms (Enter), "+
			"navigating (Tab), or keyboard shortcuts. Requires browser_navigate to be called first.",
			map[string]any{"key": stringParam("Key to press (e.g., 'Enter', 'Tab', 'Escape', 'ArrowDown')")}, "key"), configured,
			r.handler(func(s *scope, a map[string]any, task string) string { return s.press(arg(a, "key", ""), task) })),
		def("browser_get_images", "List images", "List the images on the page", tools.IconRead, "", tools.ReadOnly, schema("Get a list of all images on the current page with their "+
			"URLs and alt text. Useful for finding images to analyze with the vision tool. Requires browser_navigate to be called first.",
			map[string]any{}), configured,
			r.handler(func(s *scope, _ map[string]any, task string) string { return s.images(task) })),
		def("browser_vision", "Look at page", "Take a screenshot of the page and look at it", tools.IconRead, "question", tools.ReadOnly, schema("Take a screenshot of the current page so you can "+
			"inspect it visually. Use this when you need to understand what the page looks like - especially for CAPTCHAs, visual verification "+
			"challenges, complex layouts, or cases where the text snapshot misses important visual information. An auxiliary vision model analyzes "+
			"the screenshot and returns a text answer to your question. Includes a screenshot_path that you can share with the user by including "+
			"MEDIA:<screenshot_path> in your response. Requires browser_navigate to be called first.", map[string]any{
			"question": stringParam("What you want to know about the page visually. Be specific about what you're looking for."),
		}, "question"), configured,
			r.handler(func(s *scope, a map[string]any, task string) string { return s.vision(arg(a, "question", ""), task) })),
		def("browser_console", "Run page script", "Run JavaScript on the page", tools.IconShell, "expression", tools.MayChange, schema("Evaluate JavaScript in the page context and "+
			"return the result when 'expression' is provided — use this for DOM inspection, reading page state, or extracting data "+
			"programmatically. Console log and JavaScript error capture are not available with the Camofox backend, so a call without 'expression' "+
			"returns an empty result. Requires browser_navigate to be called first.", map[string]any{
			"expression": stringParam("JavaScript expression to evaluate in the page context. Runs in the browser like DevTools console — full access " +
				"to DOM, window, document. Return values are serialized to JSON. Example: 'document.title' or " +
				"'document.querySelectorAll(\"a\").length'"),
		}), configured,
			r.handler(func(s *scope, a map[string]any, task string) string {
				return s.console(optional(a, "expression"), task)
			})),
		def("browser_import_cookies", "Import cookies", "Sign in to sites with cookies from a file", tools.IconWrite, "cookies_path", tools.Mutates, schema("Import cookies from a "+
			"Netscape-format cookies.txt file into the current browser session. Use to authenticate to sites without interactive login. Files must "+
			"live under browser.camofox.cookies_dir (default ~/.camofox/cookies/). Requires CAMOFOX_API_KEY for bearer-token authentication.",
			map[string]any{
				"cookies_path":  stringParam("Path to the Netscape cookies.txt file, relative to browser.camofox.cookies_dir (e.g., 'linkedin.txt')"),
				"domain_suffix": stringParam("Optional. Only import cookies whose domain ends with this suffix (e.g., '.linkedin.com')"),
			}, "cookies_path"), cookies,
			r.handler(func(s *scope, a map[string]any, task string) string {
				return s.importCookies(arg(a, "cookies_path", ""), optional(a, "domain_suffix"), task)
			})),
	}
}

// What `icebath camofox status` shows of the profile's server
type Status struct {
	URL, Identity, VNC string
	Reachable          bool
}

func (c *Camofox) Status(ctx context.Context, cfg *config.Config) Status {
	s := c.r.scope(ctx, cfg, nil)
	st := Status{URL: s.url(), Identity: s.identityMode()}
	if st.URL == "" {
		return st
	}
	if st.Reachable = s.available(); st.Reachable {
		st.VNC = s.vncURL()
	}
	return st
}

// Saves the server address, and its key when one is given, to the profile
func (c *Camofox) Setup(cfg *config.Config, address, key string) error {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address == "" {
		address = camofoxconfig.DefaultURL
	}
	if _, err := prepare(address); err != nil {
		return err
	}
	if err := cfg.Set(camofoxconfig.URL, address); err != nil {
		return err
	}
	if key = strings.TrimSpace(key); key != "" {
		return cfg.Set(camofoxconfig.APIKey, key)
	}
	return nil
}

// What a Camofox tool lacks to run: the address of the Camofox server, or the API key the cookies of a page need
func lacks(env *tools.Env) string {
	return "It needs the Camofox browser set up, with its address and, for cookies, its API key."
}

// The Camofox tools join the group of the browser tools
func (r *registrant) Groups() []tools.Group {
	return []tools.Group{{Title: "Browser"}}
}
