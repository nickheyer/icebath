package qqbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Background goroutine of the adapter with its cancel and completion
type loopTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// QQ Bot adapter over the official v2 websocket gateway for inbound and REST for outbound
type QQAdapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin

	appID           string
	clientSecret    string
	markdownSupport bool
	apiBase         string
	dedup           *gateway.MessageDeduplicator

	mu             sync.Mutex
	rest           *restClient
	conn           *wsConn
	loop           *loopTask
	sessionID      string
	lastSeq        int64
	hasSeq         bool
	heartbeatEvery time.Duration
	chatTypes      map[string]string
	lastMsgIDs     map[string]string
	typingSentAt   map[string]time.Time
}

// Class attributes the QQ adapter declares over the base defaults
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SupportsMessageEditing = false
	t.MaxMessageLength = maxMessageLength
	return t
}

// Trimmed config string, else the profile scoped secret
func configOrSecret(ctx context.Context, extra map[string]any, key, env string) (string, error) {
	if v := strings.TrimSpace(jstr(extra[key])); v != "" {
		return v, nil
	}
	v, err := gateway.GetScopedSecret(ctx, env, "", false)
	return strings.TrimSpace(v), err
}

// Sandbox REST host when QQ_SANDBOX is on, the production host otherwise
func apiBaseFor(ctx context.Context) (string, error) {
	sandbox, err := gateway.GetScopedSecret(ctx, "QQ_SANDBOX", "", false)
	if err != nil {
		return "", err
	}
	if jbool(sandbox) {
		return apiBaseSandbox, nil
	}
	return apiBaseProduction, nil
}

// Adapter configured from extra and the profile scoped QQ env
func NewQQAdapter(ctx context.Context, config *gateway.PlatformConfig) (*QQAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &QQAdapter{heartbeatEvery: 30 * time.Second, chatTypes: map[string]string{}, lastMsgIDs: map[string]string{},
		typingSentAt: map[string]time.Time{}, dedup: gateway.NewMessageDeduplicator(dedupMaxSize, dedupWindowSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformQQBot, a)
	a.Traits = adapterTraits()
	policy, err := gateway.NewOwnAccessPolicyMixin("QQAdapter", "QQ")
	if err != nil {
		return nil, err
	}
	a.OwnAccessPolicyMixin = policy
	policy.EntryMatches = entryMatches
	extra := config.Extra
	if a.appID, err = configOrSecret(ctx, extra, "app_id", "QQ_APP_ID"); err != nil {
		return nil, err
	}
	if a.clientSecret, err = configOrSecret(ctx, extra, "client_secret", "QQ_CLIENT_SECRET"); err != nil {
		return nil, err
	}
	a.markdownSupport = true
	if v, ok := extra["markdown_support"]; ok && v != nil {
		a.markdownSupport = jbool(v)
	} else if env, err := gateway.GetScopedSecret(ctx, "QQ_MARKDOWN_SUPPORT", "", false); err != nil {
		return nil, err
	} else if strings.TrimSpace(env) != "" {
		a.markdownSupport = jbool(env)
	}
	if a.apiBase, err = apiBaseFor(ctx); err != nil {
		return nil, err
	}
	policy.DMPolicy = policyOf(extra["dm_policy"])
	policy.GroupPolicy = policyOf(extra["group_policy"])
	for _, entry := range coerceList(firstNonEmpty(extra["allow_from"], extra["allowFrom"])) {
		policy.AllowFrom[entry] = true
	}
	for _, entry := range coerceList(firstNonEmpty(extra["group_allow_from"], extra["groupAllowFrom"])) {
		policy.GroupAllowFrom[entry] = true
	}
	return a, nil
}

// Lowercased policy name, pairing when unset
func policyOf(v any) string {
	if v == nil {
		return "pairing"
	}
	return strings.ToLower(strings.TrimSpace(jstr(v)))
}

// First value that is neither nil nor empty
func firstNonEmpty(values ...any) any {
	for _, v := range values {
		switch x := v.(type) {
		case nil:
			continue
		case string:
			if x == "" {
				continue
			}
		case []any:
			if len(x) == 0 {
				continue
			}
		}
		return v
	}
	return nil
}

// Case insensitive allowlist match where a star admits everyone
func entryMatches(_ context.Context, entries map[string]bool, target string) (bool, error) {
	want := strings.ToLower(strings.TrimSpace(target))
	for entry := range entries {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "*" || e == want {
			return true, nil
		}
	}
	return false, nil
}

// Platform display name
func (a *QQAdapter) Name() string { return "QQBot" }

// Log prefix carrying the app id so several bots stay apart
func (a *QQAdapter) logTag() string {
	if a.appID != "" {
		return "QQBot:" + a.appID
	}
	return "QQBot"
}

// The adapter gates DMs and groups itself
func (a *QQAdapter) EnforcesOwnAccessPolicy() bool { return a.OwnAccessPolicyMixin.EnforcesOwnAccessPolicy() }

// Whether the websocket transport is up
func (a *QQAdapter) IsConnected() bool {
	if !a.BasePlatformAdapter.IsConnected() {
		return false
	}
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	return conn != nil && !conn.isClosed()
}

// REST client of the live connection, an error before connect or after disconnect
func (a *QQAdapter) client() (*restClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rest == nil {
		return nil, errors.New("QQ HTTP client not initialized, the adapter is not connected")
	}
	return a.rest, nil
}

// Authenticates, opens the gateway websocket and starts the receive loop
func (a *QQAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.appID == "" || a.clientSecret == "" {
		msg := "QQ startup failed: QQ_APP_ID and QQ_CLIENT_SECRET are required"
		a.SetFatalError(ctx, "qq_missing_credentials", msg, true)
		slog.Warn(fmt.Sprintf("[%s] %s", a.logTag(), msg))
		return false, nil
	}
	locked, err := a.AcquirePlatformLock(ctx, "qqbot-appid", a.appID, "QQBot app ID")
	if err != nil || !locked {
		return false, err
	}
	noTimeout := time.Duration(0)
	rest := newRESTClient(a.appID, a.clientSecret, a.apiBase, a.logTag(),
		gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{Timeout: &noTimeout}))
	a.mu.Lock()
	a.rest, a.heartbeatEvery = rest, 30*time.Second
	a.mu.Unlock()
	conn, err := a.openGateway(ctx, true)
	if err != nil {
		msg := "QQ startup failed: " + err.Error()
		a.SetFatalError(ctx, "qq_connect_error", msg, true)
		slog.Error(fmt.Sprintf("[%s] %s", a.logTag(), msg))
		a.mu.Lock()
		a.rest = nil
		a.mu.Unlock()
		rest.http.CloseIdleConnections()
		a.ReleasePlatformLock()
		return false, nil
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	task := &loopTask{cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	a.conn, a.loop = conn, task
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	go func() {
		defer close(task.done)
		a.listen(loopCtx)
	}()
	slog.Info(fmt.Sprintf("[%s] Connected", a.logTag()))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the receive loop, closes the websocket and HTTP client and releases the app id lock
func (a *QQAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	task := a.loop
	a.loop = nil
	a.mu.Unlock()
	if task != nil {
		task.cancel()
	}
	a.mu.Lock()
	conn := a.conn
	a.conn = nil
	a.mu.Unlock()
	if conn != nil {
		conn.close()
	}
	if task != nil {
		select {
		case <-task.done:
		case <-ctx.Done():
		}
	}
	a.mu.Lock()
	late, rest := a.conn, a.rest
	a.conn, a.rest = nil, nil
	a.mu.Unlock()
	if late != nil {
		late.close()
	}
	if rest != nil {
		rest.http.CloseIdleConnections()
	}
	a.MarkDisconnected(ctx)
	a.ReleasePlatformLock()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.logTag()))
	return nil
}

// Records a fatal error and tells the gateway from a detached goroutine so its disconnect cannot cancel the report
func (a *QQAdapter) fail(ctx context.Context, code, message string, retryable bool) {
	a.SetFatalError(ctx, code, message, retryable)
	go func() {
		if err := a.NotifyFatalError(context.WithoutCancel(ctx)); err != nil {
			slog.Warn(fmt.Sprintf("[%s] fatal-error notification failed: %v", a.logTag(), err))
		}
	}()
}

// Publishes the transport as down while the receive loop keeps reconnecting
func (a *QQAdapter) markTransportDisconnected(ctx context.Context) {
	if a.HasFatalError() {
		return
	}
	a.WriteRuntimeStatusSafe(ctx, "disconnected", map[string]any{"platform_state": "disconnected", "error_code": nil, "error_message": nil})
}

// Remembers which QQ surface a chat id belongs to
func (a *QQAdapter) setChatType(chatID, kind string) {
	a.mu.Lock()
	a.chatTypes[chatID] = kind
	a.mu.Unlock()
}

// QQ surface of a chat id seen inbound, empty when unknown
func (a *QQAdapter) knownChatType(chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chatTypes[chatID]
}

// Last inbound message id of a chat, the anchor passive replies need
func (a *QQAdapter) lastMsgID(chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastMsgIDs[chatID]
}

// Records the inbound message id of the chat before the base admits the event
func (a *QQAdapter) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	if event.MessageID != nil && *event.MessageID != "" && event.Source != nil && event.Source.ChatID != "" {
		a.mu.Lock()
		a.lastMsgIDs[event.Source.ChatID] = *event.MessageID
		a.mu.Unlock()
	}
	return a.BasePlatformAdapter.HandleMessage(ctx, event)
}

// Markdown passed through when the bot may send it, stripped otherwise
func (a *QQAdapter) FormatMessage(content string) string {
	if a.markdownSupport {
		return content
	}
	return gateway.StripMarkdown(content, false)
}

// Chat name and dm or group kind of a chat id
func (a *QQAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	kind := "dm"
	switch a.knownChatType(chatID) {
	case "group", "guild":
		kind = "group"
	}
	return map[string]any{"name": chatID, "type": kind}, nil
}
