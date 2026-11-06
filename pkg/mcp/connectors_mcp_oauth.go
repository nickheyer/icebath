package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// How long a card start waits for the authorization URL
const cardOAuthURLTimeout = 30 * time.Second

// AttemptCanceled is a card attempt the user canceled before it committed, nothing of it kept
type AttemptCanceled struct{}

func (*AttemptCanceled) Error() string { return "canceled" }

type cardKey struct{ home, server string }

// One lock orders a cancel against the commit and guards the newest card attempt per home and server
var (
	cardCommitGuard sync.Mutex
	cardActive      = map[cardKey]*DashboardOAuthFlow{}
)

// Rolls back failures through initialize and commits the authorization before tool discovery
func probeWithRollback(ctx context.Context, env *tools.Env, serverName string, cfg map[string]any, homeDir string,
	flow *DashboardOAuthFlow, reconnectLive bool, onCommit func(context.Context) error) error {
	manager := getManager()
	storage := NewTokenStorage(ctx, serverName, "")
	var backup map[string][]byte
	if flow != nil {
		cardCommitGuard.Lock()
		backup = flow.inheritedBackup
		cardCommitGuard.Unlock()
	}
	if backup == nil {
		backup = storage.Snapshot()
	}
	if flow != nil {
		cardCommitGuard.Lock()
		flow.backup = backup
		cardCommitGuard.Unlock()
	}
	var previous *providerEntry
	details := &ProbeDetails{}
	var summaries []ToolSummary
	discoveryError := ""
	undo := func() {
		if flow != nil {
			cardCommitGuard.Lock()
			active := cardActive[cardKey{homeDir, serverName}]
			cardCommitGuard.Unlock()
			if active != nil && active != flow {
				return
			}
		}
		_ = storage.Restore(backup, false)
		manager.restoreEntry(ctx, serverName, previous, homeDir)
	}
	err := func() error {
		prev, err := manager.remove(ctx, serverName, homeDir)
		if err != nil {
			return err
		}
		previous = prev
		timeout := LoginConnectTimeout(cfg)
		summaries, err = probeServer(ctx, env, serverName, cfg, &timeout, details)
		if err != nil {
			return err
		}
		if !oauthTokensPresent(ctx, serverName) {
			details.Initialized = false
			return &ProbeError{Err: errors.New("The server responded, but no OAuth token was obtained — " +
				"this provider may require a manually-registered OAuth client."), Class: "auth_required"}
		}
		return nil
	}()
	if err != nil {
		if !details.Initialized {
			undo()
			return err
		}
		summaries, discoveryError = nil, ExceptionMessage(err)
	}
	if err := commitCardAttempt(ctx, serverName, cfg, onCommit, flow); err != nil {
		var canceled *AttemptCanceled
		if errors.As(err, &canceled) {
			undo()
		}
		return err
	}
	if flow != nil {
		flow.Tools = summaryDicts(summaries)
		flow.DiscoveryError = discoveryError
		if err := flow.MarkApproved(); err != nil {
			return err
		}
	}
	if discoveryError != "" {
		return nil
	}
	if reconnectLive {
		ReconnectServer(ctx, serverName)
	}
	return nil
}

func summaryDicts(summaries []ToolSummary) []map[string]any {
	out := make([]map[string]any, len(summaries))
	for i, s := range summaries {
		out[i] = map[string]any{"name": s.Name, "description": s.Description}
	}
	return out
}

// Cancels a card attempt, true when it had already committed so the authorization stays
func CancelAttempt(flow *DashboardOAuthFlow) bool {
	cardCommitGuard.Lock()
	if flow.committed {
		cardCommitGuard.Unlock()
		return true
	}
	flow.mu.Lock()
	flow.cancelled = true
	flow.mu.Unlock()
	cardCommitGuard.Unlock()
	flow.MarkError("canceled", true)
	return false
}

// Saves the configuration and runs the commit hook unless the attempt was canceled first
func commitCardAttempt(ctx context.Context, serverName string, cfg map[string]any, onCommit func(context.Context) error, flow *DashboardOAuthFlow) error {
	cardCommitGuard.Lock()
	defer cardCommitGuard.Unlock()
	if flow != nil && flow.Cancelled() {
		return &AttemptCanceled{}
	}
	ok, err := SaveServer(ctx, headlessUI{}, serverName, cfg)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("'%s' was rejected: suspicious command/args configuration", serverName)
	}
	if onCommit != nil {
		if err := onCommit(ctx); err != nil {
			return err
		}
	}
	if flow != nil {
		flow.committed = true
	}
	return nil
}

// Connects with the tokens already on disk with no browser step, false when they do not work
func reuseSavedAuthorization(ctx context.Context, env *tools.Env, serverName string, cfg map[string]any,
	flow *DashboardOAuthFlow, onCommit func(context.Context) error) bool {
	if !oauthTokensPresent(ctx, serverName) {
		return false
	}
	timeout := 30.0
	summaries, err := probeServer(SuppressInteractiveOAuth(ctx), env, serverName, cfg, &timeout, nil)
	if err == nil {
		err = commitCardAttempt(ctx, serverName, cfg, onCommit, flow)
	}
	if err != nil {
		mcpLogger().Debug(fmt.Sprintf("saved authorization for %s was not usable: %s", serverName, excStr(err)))
		return false
	}
	if flow != nil {
		flow.Tools = summaryDicts(summaries)
		flow.DiscoveryError = ""
		if err := flow.MarkApproved(); err != nil {
			return false
		}
	}
	return true
}

// Worker and receiver seams of the card start, swapped by tests
var (
	cardOAuthWorker  = RunOAuthWorker
	loopbackReceiver = startLoopbackReceiver
)

// OAuthWorkerOptions carries the flow, completion hook, setup values and saved token rule of one worker
type OAuthWorkerOptions struct {
	Flow       *DashboardOAuthFlow
	OnDone     func()
	Env        *pyjson.Object
	OnCommit   func(context.Context) error
	ReuseSaved bool
}

// Drives the interactive MCP OAuth probe under the profile home and its secret scope
func RunOAuthWorker(ctx context.Context, env *tools.Env, homeDir, serverName string, cfg map[string]any, reconnectLive bool, o OAuthWorkerOptions) {
	flow := o.Flow
	defer func() {
		if flow != nil {
			flow.MarkWorkerDone()
			cardCommitGuard.Lock()
			if cardActive[cardKey{homeDir, serverName}] == flow {
				delete(cardActive, cardKey{homeDir, serverName})
			}
			cardCommitGuard.Unlock()
		}
		if o.OnDone != nil {
			o.OnDone()
		}
	}()
	err := func() error {
		wctx := home.WithOverride(ctx, homeDir)
		scope := provider.BuildProfileSecretScope(homeDir)
		if o.Env != nil {
			for _, k := range o.Env.Keys {
				scope[k], _ = o.Env.Vals[k].(string)
			}
		}
		wctx = provider.WithSecretScope(wctx, scope, homeDir)
		if o.ReuseSaved && flow != nil && reuseSavedAuthorization(wctx, env, serverName, cfg, flow, o.OnCommit) {
			return nil
		}
		ictx := withDashboardOAuthFlow(ForceInteractiveOAuth(wctx), flow)
		return probeWithRollback(ictx, env, serverName, cfg, homeDir, flow, reconnectLive, o.OnCommit)
	}()
	if err == nil {
		return
	}
	msg := ExceptionMessage(err)
	serverURL, _ := cfg["url"].(string)
	if human, ok := HumanizeOAuthRegistrationError(serverName, err.Error(), serverURL); ok && human != "" {
		msg = human
	}
	failureClass := ProbeFailureClass(err)
	if failureClass == "connect_failed" && !strings.Contains(msg, serverName) {
		msg = fmt.Sprintf("Could not connect to '%s' (%s). Check its URL and that this machine can reach it.", serverName, msg)
	}
	if flow != nil {
		cardCommitGuard.Lock()
		flow.failureClass = failureClass
		cardCommitGuard.Unlock()
		flow.MarkError(msg, false)
	}
}

// Accepts only plain http loopback URLs and returns the normalized redirect
func ValidateClientRedirectURI(uri string) (string, error) {
	bad := errors.New("client_redirect_uri must be a loopback http URL like http://127.0.0.1:<port>/callback")
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return "", bad
	}
	host := strings.ToLower(u.Hostname())
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n > 65535 {
			return "", fmt.Errorf("Port out of range 0-65535")
		}
		port = n
	}
	if u.Scheme != "http" || (host != "127.0.0.1" && host != "localhost" && host != "::1") || port == 0 || u.User != nil {
		return "", bad
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/callback"
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, path), nil
}

// Binds the backend hosted one shot receiver on a free loopback port and feeds its callback into flow
func startLoopbackReceiver(flow *DashboardOAuthFlow) (*http.Server, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, fmt.Sprintf("Unsupported method (%s)", pyrepr.StrRepr(r.Method)), http.StatusNotImplemented)
			return
		}
		if p := strings.TrimRight(r.URL.Path, "/"); p != "/callback" && p != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := "<h1>Authorization received</h1><p>You can close this tab and return to Icebath.</p>"
		status := http.StatusOK
		code, state, errText, iss := parseRedirectQuery(r.URL.RawQuery)
		if err := flow.DeliverCallback(code, state, errText, iss); err != nil {
			body = "<h1>OAuth callback rejected</h1><p>The callback was invalid or already used.</p>"
			status = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	return srv, ln.Addr().(*net.TCPAddr).Port, nil
}

func cfgOAuth(cfg map[string]any) map[string]any {
	o, _ := cfg["oauth"].(map[string]any)
	return o
}

func pinnedLoopback(cfg map[string]any) bool {
	o := cfgOAuth(cfg)
	return connTruthy(o["client_id"]) && connTruthy(o["redirect_port"])
}

func pinnedRedirectURI(cfg map[string]any) string {
	o := cfgOAuth(cfg)
	host := "127.0.0.1"
	if connTruthy(o["redirect_host"]) {
		host = pyrepr.Str(o["redirect_host"])
	}
	return fmt.Sprintf("http://%s:%s/callback", host, pyrepr.Str(o["redirect_port"]))
}

// Sets the redirect the SDK consumes and returns the backend receiver when one is bound
func ChooseCallbackReceiver(flow *DashboardOAuthFlow, cfg map[string]any, clientRedirectURI *string) (*http.Server, error) {
	if pinnedLoopback(cfg) {
		flow.RedirectURI = pinnedRedirectURI(cfg)
		return nil, nil
	}
	if clientRedirectURI != nil {
		uri, err := ValidateClientRedirectURI(*clientRedirectURI)
		if err != nil {
			return nil, err
		}
		flow.RedirectURI = uri
		return nil, nil
	}
	srv, port, err := loopbackReceiver(flow)
	if err != nil {
		return nil, err
	}
	flow.RedirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	return srv, nil
}

// The remote session hint for a loopback redirect, empty outside SSH
func sshDetail(redirectURI string) string {
	if (os.Getenv("SSH_CLIENT") == "" && os.Getenv("SSH_TTY") == "") || redirectURI == "" {
		return ""
	}
	u, err := url.Parse(redirectURI)
	host, port := "127.0.0.1", 0
	if err == nil {
		if h := u.Hostname(); h != "" {
			host = strings.ToLower(h)
		}
		port, _ = strconv.Atoi(u.Port())
	}
	return strings.TrimSpace(fmt.Sprintf(sshHintLoopback, host, port))
}

// OAuthAttempt is one card flow in flight, its URL, callback bridge and outcome
type OAuthAttempt struct {
	authURL string
	flow    *DashboardOAuthFlow
	detail  string
}

func (a *OAuthAttempt) AuthURL() string           { return a.authURL }
func (a *OAuthAttempt) Detail() string            { return a.detail }
func (a *OAuthAttempt) Flow() *DashboardOAuthFlow { return a.flow }

// The flow's status with its tools and discovery error once approved
func (a *OAuthAttempt) Poll() AttemptPoll {
	snap := a.flow.Snapshot()
	status, _ := snap["status"].(string)
	if status != "approved" && status != "error" {
		status = "pending"
	}
	errText, _ := snap["error"].(string)
	out := AttemptPoll{Status: status, Error: errText, Tools: []string{}}
	if status == "approved" {
		for _, t := range a.flow.Tools {
			if name := connStr(t["name"]); name != "" {
				out.Tools = append(out.Tools, name)
			}
		}
		out.DiscoveryError = a.flow.DiscoveryError
	}
	return out
}

// CardOAuthOptions carries an install's in memory config, its setup values and commit hook, and the redirect
type CardOAuthOptions struct {
	URLTimeout        time.Duration
	ClientRedirectURI *string
	Cfg               map[string]any
	Env               *pyjson.Object
	OnCommit          func(context.Context) error
}

// Starts a card OAuth flow and waits until its authorization URL is published
func StartCardOAuth(ctx context.Context, env *tools.Env, serverName string, o CardOAuthOptions) (*OAuthAttempt, error) {
	var cfg map[string]any
	if o.Cfg != nil {
		cfg = maps.Clone(o.Cfg)
	} else {
		servers, err := getMCPServers(ctx, nil)
		if err != nil {
			return nil, err
		}
		saved, _ := servers[serverName].(map[string]any)
		cfg = maps.Clone(saved)
	}
	if len(cfg) == 0 {
		return nil, fmt.Errorf("'%s' is not a configured MCP server", serverName)
	}
	if !connTruthy(cfg["url"]) {
		return nil, fmt.Errorf("'%s' is a stdio server: it takes env keys, not OAuth", serverName)
	}
	cfg["auth"] = "oauth"
	homeDir, err := pypath.Resolve(pypath.Expanduser(home.Dir(ctx)))
	if err != nil {
		return nil, err
	}
	flow := NewDashboardOAuthFlow(tokenURLSafe(24), serverName, "", homeDir, "", false)
	key := cardKey{homeDir, serverName}
	cardCommitGuard.Lock()
	older := cardActive[key]
	cardActive[key] = flow
	cardCommitGuard.Unlock()
	if older != nil && !older.WorkerDone() {
		cardCommitGuard.Lock()
		flow.inheritedBackup = older.backup
		cardCommitGuard.Unlock()
		CancelAttempt(older)
	}
	httpd, err := ChooseCallbackReceiver(flow, cfg, o.ClientRedirectURI)
	if err != nil {
		return nil, err
	}
	RegisterOAuthFlow(flow, httpd)
	go cardOAuthWorker(context.WithoutCancel(ctx), env, homeDir, serverName, cfg, false, OAuthWorkerOptions{
		Flow: flow, OnDone: func() { FinishOAuthFlow(flow.FlowID) }, ReuseSaved: true, Env: o.Env, OnCommit: o.OnCommit})
	timeout := o.URLTimeout
	if timeout <= 0 {
		timeout = cardOAuthURLTimeout
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := flow.Snapshot()
		if snap["status"] == "approved" {
			return &OAuthAttempt{flow: flow}, nil
		}
		if u, _ := snap["authorization_url"].(string); u != "" {
			detail := ""
			if o.ClientRedirectURI == nil {
				detail = sshDetail(flow.RedirectURI)
			}
			return &OAuthAttempt{authURL: u, flow: flow, detail: detail}, nil
		}
		if snap["status"] == "error" {
			msg, _ := snap["error"].(string)
			cardCommitGuard.Lock()
			class := flow.failureClass
			cardCommitGuard.Unlock()
			return nil, &ProbeError{Err: errors.New(orText(msg, "the OAuth flow failed before authorization")), Class: class}
		}
		time.Sleep(50 * time.Millisecond)
	}
	flow.MarkError("Timed out waiting for MCP authorization URL", false)
	return nil, &CardURLTimeout{Server: serverName}
}

// CardURLTimeout is a card flow whose provider never published an authorization URL
type CardURLTimeout struct{ Server string }

func (e *CardURLTimeout) Error() string {
	return fmt.Sprintf("timed out waiting for the authorization URL for '%s'", e.Server)
}
