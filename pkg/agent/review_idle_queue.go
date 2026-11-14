package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	reviewIdleSettleSeconds    = 15.0
	reviewPollInterval         = 5 * time.Second
	reviewMaxAgeDefaultSeconds = 30.0 * 60.0
)

// auto by default or never from auxiliary.background_review.defer
func reviewDeferMode(taskCfg map[string]any) string {
	raw := "auto"
	if v, ok := taskCfg["defer"]; ok {
		raw = pyrepr.Str(v)
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "auto" || raw == "never" {
		return raw
	}
	return "auto"
}

// Seconds a deferred review may wait before dispatching regardless of idleness
func reviewDeferMaxAgeS(taskCfg map[string]any) float64 {
	v, ok := taskCfg["defer_max_age_s"]
	if !ok {
		return reviewMaxAgeDefaultSeconds
	}
	value, valid := pyFloatCoerce(v)
	if !valid || !(value > 0) {
		return reviewMaxAgeDefaultSeconds
	}
	return value
}

// Python float() over a number, a bool or a numeric string, false where float() raises
func pyFloatCoerce(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		switch strings.TrimLeft(s, "+-") {
		case "inf", "infinity", "nan":
		default:
			if strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "x") {
				return 0, false
			}
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
		return f, err == nil
	}
	return 0, false
}

// Whether the review fork would decode on the managed llama-server, false on any failure
var reviewTargetsManagedLocal = func(ctx context.Context, a *AIAgent, taskCfg map[string]any) (managed bool) {
	defer func() {
		if recover() != nil {
			managed = false
		}
	}()
	if provider.ManagedLocalNetloc() == "" {
		return false
	}
	runtime := resolveReviewRuntime(ctx, a, taskCfg)
	return provider.IsManagedLocalEndpoint(runtime.BaseURL)
}

// Agent the idle queue dispatches a deferred review to
type deferredReviewSpawner interface {
	spawnBackgroundReviewNow(ctx context.Context, kw backgroundReviewKwargs)
}

// One session's queued review with the context it was enqueued under
type pendingReview struct {
	agent      deferredReviewSpawner
	sessionKey string
	kwargs     backgroundReviewKwargs
	enqueuedAt float64
	ctx        context.Context
}

// Wake signal of the dispatcher goroutine
type reviewWake interface {
	Wait()
	Set()
	Clear()
}

// Event backed wake that waits without a timeout
type eventWake struct{ ev pythread.Event }

func (w *eventWake) Wait()  { w.ev.Wait(-1) }
func (w *eventWake) Set()   { w.ev.Set() }
func (w *eventWake) Clear() { w.ev.Clear() }

// Session coalescing queue with an idle gated dispatcher goroutine
type reviewIdleQueue struct {
	mu           sync.Mutex
	pending      map[string]*pendingReview
	order        []string
	wake         reviewWake
	running      bool
	liveTurns    int
	quietSince   *float64
	now          func() float64
	serverIdle   func() bool
	ensureThread func()
}

// Empty queue on the monotonic clock probing the managed server for idleness
func newReviewIdleQueue() *reviewIdleQueue {
	q := &reviewIdleQueue{pending: map[string]*pendingReview{}, wake: &eventWake{}, now: pythread.Monotonic, serverIdle: managedServerIdle}
	q.ensureThread = q.startDispatcher
	return q
}

// Counts a live turn and stops the quiet clock
func (q *reviewIdleQueue) noteTurnStarted() {
	q.mu.Lock()
	q.liveTurns++
	q.quietSince = nil
	q.mu.Unlock()
}

// Ends a live turn, starting the quiet clock when none remain, and wakes the dispatcher
func (q *reviewIdleQueue) noteTurnFinished() {
	q.mu.Lock()
	q.liveTurns = max(0, q.liveTurns-1)
	if q.liveTurns == 0 {
		now := q.now()
		q.quietSince = &now
	}
	q.mu.Unlock()
	q.wake.Set()
}

// Adds or replaces a session's pending review, keeping the original enqueue time on coalesce
func (q *reviewIdleQueue) enqueue(ctx context.Context, agent deferredReviewSpawner, sessionKey string, kw backgroundReviewKwargs) {
	q.mu.Lock()
	enqueuedAt := q.now()
	if existing, ok := q.pending[sessionKey]; ok {
		enqueuedAt = existing.enqueuedAt
	} else {
		q.order = append(q.order, sessionKey)
	}
	q.pending[sessionKey] = &pendingReview{agent: agent, sessionKey: sessionKey, kwargs: kw, enqueuedAt: enqueuedAt, ctx: context.WithoutCancel(ctx)}
	queued := len(q.pending)
	q.mu.Unlock()
	q.ensureThread()
	q.wake.Set()
	runAgentLog().Info(fmt.Sprintf("Background review deferred (session=%s, queued=%d)", lastRunes(sessionKey, 12), queued))
}

// Number of sessions with a pending review
func (q *reviewIdleQueue) pendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// Starts the dispatcher goroutine unless one is running
func (q *reviewIdleQueue) startDispatcher() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running {
		return
	}
	q.running = true
	go func() {
		defer func() {
			q.mu.Lock()
			q.running = false
			q.mu.Unlock()
		}()
		q.run()
	}()
}

// Seconds since the last live turn ended with the stamp it began at, zero and nil while a turn runs
func (q *reviewIdleQueue) quietPeriod() (float64, *float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.liveTurns > 0 || q.quietSince == nil {
		return 0, nil
	}
	since := *q.quietSince
	return q.now() - since, &since
}

// Seconds this process has been turn free
func (q *reviewIdleQueue) quietFor() float64 {
	quiet, _ := q.quietPeriod()
	return quiet
}

// Oldest pending review among those holding the minimum enqueue time
func oldestPendingReview(items []*pendingReview) *pendingReview {
	var best *pendingReview
	for _, p := range items {
		if best == nil || p.enqueuedAt < best.enqueuedAt {
			best = p
		}
	}
	return best
}

// Oldest aged out item, else the oldest item once the process is quiet and the server idle
func (q *reviewIdleQueue) popDispatchable() *pendingReview {
	q.mu.Lock()
	if len(q.pending) == 0 {
		q.mu.Unlock()
		return nil
	}
	now := q.now()
	var aged []*pendingReview
	for _, key := range q.pendingOrder() {
		p := q.pending[key]
		if now-p.enqueuedAt >= reviewDeferMaxAgeS(p.kwargs.taskCfg) {
			aged = append(aged, p)
		}
	}
	candidate := oldestPendingReview(aged)
	q.mu.Unlock()
	if candidate == nil && (q.quietFor() < reviewIdleSettleSeconds || !q.serverIdle()) {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if candidate == nil {
		if len(q.pending) == 0 {
			return nil
		}
		items := make([]*pendingReview, 0, len(q.pending))
		for _, key := range q.pendingOrder() {
			items = append(items, q.pending[key])
		}
		candidate = oldestPendingReview(items)
	}
	item, ok := q.pending[candidate.sessionKey]
	if !ok {
		return nil
	}
	q.removePending(candidate.sessionKey)
	return item
}

// Pending session keys in first insertion order like the hermes dict
func (q *reviewIdleQueue) pendingOrder() []string {
	return append([]string(nil), q.order...)
}

// Drops a session key from the pending map and its insertion order
func (q *reviewIdleQueue) removePending(key string) {
	delete(q.pending, key)
	for i, k := range q.order {
		if k == key {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

// Dispatcher loop that parks while empty and polls while items wait for quiet
func (q *reviewIdleQueue) run() {
	for {
		q.wake.Wait()
		q.mu.Lock()
		empty := len(q.pending) == 0
		if empty {
			q.wake.Clear()
		}
		q.mu.Unlock()
		if empty {
			continue
		}
		var item *pendingReview
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Warn(fmt.Sprintf("Deferred review dispatch failed: %v", r))
				}
			}()
			item = q.popDispatchable()
			if item != nil {
				q.dispatch(item)
			}
		}()
		if item == nil {
			time.Sleep(reviewPollInterval)
		}
	}
}

// Spawns the review under its enqueue context unless reviews were disabled while it waited
func (q *reviewIdleQueue) dispatch(item *pendingReview) {
	if !reviewStillEnabled(item) {
		runAgentLog().Info(fmt.Sprintf("Deferred background review dropped: reviews were disabled while it was queued (session=%s)",
			lastRunes(item.sessionKey, 12)))
		return
	}
	runAgentLog().Info(fmt.Sprintf("Dispatching deferred background review (session=%s, waited=%.0fs, queued=%d)",
		lastRunes(item.sessionKey, 12), q.now()-item.enqueuedAt, q.pendingCount()))
	item.agent.spawnBackgroundReviewNow(item.ctx, item.kwargs)
}

// Enabled gate read again at dispatch under the enqueue context, true when the read fails
func reviewStillEnabled(item *pendingReview) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = true
		}
	}()
	enabled, _ = loadBackgroundReviewSettings(item.ctx)
	return enabled
}

// Last n code points of s
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

var managedIdleHTTPClient = &http.Client{Timeout: 3 * time.Second}

// No processing slot on any loaded model of the managed router, an unreachable server or missing state reading idle
func managedServerIdle() (idle bool) {
	defer func() {
		if recover() != nil {
			idle = true
		}
	}()
	raw, err := os.ReadFile(provider.LocalRuntimeStatePath())
	if err != nil {
		return true
	}
	var doc map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &doc) != nil || doc == nil {
		return true
	}
	baseURL := ""
	if v, ok := doc["base_url"]; ok {
		baseURL = pyrepr.Str(v)
	}
	base := baseURL
	if i := strings.LastIndex(baseURL, "/v1"); i >= 0 {
		base = baseURL[:i]
	}
	apiKey := ""
	if v, ok := doc["api_key"]; ok {
		apiKey = pyrepr.Str(v)
	}
	if base == "" {
		return true
	}
	get := func(path string) (any, error) {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := managedIdleHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP Error %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var out any
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	models, err := get("/models")
	if err != nil {
		return true
	}
	data, ok := models.(map[string]any)["data"]
	if !ok {
		data = []any{}
	}
	var loaded []string
	for _, m := range pyIterItems(data) {
		entry := m.(map[string]any)
		var value any
		if st := entry["status"]; pyTruthy(st) {
			value = st.(map[string]any)["value"]
		}
		if value == "loaded" || value == "ready" {
			loaded = append(loaded, entry["id"].(string))
		}
	}
	for _, mid := range loaded {
		slots, err := get("/slots?model=" + pyurl.Quote(mid, "/") + "&autoload=false")
		if err != nil {
			return true
		}
		for _, s := range pyIterItems(slots) {
			if slot, ok := s.(map[string]any); ok && pyTruthy(slot["is_processing"]) {
				return false
			}
		}
	}
	return true
}

// Process wide idle queue of deferred background reviews
var reviewQueue = newReviewIdleQueue()

// Queues an automatic managed local review for dispatch once the machine is quiet
var enqueueIdleReview = func(ctx context.Context, a *AIAgent, key string, kw backgroundReviewKwargs) {
	reviewQueue.enqueue(ctx, a, key, kw)
}

// Marks a live turn started on the idle queue clock
func noteReviewTurnStarted() { reviewQueue.noteTurnStarted() }

// Marks a live turn finished on the idle queue clock
func noteReviewTurnFinished() { reviewQueue.noteTurnFinished() }

// Items iterating a decoded JSON value yields in Python, panicking where iteration raises
func pyIterItems(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		out := make([]any, 0, len(x))
		for k := range x {
			out = append(out, k)
		}
		return out
	case string:
		out := make([]any, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	}
	panic(fmt.Sprintf("'%T' object is not iterable", v))
}
