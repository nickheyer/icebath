package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	maxMessageLength       = 8000
	maxAttachmentBytes     = 100 * 1024 * 1024
	defaultHTTPURL         = "http://127.0.0.1:8080"
	sseRetryInitial        = 2 * time.Second
	sseRetryMax            = 60 * time.Second
	healthCheckInterval    = 30 * time.Second
	healthStaleThreshold   = 120 * time.Second
	recentSentCapacity     = 512
	recentSentTTL          = 300 * time.Second
	sentMessagesCapacity   = 500
	unresolvedRecipientTTL = time.Minute
)

// Signal adapter over a signal-cli daemon, SSE for inbound and JSON-RPC for outbound
type SignalAdapter struct {
	*gateway.BasePlatformAdapter

	httpURL        string
	account        string
	ignoreStories  bool
	requireMention bool
	groupAllow     map[string]bool
	dmAllow        map[string]bool

	mu           sync.Mutex
	rpc          *rpcClient
	stop         context.CancelFunc
	loops        sync.WaitGroup
	streamCancel context.CancelFunc
	lastEventID  string

	epoch        time.Time
	lastActivity atomic.Int64

	recipientMu  sync.Mutex
	uuidByNumber map[string]string
	numberByUUID map[string]string
	unresolved   map[string]time.Time
	resolveMu    sync.Mutex

	sentMu       sync.Mutex
	recentSent   *timestampLRU
	sentMessages *timestampLRU

	typingMu sync.Mutex
	typing   map[string]*typingBackoff
}

// Consecutive typing failures of a chat and the time typing RPCs resume
type typingBackoff struct {
	failures  int
	skipUntil time.Time
}

// Class traits of the Signal adapter over the base defaults
func adapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	traits.SplitsLongMessages = true
	traits.SerialReceiveLoop = true
	traits.SupportsMessageEditing = false
	return traits
}

// Items of a comma separated list with blanks dropped
func commaSet(value string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out[part] = true
		}
	}
	return out
}

// Text of a config extra value, def when absent
func extraString(extra map[string]any, key, def string) string {
	switch v := extra[key].(type) {
	case nil:
		return def
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// Boolean of a config extra value, strings read as truthy tokens and def when absent
func extraBool(extra map[string]any, key string, def bool) bool {
	switch v := extra[key].(type) {
	case nil:
		return def
	case bool:
		return v
	case string:
		return truthyToken(v)
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	default:
		return true
	}
}

// Whether an env style token reads as true
func truthyToken(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Adapter reading the daemon URL and account from extra and the allowlists from the profile scoped env
func NewSignalAdapter(ctx context.Context, config *gateway.PlatformConfig) (*SignalAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &SignalAdapter{epoch: time.Now(), uuidByNumber: map[string]string{}, numberByUUID: map[string]string{},
		unresolved: map[string]time.Time{}, recentSent: newTimestampLRU(recentSentCapacity, recentSentTTL),
		sentMessages: newTimestampLRU(sentMessagesCapacity, 0), typing: map[string]*typingBackoff{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformSignal, a)
	a.Traits = adapterTraits()
	extra := config.Extra
	a.httpURL = strings.TrimRight(extraString(extra, "http_url", defaultHTTPURL), "/")
	a.account = strings.TrimSpace(extraString(extra, "account", ""))
	a.ignoreStories = extraBool(extra, "ignore_stories", true)
	read := func(name, def string) (string, error) { return gateway.GetScopedSecret(ctx, name, def, false) }
	groups, err := read("SIGNAL_GROUP_ALLOWED_USERS", "")
	if err != nil {
		return nil, err
	}
	a.groupAllow = commaSet(groups)
	if _, set := extra["require_mention"]; set && extra["require_mention"] != nil {
		a.requireMention = extraBool(extra, "require_mention", false)
	} else {
		rm, err := read("SIGNAL_REQUIRE_MENTION", "false")
		if err != nil {
			return nil, err
		}
		a.requireMention = truthyToken(rm)
	}
	dms, err := read("SIGNAL_ALLOWED_USERS", "*")
	if err != nil {
		return nil, err
	}
	a.dmAllow = commaSet(dms)
	groupsState := "disabled"
	if len(a.groupAllow) > 0 {
		groupsState = "enabled"
	}
	slog.Info(fmt.Sprintf("Signal adapter initialized: url=%s account=%s groups=%s", a.httpURL, gateway.RedactPhone(a.account), groupsState))
	return a, nil
}

// Message length in UTF-16 code units, the unit Signal limits and the splitter counts
func (a *SignalAdapter) MessageLenFn() func(string) int { return utf16Units }

// Whether the adapter chunks long messages itself
func (a *SignalAdapter) SplitsLongMessages() bool { return a.Traits.SplitsLongMessages }

// Live RPC client, nil while disconnected
func (a *SignalAdapter) client() *rpcClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rpc
}

// Records receive activity for the health monitor
func (a *SignalAdapter) touch() { a.lastActivity.Store(int64(time.Since(a.epoch))) }

// Time since the last receive activity
func (a *SignalAdapter) idle() time.Duration {
	return time.Since(a.epoch) - time.Duration(a.lastActivity.Load())
}

// Takes the phone lock, probes the daemon and starts the SSE listener and health monitor
func (a *SignalAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.httpURL == "" || a.account == "" {
		slog.Error("Signal: SIGNAL_HTTP_URL and SIGNAL_ACCOUNT are required")
		return false, nil
	}
	a.stopLoops()
	locked, err := a.AcquirePlatformLock(ctx, "signal-phone", a.account, "Signal account")
	switch {
	case err != nil:
		slog.Warn(fmt.Sprintf("Signal: Could not acquire phone lock (non-fatal): %v", err))
	case !locked:
		return false, nil
	}
	c := &rpcClient{baseURL: a.httpURL, account: a.account, http: gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(), false)}
	if err := c.check(ctx); err != nil {
		var status *checkStatusError
		if errors.As(err, &status) {
			slog.Error(fmt.Sprintf("Signal: health check failed (status %d)", status.status))
		} else {
			slog.Error(fmt.Sprintf("Signal: cannot reach signal-cli at %s: %v", a.httpURL, err))
		}
		c.http.CloseIdleConnections()
		a.ReleasePlatformLock()
		return false, nil
	}
	loopCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	a.mu.Lock()
	a.rpc, a.stop = c, stop
	a.mu.Unlock()
	a.SetRunning(true)
	a.touch()
	a.loops.Add(2)
	go func() {
		defer a.loops.Done()
		a.sseListener(loopCtx)
	}()
	go func() {
		defer a.loops.Done()
		a.healthMonitor(loopCtx)
	}()
	slog.Info("Signal: connected to " + a.httpURL)
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Cancels the listener and monitor and waits for both to exit
func (a *SignalAdapter) stopLoops() {
	a.mu.Lock()
	stop := a.stop
	a.stop = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	a.loops.Wait()
}

// Stops the listener, closes the client and releases the phone lock
func (a *SignalAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.stopLoops()
	a.mu.Lock()
	c := a.rpc
	a.rpc = nil
	a.mu.Unlock()
	if c != nil {
		c.http.CloseIdleConnections()
	}
	a.typingMu.Lock()
	clear(a.typing)
	a.typingMu.Unlock()
	a.ReleasePlatformLock()
	slog.Info("Signal: disconnected")
	return nil
}

// Reads the SSE stream, reconnecting with jittered exponential backoff until stopped
func (a *SignalAdapter) sseListener(ctx context.Context) {
	backoff := sseRetryInitial
	for ctx.Err() == nil {
		connected, err := a.streamEvents(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = sseRetryInitial
		}
		delay := backoff + time.Duration(rand.Float64()*0.2*float64(backoff))
		if err != nil {
			slog.Warn(fmt.Sprintf("Signal SSE: error: %v (reconnecting in %.0fs)", err, delay.Seconds()))
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		backoff = min(backoff*2, sseRetryMax)
	}
}

// One SSE connection resuming after the last seen event id, connected true once the stream opened
func (a *SignalAdapter) streamEvents(ctx context.Context) (connected bool, err error) {
	c := a.client()
	if c == nil {
		return false, errors.New("client not connected")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.streamCancel = cancel
	lastID := a.lastEventID
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.streamCancel = nil
		a.mu.Unlock()
	}()
	eventsURL := a.httpURL + "/api/v1/events?account=" + url.QueryEscape(a.account)
	slog.Debug("Signal SSE: connecting to " + eventsURL)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("events endpoint answered HTTP %d", resp.StatusCode)
	}
	a.touch()
	slog.Info("Signal SSE: connected")
	reader := bufio.NewReader(resp.Body)
	var data []string
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" || readErr == nil {
			a.touch()
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 && readErr == nil {
				a.dispatchEvent(ctx, strings.Join(data, "\n"))
			}
			data = data[:0]
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
			case "id":
				if !strings.ContainsRune(value, 0) {
					a.mu.Lock()
					a.lastEventID = value
					a.mu.Unlock()
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return true, nil
			}
			return true, readErr
		}
	}
}

// Decodes one event payload and handles its envelope, a failure logged without stopping the stream
func (a *SignalAdapter) dispatchEvent(ctx context.Context, payload string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("Signal SSE: error handling event: %v", r))
		}
	}()
	env, err := decodeEnvelope([]byte(payload))
	if err != nil {
		slog.Debug(fmt.Sprintf("Signal SSE: invalid JSON: %.100s", payload))
		return
	}
	if err := a.handleEnvelope(ctx, env); err != nil && ctx.Err() == nil {
		slog.Error(fmt.Sprintf("Signal SSE: error handling event: %v", err))
	}
}

// Probes the daemon when the stream goes quiet and forces a reconnect when it does not answer
func (a *SignalAdapter) healthMonitor(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		idle := a.idle()
		if idle <= healthStaleThreshold {
			continue
		}
		slog.Warn(fmt.Sprintf("Signal: SSE idle for %.0fs, checking daemon health", idle.Seconds()))
		c := a.client()
		if c == nil {
			continue
		}
		err := c.check(ctx)
		var status *checkStatusError
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			a.touch()
			slog.Debug("Signal: daemon healthy, SSE idle")
		case errors.As(err, &status):
			slog.Warn(fmt.Sprintf("Signal: health check failed (%d), forcing reconnect", status.status))
			a.forceReconnect()
		default:
			slog.Warn(fmt.Sprintf("Signal: health check error: %v, forcing reconnect", err))
			a.forceReconnect()
		}
	}
}

// Closes the current SSE connection so the listener reconnects
func (a *SignalAdapter) forceReconnect() {
	a.mu.Lock()
	cancel := a.streamCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Bounded insertion ordered set of message timestamps with an optional time to live
type timestampLRU struct {
	capacity int
	ttl      time.Duration
	order    []int64
	added    map[int64]time.Time
}

func newTimestampLRU(capacity int, ttl time.Duration) *timestampLRU {
	return &timestampLRU{capacity: capacity, ttl: ttl, added: map[int64]time.Time{}}
}

// Inserts or refreshes ts as newest, evicting expired entries then the oldest beyond capacity
func (l *timestampLRU) add(ts int64, now time.Time) {
	if _, ok := l.added[ts]; ok {
		l.remove(ts)
	}
	l.added[ts] = now
	l.order = append(l.order, ts)
	for len(l.order) > 0 && l.ttl > 0 && now.Sub(l.added[l.order[0]]) > l.ttl {
		delete(l.added, l.order[0])
		l.order = l.order[1:]
	}
	for len(l.order) > l.capacity {
		delete(l.added, l.order[0])
		l.order = l.order[1:]
	}
}

// Drops ts from the order and the index
func (l *timestampLRU) remove(ts int64) {
	delete(l.added, ts)
	for i, v := range l.order {
		if v == ts {
			l.order = append(l.order[:i], l.order[i+1:]...)
			return
		}
	}
}

// Whether ts is held and unexpired
func (l *timestampLRU) has(ts int64, now time.Time) bool {
	at, ok := l.added[ts]
	return ok && (l.ttl <= 0 || now.Sub(at) <= l.ttl)
}

// Removes ts and reports whether it was held and unexpired
func (l *timestampLRU) pop(ts int64, now time.Time) bool {
	held := l.has(ts, now)
	if _, ok := l.added[ts]; ok {
		l.remove(ts)
	}
	return held
}

// Records an outbound timestamp for echo filtering and quote matching
func (a *SignalAdapter) trackSent(ts int64) {
	if ts == 0 {
		return
	}
	now := time.Now()
	a.sentMu.Lock()
	a.sentMessages.add(ts, now)
	a.recentSent.add(ts, now)
	a.sentMu.Unlock()
}

// Pops a timestamp this adapter sent, true for an echo of its own outbound message
func (a *SignalAdapter) consumeSent(ts int64) bool {
	if ts == 0 {
		return false
	}
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	return a.recentSent.pop(ts, time.Now())
}

// Whether ts is an outbound message of this adapter
func (a *SignalAdapter) isSentMessage(ts int64) bool {
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	return a.sentMessages.has(ts, time.Now())
}

// Raw JSON of a result for send results and logs
func rawJSON(raw json.RawMessage) any {
	var v any
	if present(raw) && json.Unmarshal(raw, &v) == nil {
		return v
	}
	return nil
}
