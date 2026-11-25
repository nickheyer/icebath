package camofox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

const (
	defaultTimeout = 30
	noSessionError = "No browser session. Call browser_navigate first."
	dockerHint     = "docker run -p 9377:9377 -e CAMOFOX_PORT=9377 jo-inc/camofox-browser"
	// Timeout argument that resolves to browser.command_timeout
	commandTimeoutDefault = 0
)

// Largest timeout in whole seconds a duration holds
var maxTimeoutSeconds = int64(math.MaxInt64 / int64(time.Second))

// Transports Camofox calls ride, environment proxies honoured on one and never used on the other
var (
	proxiedTransport = netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
	directTransport  = func() *http.Transport {
		t := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
		t.Proxy = nil
		return t
	}()
)

// Active profile of one call, its context, tool env and home
type scope struct {
	ctx  context.Context
	env  *tools.Env
	home string
}

// Scope of a call, the session env when it names a home else the context home
func newScope(ctx context.Context, env *tools.Env) *scope {
	if ctx == nil {
		ctx = context.Background()
	}
	if env == nil {
		env = tools.ToolEnvFrom(ctx)
	}
	if env == nil || env.Home == "" {
		env = tools.EnvForProfileHome(ctx, env, home.Dir(ctx))
	}
	return &scope{ctx: ctx, env: env, home: env.Home}
}

// CAMOFOX value from the profile secret scope stripped, empty when unset, the unscoped read raised
func (s *scope) secret(name string) string {
	v, err := tools.SecretThroughScope(s.ctx, s.env, name)
	if err != nil {
		panic(err)
	}
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// Configured Camofox server URL with trailing slashes dropped, empty when unset
func (s *scope) camofoxURL() string { return strings.TrimRight(s.secret("CAMOFOX_URL"), "/") }

// Authorization Bearer header when CAMOFOX_API_KEY is set
func (s *scope) authHeaders() http.Header {
	h := http.Header{}
	if key := s.secret("CAMOFOX_API_KEY"); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return h
}

// Raw config.yaml of the profile with no defaults merged
func (s *scope) rawConfig() (map[string]any, error) {
	return config.ReadRawConfigReadonly(s.ctx, s.home)
}

// Value at a nested key path of a config mapping, def on any miss and an explicit null kept
func cfgGet(cfg map[string]any, def any, keys ...string) any {
	if v, ok := config.CfgGet(cfg, keys...); ok {
		return v
	}
	return def
}

// The browser.camofox block of the profile, empty when unreadable or not a mapping
func (s *scope) camofoxConfig() map[string]any {
	raw, err := s.rawConfig()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("camofox config read failed, using defaults: %v", err))
		return map[string]any{}
	}
	if block := config.AsMap(cfgGet(raw, map[string]any{}, "browser", "camofox")); block != nil {
		return block
	}
	return map[string]any{}
}

// browser.command_timeout of the profile floored at 5 seconds and capped at what a duration holds, 30 when unset or unreadable
func (s *scope) commandTimeout() int64 {
	raw, err := s.rawConfig()
	if err == nil {
		val := cfgGet(raw, nil, "browser", "command_timeout")
		if val == nil {
			return defaultTimeout
		}
		// A number past the int64 range converts to its bound, which the cap and the floor bring into range
		var n int64
		if n, err = anyutil.ToInt(val); err == nil || errors.Is(err, anyutil.ErrRange) {
			return max(min(n, maxTimeoutSeconds), 5)
		}
	}
	pkgLog().Debug(fmt.Sprintf("Could not read browser.command_timeout: %v", err))
	return defaultTimeout
}

// browser.camofox.managed_persistence, a stable profile scoped userId instead of a random one
func managedPersistenceEnabled(cfg map[string]any) bool {
	return anyutil.IsTrue(cfg["managed_persistence"])
}

// Env value first, then the browser.camofox key as stripped text, else empty
func (s *scope) envOrCfg(envName string, cfg map[string]any, key string) string {
	if v := s.secret(envName); v != "" {
		return v
	}
	return strings.TrimSpace(anyutil.String(anyutil.Or(cfg[key], "")))
}

// Boolean toggle where a valid env spelling wins over the config key
func (s *scope) flag(envName string, cfg map[string]any, key string) bool {
	raw := strings.ToLower(s.secret(envName))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if raw != "" {
		pkgLog().Debug(fmt.Sprintf("Ignoring invalid boolean env %s=%s", envName, strconv.Quote(raw)))
	}
	return anyutil.IsTrue(cfg[key])
}

// Profile scoped root directory of Camofox persistence
func (s *scope) stateDir() string { return filepath.Join(s.home, "browser_auth", "camofox") }

// Camofox userId and session key of one session
type identity struct {
	userID, sessionKey string
}

// Hex digits of a uuid5 in the URL namespace
func uuid5Hex(name string) string {
	return strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String(), "-", "")
}

// Profile scoped userId and task scoped session key, stable for one profile home
func (s *scope) managedIdentity(taskID string) identity {
	root := s.stateDir()
	user := uuid5Hex("camofox-user:" + root)[:10]
	sess := uuid5Hex("camofox-session:" + root + ":" + taskOrDefault(taskID))[:16]
	return identity{userID: "icebath_" + user, sessionKey: "task_" + sess}
}

// Externally configured identity from CAMOFOX_USER_ID or browser.camofox.user_id, nil when none
func (s *scope) identityOverride(taskID string, cfg map[string]any) *identity {
	userID := s.envOrCfg("CAMOFOX_USER_ID", cfg, "user_id")
	if userID == "" {
		return nil
	}
	sessionKey := s.envOrCfg("CAMOFOX_SESSION_KEY", cfg, "session_key")
	if sessionKey == "" {
		sessionKey = "task_" + textutil.First(taskOrDefault(taskID), 16)
	}
	return &identity{userID: userID, sessionKey: sessionKey}
}

// Whether a hostname is localhost or a loopback address
func isLoopbackHostname(hostname string) bool {
	if hostname == "" {
		return false
	}
	host := strings.ToLower(strings.Trim(strings.TrimSpace(hostname), "[]"))
	if host == "localhost" || host == "localhost.localdomain" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Loopback page URL rewritten to the Docker host alias when enabled, with the metadata naming the rewrite
func (s *scope) rewriteLoopbackURL(raw string) (string, *jsonutil.Object, error) {
	cfg := s.camofoxConfig()
	if !s.flag("CAMOFOX_REWRITE_LOOPBACK_URLS", cfg, "rewrite_loopback_urls") {
		return raw, nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw, nil, nil
	}
	alias := s.envOrCfg("CAMOFOX_LOOPBACK_HOST_ALIAS", cfg, "loopback_host_alias")
	if alias == "" {
		alias = "host.docker.internal"
	}
	hostname := strings.ToLower(parsed.Hostname())
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || !isLoopbackHostname(hostname) {
		return raw, nil, nil
	}
	port, hasPort, err := urlutil.Port(parsed)
	if err != nil {
		return "", nil, &kindError{"ValueError", err.Error()}
	}
	host := urlutil.FormatHost(alias)
	if hasPort && port != 0 {
		host += ":" + strconv.Itoa(port)
	}
	next := *parsed
	next.Host = host
	if next.User.Username() == "" {
		next.User = nil
	}
	rewritten := next.String()
	return rewritten, jsonutil.ObjectOf("from", hostname, "to", alias, "original_url", raw, "rewritten_url", rewritten), nil
}

// VNC address recorded per Camofox server URL, nil recording that the server advertised none
var (
	vncMu    sync.Mutex
	vncByURL = map[string]*string{}
)

// VNC address a health reply advertises through vncPort, nil when it carries no valid port
func vncURLFromHealth(serverURL string, resp *httputil.Response) *string {
	body, err := decodeJSON(resp.Content)
	if err != nil {
		return nil
	}
	port, err := get(body, "vncPort", nil)
	if err != nil {
		return nil
	}
	n, ok := anyutil.Int(port)
	if !ok || n < 1 || n > 65535 {
		return nil
	}
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil
	}
	host := urlutil.FormatHost(strings.ToLower(parsed.Hostname()))
	if host == "" {
		host = "localhost"
	}
	out := "http://" + host + ":" + strconv.FormatInt(n, 10)
	return &out
}

// Whether the configured server answers /health with 200, recording its VNC address once
func (s *scope) checkAvailable() bool {
	serverURL := s.camofoxURL()
	if serverURL == "" {
		return false
	}
	resp, err := s.send("GET", serverURL+"/health", http.Header{}, nil, nil, 5, false)
	if err != nil {
		return false
	}
	if resp.StatusCode == 200 {
		vncMu.Lock()
		if _, known := vncByURL[serverURL]; !known {
			vncByURL[serverURL] = vncURLFromHealth(serverURL, resp)
		}
		vncMu.Unlock()
	}
	return resp.StatusCode == 200
}

// VNC address of the configured server, empty when it exposes none
func (s *scope) vncURL() string {
	serverURL := s.camofoxURL()
	vncMu.Lock()
	_, known := vncByURL[serverURL]
	vncMu.Unlock()
	if !known {
		s.checkAvailable()
	}
	vncMu.Lock()
	defer vncMu.Unlock()
	if v := vncByURL[serverURL]; v != nil {
		return *v
	}
	return ""
}

// Camofox reply with a 4xx or 5xx status, its text carrying the server's own explanation
type httpError struct {
	status int
	text   string
	resp   *httputil.Response
}

func (e *httpError) Error() string { return e.text }

// Kind label the error records under
func (e *httpError) ErrorKind() string { return "HTTPError" }

// Whether an error is a Camofox status failure with one of the statuses
func httpStatusIn(err error, statuses ...int) bool {
	he, ok := err.(*httpError)
	if !ok {
		return false
	}
	for _, st := range statuses {
		if he.status == st {
			return true
		}
	}
	return false
}

// Whether a request failed to reach the server: no connection, a connect timeout, a proxy or TLS failure, a read timeout and a redirect loop not counted
func isConnectionError(err error) bool {
	var ue *url.Error
	var te *httputil.TimeoutError
	return errors.As(err, &ue) && !(errors.As(err, &te) && te.Phase == httputil.PhaseRead) &&
		!errors.Is(err, httputil.ErrTooManyRedirects)
}

// Request URL of an http or https address with a host, the reason it cannot be sent otherwise
func prepareURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Camofox URL: %w", err)
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("invalid Camofox URL %q: no scheme, perhaps https://%s", raw, raw)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("invalid Camofox URL %q: scheme %q is neither http nor https", raw, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid Camofox URL %q: no host", raw)
	}
	return u, nil
}

// One HTTP call with headers, params and a JSON body, connect and read each held to the timeout, direct skipping proxies and redirects
func (s *scope) send(method, raw string, headers http.Header, params [][2]string, body any, timeout int64, direct bool) (*httputil.Response, error) {
	u, err := prepareURL(raw)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		if u.RawQuery != "" {
			u.RawQuery += "&" + urlutil.EncodeQuery(params)
		} else {
			u.RawQuery = urlutil.EncodeQuery(params)
		}
	}
	var reader io.Reader
	if body != nil {
		payload, err := jsonutil.Append(nil, body, jsonutil.ASCII())
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(s.ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		req.Header[k] = vs
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	limit := time.Duration(timeout) * time.Second
	t := httputil.Timeout{Connect: limit, Read: limit}
	client := &http.Client{Transport: httputil.TrustEnvCA(proxiedTransport), CheckRedirect: httputil.RedirectPolicy(true)}
	if direct {
		client = &http.Client{Transport: httputil.TrustEnvCA(directTransport), CheckRedirect: httputil.RedirectPolicy(false)}
	}
	return httputil.Do(client, req, t)
}

// The server's error, code and recovery fields of an error reply joined, empty when the body has none
func errorDetail(resp *httputil.Response) string {
	body, err := decodeJSON(resp.Content)
	if err != nil || !isDict(body) {
		return ""
	}
	var parts []string
	for _, k := range []string{"error", "code", "recovery"} {
		if v, _ := get(body, k, nil); anyutil.IsTrue(v) {
			parts = append(parts, k+": "+anyutil.String(v))
		}
	}
	return strings.Join(parts, "; ")
}

// Authenticated request to the Camofox server, a 4xx or 5xx returned as an *httpError with the server's explanation
func (s *scope) request(method, path string, timeout int64, params [][2]string, body any, direct bool) (*httputil.Response, error) {
	target := s.camofoxURL() + path
	headers := s.authHeaders()
	if timeout == commandTimeoutDefault {
		timeout = s.commandTimeout()
	}
	resp, err := s.send(method, target, headers, params, body, timeout, direct)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		text := resp.CheckStatus().Error()
		if detail := errorDetail(resp); detail != "" {
			text += " (" + detail + ")"
		}
		return nil, &httpError{status: resp.StatusCode, text: text, resp: resp}
	}
	return resp, nil
}

// POST of a JSON body, the decoded reply
func (s *scope) post(path string, body any, timeout int64) (any, error) {
	resp, err := s.request("POST", path, timeout, nil, body, false)
	if err != nil {
		return nil, err
	}
	return decodeJSON(resp.Content)
}

// GET with params, the decoded reply
func (s *scope) get(path string, params [][2]string, timeout int64) (any, error) {
	resp, err := s.getRaw(path, params, timeout)
	if err != nil {
		return nil, err
	}
	return decodeJSON(resp.Content)
}

// GET returning the raw response for binary payloads such as screenshots
func (s *scope) getRaw(path string, params [][2]string, timeout int64) (*httputil.Response, error) {
	return s.request("GET", path, timeout, params, nil, false)
}

// DELETE with an optional JSON body, the decoded reply
func (s *scope) delete(path string, body any, timeout int64) (any, error) {
	resp, err := s.request("DELETE", path, timeout, nil, body, false)
	if err != nil {
		return nil, err
	}
	return decodeJSON(resp.Content)
}

// Text naming the unreachable server and how to start it
func (s *scope) connectionErrorMessage() string {
	return fmt.Sprintf("Cannot connect to Camofox at %s. Is the server running? "+
		"Start with: npm start (in camofox-browser dir) or: %s", s.camofoxURL(), dockerHint)
}

// One task's Camofox session, the tab id guarded for concurrent tools
type session struct {
	userID, sessionKey string
	adopt              bool
	mu                 sync.Mutex
	tabID              any
}

// Tracked tab id, nil when none
func (ss *session) tab() any {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.tabID
}

// Replaces the tracked tab id
func (ss *session) setTab(v any) {
	ss.mu.Lock()
	ss.tabID = v
	ss.mu.Unlock()
}

// Whether a tab id is tracked, a missing, empty or zero id counting as none
func (ss *session) hasTab() bool { return anyutil.IsTrue(ss.tab()) }

// Session map slot of one profile home and task
type sessionSlot struct {
	home, task string
}

var (
	sessionsMu sync.Mutex
	sessions   = map[sessionSlot]*session{}
)

// Slot of a task in the active profile, so one process serving several profiles never shares a tab
func (s *scope) slot(taskID string) sessionSlot {
	return sessionSlot{home: home.Key(s.home), task: taskOrDefault(taskID)}
}

// Path of a tab action under the session's tab
func tabPath(ss *session, suffix string) string {
	return "/tabs/" + anyutil.String(ss.tab()) + "/" + suffix
}

// userId query params of a session
func userParams(ss *session) [][2]string { return [][2]string{{"userId", ss.userID}} }

// Tabs list of a GET /tabs reply for the session's user
func (s *scope) listTabs(ss *session, timeout int64) (any, error) {
	data, err := s.get("/tabs", userParams(ss), timeout)
	if err != nil {
		return nil, err
	}
	return get(data, "tabs", []any{})
}

// Rehydrates the tab id from an open tab carrying this session's key, adopting no other session's tab
func (s *scope) adoptExistingTab(ss *session) *session {
	if ss.hasTab() || !ss.adopt || s.camofoxURL() == "" {
		return ss
	}
	tabs, err := func() (tabs any, err error) {
		err = try(func() error {
			tabs, err = s.listTabs(ss, 5)
			return err
		})
		return tabs, err
	}()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Camofox tab adoption failed for %s: %v", ss.userID, err))
		return ss
	}
	list, _ := tabs.([]any)
	var candidates []*jsonutil.Object
	for _, tab := range list {
		if o, ok := tab.(*jsonutil.Object); ok && o != nil {
			if key, _ := get(o, "listItemId", nil); anyutil.Equal(key, ss.sessionKey) {
				candidates = append(candidates, o)
			}
		}
	}
	if len(candidates) == 0 {
		return ss
	}
	tabID, _ := get(candidates[len(candidates)-1], "tabId", nil)
	if id, ok := tabID.(string); ok && id != "" {
		ss.setTab(id)
		pkgLog().Debug(fmt.Sprintf("Adopted existing Camofox tab %s for %s", id, ss.userID))
	}
	return ss
}

// Session of a task, created with the external, managed or random ephemeral identity in that order
func (s *scope) getSession(taskID string) *session {
	key := s.slot(taskID)
	sessionsMu.Lock()
	existing := sessions[key]
	sessionsMu.Unlock()
	if existing != nil {
		return s.adoptExistingTab(existing)
	}
	created, fresh := func() (*session, bool) {
		sessionsMu.Lock()
		defer sessionsMu.Unlock()
		if found := sessions[key]; found != nil {
			return found, false
		}
		cfg := s.camofoxConfig()
		id := s.identityOverride(key.task, cfg)
		if id == nil && managedPersistenceEnabled(cfg) {
			managed := s.managedIdentity(key.task)
			id = &managed
		}
		ss := &session{}
		if id == nil {
			ss.userID = "icebath_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
			ss.sessionKey = "task_" + textutil.First(key.task, 16)
		} else {
			ss.userID, ss.sessionKey = id.userID, id.sessionKey
			ss.adopt = s.flag("CAMOFOX_ADOPT_EXISTING_TAB", cfg, "adopt_existing_tab")
		}
		sessions[key] = ss
		return ss, true
	}()
	if !fresh {
		return created
	}
	return s.adoptExistingTab(created)
}

// After a 404 or 410 for the cached tab, whether a live tab is tracked again
func (s *scope) resyncTab(ss *session) bool {
	tabs, err := func() (tabs any, err error) {
		err = try(func() error {
			tabs, err = s.listTabs(ss, 5)
			return err
		})
		return tabs, err
	}()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Camofox tab resync failed for %s: %v", ss.userID, err))
		return false
	}
	list, _ := tabs.([]any)
	var live []*jsonutil.Object
	for _, tab := range list {
		if o, ok := tab.(*jsonutil.Object); ok && o != nil {
			if id, _ := get(o, "tabId", nil); anyutil.IsTrue(id) {
				live = append(live, o)
			}
		}
	}
	tracked := ss.tab()
	for _, tab := range live {
		if id, _ := get(tab, "tabId", nil); anyutil.Equal(id, tracked) {
			return true
		}
	}
	var mine []*jsonutil.Object
	for _, tab := range live {
		if key, _ := get(tab, "listItemId", nil); anyutil.Equal(key, ss.sessionKey) {
			mine = append(mine, tab)
		}
	}
	if len(mine) == 0 {
		ss.setTab(nil)
		return false
	}
	id, _ := get(mine[len(mine)-1], "tabId", nil)
	ss.setTab(id)
	return true
}

// Session of a task with a tab, one created at the URL when none is tracked
func (s *scope) ensureTab(taskID, at string) (*session, error) {
	ss := s.getSession(taskID)
	if !ss.hasTab() {
		data, err := s.post("/tabs", jsonutil.ObjectOf("userId", ss.userID, "listItemId", ss.sessionKey, "url", at), commandTimeoutDefault)
		if err != nil {
			return nil, err
		}
		id, err := get(data, "tabId", nil)
		if err != nil {
			return nil, err
		}
		ss.setTab(id)
	}
	return ss, nil
}

// Removes and returns the task's session of the active profile
func (s *scope) dropSession(taskID string) *session {
	key := s.slot(taskID)
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	ss := sessions[key]
	delete(sessions, key)
	return ss
}

// Session tracked for the task without creating one
func (s *scope) trackedSession(taskID string) *session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[s.slot(taskID)]
}

// Drops only the local entry of a managed identity, false for an ephemeral session the caller closes
func (s *scope) softCleanup(taskID string) bool {
	cfg := s.camofoxConfig()
	if managedPersistenceEnabled(cfg) || s.identityOverride(taskID, cfg) != nil {
		s.dropSession(taskID)
		pkgLog().Debug(fmt.Sprintf("Camofox soft cleanup for task %s (managed persistence)", logTask(taskID)))
		return true
	}
	return false
}

// Drops the task's session and deletes it on the server, the failure text returned
func (s *scope) closeSession(taskID string) string {
	ss := s.dropSession(taskID)
	if ss == nil {
		return ""
	}
	if err := try(func() error {
		_, err := s.delete("/sessions/"+ss.userID, nil, commandTimeoutDefault)
		return err
	}); err != nil {
		return err.Error()
	}
	return ""
}
