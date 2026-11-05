package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// DashboardOAuthValueError is a rejected authorization URL or browser callback, a client error for the dashboard
type DashboardOAuthValueError struct{ Msg string }

func (e *DashboardOAuthValueError) Error() string { return e.Msg }

// DashboardOAuthTimeoutError is a wait for the authorization URL or the callback that ran out of time
type DashboardOAuthTimeoutError struct{ Msg string }

func (e *DashboardOAuthTimeoutError) Error() string { return e.Msg }

// Error text, or the error type name when the text is empty
func ExceptionMessage(err error) string {
	if msg := err.Error(); msg != "" {
		return msg
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() != "" {
		return t.Name()
	}
	return fmt.Sprintf("%T", err)
}

// flowLatch is a resettable one shot signal, every method called with the flow lock held
type flowLatch struct {
	ch  chan struct{}
	set bool
}

func (l *flowLatch) channel() chan struct{} {
	if l.ch == nil {
		l.ch = make(chan struct{})
	}
	return l.ch
}

func (l *flowLatch) fire() {
	if !l.set {
		close(l.channel())
		l.set = true
	}
}

func (l *flowLatch) reset() {
	if l.set {
		l.ch = make(chan struct{})
		l.set = false
	}
}

// DashboardOAuthFlow moves the authorization URL and the browser callback of one MCP login into a dashboard session
type DashboardOAuthFlow struct {
	FlowID         string
	ServerName     string
	Profile        string
	HomeDir        string
	RedirectURI    string
	ReconnectLive  bool
	CreatedAt      time.Time
	Tools          []map[string]any
	DiscoveryError string

	mu               sync.Mutex
	status           string
	authorizationURL string
	failure          string
	cancelled        bool
	expectedState    string
	callback         *[3]string
	callbackErr      string
	authorization    flowLatch
	callbackReady    flowLatch
	workerDone       flowLatch
}

// Flow in the starting state stamped with its creation time
func NewDashboardOAuthFlow(flowID, serverName, profile, homeDir, redirectURI string, reconnectLive bool) *DashboardOAuthFlow {
	return &DashboardOAuthFlow{FlowID: flowID, ServerName: serverName, Profile: profile, HomeDir: homeDir,
		RedirectURI: redirectURI, ReconnectLive: reconnectLive, CreatedAt: time.Now(), status: "starting"}
}

// Current status with a zero value flow reading as starting, caller holds the lock
func (f *DashboardOAuthFlow) statusLocked() string {
	if f.status == "" {
		return "starting"
	}
	return f.status
}

// Status of the flow, one of starting, authorization_required, approved or error
func (f *DashboardOAuthFlow) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusLocked()
}

// Authorization URL the dashboard shows, empty before one was published
func (f *DashboardOAuthFlow) AuthorizationURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorizationURL
}

// Recorded failure cause, empty while the flow has not failed
func (f *DashboardOAuthFlow) Failure() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failure
}

// State parameter the next callback must carry, empty when none is expected
func (f *DashboardOAuthFlow) ExpectedState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expectedState
}

// Reports whether the user abandoned the flow for good
func (f *DashboardOAuthFlow) Cancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// Records the authorization URL and its state, re-minting an ended attempt unless the user cancelled
func (f *DashboardOAuthFlow) PublishAuthorizationURL(_ context.Context, rawURL string) error {
	u, err := pyURLParse(rawURL)
	if err != nil {
		return &DashboardOAuthValueError{err.Error()}
	}
	state := ""
	for _, kv := range pyParseQSL(u.Query, false) {
		if kv[0] == "state" {
			state = kv[1]
			break
		}
	}
	if state == "" {
		return &DashboardOAuthValueError{"OAuth authorization URL did not include state"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled {
		return errors.New("OAuth flow already ended: cancelled by user")
	}
	if s := f.statusLocked(); s == "approved" || s == "error" {
		f.reopenEndedAttempt()
	}
	f.expectedState = state
	f.authorizationURL = rawURL
	f.status = "authorization_required"
	f.authorization.fire()
	return nil
}

// Drops the ended attempt's URL, state and spent code so the next URL mints a fresh attempt, caller holds the lock
func (f *DashboardOAuthFlow) reopenEndedAttempt() {
	mcpLogger().Info(fmt.Sprintf("MCP OAuth: dashboard flow %s for '%s' ended (%s); re-minting for the next attempt",
		f.FlowID, f.ServerName, f.statusLocked()))
	f.authorizationURL = ""
	f.expectedState = ""
	f.callback = nil
	f.callbackErr = ""
	f.callbackReady.reset()
	f.failure = ""
	f.status = "starting"
}

// Waits on a latch channel until it fires, ctx ends or the timeout passes
func waitFlowLatch(ctx context.Context, ch chan struct{}, timeout time.Duration) (bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true, nil
	case <-timer.C:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Authorization URL once published, failing on timeout or when the flow ended without one
func (f *DashboardOAuthFlow) WaitForAuthorizationURL(ctx context.Context, timeout time.Duration) (string, error) {
	f.mu.Lock()
	ch := f.authorization.channel()
	f.mu.Unlock()
	ok, err := waitFlowLatch(ctx, ch, timeout)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &DashboardOAuthTimeoutError{"Timed out waiting for MCP authorization URL"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authorizationURL == "" {
		if f.failure != "" {
			return "", errors.New(f.failure)
		}
		return "", errors.New("MCP OAuth flow ended before authorization")
	}
	return f.authorizationURL, nil
}

// Hands the browser redirect to the waiting flow once, its state matching exactly
func (f *DashboardOAuthFlow) DeliverCallback(code, state, errText, iss string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callbackReady.set {
		return &DashboardOAuthValueError{"OAuth callback already received"}
	}
	if f.expectedState == "" || subtle.ConstantTimeCompare([]byte(f.expectedState), []byte(state)) != 1 {
		return &DashboardOAuthValueError{"OAuth callback state mismatch"}
	}
	switch {
	case errText != "":
		f.callbackErr = errText
	case code != "":
		f.callback = &[3]string{code, state, iss}
	default:
		f.callbackErr = "OAuth callback did not include code or error"
	}
	f.callbackReady.fire()
	return nil
}

// Code, state and iss of the delivered callback, failing on timeout, a callback error or a codeless end
func (f *DashboardOAuthFlow) WaitForCallback(ctx context.Context, timeout time.Duration) (code, state, iss string, err error) {
	f.mu.Lock()
	ch := f.callbackReady.channel()
	f.mu.Unlock()
	ok, err := waitFlowLatch(ctx, ch, timeout)
	if err != nil {
		return "", "", "", err
	}
	if !ok {
		return "", "", "", &DashboardOAuthTimeoutError{"Timed out waiting for MCP OAuth callback"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callbackErr != "" {
		return "", "", "", fmt.Errorf("OAuth authorization failed: %s", f.callbackErr)
	}
	if f.callback == nil {
		var failure any
		if f.failure != "" {
			failure = f.failure
		}
		return "", "", "", fmt.Errorf("MCP OAuth flow for '%s' ended without an authorization code (status=%s, error=%s)",
			f.ServerName, f.statusLocked(), pyRepr(failure))
	}
	return f.callback[0], f.callback[1], f.callback[2], nil
}

// Marks the flow approved unless it already failed
func (f *DashboardOAuthFlow) MarkApproved() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusLocked() == "error" {
		return errors.New("OAuth flow already ended")
	}
	f.status = "approved"
	f.failure = ""
	return nil
}

// Fails the flow keeping the first cause, waking both waiters, a cancellation always recorded
func (f *DashboardOAuthFlow) MarkError(errText string, cancelled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = f.cancelled || cancelled
	if s := f.statusLocked(); s == "approved" || s == "error" {
		return
	}
	f.status = "error"
	f.failure = errText
	if f.failure == "" {
		f.failure = "MCP OAuth flow failed before the callback was received (empty error message)"
	}
	if f.callback == nil && f.callbackErr == "" {
		f.callbackErr = f.failure
	}
	f.authorization.fire()
	f.callbackReady.fire()
}

// Flow id, server, status, authorization URL and error, never the authorization code or state
func (f *DashboardOAuthFlow) Snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var authURL, failure any
	if f.authorizationURL != "" {
		authURL = f.authorizationURL
	}
	if f.failure != "" {
		failure = f.failure
	}
	return map[string]any{"flow_id": f.FlowID, "server_name": f.ServerName, "status": f.statusLocked(),
		"authorization_url": authURL, "error": failure}
}

// Records that the worker driving the flow returned
func (f *DashboardOAuthFlow) MarkWorkerDone() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workerDone.fire()
}

// Reports whether the worker driving the flow returned
func (f *DashboardOAuthFlow) WorkerDone() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workerDone.set
}

type dashboardOAuthFlowKey struct{}

// Active dashboard OAuth flow carried by ctx, nil when none
func dashboardOAuthFlowFrom(ctx context.Context) *DashboardOAuthFlow {
	f, _ := ctx.Value(dashboardOAuthFlowKey{}).(*DashboardOAuthFlow)
	return f
}

// Context whose MCP OAuth callbacks go through the dashboard flow
func withDashboardOAuthFlow(ctx context.Context, flow *DashboardOAuthFlow) context.Context {
	return context.WithValue(ctx, dashboardOAuthFlowKey{}, flow)
}
