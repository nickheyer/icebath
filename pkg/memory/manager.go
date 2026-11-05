package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Default bound on one external provider prefetch
const externalPrefetchTimeout = 8 * time.Second

// Gate and drain bounds, variables so tests can shorten them
var (
	// Bound on the shutdown drain of queued provider work
	syncDrainTimeout = 5 * time.Second
	// A background provider call running longer than this counts as stuck
	backgroundCallStall = 30 * time.Second
	// New calls stop once this many calls started after the last success are overdue
	maxStuckCalls = 4
	// While the gate is closed one probe call goes through per interval
	probeInterval = 60 * time.Second
	// Hard cap on calls in flight per home and provider, orphans included
	maxInflightCalls = 16
)

// Clock the stuck call gates read, replaced in tests
var monotonicNow = time.Now

// Process wide gate key, a profile home and a provider name
type gateKey struct {
	home     string
	provider string
}

type gateCall struct {
	start    time.Time
	deadline time.Time
}

// Calls a closed gate skipped, the stuck and in flight counts and the running skip count
type gateSkip struct {
	stuck    int
	inFlight int
	skips    int
}

// Process wide admission for one kind of external provider call keyed by home and provider
type stuckCallGate struct {
	mu      sync.Mutex
	calls   map[gateKey]map[uint64]gateCall
	lastOK  map[gateKey]time.Time
	probeAt map[gateKey]time.Time
	skips   map[gateKey]int
}

func newStuckCallGate() *stuckCallGate {
	return &stuckCallGate{
		calls:   map[gateKey]map[uint64]gateCall{},
		lastOK:  map[gateKey]time.Time{},
		probeAt: map[gateKey]time.Time{},
		skips:   map[gateKey]int{},
	}
}

// Registers a call and returns nil, or the counts when the closed gate skips it
func (g *stuckCallGate) enter(key gateKey, token uint64, timeout time.Duration) *gateSkip {
	now := monotonicNow()
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls[key]
	if calls == nil {
		calls = map[uint64]gateCall{}
		g.calls[key] = calls
	}
	lastOK, hasOK := g.lastOK[key]
	stuck := 0
	for _, c := range calls {
		if (!hasOK || c.start.After(lastOK)) && now.After(c.deadline) {
			stuck++
		}
	}
	admit := false
	switch {
	case len(calls) >= maxInflightCalls:
	case stuck < maxStuckCalls:
		admit = true
		delete(g.probeAt, key)
		delete(g.skips, key)
	default:
		at, ok := g.probeAt[key]
		if !ok {
			at = now
			g.probeAt[key] = now
		}
		admit = now.Sub(at) >= probeInterval
		if admit {
			g.probeAt[key] = now
		}
	}
	if !admit {
		g.skips[key]++
		return &gateSkip{stuck: stuck, inFlight: len(calls), skips: g.skips[key]}
	}
	calls[token] = gateCall{start: now, deadline: now.Add(timeout)}
	return nil
}

// Removes only this call, recording a success while others are in flight and clearing the key when none remain
func (g *stuckCallGate) leave(key gateKey, token uint64, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls[key]
	if calls == nil {
		return
	}
	if _, found := calls[token]; !found {
		return
	}
	delete(calls, token)
	if len(calls) > 0 {
		if ok {
			g.lastOK[key] = monotonicNow()
		}
		return
	}
	delete(g.calls, key)
	delete(g.lastOK, key)
	delete(g.probeAt, key)
	delete(g.skips, key)
}

var (
	prefetchGate   = newStuckCallGate()
	backgroundGate = newStuckCallGate()
	gateTokens     atomic.Uint64
)

const (
	futurePending = iota
	futureRunning
	futureDone
	futureCancelled
)

// One queued background task and its durability class, write or prefetch
type bgFuture struct {
	fn     func()
	kind   string
	state  int
	done   chan struct{}
	onDone func(*bgFuture)
}

func (f *bgFuture) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

var errExecutorShutdown = errors.New("cannot schedule new futures after shutdown")

// Single worker FIFO executor whose worker starts on first submit and exits once shut down and drained
type serialExecutor struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*bgFuture
	closed  bool
	running bool
}

func newSerialExecutor() *serialExecutor {
	e := &serialExecutor{}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *serialExecutor) submit(fn func(), kind string, onDone func(*bgFuture)) (*bgFuture, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errExecutorShutdown
	}
	f := &bgFuture{fn: fn, kind: kind, done: make(chan struct{}), onDone: onDone}
	e.queue = append(e.queue, f)
	if !e.running {
		e.running = true
		go e.work()
	}
	e.cond.Signal()
	return f, nil
}

func (e *serialExecutor) work() {
	for {
		e.mu.Lock()
		for len(e.queue) == 0 && !e.closed {
			e.cond.Wait()
		}
		if len(e.queue) == 0 {
			e.running = false
			e.mu.Unlock()
			return
		}
		f := e.queue[0]
		e.queue = e.queue[1:]
		if f.state == futureCancelled {
			e.mu.Unlock()
			continue
		}
		f.state = futureRunning
		e.mu.Unlock()
		runRecovered(f.fn)
		e.mu.Lock()
		f.state = futureDone
		e.mu.Unlock()
		close(f.done)
		if f.onDone != nil {
			f.onDone(f)
		}
	}
}

// Closes submission without touching the queue, the worker keeps running every queued task
func (e *serialExecutor) shutdown() {
	e.mu.Lock()
	e.closed = true
	e.cond.Broadcast()
	e.mu.Unlock()
}

// Cancels a task that has not started, false once it runs or finished
func (e *serialExecutor) cancel(f *bgFuture) bool {
	e.mu.Lock()
	switch f.state {
	case futureCancelled:
		e.mu.Unlock()
		return true
	case futurePending:
		f.state = futureCancelled
		e.mu.Unlock()
		close(f.done)
		if f.onDone != nil {
			f.onDone(f)
		}
		return true
	}
	e.mu.Unlock()
	return false
}

// Runs fn with a panic swallowed the way the worker swallows a task exception
func runRecovered(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// Waits for the futures up to timeout and returns the ones still not done
func waitFutures(fs []*bgFuture, timeout time.Duration) []*bgFuture {
	deadline := time.Now().Add(timeout)
	for _, f := range fs {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case <-f.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	var pending []*bgFuture
	for _, f := range fs {
		if !f.isDone() {
			pending = append(pending, f)
		}
	}
	return pending
}

// Outcome of the most recent bounded shutdown drain
type DrainState struct {
	Status              string
	AbandonedWrites     int
	AbandonedPrefetches int
	ActiveTasks         int
}

// Provenance an agent stamps on mirrored memory writes
type WriteProvenance struct {
	WriteOrigin     string
	WriteContext    string
	SessionID       string
	ParentSessionID string
	Platform        string
}

// Built in store plus at most one external provider, one failing provider never blocking another
type MemoryManager struct {
	env   *tools.Env
	store *Store

	provMu          sync.RWMutex
	providers       []Provider
	toolToProvider  map[string]Provider
	prefetchSpill   *tools.SpillConfig
	hasExternal     bool
	prefetchTimeout time.Duration
	homeKey         string
	bound           *BoundScope
	provenance      WriteProvenance

	execMu        sync.Mutex
	executor      *serialExecutor
	futures       map[*bgFuture]string
	shuttingDown  bool
	teardownClaim string
	drainState    DrainState
}

// Manager over the session store with the default external prefetch bound
func NewMemoryManager(ctx context.Context, env *tools.Env, store *Store) *MemoryManager {
	m, _ := NewMemoryManagerWithTimeout(ctx, env, store, externalPrefetchTimeout)
	return m
}

// Manager with an explicit external prefetch bound, an error when the bound is not positive
func NewMemoryManagerWithTimeout(ctx context.Context, env *tools.Env, store *Store, prefetchTimeout time.Duration) (*MemoryManager, error) {
	if prefetchTimeout <= 0 {
		return nil, errors.New("external_prefetch_timeout must be positive")
	}
	return &MemoryManager{
		env:             env,
		store:           store,
		toolToProvider:  map[string]Provider{},
		prefetchTimeout: prefetchTimeout,
		homeKey:         homeKey(registryHome(ctx, env)),
		futures:         map[*bgFuture]string{},
		drainState:      DrainState{Status: "not_started"},
	}, nil
}

// Built in store the memory tool writes through
func (m *MemoryManager) Store() *Store { return m.store }

func (m *MemoryManager) log() *slog.Logger { return envLog(m.env) }

func (m *MemoryManager) logf(level slog.Level, format string, args ...any) {
	m.log().Log(context.Background(), level, fmt.Sprintf(format, args...))
}

// Identity comparison that never panics on an incomparable provider value
func sameProvider(a, b Provider) (same bool) {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() == reflect.Pointer && rb.Kind() == reflect.Pointer {
		return ra.Type() == rb.Type() && ra.Pointer() == rb.Pointer()
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// Runs one provider call with a panic reported as an error
func callProvider(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	return fn()
}

// Calls fn per provider, logging and swallowing failures
func (m *MemoryManager) eachProvider(label string, level slog.Level, providers []Provider, fn func(Provider) error) {
	if providers == nil {
		providers = m.Providers()
	}
	for _, p := range providers {
		if err := callProvider(func() error { return fn(p) }); err != nil {
			m.logf(level, "Memory provider '%s' %s: %s", p.Name(), label, err)
		}
	}
}

// Whether a lifecycle hook may reach the providers, only from the profile they were initialized for
func (m *MemoryManager) ownerAllows(ctx context.Context, hook string) bool {
	if len(m.Providers()) == 0 {
		return true
	}
	_, ok := CallerScope(ctx, m.boundScope(), "hook "+hook)
	return ok
}

func (m *MemoryManager) boundScope() *BoundScope {
	m.provMu.RLock()
	defer m.provMu.RUnlock()
	return m.bound
}

func (m *MemoryManager) dropProvider(p Provider) {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	kept := m.providers[:0:0]
	for _, q := range m.providers {
		if !sameProvider(q, p) {
			kept = append(kept, q)
		}
	}
	m.providers = kept
	for name, q := range m.toolToProvider {
		if sameProvider(q, p) {
			delete(m.toolToProvider, name)
		}
	}
	m.hasExternal = false
	for _, q := range m.providers {
		if q.Name() != "builtin" {
			m.hasExternal = true
		}
	}
}

// Core tool names a provider tool may never shadow
func coreToolNames() map[string]bool {
	out := make(map[string]bool, len(tools.CoreTools))
	for _, n := range tools.CoreTools {
		out[n] = true
	}
	return out
}

// Registers a provider, builtin always accepted and only one external, schemas loaded before any state changes
func (m *MemoryManager) AddProvider(ctx context.Context, p Provider) error {
	name := p.Name()
	m.provMu.RLock()
	hasExternal := m.hasExternal
	existing := "unknown"
	for _, q := range m.providers {
		if q.Name() != "builtin" {
			existing = q.Name()
			break
		}
	}
	m.provMu.RUnlock()
	if name != "builtin" && hasExternal {
		m.logf(slog.LevelWarn, "Rejected memory provider '%s' — external provider '%s' is already registered. Only one "+
			"external memory provider is allowed at a time. Configure which one via memory.provider in config.yaml.", name, existing)
		return nil
	}
	var schemas []map[string]any
	if err := callProvider(func() error {
		var err error
		schemas, err = p.ToolSchemas(ctx)
		return err
	}); err != nil {
		return err
	}
	var spill *tools.SpillConfig
	if name != "builtin" {
		tree, err := registryConfig(ctx, m.env)
		if err != nil {
			return err
		}
		cfg := tools.SpillConfigFromEnv(&tools.Env{Config: tree})
		if enabled, _ := tree.Get("memory.prefetch_spill_enabled").(bool); !enabled {
			cfg.MaxChars = max(cfg.MaxChars*10, 100000)
		}
		spill = &cfg
	}
	m.provMu.Lock()
	defer m.provMu.Unlock()
	if name != "builtin" {
		m.prefetchSpill = spill
		m.hasExternal = true
	}
	m.providers = append(m.providers, p)
	core := coreToolNames()
	for _, raw := range schemas {
		schema, ok := NormalizeToolSchema(raw)
		if !ok {
			continue
		}
		toolName := schema["name"].(string)
		switch {
		case core[toolName]:
			m.logf(slog.LevelWarn, "Memory provider '%s' tool '%s' shadows a reserved core tool name; registration "+
				"ignored. Core tools always win — rename the provider's tool to something unique.", name, toolName)
		case m.toolToProvider[toolName] != nil:
			m.logf(slog.LevelWarn, "Memory tool name conflict: '%s' already registered by %s, ignoring from %s",
				toolName, m.toolToProvider[toolName].Name(), name)
		default:
			m.toolToProvider[toolName] = p
		}
	}
	m.logf(slog.LevelInfo, "Memory provider '%s' registered (%d tools)", name, len(schemas))
	return nil
}

// Registered providers in order
func (m *MemoryManager) Providers() []Provider {
	m.provMu.RLock()
	defer m.provMu.RUnlock()
	return append([]Provider(nil), m.providers...)
}

// Provider registered under name, nil when none
func (m *MemoryManager) GetProvider(name string) Provider {
	for _, p := range m.Providers() {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Every provider's non empty system prompt block joined by blank lines
func (m *MemoryManager) BuildSystemPrompt(ctx context.Context) string {
	var blocks []string
	m.eachProvider("system_prompt_block() failed", slog.LevelWarn, nil, func(p Provider) error {
		b, err := p.SystemPromptBlock(ctx)
		if err != nil {
			return err
		}
		blocks = append(blocks, b)
		return nil
	})
	return joinNonBlank(blocks)
}

func joinNonBlank(parts []string) string {
	var kept []string
	for _, p := range parts {
		if p != "" && pyStrip(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// User instruction of a skill invocation turn, the text unchanged when it is not scaffolding
func stripSkillScaffolding(text string) string {
	clean, ok := skills.ExtractUserInstructionFromSkillMessage(text)
	if !ok {
		return ""
	}
	return clean
}

// Merged prefetch context from every provider, failures non fatal
func (m *MemoryManager) PrefetchAll(ctx context.Context, query, sessionID string) string {
	clean := stripSkillScaffolding(query)
	if clean == "" || !m.ownerAllows(ctx, "prefetch") {
		return ""
	}
	clean = redactString(clean)
	var parts []string
	m.eachProvider("prefetch failed (non-fatal)", slog.LevelDebug, nil, func(p Provider) error {
		r, err := m.prefetchProvider(ctx, p, clean, sessionID)
		if err != nil {
			return err
		}
		parts = append(parts, r)
		return nil
	})
	return joinNonBlank(parts)
}

func (m *MemoryManager) registryKey(p Provider) gateKey {
	key := m.homeKey
	if b := m.boundScope(); b != nil && b.HomeKey != "" {
		key = b.HomeKey
	}
	return gateKey{home: key, provider: p.Name()}
}

type prefetchResult struct {
	value string
	err   error
}

// One provider prefetch, an external call bounded by the timeout and left running in the background when stuck
func (m *MemoryManager) prefetchProvider(ctx context.Context, p Provider, query, sessionID string) (string, error) {
	if p.Name() == "builtin" {
		return p.Prefetch(ctx, query, sessionID)
	}
	key := m.registryKey(p)
	token := gateTokens.Add(1)
	if skipped := prefetchGate.enter(key, token, m.prefetchTimeout); skipped != nil {
		m.logf(slog.LevelDebug, "Memory provider '%s' prefetch skipped (%d stuck, %d in flight)", p.Name(), skipped.stuck, skipped.inFlight)
		return "", nil
	}
	box := make(chan prefetchResult, 1)
	thread := SpawnContextThread(ctx, "memory-prefetch-"+p.Name(), func(tctx context.Context) {
		var r prefetchResult
		r.err = callProvider(func() error {
			v, err := p.Prefetch(tctx, query, sessionID)
			r.value = v
			return err
		})
		prefetchGate.leave(key, token, r.err == nil)
		box <- r
	})
	thread.Start()
	timer := time.NewTimer(m.prefetchTimeout)
	defer timer.Stop()
	var r prefetchResult
	select {
	case r = <-box:
	case <-timer.C:
		m.logf(slog.LevelWarn, "Memory provider '%s' prefetch timed out after %.1fs; the call keeps running in the background "+
			"(later turns may skip it while the provider stays stuck)", p.Name(), m.prefetchTimeout.Seconds())
		return "", nil
	}
	if r.err != nil {
		return "", r.err
	}
	result := r.value
	if result != "" && pyStrip(result) != "" {
		m.provMu.RLock()
		spill := m.prefetchSpill
		m.provMu.RUnlock()
		result = tools.SpillIfOversized(m.spillEnv(ctx), result, sessionID, p.Name()+" memory prefetch", spill)
	}
	return result, nil
}

// Session environment with the home in effect for the caller, where a spilled prefetch lands
func (m *MemoryManager) spillEnv(ctx context.Context) *tools.Env {
	env := &tools.Env{}
	if m.env != nil {
		copied := *m.env
		env = &copied
	}
	env.Home = registryHome(ctx, m.env)
	return env
}

// Deterministic recall indicator line, empty when no provider injected anything
func (m *MemoryManager) DescribeRecall(ctx context.Context) string {
	var segments []string
	m.eachProvider("recall_status failed (non-fatal)", slog.LevelDebug, nil, func(p Provider) error {
		status, err := p.RecallStatus(ctx)
		if err != nil {
			return err
		}
		if status == nil {
			return nil
		}
		detail := "recalled relevant memory"
		switch {
		case status.Count == 1:
			detail = "recalled 1 memory"
		case status.Count > 1:
			detail = fmt.Sprintf("recalled %d memories", status.Count)
		}
		segments = append(segments, fmt.Sprintf("%s %s — %s", status.Glyph, status.ProviderLabel, detail))
		return nil
	})
	return strings.Join(segments, "  ")
}

// Queues a background prefetch on every provider for the next turn
func (m *MemoryManager) QueuePrefetchAll(ctx context.Context, query, sessionID string) {
	providers := m.Providers()
	if len(providers) == 0 {
		return
	}
	clean := stripSkillScaffolding(query)
	if clean == "" || !m.ownerAllows(ctx, "queue_prefetch") {
		return
	}
	clean = redactString(clean)
	m.submitBackground(ctx, func(bctx context.Context) {
		m.eachProvider("queue_prefetch failed (non-fatal)", slog.LevelDebug, providers, func(p Provider) error {
			return m.callUnlessStalled(p, func() error { return p.QueuePrefetch(bctx, clean, sessionID) })
		})
	}, "prefetch")
}

// Syncs a completed turn to every provider on the background worker with all egress scrubbed
func (m *MemoryManager) SyncAll(ctx context.Context, userContent, assistantContent string, opts SyncOptions) {
	providers := m.Providers()
	if len(providers) == 0 {
		return
	}
	clean := stripSkillScaffolding(userContent)
	if clean == "" || !m.ownerAllows(ctx, "sync_turn") {
		return
	}
	scrubbed := RedactForProvider(clean, assistantContent, messagesToAny(opts.Messages))
	clean = scrubbed[0].(string)
	assistant := scrubbed[1].(string)
	sync := SyncOptions{SessionID: opts.SessionID, Messages: anyToMessages(scrubbed[2]), TurnAuthor: opts.TurnAuthor}
	m.submitBackground(ctx, func(bctx context.Context) {
		m.eachProvider("sync_turn failed", slog.LevelWarn, providers, func(p Provider) error {
			return m.callUnlessStalled(p, func() error { return p.SyncTurn(bctx, clean, assistant, sync) })
		})
	}, "write")
}

// Messages as the any shapes the egress scrub walks, nil kept as nil
func messagesToAny(msgs []map[string]any) any {
	if msgs == nil {
		return nil
	}
	out := make([]any, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

// Scrubbed messages back in row form, nil kept as nil
func anyToMessages(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Display metadata key holding the typed text of a vetoed user row
const withheldPromptKey = "withheld_prompt"

// Rows with the vetoed typed text dropped from display metadata, copies only where one is present
func withoutWithheldPrompts(messages []map[string]any) []map[string]any {
	if messages == nil {
		return nil
	}
	found := false
	for _, msg := range messages {
		if _, ok := withheldPromptText(msg); ok {
			found = true
			break
		}
	}
	if !found {
		return messages
	}
	out := make([]map[string]any, len(messages))
	for i, msg := range messages {
		if _, ok := withheldPromptText(msg); !ok {
			out[i] = msg
			continue
		}
		meta := msg["display_metadata"].(map[string]any)
		trimmed := make(map[string]any, len(meta))
		for k, v := range meta {
			if k != withheldPromptKey {
				trimmed[k] = v
			}
		}
		row := make(map[string]any, len(msg))
		for k, v := range msg {
			row[k] = v
		}
		row["display_metadata"] = trimmed
		out[i] = row
	}
	return out
}

func withheldPromptText(msg map[string]any) (string, bool) {
	meta, ok := msg["display_metadata"].(map[string]any)
	if !ok {
		return "", false
	}
	text, ok := meta[withheldPromptKey].(string)
	return text, ok
}

// Scrubbed copies of whole transcript rows for a provider with vetoed typed text removed
func redactMessagesForEgress(messages []map[string]any) []map[string]any {
	if messages == nil {
		return nil
	}
	return anyToMessages(RedactForProvider(messagesToAny(withoutWithheldPrompts(messages)))[0])
}

// Queues fn on the serialized worker under the owner home and tracks its durability class
func (m *MemoryManager) submitBackground(ctx context.Context, fn func(ctx context.Context), kind string) {
	run := InOwnerHome(m.boundScope(), func(c context.Context) struct{} {
		fn(c)
		return struct{}{}
	})
	base := CtxBound(ctx)
	task := func() { run(base) }
	m.execMu.Lock()
	if m.shuttingDown {
		m.execMu.Unlock()
		m.logf(slog.LevelWarn, "Memory manager is shutting down; rejecting late %s task", kind)
		return
	}
	if m.executor == nil {
		m.executor = newSerialExecutor()
	}
	f, err := m.executor.submit(task, kind, m.forgetBackgroundFuture)
	if err == nil {
		m.futures[f] = kind
		m.execMu.Unlock()
		return
	}
	shutting := m.shuttingDown
	m.execMu.Unlock()
	if shutting {
		m.logf(slog.LevelWarn, "Memory manager shut down during %s submission; task rejected", kind)
		return
	}
	if err := callProvider(func() error { task(); return nil }); err != nil {
		m.logf(slog.LevelDebug, "Inline memory background task failed: %s", err)
	}
}

func (m *MemoryManager) forgetBackgroundFuture(f *bgFuture) {
	m.execMu.Lock()
	defer m.execMu.Unlock()
	delete(m.futures, f)
}

// Runs a background provider call unless its provider has stuck calls, the builtin provider never gated
func (m *MemoryManager) callUnlessStalled(p Provider, call func() error) error {
	if p.Name() == "builtin" {
		return call()
	}
	key := m.registryKey(p)
	token := gateTokens.Add(1)
	if skipped := backgroundGate.enter(key, token, backgroundCallStall); skipped != nil {
		if skipped.skips == 1 || skipped.skips%100 == 0 {
			m.logf(slog.LevelWarn, "Memory provider '%s' has %d background calls stuck for over %.0fs (%d in flight); "+
				"skipping its sync/prefetch until it recovers (%d skipped)", p.Name(), skipped.stuck,
				backgroundCallStall.Seconds(), skipped.inFlight, skipped.skips)
		}
		return nil
	}
	err := callProvider(call)
	backgroundGate.leave(key, token, err == nil)
	return err
}

// Blocks until queued work drained, false on timeout, a negative timeout waiting forever
func (m *MemoryManager) FlushPending(timeout time.Duration) bool {
	m.execMu.Lock()
	ex := m.executor
	m.execMu.Unlock()
	if ex == nil {
		return true
	}
	f, err := ex.submit(func() {}, "sentinel", nil)
	if err != nil {
		return true
	}
	if timeout < 0 {
		<-f.done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-f.done:
		return true
	case <-timer.C:
		return false
	}
}

// Deduplicated provider tool schemas with reserved core names skipped
func (m *MemoryManager) GetAllToolSchemas(ctx context.Context) []map[string]any {
	core := coreToolNames()
	var schemas []map[string]any
	seen := map[string]bool{}
	m.eachProvider("get_tool_schemas() failed", slog.LevelWarn, nil, func(p Provider) error {
		raw, err := p.ToolSchemas(ctx)
		if err != nil {
			return err
		}
		for _, r := range raw {
			schema, ok := NormalizeToolSchema(r)
			if !ok {
				m.logf(slog.LevelWarn, "Memory provider '%s' returned a tool schema with no resolvable name; skipping (%s)",
					p.Name(), pyRepr(r))
				continue
			}
			name := schema["name"].(string)
			if !core[name] && !seen[name] {
				schemas = append(schemas, schema)
				seen[name] = true
			}
		}
		return nil
	})
	return schemas
}

// Names of every routed provider tool
func (m *MemoryManager) GetAllToolNames() map[string]bool {
	m.provMu.RLock()
	defer m.provMu.RUnlock()
	out := make(map[string]bool, len(m.toolToProvider))
	for name := range m.toolToProvider {
		out[name] = true
	}
	return out
}

// Whether a provider routes this tool
func (m *MemoryManager) HasTool(toolName string) bool {
	m.provMu.RLock()
	defer m.provMu.RUnlock()
	return m.toolToProvider[toolName] != nil
}

// Routes a tool call to its provider as the calling profile, returning a JSON string
func (m *MemoryManager) HandleToolCall(ctx context.Context, toolName string, args map[string]any, callerGSK string) string {
	m.provMu.RLock()
	p := m.toolToProvider[toolName]
	m.provMu.RUnlock()
	if p == nil {
		return toolError(fmt.Sprintf("No memory provider handles tool '%s'", toolName), nil)
	}
	scope, ok := CallerScope(ctx, m.boundScope(), "tool "+toolName)
	if !ok {
		return toolError(RefusedToolMessage(toolName), nil)
	}
	opts := ToolCallOptions{Home: scope.Home, AgentIdentity: scope.AgentIdentity, CallerGSK: callerGSK}
	scrubbedArgs, _ := RedactForProvider(args)[0].(map[string]any)
	var result string
	err := callProvider(func() error {
		var err error
		result, err = p.HandleToolCall(ctx, toolName, scrubbedArgs, opts)
		return err
	})
	if err != nil {
		var nh *NotHandledError
		if errors.As(err, &nh) && nh.Provider == "" {
			nh.Provider = p.Name()
		}
		m.logf(slog.LevelError, "Memory provider '%s' handle_tool_call(%s) failed: %s", p.Name(), toolName, err)
		recordProviderMemoryCall(m.env, p.Name(), toolName, scrubbedArgs, "", true)
		return toolError(fmt.Sprintf("Memory tool '%s' failed: %s", toolName, err), nil)
	}
	recordProviderMemoryCall(m.env, p.Name(), toolName, scrubbedArgs, result, false)
	return result
}

// Catalog plugin names the plugin package installs so the provider metric keeps known names
var (
	catalogNamesMu     sync.RWMutex
	pluginCatalogNames func() map[string]bool
)

// Installs the plugin catalog name source the memory op metric validates provider names against
func SetPluginCatalogNames(fn func() map[string]bool) {
	catalogNamesMu.Lock()
	defer catalogNamesMu.Unlock()
	pluginCatalogNames = fn
}

// Metric provider value, the provider name when bundled, catalogued, builtin or plugin, else plugin
func memoryProviderName(v any) string {
	name := normMetricValue(v)
	if name == "" {
		name = "builtin"
	}
	if name == "builtin" || name == "plugin" {
		return name
	}
	registryMu.RLock()
	for _, r := range registrations {
		if r.Source == SourceBundled && r.Name == name {
			registryMu.RUnlock()
			return name
		}
	}
	registryMu.RUnlock()
	catalogNamesMu.RLock()
	fn := pluginCatalogNames
	catalogNamesMu.RUnlock()
	if fn != nil {
		if names := safeCatalogNames(fn); names[name] {
			return name
		}
	}
	return "plugin"
}

func safeCatalogNames(fn func() map[string]bool) (names map[string]bool) {
	defer func() {
		if recover() != nil {
			names = nil
		}
	}()
	return fn()
}

// Operation a provider tool performs, its action argument else a verb in its name
func memoryProviderOp(toolName string, args map[string]any) string {
	if action, ok := args["action"].(string); ok {
		if bucket, ok := memoryOpWords[normMetricValue(action)]; ok {
			return bucket
		}
	}
	words := strings.Split(strings.ReplaceAll(normMetricValue(toolName), "-", "_"), "_")
	for i := len(words) - 1; i >= 0; i-- {
		if bucket, ok := memoryOpWords[words[i]]; ok {
			return bucket
		}
	}
	return "other"
}

// Whether a provider tool result reports an error or an unsuccessful outcome in its head
func providerResultFailed(result string) bool {
	head := runePrefix(strings.TrimLeftFunc(result, pyIsSpace), 200)
	return strings.HasPrefix(head, `{"error"`) || strings.Contains(head, `"success": false`) || strings.Contains(head, `"success":false`)
}

// One metric row per provider tool call
func recordProviderMemoryCall(env *tools.Env, provider, toolName string, args map[string]any, result string, raised bool) {
	if env == nil || env.Config == nil || !env.Config.Bool("telemetry.shared_metrics.enabled", false) {
		return
	}
	failed := ""
	switch {
	case raised:
		failed = "exception"
	case providerResultFailed(result):
		failed = "provider_error"
	}
	outcome, class := "success", "none"
	if failed != "" {
		outcome, class = "failed", failed
	}
	origin := "foreground"
	if tools.CurrentOrigin(env) == backgroundReviewOrigin {
		origin = backgroundReviewOrigin
	}
	env.Emit(tools.Event{Kind: "metric", Tool: toolName, Data: map[string]any{
		"name": memoryOpMetricName, "failure_class": class, "op": memoryProviderOp(toolName, args), "origin": origin,
		"outcome": outcome, "provider": memoryProviderName(provider),
	}})
}

// Per turn tick to every provider with the turn author
func (m *MemoryManager) OnTurnStart(ctx context.Context, turnNumber int, message string, opts TurnOptions) {
	if !m.ownerAllows(ctx, "on_turn_start") {
		return
	}
	message = redactString(message)
	m.eachProvider("on_turn_start failed", slog.LevelDebug, nil, func(p Provider) error {
		return p.OnTurnStart(ctx, turnNumber, message, opts)
	})
}

// End of session extraction on every provider
func (m *MemoryManager) OnSessionEnd(ctx context.Context, messages []map[string]any) {
	if !m.ownerAllows(ctx, "on_session_end") {
		return
	}
	if messages == nil {
		messages = []map[string]any{}
	}
	scrubbed := redactMessagesForEgress(messages)
	m.eachProvider("on_session_end failed", slog.LevelWarn, nil, func(p Provider) error {
		return p.OnSessionEnd(ctx, scrubbed)
	})
}

// Queues old session extraction then provider rebinding as one serialized task
func (m *MemoryManager) CommitSessionBoundaryAsync(ctx context.Context, messages []map[string]any, newSessionID, parentSessionID, reason string) {
	if len(m.Providers()) == 0 || !m.ownerAllows(ctx, "session_boundary") {
		return
	}
	if reason == "" {
		reason = "new_session"
	}
	snapshot := append([]map[string]any{}, messages...)
	m.submitBackground(ctx, func(bctx context.Context) {
		if err := callProvider(func() error { m.OnSessionEnd(bctx, snapshot); return nil }); err != nil {
			m.logf(slog.LevelWarn, "Session-boundary extraction failed: %s", err)
		}
		if err := callProvider(func() error {
			m.OnSessionSwitch(bctx, newSessionID, SessionSwitchOptions{ParentSessionID: parentSessionID, Reset: true, Reason: reason})
			return nil
		}); err != nil {
			m.logf(slog.LevelWarn, "Session-boundary switch failed: %s", err)
		}
	}, "write")
}

// Tells providers the session id rotated without teardown
func (m *MemoryManager) OnSessionSwitch(ctx context.Context, newSessionID string, opts SessionSwitchOptions) {
	if newSessionID == "" || !m.ownerAllows(ctx, "on_session_switch") {
		return
	}
	m.eachProvider("on_session_switch failed", slog.LevelDebug, nil, func(p Provider) error {
		return p.OnSessionSwitch(ctx, newSessionID, opts)
	})
}

// Whether an active provider guarantees the checkpoint contract at apiVersion or newer
func (m *MemoryManager) SupportsPreCompressCheckpoint(apiVersion int) bool {
	for _, p := range m.Providers() {
		if p.CheckpointAPIVersion() >= apiVersion {
			return true
		}
	}
	return false
}

// Notifies providers before compression and returns their combined summary prompt text
func (m *MemoryManager) OnPreCompress(ctx context.Context, messages, evidenceMessages []map[string]any, requireCheckpoint bool, checkpointAPIVersion int) (string, error) {
	if !m.ownerAllows(ctx, "on_pre_compress") {
		if requireCheckpoint {
			return "", errors.New("Memory pre-compress checkpoint refused; see `icebath logs errors`")
		}
		return "", nil
	}
	if messages == nil {
		messages = []map[string]any{}
	}
	scrubbed := RedactForProvider(messagesToAny(withoutWithheldPrompts(messages)), messagesToAny(withoutWithheldPrompts(evidenceMessages)))
	messages = anyToMessages(scrubbed[0])
	evidenceMessages = anyToMessages(scrubbed[1])
	var parts []string
	checkpointSucceeded := false
	for _, p := range m.Providers() {
		isCheckpoint := p.CheckpointAPIVersion() >= checkpointAPIVersion
		providerMessages := messages
		if isCheckpoint && evidenceMessages != nil {
			providerMessages = evidenceMessages
		}
		var result string
		err := callProvider(func() error {
			var err error
			result, err = p.OnPreCompress(ctx, providerMessages, isCheckpoint && requireCheckpoint)
			return err
		})
		if err != nil {
			m.logf(slog.LevelDebug, "Memory provider '%s' on_pre_compress failed: %s", p.Name(), err)
			if requireCheckpoint && isCheckpoint {
				return "", err
			}
			continue
		}
		if result != "" && pyStrip(result) != "" {
			parts = append(parts, result)
		}
		checkpointSucceeded = checkpointSucceeded || isCheckpoint
	}
	if requireCheckpoint && !checkpointSucceeded {
		return "", fmt.Errorf("No active memory provider completed pre-compress checkpoint API v%d", checkpointAPIVersion)
	}
	return strings.Join(parts, "\n\n"), nil
}

// Mirrors a built in memory tool write to external providers, the builtin source skipped
func (m *MemoryManager) OnMemoryWrite(ctx context.Context, action, target, content string, metadata map[string]any) {
	if !m.ownerAllows(ctx, "on_memory_write") {
		return
	}
	meta := map[string]any{}
	for k, v := range metadata {
		meta[k] = v
	}
	scrubbed := RedactForProvider(content, meta)
	content = scrubbed[0].(string)
	meta, _ = scrubbed[1].(map[string]any)
	var external []Provider
	for _, p := range m.Providers() {
		if p.Name() != "builtin" {
			external = append(external, p)
		}
	}
	if external == nil {
		return
	}
	m.eachProvider("on_memory_write failed", slog.LevelDebug, external, func(p Provider) error {
		copied := make(map[string]any, len(meta))
		for k, v := range meta {
			copied[k] = v
		}
		return p.OnMemoryWrite(ctx, action, target, content, copied)
	})
}

// Memory tool actions mirrored to external providers
var mirroredMemoryActions = map[string]bool{"add": true, "replace": true, "patch": true, "remove": true}

// True only when the built in memory tool committed a write, failing closed on anything else
func memoryToolResultSucceeded(result any) (map[string]any, bool) {
	if s, ok := result.(string); ok {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err != nil || dec.More() {
			return nil, false
		}
		result = parsed
	}
	data, ok := result.(map[string]any)
	if !ok {
		return nil, false
	}
	if success, _ := data["success"].(bool); !success {
		return nil, false
	}
	if staged, _ := data["staged"].(bool); staged {
		return nil, false
	}
	return data, true
}

// Value of a result entries map at a one based operation index
func indexedEntry(entries any, index int) any {
	m, ok := entries.(map[string]any)
	if !ok {
		return nil
	}
	return m[strconv.Itoa(index)]
}

// Mirrors a memory tool call to external providers with provenance from buildMetadata
func (m *MemoryManager) NotifyMemoryToolWriteContext(ctx context.Context, toolResult any, toolArgs map[string]any, buildMetadata func() map[string]any) {
	result, ok := memoryToolResultSucceeded(toolResult)
	if !ok {
		return
	}
	target := "memory"
	if pyTruthy(toolArgs["target"]) {
		target = pyStr(toolArgs["target"])
	}
	operations, isList := toolArgs["operations"].([]any)
	batched := isList && len(operations) > 0
	ops := []any{toolArgs}
	if batched {
		ops = operations
	}
	fields := map[string]string{"replace": "replaced", "patch": "patched", "remove": "removed"}
	for i, rawOp := range ops {
		index := i + 1
		op, isMap := rawOp.(map[string]any)
		action := ""
		if isMap && pyTruthy(op["action"]) {
			action = pyStr(op["action"])
		}
		if !mirroredMemoryActions[action] {
			continue
		}
		err := callProvider(func() error {
			metadata := map[string]any{}
			if buildMetadata != nil {
				for k, v := range buildMetadata() {
					metadata[k] = v
				}
			}
			delete(metadata, "previous_content")
			if oldText := op["old_text"]; pyTruthy(oldText) {
				metadata["old_text"] = pyStr(oldText)
			}
			if field, ok := fields[action]; ok {
				var previous any
				if batched {
					previous = indexedEntry(result[field+"_entries"], index)
				} else {
					previous = result[field+"_entry"]
					if field == "patched" && previous == nil {
						previous = result["patched_entry_before"]
					}
				}
				if s, ok := previous.(string); ok && s != "" {
					metadata["previous_content"] = s
				}
			}
			mirrored := action
			contentValue := op["content"]
			if !pyTruthy(contentValue) {
				contentValue = op["new_text"]
			}
			content := ""
			if pyTruthy(contentValue) {
				content = pyStr(contentValue)
			}
			if action == "patch" {
				var after any
				if batched {
					afterEntries := result["patched_entries_after"]
					if !pyTruthy(afterEntries) {
						afterEntries = map[string]any{}
					}
					if _, ok := afterEntries.(map[string]any); !ok {
						return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(afterEntries))
					}
					after = indexedEntry(afterEntries, index)
				} else {
					after = result["patched_entry_after"]
				}
				mirrored, content = "replace", ""
				if pyTruthy(after) {
					content = pyStr(after)
				}
			}
			m.OnMemoryWrite(ctx, mirrored, target, content, metadata)
			return nil
		})
		if err != nil {
			m.logf(slog.LevelDebug, "notify_memory_tool_write failed for op %s: %s", action, err)
		}
	}
}

// Records the provenance the agent stamps on mirrored writes
func (m *MemoryManager) SetProvenance(p WriteProvenance) {
	m.provMu.Lock()
	defer m.provMu.Unlock()
	m.provenance = p
}

// Provenance metadata for external provider mirrors with empty values dropped
func (m *MemoryManager) BuildWriteMetadata(taskID, toolCallID string) map[string]any {
	m.provMu.RLock()
	p := m.provenance
	m.provMu.RUnlock()
	origin := p.WriteOrigin
	if origin == "" {
		origin = "assistant_tool"
	}
	execution := p.WriteContext
	if execution == "" {
		execution = "foreground"
	}
	platform := p.Platform
	if platform == "" {
		if v, ok := os.LookupEnv("ICEBATH_SESSION_SOURCE"); ok {
			platform = v
		} else {
			platform = "cli"
		}
	}
	out := map[string]any{}
	for k, v := range map[string]string{
		"write_origin": origin, "execution_context": execution, "session_id": p.SessionID,
		"parent_session_id": p.ParentSessionID, "platform": platform, "tool_name": "memory", "task_id": taskID,
		"tool_call_id": toolCallID,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// Mirrors a memory tool call as the session home that owns this manager
func (m *MemoryManager) NotifyMemoryToolWrite(result string, args map[string]any, taskID, toolCallID string) {
	ctx := context.Background()
	if m.env != nil && pyStrip(m.env.Home) != "" {
		ctx = home.WithOverride(ctx, m.env.Home)
	}
	m.NotifyMemoryToolWriteContext(ctx, result, args, func() map[string]any { return m.BuildWriteMetadata(taskID, toolCallID) })
}

// Parent side observation of a completed delegation on every provider
func (m *MemoryManager) OnDelegation(ctx context.Context, task, result string, opts DelegationOptions) {
	if !m.ownerAllows(ctx, "on_delegation") {
		return
	}
	scrubbed := RedactForProvider(task, result)
	task, result = scrubbed[0].(string), scrubbed[1].(string)
	m.eachProvider("on_delegation failed", slog.LevelDebug, nil, func(p Provider) error {
		return p.OnDelegation(ctx, task, result, opts)
	})
}

// Claims extraction and shutdown, the first claimer winning and session end callers allowed to claim again
func (m *MemoryManager) ClaimTeardown(sessionEnd bool) bool {
	m.execMu.Lock()
	defer m.execMu.Unlock()
	if m.teardownClaim == "" && (sessionEnd || !m.shuttingDown) {
		if sessionEnd {
			m.teardownClaim = "session_end"
		} else {
			m.teardownClaim = "soft_release"
		}
		return true
	}
	return sessionEnd && m.teardownClaim == "session_end"
}

// Stops provider writers without session end extraction, flushing first
func (m *MemoryManager) ReleaseSoft(ctx context.Context) {
	if m.ClaimTeardown(false) {
		m.FlushPending(10 * time.Second)
		m.ShutdownAll(ctx)
	}
}

// Drains the background worker within its bound, then shuts providers down in reverse order
func (m *MemoryManager) ShutdownAll(ctx context.Context) {
	m.drainSyncExecutor()
	providers := m.Providers()
	reversed := make([]Provider, len(providers))
	for i, p := range providers {
		reversed[len(providers)-1-i] = p
	}
	m.eachProvider("shutdown failed", slog.LevelWarn, reversed, func(p Provider) error {
		return p.Shutdown(ctx)
	})
}

// Snapshot of the most recent bounded shutdown drain
func (m *MemoryManager) ShutdownDrainState() DrainState {
	m.execMu.Lock()
	defer m.execMu.Unlock()
	return m.drainState
}

// Gives queued work a bounded chance to run in order, then abandons the rest explicitly
func (m *MemoryManager) drainSyncExecutor() {
	m.execMu.Lock()
	m.shuttingDown = true
	ex := m.executor
	m.executor = nil
	tracked := make([]*bgFuture, 0, len(m.futures))
	for f := range m.futures {
		tracked = append(tracked, f)
	}
	active := 0
	for _, f := range tracked {
		if !f.isDone() {
			active++
		}
	}
	status := "drained"
	if ex != nil {
		status = "draining"
	}
	m.drainState = DrainState{Status: status, ActiveTasks: active}
	m.execMu.Unlock()
	if ex == nil {
		return
	}
	ex.shutdown()
	pending := waitFutures(tracked, syncDrainTimeout)
	var cancelledKinds []string
	for _, f := range pending {
		if ex.cancel(f) {
			cancelledKinds = append(cancelledKinds, f.kind)
		}
	}
	abandonedPrefetches := 0
	for _, k := range cancelledKinds {
		if k == "prefetch" {
			abandonedPrefetches++
		}
	}
	abandonedWrites := len(cancelledKinds) - abandonedPrefetches
	activeTasks := len(pending) - len(cancelledKinds)
	m.execMu.Lock()
	status = "drained"
	if len(pending) > 0 {
		status = "timed_out"
	}
	m.drainState = DrainState{Status: status, AbandonedWrites: abandonedWrites, AbandonedPrefetches: abandonedPrefetches, ActiveTasks: activeTasks}
	m.execMu.Unlock()
	if len(pending) == 0 {
		return
	}
	m.logf(slog.LevelWarn, "Memory shutdown drain timed out after %.2fs; abandoning %d queued memory write(s) and %d queued "+
		"prefetch(es); %d active task(s) remain detached", syncDrainTimeout.Seconds(), abandonedWrites, abandonedPrefetches, activeTasks)
}

// Initializes for an agent build, true while an external provider is still served, say told about dropped ones
func (m *MemoryManager) InitializeForAgent(ctx context.Context, sessionID string, opts InitOptions, say func(string)) bool {
	var before []Provider
	for _, p := range m.Providers() {
		if p.Name() != "builtin" {
			before = append(before, p)
		}
	}
	m.InitializeAll(ctx, sessionID, opts)
	kept := m.Providers()
	for _, p := range before {
		still := false
		for _, q := range kept {
			if sameProvider(p, q) {
				still = true
				break
			}
		}
		if still {
			continue
		}
		message := fmt.Sprintf("⚠ Memory provider %s is disabled for this profile: it handed out the provider instance "+
			"another profile already uses. Built-in memory still works; the operator can see why with `icebath logs errors`.",
			pyRepr(p.Name()))
		if say != nil {
			say(message)
		} else {
			m.log().Warn(message)
		}
	}
	m.provMu.RLock()
	defer m.provMu.RUnlock()
	return m.hasExternal
}

// Binds every provider to the profile home, refusing an instance another profile already bound
func (m *MemoryManager) InitializeAll(ctx context.Context, sessionID string, opts InitOptions) {
	if opts.Home == "" {
		opts.Home = home.Dir(ctx)
	}
	m.provMu.Lock()
	m.bound = NewBoundScope(opts.Home, opts.AgentIdentity)
	m.provMu.Unlock()
	for _, p := range m.Providers() {
		if p.Name() != "builtin" && !ClaimInstance(ctx, p, opts.Home) {
			m.dropProvider(p)
		}
	}
	m.eachProvider("initialize failed", slog.LevelWarn, nil, func(p Provider) error {
		return p.Initialize(ctx, sessionID, opts)
	})
}

// Turn based nudge toward a memory review
type MemoryNudge struct {
	Interval   int
	turnsSince int
}

// Nudge firing every interval user turns, zero disabling it
func NewMemoryNudge(interval int) *MemoryNudge {
	return &MemoryNudge{Interval: interval}
}

// Hydrates the counter from persisted history so a resumed session keeps its cadence
func (n *MemoryNudge) Hydrate(priorUserTurns int) {
	if priorUserTurns > 0 && n.Interval > 0 && n.turnsSince == 0 {
		n.turnsSince = priorUserTurns % n.Interval
	}
}

// Advances the counter, true when the review should fire, only while the memory tool and store exist
func (n *MemoryNudge) Tick(memoryToolValid, storePresent bool) bool {
	if n.Interval > 0 && memoryToolValid && storePresent {
		n.turnsSince++
		if n.turnsSince >= n.Interval {
			n.turnsSince = 0
			return true
		}
	}
	return false
}

// Resets the counter when the model used the memory tool itself
func (n *MemoryNudge) NoteToolCall(name string) {
	if name == "memory" {
		n.turnsSince = 0
	}
}

// Turns since the last memory write or review
func (n *MemoryNudge) TurnsSince() int { return n.turnsSince }

// Bare function tool with a resolvable name, unwrapping the OpenAI function wrapper
func NormalizeToolSchema(schema any) (map[string]any, bool) {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil, false
	}
	if m["type"] == "function" {
		if fn, ok := m["function"].(map[string]any); ok {
			m = fn
		}
	}
	name, ok := m["name"].(string)
	if !ok || name == "" {
		return nil, false
	}
	return m, true
}

// Whether external provider tools are exposed, enabled nil meaning every toolset
func MemoryProviderToolsEnabled(ctx context.Context, env *tools.Env, enabled, disabled []string, memoryToolPresent bool) bool {
	if containsString(disabled, "memory") {
		return false
	}
	if memoryToolPresent || enabled == nil {
		return true
	}
	if len(enabled) == 0 {
		return false
	}
	if containsString(enabled, "memory") {
		return true
	}
	scope := tools.ScopeKey(envHome(env))
	for _, name := range enabled {
		if containsString(tools.Default.ResolveToolset(ctx, scope, name), "memory") {
			return true
		}
	}
	return false
}

// Name of a tool in OpenAI function form
func functionToolName(tool map[string]any) string {
	fn, _ := tool["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

// Whether provider tools are exposed on a tool surface, the same gate the provider prompt block uses
func MemoryProviderToolsExposed(ctx context.Context, env *tools.Env, surface []map[string]any) bool {
	present := false
	for _, t := range surface {
		if functionToolName(t) == "memory" {
			present = true
			break
		}
	}
	var enabled, disabled []string
	if env != nil {
		enabled, disabled = env.Enabled, env.Disabled
	}
	return MemoryProviderToolsEnabled(ctx, env, enabled, disabled, present)
}

// Appends external provider tool schemas to a tool surface, returning the surface, the valid names and the count added
func InjectMemoryProviderTools(ctx context.Context, env *tools.Env, manager *MemoryManager, surface []map[string]any, validNames map[string]bool) ([]map[string]any, map[string]bool, int) {
	if manager == nil || surface == nil {
		return surface, validNames, 0
	}
	if !MemoryProviderToolsExposed(ctx, env, surface) {
		var names []string
		for _, p := range manager.Providers() {
			if p.Name() != "builtin" {
				names = append(names, p.Name())
			}
		}
		if len(names) > 0 {
			quoted := make([]any, len(names))
			for i, n := range names {
				quoted[i] = n
			}
			envLog(env).Info(fmt.Sprintf("Memory provider(s) %s configured but the 'memory' toolset is gated off for this "+
				"session (platform_toolsets / agent.disabled_toolsets) — provider tools and system-prompt block are both "+
				"withheld.", pyStr(quoted)))
		}
		return surface, validNames, 0
	}
	if validNames == nil {
		validNames = map[string]bool{}
	}
	existing := map[string]bool{}
	for _, t := range surface {
		existing[functionToolName(t)] = true
	}
	added := 0
	for _, raw := range manager.GetAllToolSchemas(ctx) {
		schema, ok := NormalizeToolSchema(raw)
		if !ok {
			envLog(env).Warn(fmt.Sprintf("Memory provider returned a tool schema with no resolvable name; skipping to avoid "+
				"poisoning the request (%s)", pyRepr(raw)))
			continue
		}
		name := schema["name"].(string)
		if existing[name] {
			continue
		}
		surface = append(surface, map[string]any{"type": "function", "function": schema})
		validNames[name] = true
		existing[name] = true
		added++
	}
	return surface, validNames, added
}

// Memory manager registers nothing at import
func registerMemoryManager() {}
