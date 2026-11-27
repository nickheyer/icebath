package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cast"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	camofoxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/httpx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	defaultTimeout = 30
	noSession      = "No browser session. Call browser_navigate first."
	dockerHint     = "docker run -p 9377:9377 -e CAMOFOX_PORT=9377 jo-inc/camofox-browser"
	// Timeout that stands for browser.command_timeout
	commandTimeout = 0
)

// One call's view of a profile's Camofox server
type scope struct {
	r   *registrant
	ctx context.Context
	cfg *config.Config
	env *tools.Env
}

// Setting of the profile, trimmed
func (s *scope) setting(key string) string { return strings.TrimSpace(s.cfg.GetString(key)) }

// Server address with its trailing slashes dropped, empty when unset
func (s *scope) url() string { return strings.TrimRight(s.setting(camofoxconfig.URL), "/") }

func (s *scope) authHeaders() http.Header {
	h := http.Header{}
	if key := s.setting(camofoxconfig.APIKey); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return h
}

// browser.command_timeout, never under five seconds
func (s *scope) commandTimeout() int64 {
	stored := s.cfg.Get(browserconfig.CommandTimeout)
	if stored == nil {
		return defaultTimeout
	}
	n, err := cast.ToInt64E(stored)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read browser.command_timeout: %v", err))
		return defaultTimeout
	}
	return max(min(n, int64(time.Duration(1<<62)/time.Second)), 5)
}

// A toggle whose variable, spelled as a boolean, wins over the setting
func (s *scope) flag(variable, key string) bool {
	raw := strings.ToLower(strings.TrimSpace(s.cfg.Getenv(variable)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if raw != "" {
		slog.Debug(fmt.Sprintf("Ignoring invalid boolean env %s=%s", variable, strconv.Quote(raw)))
	}
	return cast.ToBool(s.cfg.Stored(key))
}

// Root of the profile's Camofox state, which names its stable identity
func (s *scope) stateDir() string {
	return filepath.Join(s.cfg.ProfileDir(s.cfg.Profile()), "browser_auth", "camofox")
}

type identity struct{ userID, sessionKey string }

func uuid5Hex(name string) string {
	return strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String(), "-", "")
}

func taskOr(task string) string {
	if task == "" {
		return "default"
	}
	return task
}

// Stable user of the profile and session key of the task
func (s *scope) managedIdentity(task string) identity {
	root := s.stateDir()
	return identity{userID: "icebath_" + uuid5Hex("camofox-user:" + root)[:10], sessionKey: "task_" + uuid5Hex("camofox-session:" + root + ":" + taskOr(task))[:16]}
}

// User and session key set outside, nil when none is
func (s *scope) identityOverride(task string) *identity {
	userID := s.setting(camofoxconfig.UserID)
	if userID == "" {
		return nil
	}
	key := s.setting(camofoxconfig.SessionKey)
	if key == "" {
		key = "task_" + strs.Head(taskOr(task), 16)
	}
	return &identity{userID: userID, sessionKey: key}
}

func (s *scope) managed() bool { return s.cfg.GetBool(camofoxconfig.ManagedPersistence) }

// Whether the host is this machine's loopback, the name some systems give it included
func loopbackHost(hostname string) bool {
	host := strings.ToLower(strings.TrimSpace(hostname))
	return host != "" && (host == "localhost.localdomain" || httpx.Loopback(host))
}

// A loopback page address sent to the Docker host alias when that is on, with what the rewrite did
func (s *scope) rewriteLoopback(raw string) (string, map[string]any, error) {
	if !s.flag(camofoxconfig.RewriteLoopbackEnv, camofoxconfig.RewriteLoopback) {
		return raw, nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw, nil, nil
	}
	alias := s.setting(camofoxconfig.LoopbackHostAlias)
	if alias == "" {
		alias = "host.docker.internal"
	}
	hostname := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "http" && parsed.Scheme != "https" || !loopbackHost(hostname) {
		return raw, nil, nil
	}
	host := alias
	if strings.Contains(alias, ":") && !strings.HasPrefix(alias, "[") {
		host = "[" + alias + "]"
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return "", nil, fmt.Errorf("Port could not be cast to integer value as %s", strconv.Quote(port))
		}
		if n != 0 {
			host += ":" + strconv.Itoa(n)
		}
	}
	next := *parsed
	next.Host = host
	if next.User.Username() == "" {
		next.User = nil
	}
	rewritten := next.String()
	return rewritten, map[string]any{"from": hostname, "to": alias, "original_url": raw, "rewritten_url": rewritten}, nil
}

// A reply of the server
type reply struct {
	status int
	body   []byte
	url    string
}

// A reply with a 4xx or 5xx status, its text carrying the server's own explanation
type httpError struct {
	status int
	text   string
	body   []byte
}

func (e *httpError) Error() string { return e.text }

func statusIn(err error, statuses ...int) bool {
	var he *httpError
	if !errors.As(err, &he) {
		return false
	}
	for _, st := range statuses {
		if he.status == st {
			return true
		}
	}
	return false
}

// A request that never reached the server: no connection, a refused or timed out connect, a proxy or TLS failure
type connectionError struct{ err error }

func (e *connectionError) Error() string { return e.err.Error() }
func (e *connectionError) Unwrap() error { return e.err }

// Request address of an http or https address with a host, else why it can't be sent
func prepare(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("The Camofox address “%s” cannot be read. Give one such as http://localhost:9377.", raw)
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("The Camofox address “%s” does not start with http:// or https://. Perhaps you meant https://%s.", raw, raw)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("The Camofox address “%s” starts with %s://, but it must start with http:// or https://.", raw, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("The Camofox address “%s” names no computer. Give one such as http://localhost:9377.", raw)
	}
	return u, nil
}

// Body read with every pause between bytes held to the limit
func readIdle(body io.ReadCloser, limit time.Duration) ([]byte, error) {
	timer := time.AfterFunc(limit, func() { body.Close() })
	defer timer.Stop()
	var out bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		out.Write(buf[:n])
		if n > 0 {
			timer.Reset(limit)
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			if !timer.Stop() {
				return nil, fmt.Errorf("read timed out after %ds", int(limit.Seconds()))
			}
			return nil, err
		}
	}
}

// One HTTP call, the connect and each read held to the timeout; direct skips proxies and redirects
func (s *scope) send(method, raw string, headers http.Header, params url.Values, body any, timeout int64, direct bool) (*reply, error) {
	u, err := prepare(raw)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		if u.RawQuery != "" {
			u.RawQuery += "&" + params.Encode()
		} else {
			u.RawQuery = params.Encode()
		}
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: limit, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout = limit, limit
	client := &http.Client{Transport: transport}
	if direct {
		transport.Proxy = nil
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "awaiting response headers") || errors.Is(err, context.Canceled) {
			return nil, err
		}
		var ue *url.Error
		if errors.As(err, &ue) && !strings.Contains(err.Error(), "stopped after 10 redirects") {
			return nil, &connectionError{err}
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, err := readIdle(resp.Body, limit)
	if err != nil {
		return nil, err
	}
	return &reply{status: resp.StatusCode, body: data, url: resp.Request.URL.String()}, nil
}

// Decoded body, invalid UTF-8 replaced first
func decode(body []byte) (any, error) {
	var v any
	err := json.Unmarshal([]byte(strings.ToValidUTF8(string(body), "�")), &v)
	return v, err
}

// The server's error, code and recovery of an error reply, empty when it has none
func errorDetail(body []byte) string {
	v, err := decode(body)
	obj, ok := v.(map[string]any)
	if err != nil || !ok {
		return ""
	}
	var parts []string
	for _, k := range []string{"error", "code", "recovery"} {
		if x := obj[k]; config.Truthy(x) {
			parts = append(parts, k+": "+text(x))
		}
	}
	return strings.Join(parts, "; ")
}

// Text of a failed status as HTTP clients have always phrased it
func statusText(r *reply) string {
	kind := "Client"
	if r.status >= 500 {
		kind = "Server"
	}
	return fmt.Sprintf("%d %s Error: %s for url: %s", r.status, kind, http.StatusText(r.status), r.url)
}

// Authenticated request to the server, a 4xx or 5xx an *httpError with the server's explanation
func (s *scope) request(method, path string, timeout int64, params url.Values, body any, direct bool) (*reply, error) {
	if timeout == commandTimeout {
		timeout = s.commandTimeout()
	}
	r, err := s.send(method, s.url()+path, s.authHeaders(), params, body, timeout, direct)
	if err != nil {
		return nil, err
	}
	if r.status >= 400 {
		msg := statusText(r)
		if detail := errorDetail(r.body); detail != "" {
			msg += " (" + detail + ")"
		}
		return nil, &httpError{status: r.status, text: msg, body: r.body}
	}
	return r, nil
}

func (s *scope) post(path string, body any, timeout int64) (any, error) {
	r, err := s.request("POST", path, timeout, nil, body, false)
	if err != nil {
		return nil, err
	}
	return decode(r.body)
}

func (s *scope) get(path string, params url.Values, timeout int64) (any, error) {
	r, err := s.request("GET", path, timeout, params, nil, false)
	if err != nil {
		return nil, err
	}
	return decode(r.body)
}

func (s *scope) delete(path string, timeout int64) (any, error) {
	r, err := s.request("DELETE", path, timeout, nil, nil, false)
	if err != nil {
		return nil, err
	}
	return decode(r.body)
}

func (s *scope) connectionMessage() string {
	return fmt.Sprintf("Cannot connect to Camofox at %s. Is the server running? Start with: npm start (in camofox-browser dir) or: %s", s.url(), dockerHint)
}

// Field of a decoded object, def when missing; an error for a value that is no object
func field(v any, key string, def any) (any, error) {
	if o, ok := v.(map[string]any); ok {
		if val, has := o[key]; has {
			return val, nil
		}
		return def, nil
	}
	return nil, fmt.Errorf("expected an object, got %T", v)
}

// Address a health reply advertises a VNC viewer at, nil for none
func vncFromHealth(server string, body []byte) *string {
	v, err := decode(body)
	if err != nil {
		return nil
	}
	port, err := field(v, "vncPort", nil)
	if err != nil {
		return nil
	}
	n, err := cast.ToInt64E(port)
	if err != nil || n < 1 || n > 65535 {
		return nil
	}
	parsed, err := url.Parse(server)
	if err != nil {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "":
		host = "localhost"
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	out := "http://" + host + ":" + strconv.FormatInt(n, 10)
	return &out
}

// Whether the server answers /health, its VNC address noted once
func (s *scope) available() bool {
	server := s.url()
	if server == "" {
		return false
	}
	r, err := s.send("GET", server+"/health", http.Header{}, nil, nil, 5, false)
	if err != nil {
		return false
	}
	if r.status == 200 {
		s.r.vncMu.Lock()
		if _, known := s.r.vnc[server]; !known {
			s.r.vnc[server] = vncFromHealth(server, r.body)
		}
		s.r.vncMu.Unlock()
	}
	return r.status == 200
}

// VNC address of the server, empty when it shows none
func (s *scope) vncURL() string {
	server := s.url()
	s.r.vncMu.Lock()
	_, known := s.r.vnc[server]
	s.r.vncMu.Unlock()
	if !known {
		s.available()
	}
	s.r.vncMu.Lock()
	defer s.r.vncMu.Unlock()
	if v := s.r.vnc[server]; v != nil {
		return *v
	}
	return ""
}

// One task's Camofox session, its tab guarded for tools running at once
type session struct {
	userID, sessionKey string
	adopt              bool
	mu                 sync.Mutex
	tabID              any
}

func (ss *session) tab() any {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.tabID
}

func (ss *session) setTab(v any) {
	ss.mu.Lock()
	ss.tabID = v
	ss.mu.Unlock()
}

func (ss *session) hasTab() bool { return config.Truthy(ss.tab()) }

type slot struct{ profile, task string }

func (s *scope) slot(task string) slot { return slot{s.cfg.Profile(), taskOr(task)} }

func tabPath(ss *session, suffix string) string { return "/tabs/" + text(ss.tab()) + "/" + suffix }

func userParams(ss *session) url.Values { return url.Values{"userId": {ss.userID}} }

func (s *scope) listTabs(ss *session, timeout int64) ([]any, error) {
	data, err := s.get("/tabs", userParams(ss), timeout)
	if err != nil {
		return nil, err
	}
	tabs, err := field(data, "tabs", []any{})
	if err != nil {
		return nil, err
	}
	list, _ := tabs.([]any)
	return list, nil
}

// Takes up the open tab of the session's key, never another session's
func (s *scope) adopt(ss *session) *session {
	if ss.hasTab() || !ss.adopt || s.url() == "" {
		return ss
	}
	tabs, err := s.listTabs(ss, 5)
	if err != nil {
		slog.Debug(fmt.Sprintf("Camofox tab adoption failed for %s: %v", ss.userID, err))
		return ss
	}
	var mine []map[string]any
	for _, tab := range tabs {
		if o, ok := tab.(map[string]any); ok && o["listItemId"] == ss.sessionKey {
			mine = append(mine, o)
		}
	}
	if len(mine) == 0 {
		return ss
	}
	if id, ok := mine[len(mine)-1]["tabId"].(string); ok && id != "" {
		ss.setTab(id)
		slog.Debug(fmt.Sprintf("Adopted existing Camofox tab %s for %s", id, ss.userID))
	}
	return ss
}

// Session of a task, made with the identity set outside, the profile's stable one or a fresh one, in that order
func (s *scope) session(task string) *session {
	key := s.slot(task)
	s.r.mu.Lock()
	existing := s.r.sessions[key]
	s.r.mu.Unlock()
	if existing != nil {
		return s.adopt(existing)
	}
	s.r.mu.Lock()
	if found := s.r.sessions[key]; found != nil {
		s.r.mu.Unlock()
		return found
	}
	id := s.identityOverride(key.task)
	if id == nil && s.managed() {
		managed := s.managedIdentity(key.task)
		id = &managed
	}
	ss := &session{}
	if id == nil {
		ss.userID = "icebath_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
		ss.sessionKey = "task_" + strs.Head(key.task, 16)
	} else {
		ss.userID, ss.sessionKey = id.userID, id.sessionKey
		ss.adopt = s.flag(camofoxconfig.AdoptExistingTabEnv, camofoxconfig.AdoptExistingTab)
	}
	s.r.sessions[key] = ss
	s.r.mu.Unlock()
	return s.adopt(ss)
}

// After a 404 or 410 for the tab, whether a live tab of the session is tracked again
func (s *scope) resync(ss *session) bool {
	tabs, err := s.listTabs(ss, 5)
	if err != nil {
		slog.Debug(fmt.Sprintf("Camofox tab resync failed for %s: %v", ss.userID, err))
		return false
	}
	var live []map[string]any
	for _, tab := range tabs {
		if o, ok := tab.(map[string]any); ok && config.Truthy(o["tabId"]) {
			live = append(live, o)
		}
	}
	tracked := ss.tab()
	for _, tab := range live {
		if tab["tabId"] == tracked {
			return true
		}
	}
	var mine []map[string]any
	for _, tab := range live {
		if tab["listItemId"] == ss.sessionKey {
			mine = append(mine, tab)
		}
	}
	if len(mine) == 0 {
		ss.setTab(nil)
		return false
	}
	ss.setTab(mine[len(mine)-1]["tabId"])
	return true
}

// Session of a task with a tab, one opened at the address when none is tracked
func (s *scope) ensureTab(task, at string) (*session, error) {
	ss := s.session(task)
	if !ss.hasTab() {
		data, err := s.post("/tabs", map[string]any{"userId": ss.userID, "listItemId": ss.sessionKey, "url": at}, commandTimeout)
		if err != nil {
			return nil, err
		}
		id, err := field(data, "tabId", nil)
		if err != nil {
			return nil, err
		}
		ss.setTab(id)
	}
	return ss, nil
}

func (s *scope) drop(task string) *session {
	key := s.slot(task)
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	ss := s.r.sessions[key]
	delete(s.r.sessions, key)
	return ss
}

func (s *scope) tracked(task string) *session {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	return s.r.sessions[s.slot(task)]
}

// Forgets the session of a stable identity here alone, false for a fresh one the caller closes on the server
func (s *scope) softCleanup(task string) bool {
	if s.managed() || s.identityOverride(task) != nil {
		s.drop(task)
		slog.Debug(fmt.Sprintf("Camofox soft cleanup for task %s (managed persistence)", task))
		return true
	}
	return false
}

// Forgets the task's session and deletes it on the server, the failure said
func (s *scope) closeSession(task string) string {
	ss := s.drop(task)
	if ss == nil {
		return ""
	}
	if _, err := s.delete("/sessions/"+ss.userID, commandTimeout); err != nil {
		return err.Error()
	}
	return ""
}

// Ends the task's session: a stable identity keeps its server profile, a fresh one is deleted
func (s *scope) cleanupTask(task string) {
	if s.softCleanup(task) {
		return
	}
	if warning := s.closeSession(task); warning != "" {
		slog.Debug(fmt.Sprintf("Camofox close for task %s: %s", task, warning))
	}
}

// How the profile's sessions pick their Camofox user
func (s *scope) identityMode() string {
	switch {
	case s.identityOverride("") != nil:
		return "The user you chose, under Browser › Camofox › User or with CAMOFOX_USER_ID"
	case s.managed():
		return "One user for every session of this profile, so sign-ins last"
	}
	return "A new user for each session"
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return cast.ToString(v)
}
