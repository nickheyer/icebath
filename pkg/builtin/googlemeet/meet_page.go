package googlemeet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Page surface the bot drives, the Playwright sync page calls the hermes bot makes
type meetPage interface {
	Evaluate(js string) (any, error)
	Locator(selector string) meetLocator
	GetByRole(role, name string, exact bool) meetLocator
	IsClosed() bool
}

// Locator surface the bot drives, first match counting, visibility, click and fill
type meetLocator interface {
	First() meetLocator
	Count() (int, error)
	IsVisible() (bool, error)
	Click(timeout time.Duration) error
	Fill(value string, timeout time.Duration) error
}

const cdpCallTimeout = 30 * time.Second

var functionLikeRe = regexp.MustCompile(`^(async\s+)?(function\b|\(?[\w$\s,]*\)?\s*=>)`)

// Launched Chromium, its CDP connection and the isolated browser context pages open in
type cdpBrowser struct {
	key       string
	conn      *browser.CDPConn
	contextID string
	mu        sync.Mutex
	origins   []string
}

// One page target attached through a flattened session
type cdpPage struct {
	b         *cdpBrowser
	targetID  string
	sessionID string
	closed    atomic.Bool
	unsub     func()
}

var meetBrowserSeq atomic.Int64

// Starts Chromium like Playwright chromium.launch with the executable, headless flag and extra args
func launchMeetBrowser(ctx context.Context, env *tools.Env, headed bool, args []string) (*cdpBrowser, error) {
	key := fmt.Sprintf("google-meet-%d-%d", os.Getpid(), meetBrowserSeq.Add(1))
	launched, failure := browser.LaunchChromium(tools.WithEnvHome(ctx, env), env, key, browser.LaunchOptions{
		Executable: browser.ChromiumExecutable(env, true), Ephemeral: true, Headed: &headed, ExtraArgs: args,
	})
	if launched == nil {
		if failure == "" {
			failure = "Chromium did not start"
		}
		return nil, errors.New(failure)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cdpCallTimeout)
	defer cancel()
	conn, err := browser.DialCDP(ctx, launched.CDPURL, 0)
	if err != nil {
		browser.StopBrowser(tools.WithEnvHome(ctx, env), key)
		return nil, err
	}
	return &cdpBrowser{key: key, conn: conn}, nil
}

// Sends one browser level CDP command with the default call timeout
func (b *cdpBrowser) call(method string, params any, sessionID string) (json.RawMessage, error) {
	return b.conn.CallTimeout(method, params, sessionID, cdpCallTimeout)
}

// Creates the browser context with viewport, user agent, permissions and an optional storage state file
func (b *cdpBrowser) newContext(args contextArgs) (*cdpContext, error) {
	var out struct {
		BrowserContextID string `json:"browserContextId"`
	}
	raw, err := b.call("Target.createBrowserContext", map[string]any{}, "")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	b.contextID = out.BrowserContextID
	c := &cdpContext{b: b, args: args}
	if len(args.Permissions) > 0 {
		perms := make([]string, 0, len(args.Permissions))
		for _, p := range args.Permissions {
			perms = append(perms, map[string]string{"microphone": "audioCapture", "camera": "videoCapture"}[p])
		}
		if _, err := b.call("Browser.grantPermissions", map[string]any{"permissions": perms, "browserContextId": b.contextID}, ""); err != nil {
			return nil, err
		}
	}
	if args.StorageState != "" {
		if err := c.applyStorageState(args.StorageState); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Closes the CDP connection and stops the launched Chromium
func (b *cdpBrowser) close(ctx context.Context) {
	_ = b.conn.Close()
	browser.StopBrowser(ctx, b.key)
}

// Records an origin a page of the context navigated to, for storage state collection
func (b *cdpBrowser) noteOrigin(origin string) {
	if origin == "" || origin == "null" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, o := range b.origins {
		if o == origin {
			return
		}
	}
	b.origins = append(b.origins, origin)
}

// Browser context options the bot passes to new_context
type contextArgs struct {
	Width, Height int
	UserAgent     string
	Permissions   []string
	StorageState  string
}

// Isolated browser context with its emulation settings
type cdpContext struct {
	b     *cdpBrowser
	args  contextArgs
	pages []*cdpPage
}

// Opens a page in the context with the context viewport and user agent applied
func (c *cdpContext) newPage() (*cdpPage, error) {
	var out struct {
		TargetID string `json:"targetId"`
	}
	raw, err := c.b.call("Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": c.b.contextID}, "")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cdpCallTimeout)
	defer cancel()
	sid, err := c.b.conn.Attach(ctx, out.TargetID)
	if err != nil {
		return nil, err
	}
	p := &cdpPage{b: c.b, targetID: out.TargetID, sessionID: sid}
	p.unsub = c.b.conn.OnEvent(p.watch)
	steps := []struct {
		method string
		params map[string]any
	}{
		{"Page.enable", map[string]any{}},
		{"Runtime.enable", map[string]any{}},
	}
	if c.args.Width > 0 {
		steps = append(steps, struct {
			method string
			params map[string]any
		}{"Emulation.setDeviceMetricsOverride", map[string]any{"width": c.args.Width, "height": c.args.Height, "deviceScaleFactor": 1, "mobile": false}})
	}
	if c.args.UserAgent != "" {
		steps = append(steps, struct {
			method string
			params map[string]any
		}{"Emulation.setUserAgentOverride", map[string]any{"userAgent": c.args.UserAgent}})
	}
	for _, s := range steps {
		if _, err := c.b.call(s.method, s.params, sid); err != nil {
			return nil, err
		}
	}
	c.pages = append(c.pages, p)
	return p, nil
}

// Closes every page and disposes the browser context
func (c *cdpContext) close() {
	for _, p := range c.pages {
		p.close()
	}
	if c.b.contextID != "" {
		_, _ = c.b.call("Target.disposeBrowserContext", map[string]any{"browserContextId": c.b.contextID}, "")
	}
}

// Loads cookies and per origin localStorage from a Playwright storage state file
func (c *cdpContext) applyStorageState(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st struct {
		Cookies []map[string]any `json:"cookies"`
		Origins []struct {
			Origin       string              `json:"origin"`
			LocalStorage []map[string]string `json:"localStorage"`
		} `json:"origins"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	if len(st.Cookies) > 0 {
		cookies := make([]map[string]any, 0, len(st.Cookies))
		for _, ck := range st.Cookies {
			out := map[string]any{}
			for _, k := range []string{"name", "value", "domain", "path", "httpOnly", "secure", "sameSite"} {
				if v, ok := ck[k]; ok {
					out[k] = v
				}
			}
			if exp, ok := ck["expires"].(float64); ok && exp != -1 {
				out["expires"] = exp
			}
			cookies = append(cookies, out)
		}
		if _, err := c.b.call("Storage.setCookies", map[string]any{"cookies": cookies, "browserContextId": c.b.contextID}, ""); err != nil {
			return err
		}
	}
	for _, o := range st.Origins {
		if len(o.LocalStorage) == 0 {
			continue
		}
		items, _ := json.Marshal(o.LocalStorage)
		js := "(() => { for (const {name, value} of " + string(items) + ") localStorage.setItem(name, value); return true; })()"
		if _, err := c.onOrigin(o.Origin, js); err != nil {
			return err
		}
	}
	return nil
}

// Runs js in a scratch page served an empty document at origin, as Playwright reaches other origins
func (c *cdpContext) onOrigin(origin, js string) (any, error) {
	p, err := c.newPage()
	if err != nil {
		return nil, err
	}
	defer func() {
		p.close()
		c.pages = c.pages[:len(c.pages)-1]
	}()
	stop := c.b.conn.OnEvent(func(ev browser.CDPEvent) {
		if ev.Method != "Fetch.requestPaused" || ev.SessionID != p.sessionID {
			return
		}
		var req struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(ev.Params, &req) != nil {
			return
		}
		go p.b.call("Fetch.fulfillRequest", map[string]any{"requestId": req.RequestID, "responseCode": 200,
			"responseHeaders": []map[string]string{{"name": "Content-Type", "value": "text/html"}},
			"body":            base64.StdEncoding.EncodeToString([]byte("<html></html>"))}, p.sessionID)
	})
	defer stop()
	if _, err := c.b.call("Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}}}, p.sessionID); err != nil {
		return nil, err
	}
	if err := p.gotoURL(origin+"/", 30*time.Second); err != nil {
		return nil, err
	}
	return p.Evaluate(js)
}

// Playwright storage state of the context, cookies plus localStorage of every visited origin
func (c *cdpContext) storageState() (*pyjson.Object, error) {
	var out struct {
		Cookies []map[string]any `json:"cookies"`
	}
	raw, err := c.b.call("Storage.getCookies", map[string]any{"browserContextId": c.b.contextID}, "")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	cookies := []any{}
	for _, ck := range out.Cookies {
		expires := ck["expires"]
		if session, _ := ck["session"].(bool); session {
			expires = -1
		}
		sameSite, ok := ck["sameSite"]
		if !ok {
			sameSite = "Lax"
		}
		cookies = append(cookies, pyjson.ObjectOf("name", ck["name"], "value", ck["value"], "domain", ck["domain"],
			"path", ck["path"], "expires", expires, "httpOnly", ck["httpOnly"], "secure", ck["secure"], "sameSite", sameSite))
	}
	read := "(() => Object.keys(localStorage).map(name => ({name, value: localStorage.getItem(name)})))()"
	c.b.mu.Lock()
	origins := append([]string(nil), c.b.origins...)
	c.b.mu.Unlock()
	entries := []any{}
	for _, origin := range origins {
		var items any
		var err error
		if live := c.pageAt(origin); live != nil {
			items, err = live.Evaluate(read)
		} else {
			items, err = c.onOrigin(origin, read)
		}
		if err != nil {
			return nil, err
		}
		list, _ := items.([]any)
		if len(list) == 0 {
			continue
		}
		storage := []any{}
		for _, it := range list {
			m, _ := it.(map[string]any)
			storage = append(storage, pyjson.ObjectOf("name", m["name"], "value", m["value"]))
		}
		entries = append(entries, pyjson.ObjectOf("origin", origin, "localStorage", storage))
	}
	return pyjson.ObjectOf("cookies", cookies, "origins", entries), nil
}

// Open page of the context whose document is at origin
func (c *cdpContext) pageAt(origin string) *cdpPage {
	for _, p := range c.pages {
		if p.IsClosed() {
			continue
		}
		if v, err := p.Evaluate("location.origin"); err == nil && v == origin {
			return p
		}
	}
	return nil
}

// Writes the storage state as two space indented JSON like Playwright storage_state(path=)
func (c *cdpContext) saveStorageState(path string) error {
	st, err := c.storageState()
	if err != nil {
		return err
	}
	text, err := pyjson.Dumps(st, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// Tracks target loss and navigated origins for this page session
func (p *cdpPage) watch(ev browser.CDPEvent) {
	switch ev.Method {
	case "Target.detachedFromTarget":
		var d struct {
			SessionID string `json:"sessionId"`
			TargetID  string `json:"targetId"`
		}
		if json.Unmarshal(ev.Params, &d) == nil && (d.SessionID == p.sessionID || d.TargetID == p.targetID) {
			p.closed.Store(true)
		}
	case "Target.targetDestroyed", "Target.targetCrashed":
		var d struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(ev.Params, &d) == nil && d.TargetID == p.targetID {
			p.closed.Store(true)
		}
	case "Inspector.detached", "Inspector.targetCrashed":
		if ev.SessionID == p.sessionID {
			p.closed.Store(true)
		}
	case "Page.frameNavigated":
		if ev.SessionID != p.sessionID {
			return
		}
		var d struct {
			Frame struct {
				URL            string `json:"url"`
				SecurityOrigin string `json:"securityOrigin"`
			} `json:"frame"`
		}
		if json.Unmarshal(ev.Params, &d) == nil {
			origin := d.Frame.SecurityOrigin
			if origin == "" {
				if u, err := url.Parse(d.Frame.URL); err == nil && u.Host != "" {
					origin = u.Scheme + "://" + u.Host
				}
			}
			p.b.noteOrigin(origin)
		}
	}
}

// Whether the page target is gone or the connection dropped
func (p *cdpPage) IsClosed() bool {
	if p.closed.Load() {
		return true
	}
	select {
	case <-p.b.conn.Done():
		return true
	default:
		return false
	}
}

// Closes the page target
func (p *cdpPage) close() {
	if p.unsub != nil {
		p.unsub()
	}
	if !p.IsClosed() {
		_, _ = p.b.call("Target.closeTarget", map[string]any{"targetId": p.targetID}, "")
	}
	p.closed.Store(true)
}

// Navigates and waits for DOMContentLoaded within timeout like goto with wait_until domcontentloaded
func (p *cdpPage) gotoURL(target string, timeout time.Duration) error {
	loaded := make(chan struct{}, 1)
	stop := p.b.conn.OnEvent(func(ev browser.CDPEvent) {
		if ev.SessionID == p.sessionID && ev.Method == "Page.domContentEventFired" {
			select {
			case loaded <- struct{}{}:
			default:
			}
		}
	})
	defer stop()
	raw, err := p.b.conn.CallTimeout("Page.navigate", map[string]any{"url": target}, p.sessionID, timeout)
	if err != nil {
		return err
	}
	var nav struct {
		ErrorText string `json:"errorText"`
		LoaderID  string `json:"loaderId"`
	}
	if err := json.Unmarshal(raw, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("%s at %s", nav.ErrorText, target)
	}
	if nav.LoaderID == "" {
		return nil
	}
	select {
	case <-loaded:
		return nil
	case <-p.b.conn.Done():
		return errors.New("Target page, context or browser has been closed")
	case <-time.After(timeout):
		return fmt.Errorf("Timeout %dms exceeded.", timeout.Milliseconds())
	}
}

// Evaluates an expression or a function source in the page, awaiting promises and returning the value
func (p *cdpPage) Evaluate(js string) (any, error) {
	expr := strings.TrimSpace(js)
	if functionLikeRe.MatchString(expr) {
		expr = "(" + expr + ")()"
	}
	raw, err := p.b.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true, "userGesture": true}, p.sessionID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.ExceptionDetails != nil {
		msg := out.ExceptionDetails.Exception.Description
		if msg == "" {
			msg = out.ExceptionDetails.Text
		}
		return nil, errors.New(msg)
	}
	if len(out.Result.Value) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(out.Result.Value, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// CSS locator over querySelectorAll
func (p *cdpPage) Locator(selector string) meetLocator {
	return &cdpLocator{p: p, spec: map[string]any{"kind": "css", "css": selector}}
}

// ARIA role locator matching the accessible name by substring ignoring case unless exact
func (p *cdpPage) GetByRole(role, name string, exact bool) meetLocator {
	return &cdpLocator{p: p, spec: map[string]any{"kind": "role", "role": role, "name": name, "exact": exact}}
}

// Locator resolved in the page by the injected resolver on every action
type cdpLocator struct {
	p    *cdpPage
	spec map[string]any
}

// Element resolver and actions run in the page, role matching follows the implicit ARIA roles and names
const locatorJS = `(spec, op, arg) => {
  const norm = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const styleHidden = (el) => { const st = getComputedStyle(el); return st.visibility === 'hidden' || st.visibility === 'collapse'; };
  const boxed = (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
  const visible = (el) => boxed(el) && !styleHidden(el);
  const ariaHidden = (el) => { for (let e = el; e; e = e.parentElement) { if (e.getAttribute && e.getAttribute('aria-hidden') === 'true') return true; const st = getComputedStyle(e); if (st.display === 'none') return true; } return styleHidden(el); };
  const implicitRole = (el) => {
    const tag = el.tagName.toLowerCase();
    if (tag === 'button') return 'button';
    if (tag === 'input') { const t = (el.getAttribute('type') || 'text').toLowerCase(); if (['button', 'submit', 'reset', 'image'].includes(t)) return 'button'; if (t === 'checkbox') return 'checkbox'; if (t === 'radio') return 'radio'; return 'textbox'; }
    if (tag === 'a' && el.hasAttribute('href')) return 'link';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'summary') return 'button';
    return '';
  };
  const roleOf = (el) => { const r = (el.getAttribute('role') || '').trim().split(/\s+/)[0]; return r || implicitRole(el); };
  const nameOf = (el) => {
    const by = el.getAttribute('aria-labelledby');
    if (by) { const t = by.split(/\s+/).map((id) => { const r = document.getElementById(id); return r ? r.textContent : ''; }).join(' '); if (norm(t)) return norm(t); }
    const label = el.getAttribute('aria-label');
    if (norm(label)) return norm(label);
    if (el.tagName === 'INPUT') { const t = (el.getAttribute('type') || '').toLowerCase(); if (['button', 'submit', 'reset'].includes(t)) return norm(el.value || (t === 'submit' ? 'Submit' : t === 'reset' ? 'Reset' : '')); if (t === 'image') return norm(el.getAttribute('alt') || ''); }
    const text = norm(el.innerText !== undefined ? el.innerText : el.textContent);
    if (text) return text;
    return norm(el.getAttribute('title') || '');
  };
  let els;
  if (spec.kind === 'css') els = Array.from(document.querySelectorAll(spec.css));
  else {
    const want = norm(spec.name);
    els = Array.from(document.querySelectorAll('*')).filter((el) => roleOf(el) === spec.role && !ariaHidden(el) && (() => {
      if (!spec.name) return true;
      const n = nameOf(el);
      return spec.exact ? n === want : n.toLowerCase().includes(want.toLowerCase());
    })());
  }
  const el = els[0];
  if (op === 'count') return els.length;
  if (!el) return op === 'visible' ? false : {ok: false, reason: 'no element'};
  if (op === 'visible') return visible(el);
  if (op === 'point') {
    if (!el.isConnected) return {ok: false, reason: 'detached'};
    el.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
    if (!visible(el)) return {ok: false, reason: 'not visible'};
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') return {ok: false, reason: 'not enabled'};
    const r = el.getBoundingClientRect();
    const x = r.left + r.width / 2, y = r.top + r.height / 2;
    const hit = document.elementFromPoint(x, y);
    if (!hit || !(hit === el || el.contains(hit))) return {ok: false, reason: 'intercepted'};
    return {ok: true, x, y};
  }
  if (op === 'fill') {
    if (!visible(el)) return {ok: false, reason: 'not visible'};
    const tag = el.tagName;
    const editable = el.isContentEditable || ((tag === 'INPUT' || tag === 'TEXTAREA') && !el.disabled && !el.readOnly);
    if (!editable) return {ok: false, reason: 'not editable'};
    el.focus();
    if (tag === 'INPUT' || tag === 'TEXTAREA') el.select();
    else { const range = document.createRange(); range.selectNodeContents(el); const sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(range); }
    return {ok: true};
  }
  return {ok: false, reason: 'unknown op ' + op};
}`

// Runs one resolver operation for this locator
func (l *cdpLocator) run(op string) (any, error) {
	spec, _ := json.Marshal(l.spec)
	return l.p.Evaluate("(" + locatorJS + ")(" + string(spec) + ", " + fmt.Sprintf("%q", op) + ")")
}

// Locator itself, every action already targets the first match
func (l *cdpLocator) First() meetLocator { return l }

// Number of matching elements
func (l *cdpLocator) Count() (int, error) {
	v, err := l.run("count")
	if err != nil {
		return 0, err
	}
	n, _ := v.(float64)
	return int(n), nil
}

// Whether the first match has a box and is not visibility hidden
func (l *cdpLocator) IsVisible() (bool, error) {
	v, err := l.run("visible")
	if err != nil {
		return false, err
	}
	b, _ := v.(bool)
	return b, nil
}

// Retries an actionability check until it passes or timeout elapses
func (l *cdpLocator) until(op string, timeout time.Duration, act func(map[string]any) error) error {
	deadline := time.Now().Add(timeout)
	reason := "no element"
	for {
		v, err := l.run(op)
		if err != nil {
			return err
		}
		res, _ := v.(map[string]any)
		if ok, _ := res["ok"].(bool); ok {
			return act(res)
		}
		if r, _ := res["reason"].(string); r != "" {
			reason = r
		}
		if l.p.IsClosed() {
			return errors.New("Target page, context or browser has been closed")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Timeout %dms exceeded waiting for element (%s)", timeout.Milliseconds(), reason)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Clicks the centre of the first match with mouse move, press and release once it is actionable
func (l *cdpLocator) Click(timeout time.Duration) error {
	return l.until("point", timeout, func(res map[string]any) error {
		x, _ := res["x"].(float64)
		y, _ := res["y"].(float64)
		for _, ev := range []map[string]any{
			{"type": "mouseMoved", "x": x, "y": y, "button": "none", "buttons": 0},
			{"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1},
			{"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": 1},
		} {
			if _, err := l.p.b.call("Input.dispatchMouseEvent", ev, l.p.sessionID); err != nil {
				return err
			}
		}
		return nil
	})
}

// Focuses and selects the first editable match, then inserts value or presses Delete for an empty value
func (l *cdpLocator) Fill(value string, timeout time.Duration) error {
	return l.until("fill", timeout, func(map[string]any) error {
		if value != "" {
			_, err := l.p.b.call("Input.insertText", map[string]any{"text": value}, l.p.sessionID)
			return err
		}
		for _, typ := range []string{"keyDown", "keyUp"} {
			if _, err := l.p.b.call("Input.dispatchKeyEvent", map[string]any{"type": typ, "key": "Delete", "code": "Delete", "windowsVirtualKeyCode": 46}, l.p.sessionID); err != nil {
				return err
			}
		}
		return nil
	})
}
