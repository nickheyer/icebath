package gateway

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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Ceiling of one synchronous wake self post turn
const WakeTurnTimeoutSeconds = 600.0

// Backoff between self post retries on transient failures
var wakeRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// Store refusal codes that refuse a notice for good by themselves
var PermanentRefusalReasons = map[string]bool{"not_found": true, "invalid_params": true}

// Logger of the wake module
func wakeLog() *slog.Logger { return slog.Default().With("logger", "gateway.wake") }

// No adapter admission, the store refusal code in Reason and Permanent once the refusal is proven final
type WakeNotAccepted struct {
	Msg       string
	Reason    *string
	Permanent bool
	cause     error
}

func (e *WakeNotAccepted) Error() string { return e.Msg }

func (e *WakeNotAccepted) Unwrap() error { return e.cause }

// Refusal with the default message and no reason
func newWakeNotAccepted(msg string, reason *string) *WakeNotAccepted {
	if msg == "" {
		msg = "internal wake not accepted by adapter"
	}
	return &WakeNotAccepted{Msg: msg, Reason: reason}
}

// Whether the adapter can push a message after a turn ends
func AdapterSupportsPush(adapter PlatformAdapter) bool {
	if base := adapter.Base(); base != nil {
		return base.Traits.SupportsAsyncDelivery
	}
	return true
}

// Whether served profile's own session store canonically owns a stateless raw session id
func SessionOwnedByProfile(ctx context.Context, cfg *GatewayConfig, profile string, sessionID string) bool {
	if sessionID == "" || profile == "" {
		return false
	}
	homes, err := MultiplexProfileHomes(ctx, cfg)
	if err != nil {
		wakeLog().Debug(fmt.Sprintf("wake: session ownership check unavailable for %s/%s: %s", profile, sessionID, err))
		return false
	}
	home := ""
	for _, served := range homes {
		if served.Name == profile {
			home = served.Home
		}
	}
	if home == "" {
		return false
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(home, "state.db"), true)
	if err != nil {
		wakeLog().Debug(fmt.Sprintf("wake: session ownership check unavailable for %s/%s: %s", profile, sessionID, err))
		return false
	}
	defer db.Close()
	row, err := db.GetSession(ctx, sessionID)
	if err != nil {
		wakeLog().Debug(fmt.Sprintf("wake: session ownership lookup failed for %s/%s: %s", profile, sessionID, err))
		return false
	}
	if row == nil || row.Len() == 0 {
		return false
	}
	owner := profile
	if v, _ := row.Get("profile_name"); pyrepr.Truthy(v) {
		owner = pyrepr.Str(v)
	}
	return owner == profile
}

// Requires a concrete adapter admission of the event, refusing it while the runner drains
func AdmitInternalEvent(ctx context.Context, adapter PlatformAdapter, event *MessageEvent) error {
	event.setGatewayAccepted(false)
	draining := "runtime_draining"
	if runnerDraining(adapter) {
		return newWakeNotAccepted("gateway is draining; retry after restart", &draining)
	}
	if err := adapter.HandleMessage(ctx, event); err != nil {
		return err
	}
	if !event.GatewayAccepted() {
		return newWakeNotAccepted("", nil)
	}
	if runnerDraining(adapter) {
		return newWakeNotAccepted("gateway began draining during admission; retry after restart", &draining)
	}
	gatewayAcceptedMu.Lock()
	event.admittedBeforeDrain = true
	gatewayAcceptedMu.Unlock()
	return nil
}

// Whether the adapter's gateway runner is draining
func runnerDraining(adapter PlatformAdapter) bool {
	base := adapter.Base()
	if base == nil {
		return false
	}
	runner, ok := base.GatewayRunner.(interface{ Draining() bool })
	return ok && runner.Draining()
}

// Wake turn delivery options of DeliverWake
type WakeOptions struct {
	Text                 string
	SessionID            string
	Source               *SessionSource
	NotificationCategory string
	Profile              string
}

// Delivers a wake turn to the session behind the adapter, an error so the caller can retry
func DeliverWake(ctx context.Context, adapter PlatformAdapter, opts WakeOptions) error {
	category := opts.NotificationCategory
	if category == "" {
		category = "result"
	}
	if AdapterSupportsPush(adapter) {
		if opts.Source == nil {
			return &PyError{Kind: "ValueError", Msg: "deliver_wake: push-capable adapter requires a SessionSource"}
		}
		if runner, ok := adapter.Base().GatewayRunner.(AuthorityRunner); ok && runner != nil {
			metadata := map[string]any{}
			if category != "result" {
				metadata["notification_category"] = category
			}
			admitted, err := admitRouteWake(ctx, runner, adapter, opts.Source, opts.Text, "kanban-wake", metadata)
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				reason := refusal.Reason
				wake := newWakeNotAccepted(refusal.Error(), &reason)
				wake.cause = err
				return wake
			}
			if err != nil {
				return err
			}
			if admitted {
				return nil
			}
		}
		synth := &MessageEvent{Text: opts.Text, MessageType: MessageTypeText, Source: opts.Source, Internal: true,
			Metadata: map[string]any{"notification_category": category}}
		return AdmitInternalEvent(ctx, adapter, synth)
	}
	if opts.SessionID == "" {
		return &PyError{Kind: "ValueError", Msg: "deliver_wake: non-push adapter (supports_async_delivery=False) requires the raw session id to self-post the wake turn"}
	}
	return selfPostChatCompletion(ctx, adapter, opts.Text, opts.SessionID, category, opts.Profile)
}

// Display metadata of a persisted delegation delivery row
func delegationDisplayMetadata(ctx context.Context, evt map[string]any) *pyjson.Object {
	var results []map[string]any
	if raw, ok := evt["results"].([]any); ok {
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				results = append(results, m)
			}
		}
	}
	taskCount := len(results)
	if taskCount == 0 {
		taskCount = 1
	}
	completed, failed := 0, 0
	for _, r := range results {
		switch r["status"] {
		case "completed", "success":
			completed++
		case "failed", "error":
			failed++
		}
	}
	if completed == 0 {
		completed = taskCount - failed
	}
	metadata := pyjson.NewObject()
	metadata.Set("delegation_id", truthyString(evt["delegation_id"]))
	metadata.Set("task_count", taskCount)
	metadata.Set("completed_count", completed)
	metadata.Set("failed_count", failed)
	if pyrepr.Truthy(evt["task_failure_notice"]) {
		index := ""
		if len(results) > 0 {
			if v, ok := results[0]["task_index"]; ok {
				index = pyrepr.Str(v)
			}
		}
		metadata.Set("delivery_notice", "task_failure:"+index)
		metadata.Set("notification_category", "diagnostic")
		if !config.WarningNotificationsEnabled(ctx, "api_server", nil) {
			metadata.Set("presentation_suppressed", true)
		}
	}
	duration := evt["total_duration_seconds"]
	if !pyrepr.Truthy(duration) {
		duration = evt["duration_seconds"]
	}
	switch duration.(type) {
	case int, int64, float64:
		metadata.Set("duration_seconds", duration)
	}
	return metadata
}

// Python str of a value, empty for a falsy one
func truthyString(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Stateless adapter whose session store takes delegation delivery rows
type DelegationDeliveryStore interface {
	EnsureSessionDB(ctx context.Context) (*state.SessionDB, error)
}

// Persists an async delegation completion as a durable delivery row without running a turn
func PersistDelegationDelivery(ctx context.Context, adapter PlatformAdapter, text, sessionID string, evt map[string]any) error {
	if sessionID == "" {
		return &PyError{Kind: "ValueError", Msg: "persist_delegation_delivery: raw session id required to persist the completion on the api_server session transcript"}
	}
	var db *state.SessionDB
	if ensure, ok := adapter.(DelegationDeliveryStore); ok {
		opened, err := ensure.EnsureSessionDB(ctx)
		if err != nil {
			return err
		}
		db = opened
	}
	if db == nil {
		return fmt.Errorf("persist_delegation_delivery: api_server SessionDB unavailable — cannot persist completion for session %s", sessionID)
	}
	if resolved, err := db.ResolveResumeSessionID(sessionID); err != nil {
		wakeLog().Debug(fmt.Sprintf("delegation delivery continuation resolve failed for %s", sessionID), "error", err)
	} else if resolved != "" {
		sessionID = resolved
	}
	if evt == nil {
		evt = map[string]any{}
	}
	if _, err := db.AppendDelegationDelivery(ctx, sessionID, text, delegationDisplayMetadata(ctx, evt)); err != nil {
		return err
	}
	wakeLog().Info(fmt.Sprintf("async delegation completion persisted as delivery row for api_server session %s (no wake turn)", sessionID))
	return nil
}

// Stateless adapter that runs a served profile's session turn in process
type InternalSessionTurnRunner interface {
	RunInternalSessionTurn(ctx context.Context, sessionID, text, profile, notificationCategory string) error
}

// Bind address, key and model name a stateless adapter's own listener answers on
type SelfPostEndpoint interface {
	SelfPostTarget() (host string, port int, apiKey, modelName string)
}

// Posts the wake text to the in pod API server as a session turn, in process for a served profile
func selfPostChatCompletion(ctx context.Context, adapter PlatformAdapter, text, sessionID, category, profile string) error {
	if profile != "" && profile != "default" {
		runner, ok := adapter.(InternalSessionTurnRunner)
		if !ok {
			return fmt.Errorf("wake self-post for served profile %s requires in-process session delivery; refusing to self-post as the default profile", pyrepr.Repr(profile))
		}
		return runner.RunInternalSessionTurn(ctx, sessionID, text, profile, category)
	}
	var host, apiKey, modelName string
	var port int
	if endpoint, ok := adapter.(SelfPostEndpoint); ok {
		host, port, apiKey, modelName = endpoint.SelfPostTarget()
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if host == "0.0.0.0" || host == "::" || host == "*" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 8642
	}
	if apiKey == "" {
		return errors.New("wake self-post requires API_SERVER_KEY: session continuation via X-Hermes-Session-Id is rejected (403) on an unauthenticated API server, so the wake cannot reach the target session")
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if modelName == "" {
		modelName = "icebath-agent"
	}
	url := "http://" + host + ":" + strconv.Itoa(port) + "/v1/chat/completions"
	payload := map[string]any{"model": modelName, "messages": []any{map[string]any{"role": "user", "content": text}}, "stream": false}
	if category == "diagnostic" {
		payload["hermes_notification_category"] = "diagnostic"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Duration(WakeTurnTimeoutSeconds * float64(time.Second))}
	attempts := 1 + len(wakeRetryDelays)
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wakeRetryDelays[attempt-1]):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-Icebath-Session-Id", sessionID)
		resp, err := client.Do(req)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) || isWakeTransportError(err) {
				lastErr = err
				wakeLog().Warn(fmt.Sprintf("wake self-post transient failure for session %s (attempt %d/%d): %s", sessionID, attempt+1, attempts, err))
				continue
			}
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("wake self-post got HTTP 429 (concurrency cap) for session %s", sessionID)
			wakeLog().Warn(fmt.Sprintf("%s; attempt %d/%d", lastErr, attempt+1, attempts))
			continue
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return fmt.Errorf("wake self-post failed for session %s: HTTP %d: %s", sessionID, resp.StatusCode, pystr.Slice(string(raw), 0, 300))
		}
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			wakeLog().Warn(fmt.Sprintf("wake self-post transient failure for session %s (attempt %d/%d): %s", sessionID, attempt+1, attempts, err))
			continue
		}
		wakeLog().Info(fmt.Sprintf("wake self-post delivered for session %s (attempt %d)", sessionID, attempt+1))
		return nil
	}
	return fmt.Errorf("wake self-post gave up for session %s after %d attempts: %w", sessionID, attempts, lastErr)
}

// Whether a client error is a connection or OS level failure aiohttp reports as ClientError or OSError
func isWakeTransportError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
