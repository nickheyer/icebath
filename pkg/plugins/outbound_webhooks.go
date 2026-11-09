package plugins

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const (
	OutboundWebhookDefaultTimeoutSeconds = 10
	OutboundWebhookMaxTimeoutSeconds     = 60
	OutboundWebhookMaxDeliveryAttempts   = 2
	outboundWebhookRetryBackoff          = time.Second
	OutboundWebhookQueueMaxSize          = 256
)

type outboundWebhookKey struct{ home, event, url string }

// Delivery queue, worker and idempotence keys of agent/outbound_webhooks.py, one set per process
var (
	outboundRegistered   = map[outboundWebhookKey]bool{}
	outboundRegisteredMu sync.Mutex
	outboundQueue        = make(chan *outboundDelivery, OutboundWebhookQueueMaxSize)
	outboundUnfinished   sync.WaitGroup
	outboundPending      int
	outboundPendingMu    sync.Mutex
	outboundWorkerMu     sync.Mutex
	outboundWorkerUp     bool
	outboundSleep        = time.Sleep
	outboundNow          = time.Now
)

// One hooks.outbound entry parsed and validated, the matcher regex compiled for the tool events
type OutboundWebhookTarget struct {
	URL      string
	Events   []string
	Name     string
	Secret   string
	Matcher  string
	Timeout  int
	compiled *regexp2.Regexp
}

// Label is the configured name, the url when there is none
func (t *OutboundWebhookTarget) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.URL
}

// NewOutboundWebhookTarget builds a target the way the dataclass does, the matcher stripped and compiled or kept literal
func NewOutboundWebhookTarget(url string, events []string, name, secret, matcher string, timeout int) *OutboundWebhookTarget {
	t := &OutboundWebhookTarget{URL: url, Events: events, Name: name, Secret: secret, Matcher: strings.TrimSpace(matcher), Timeout: timeout}
	if t.Matcher != "" {
		re, err := pyre.CompileFullmatch(t.Matcher)
		if err != nil {
			logWarn(fmt.Sprintf("outbound webhook matcher %s is invalid (%v) — treating as literal equality", pyrepr.StrRepr(t.Matcher), err))
		}
		t.compiled = re
	}
	return t
}

// MatchesTool reports whether the tool name passes the matcher, every name without one and never a missing name
func (t *OutboundWebhookTarget) MatchesTool(toolName any) bool {
	if t.Matcher == "" {
		return true
	}
	if toolName == nil {
		return false
	}
	name, isStr := toolName.(string)
	if t.compiled == nil {
		return isStr && name == t.Matcher
	}
	if !isStr {
		return false
	}
	ok, _ := t.compiled.MatchString(name)
	return ok
}

// RegisterOutboundWebhooksFromConfig wires every configured target once per home and event, the targets newly wired
func RegisterOutboundWebhooksFromConfig(ctx context.Context, cfg any) ([]*OutboundWebhookTarget, error) {
	if config.SafeWorkerEnabled() {
		return nil, nil
	}
	if _, ok := cfg.(map[string]any); !ok {
		return nil, nil
	}
	if config.EnvVarEnabled("ICEBATH_SAFE_MODE") {
		logInfo("ICEBATH_SAFE_MODE=1 — outbound webhook registration skipped")
		return nil, nil
	}
	targets, err := IterConfiguredOutboundWebhooks(cfg)
	if err != nil || len(targets) == 0 {
		return nil, err
	}
	manager := GetPluginManager(ctx)
	homeKey := shellHomeKey(ctx)
	var registered []*OutboundWebhookTarget
	outboundRegisteredMu.Lock()
	defer outboundRegisteredMu.Unlock()
	for _, target := range targets {
		wiredAny := false
		for _, event := range target.Events {
			key := outboundWebhookKey{homeKey, event, target.URL}
			if outboundRegistered[key] {
				continue
			}
			entry := &hookEntry{fn: makeOutboundWebhookCallback(event, target), name: fmt.Sprintf("outbound_webhook[%s:%s]", event, target.Label()),
				module: "agent.outbound_webhooks"}
			manager.mu.Lock()
			manager.hooks[event] = append(manager.hooks[event], entry)
			manager.mu.Unlock()
			outboundRegistered[key] = true
			wiredAny = true
			matcher := "None"
			if target.Matcher != "" {
				matcher = target.Matcher
			}
			logInfo(fmt.Sprintf("outbound webhook registered: %s -> %s (matcher=%s, timeout=%ds)", event, target.Label(), matcher, target.Timeout))
		}
		if wiredAny {
			registered = append(registered, target)
		}
	}
	return registered, nil
}

// IterConfiguredOutboundWebhooks parses hooks.outbound without registering anything, the error an unscoped secret read
func IterConfiguredOutboundWebhooks(cfg any) ([]*OutboundWebhookTarget, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, nil
	}
	var raw any
	if hooks, ok := m["hooks"].(map[string]any); ok {
		raw = hooks["outbound"]
	}
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		logWarn(fmt.Sprintf("hooks.outbound must be a list of webhook targets; got %s", pyrepr.TypeName(raw)))
		return nil, nil
	}
	var out []*OutboundWebhookTarget
	for i, entry := range list {
		t, err := parseOutboundWebhookTarget(i, entry)
		if err != nil {
			return nil, err
		}
		if t != nil {
			out = append(out, t)
		}
	}
	return out, nil
}

// FlushOutboundWebhooks blocks until every queued delivery finished or the timeout passed, true when drained
func FlushOutboundWebhooks(timeout time.Duration) bool {
	return pydeadline.PollUntil(func() (bool, bool) {
		outboundPendingMu.Lock()
		drained := outboundPending == 0
		outboundPendingMu.Unlock()
		return drained, drained
	}, timeout, 20*time.Millisecond)
}

// ReRegisterOutboundWebhooks drops the current home's wired keys and registers the saved config again after a force reload
func ReRegisterOutboundWebhooks(ctx context.Context) error {
	h := shellHomeKey(ctx)
	outboundRegisteredMu.Lock()
	for k := range outboundRegistered {
		if k.home == h {
			delete(outboundRegistered, k)
		}
	}
	outboundRegisteredMu.Unlock()
	cfg, err := loadConfig(home.Dir(ctx))
	if err != nil {
		return err
	}
	_, err = RegisterOutboundWebhooksFromConfig(ctx, cfg)
	return err
}

// ResetOutboundWebhooksForTests clears the idempotence set and drains the queue
func ResetOutboundWebhooksForTests() {
	outboundRegisteredMu.Lock()
	outboundRegistered = map[outboundWebhookKey]bool{}
	outboundRegisteredMu.Unlock()
	for {
		select {
		case <-outboundQueue:
			outboundTaskDone()
		default:
			return
		}
	}
}

func outboundTaskDone() {
	outboundPendingMu.Lock()
	outboundPending--
	outboundPendingMu.Unlock()
	outboundUnfinished.Done()
}

// One hooks.outbound[index] entry validated field by field, nil with a warning for a malformed one
func parseOutboundWebhookTarget(index int, raw any) (*OutboundWebhookTarget, error) {
	warn := func(format string, args ...any) {
		logWarn(fmt.Sprintf("hooks.outbound[%d]"+format, append([]any{index}, args...)...))
	}
	entry, ok := raw.(map[string]any)
	if !ok {
		warn(" must be a mapping with 'url' and 'events' keys; got %s", pyrepr.TypeName(raw))
		return nil, nil
	}
	url, isStr := entry["url"].(string)
	if !isStr || strings.TrimSpace(url) == "" {
		warn(" is missing a non-empty 'url'")
		return nil, nil
	}
	url = strings.TrimSpace(url)
	lower := strings.ToLower(url)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		warn(".url must be http(s); got %s — skipped", pyrepr.Repr(url))
		return nil, nil
	}
	if strings.HasPrefix(lower, "http://") {
		warn(".url uses plain http:// — payloads (including tool inputs) travel unencrypted. Prefer https.")
	}
	valid := ValidHooks()
	validList := strings.Join(valid, ", ")
	eventsRaw, isList := entry["events"].([]any)
	if !isList || len(eventsRaw) == 0 {
		warn(" needs a non-empty 'events' list (valid: %s)", validList)
		return nil, nil
	}
	var events []string
	for _, ev := range eventsRaw {
		if name, ok := ev.(string); ok && validHooks[name] {
			events = append(events, name)
		}
	}
	for _, ev := range eventsRaw {
		if name, ok := ev.(string); !ok || !validHooks[name] {
			warn(": unknown event %s ignored (valid: %s)", pyrepr.Repr(ev), validList)
		}
	}
	if len(events) == 0 {
		warn(" has no valid events — skipped")
		return nil, nil
	}
	matcher := ""
	if raw, present := entry["matcher"]; present && raw != nil {
		if s, ok := raw.(string); ok {
			matcher = s
		} else {
			warn(".matcher must be a string regex; ignoring")
		}
	}
	if matcher != "" {
		toolScoped := false
		for _, ev := range events {
			if shellHookToolEvents[ev] {
				toolScoped = true
			}
		}
		if !toolScoped {
			warn(".matcher=%s will be ignored — matcher is only honored for pre_tool_call / post_tool_call.", pyrepr.Repr(matcher))
			matcher = ""
		}
	}
	timeout := OutboundWebhookDefaultTimeoutSeconds
	if rawTimeout, present := entry["timeout"]; present {
		if n, ok := pyIntOf(rawTimeout); ok {
			timeout = n
		} else {
			warn(".timeout must be an int (got %s); using default %ds", pyrepr.Repr(rawTimeout), OutboundWebhookDefaultTimeoutSeconds)
		}
	}
	name := ""
	if s, ok := entry["name"].(string); ok {
		name = strings.TrimSpace(s)
	}
	secret := ""
	if secretEnv, ok := entry["secret_env"].(string); ok && strings.TrimSpace(secretEnv) != "" {
		secretEnv = strings.TrimSpace(secretEnv)
		value, err := outboundSecret(secretEnv)
		if err != nil {
			return nil, err
		}
		secret = value
		if secret == "" {
			warn(".secret_env=%s is not set in the environment — deliveries will be UNSIGNED", pyrepr.Repr(secretEnv))
		}
	} else if s, ok := entry["secret"].(string); ok {
		secret = s
	}
	return NewOutboundWebhookTarget(url, events, name, secret, matcher, max(1, min(timeout, OutboundWebhookMaxTimeoutSeconds))), nil
}

// get_secret of the bound profile scope through the installed secret service, empty when unset
func outboundSecret(name string) (string, error) {
	get, err := needService("GetSecret", func(s *Services) func(context.Context, string) (string, bool, error) { return s.GetSecret })
	if err != nil {
		return "", err
	}
	value, _, err := get(context.Background(), name)
	return value, err
}

// Python int() of a config value, false when the value is not an int, a float or a digit string
func pyIntOf(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case float64:
		return int(n), true
	case string:
		value, ok := pystr.IntText(strings.TrimSpace(n))
		if !ok || !value.IsInt64() {
			return 0, false
		}
		return int(value.Int64()), true
	}
	return 0, false
}

// Notify only callback invoke_hook runs per firing, the delivery queued and never a directive
func makeOutboundWebhookCallback(event string, target *OutboundWebhookTarget) HookFunc {
	return func(ctx context.Context, kwargs map[string]any) (any, error) {
		if config.SafeWorkerEnabled() {
			return nil, nil
		}
		if shellHookToolEvents[event] && !target.MatchesTool(kwargs["tool_name"]) {
			return nil, nil
		}
		deliveryID := strings.ReplaceAll(uuid.New().String(), "-", "")
		body, err := serializeOutboundWebhookPayload(ctx, event, kwargs, deliveryID)
		if err != nil {
			logWarn(fmt.Sprintf("outbound webhook payload serialization failed (event=%s target=%s)", event, target.Label()), "error", err)
			return nil, nil
		}
		enqueueOutboundWebhook(buildOutboundDelivery(event, target, body, deliveryID))
		return nil, nil
	}
}

// POST body of one firing, the shell hooks stdin shape plus the delivery id and timestamp inside the signed bytes
func serializeOutboundWebhookPayload(ctx context.Context, event string, kwargs map[string]any, deliveryID string) ([]byte, error) {
	payload := pyjson.ObjectOf("hook_event_name", event, "profile", profiles.GetActiveProfileName(ctx))
	payload.Update(shellHookPayloadFields(ctx, kwargs))
	payload.Set("delivery_id", deliveryID)
	payload.Set("timestamp", shellHookISO(outboundNow()))
	text, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// One queued POST with its headers, the HMAC signature header present when the target has a secret
type outboundDelivery struct {
	URL, Label, Event string
	Body              []byte
	Headers           map[string]string
	Timeout           int
}

func buildOutboundDelivery(event string, target *OutboundWebhookTarget, body []byte, deliveryID string) *outboundDelivery {
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": "Icebath-Agent-Outbound-Webhook",
		"X-Hermes-Event": event, "X-Hermes-Delivery": deliveryID}
	if target.Secret != "" {
		mac := hmac.New(sha256.New, []byte(target.Secret))
		mac.Write(body)
		headers["X-Hermes-Signature-256"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return &outboundDelivery{URL: target.URL, Label: target.Label(), Event: event, Body: body, Headers: headers, Timeout: target.Timeout}
}

// Starts the daemon worker once and queues the delivery, dropping it with a warning when the queue is full
func enqueueOutboundWebhook(delivery *outboundDelivery) {
	outboundWorkerMu.Lock()
	if !outboundWorkerUp {
		outboundWorkerUp = true
		go outboundWorkerLoop()
	}
	outboundWorkerMu.Unlock()
	outboundPendingMu.Lock()
	outboundPending++
	outboundPendingMu.Unlock()
	outboundUnfinished.Add(1)
	select {
	case outboundQueue <- delivery:
	default:
		outboundTaskDone()
		logWarn(fmt.Sprintf("outbound webhook queue full (%d pending) — dropping %s event for %s", OutboundWebhookQueueMaxSize, delivery.Event, delivery.Label))
	}
}

func outboundWorkerLoop() {
	for delivery := range outboundQueue {
		func() {
			defer outboundTaskDone()
			defer func() {
				if r := recover(); r != nil {
					logWarn(fmt.Sprintf("outbound webhook delivery crashed (target=%s)", delivery.Label), "error", r)
				}
			}()
			deliverOutboundWebhook(delivery)
		}()
	}
}

// Transport that refuses redirects so a redirected POST never turns into a body less GET
var outboundWebhookClient = &http.Client{Transport: &http.Transport{Proxy: pyurl.UrlopenProxy},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// POST with bounded retries, connection errors and 5xx retried once, 4xx and 3xx final
func deliverOutboundWebhook(delivery *outboundDelivery) {
	lastError := ""
	for attempt := 1; attempt <= OutboundWebhookMaxDeliveryAttempts; attempt++ {
		status, location, err := postOutboundWebhook(delivery)
		switch {
		case err != nil:
			lastError = err.Error()
			if lastError == "" {
				lastError = fmt.Sprintf("%T", err)
			}
		case status >= 200 && status < 300:
			logDebug(fmt.Sprintf("outbound webhook delivered: %s -> %s (HTTP %d)", delivery.Event, delivery.Label, status))
			return
		case status >= 300 && status < 400:
			lastError = fmt.Sprintf("HTTP %d", status)
			if location == "" {
				location = "?"
			}
			logWarn(fmt.Sprintf("outbound webhook target redirected (event=%s target=%s): %s -> %s — redirects are not followed; update the configured url",
				delivery.Event, delivery.Label, lastError, location))
			return
		case status >= 400 && status < 500:
			lastError = fmt.Sprintf("HTTP %d", status)
			logWarn(fmt.Sprintf("outbound webhook rejected (event=%s target=%s): %s — not retrying", delivery.Event, delivery.Label, lastError))
			return
		default:
			lastError = fmt.Sprintf("HTTP %d", status)
		}
		if attempt < OutboundWebhookMaxDeliveryAttempts {
			outboundSleep(outboundWebhookRetryBackoff * time.Duration(attempt))
		}
	}
	logWarn(fmt.Sprintf("outbound webhook delivery failed after %d attempt(s) (event=%s target=%s): %s",
		OutboundWebhookMaxDeliveryAttempts, delivery.Event, delivery.Label, lastError))
}

// One POST attempt, the status and redirect location of the response or the transport error
func postOutboundWebhook(delivery *outboundDelivery) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(delivery.Timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.URL, bytes.NewReader(delivery.Body))
	if err != nil {
		return 0, "", err
	}
	for k, v := range delivery.Headers {
		req.Header.Set(k, v)
	}
	resp, err := outboundWebhookClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, "", errors.New("timed out")
		}
		return 0, "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), nil
}
