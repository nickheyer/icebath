package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	checkTTL        = 30 * time.Second
	checkGrace      = 60 * time.Second
	checkGraceReuse = 5 * time.Second
	checkCacheMax   = 512
)

type checkKey struct {
	fn    uintptr
	scope string
}

type checkVerdict struct {
	at    time.Time
	value bool
}

var (
	checkMu         sync.Mutex
	checkCache      = map[checkKey]checkVerdict{}
	checkCacheOrder []checkKey
	checkLastGood   = map[checkKey]time.Time{}
	checkGoodOrder  []checkKey
	checkEverGood   = map[checkKey]bool{}
	checkDropWarned = map[checkKey]bool{}
	checkPins       = map[uintptr]func(*Env) bool{}
	checkEpoch      int64
	noCacheChecks   = map[uintptr]func(*Env) bool{}
	checkNow        = time.Now
)

// Closure identity of a check, distinct per closure value like the Python callable it keys
func fnPointer(fn func(*Env) bool) uintptr {
	return uintptr(*(*unsafe.Pointer)(unsafe.Pointer(&fn)))
}

// NoCacheCheck marks a config backed availability check as never cached
func NoCacheCheck(fn func(*Env) bool) func(*Env) bool {
	checkMu.Lock()
	noCacheChecks[fnPointer(fn)] = fn
	checkMu.Unlock()
	return fn
}

// CheckCacheEpoch changes whenever InvalidateCheckCache drops the cached verdicts
func CheckCacheEpoch() int64 {
	checkMu.Lock()
	defer checkMu.Unlock()
	return checkEpoch
}

// InvalidateCheckCache drops every cached availability verdict
func InvalidateCheckCache() {
	checkMu.Lock()
	defer checkMu.Unlock()
	checkEpoch++
	checkCache = map[checkKey]checkVerdict{}
	checkCacheOrder = nil
	checkLastGood = map[checkKey]time.Time{}
	checkGoodOrder = nil
	checkEverGood = map[checkKey]bool{}
	checkDropWarned = map[checkKey]bool{}
	checkPins = map[uintptr]func(*Env) bool{}
}

type verdictWindowKey struct{}

// VerdictWindow collects the earliest expiry of every cached verdict consulted inside it
type VerdictWindow struct {
	mu     sync.Mutex
	expiry time.Time
	set    bool
}

// Expiry returns the earliest consulted verdict deadline, zero when every verdict was uncached
func (w *VerdictWindow) Expiry() (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expiry, w.set
}

func (w *VerdictWindow) note(deadline time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.set || deadline.Before(w.expiry) {
		w.expiry, w.set = deadline, true
	}
}

// WithVerdictWindow binds a window that definition reads stamp with their verdict deadlines
func WithVerdictWindow(ctx context.Context) (context.Context, *VerdictWindow) {
	w := &VerdictWindow{}
	return context.WithValue(ctx, verdictWindowKey{}, w), w
}

func verdictWindowOf(ctx context.Context) *VerdictWindow {
	if ctx == nil {
		return nil
	}
	w, _ := ctx.Value(verdictWindowKey{}).(*VerdictWindow)
	return w
}

// Expires stale verdicts and evicts the oldest inserted entries past the cap, caller holds the lock
func pruneCheckCaches(now time.Time) {
	checkCacheOrder = pruneOrdered(checkCache, checkCacheOrder, func(k checkKey) bool { return now.Sub(checkCache[k].at) >= checkTTL })
	checkGoodOrder = pruneOrdered(checkLastGood, checkGoodOrder, func(k checkKey) bool { return now.Sub(checkLastGood[k]) >= checkGrace })
}

// Drops expired keys then the first inserted keys until the map is under the cap
func pruneOrdered[V any](m map[checkKey]V, order []checkKey, expired func(checkKey) bool) []checkKey {
	kept := order[:0]
	for _, k := range order {
		if _, ok := m[k]; !ok {
			continue
		}
		if expired(k) {
			delete(m, k)
			continue
		}
		kept = append(kept, k)
	}
	for len(kept) >= checkCacheMax {
		delete(m, kept[0])
		kept = kept[1:]
	}
	return kept
}

// Stores a verdict keeping the first insertion position of a key already present
func storeCheckVerdict(key checkKey, v checkVerdict) {
	if _, ok := checkCache[key]; !ok {
		checkCacheOrder = append(checkCacheOrder, key)
	}
	checkCache[key] = v
}

// Stores a last success stamp keeping the first insertion position of a key already present
func storeCheckLastGood(key checkKey, at time.Time) {
	if _, ok := checkLastGood[key]; !ok {
		checkGoodOrder = append(checkGoodOrder, key)
	}
	checkLastGood[key] = at
}

// Qualified function name of a probe for log lines
func fnLabel(fn func(*Env) bool) string {
	if f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()); f != nil {
		return f.Name()
	}
	return fmt.Sprintf("%p", fn)
}

// CheckFnCacheScope is the profile key availability verdicts are cached under, empty for the shared process cache
func CheckFnCacheScope(env *Env) (scope string, bypass bool) {
	if env == nil || env.unscopedChecks {
		return "", true
	}
	if !servesRoutedEnv(env) {
		return "", false
	}
	if env.Home == "" {
		return "", true
	}
	return ScopeKey(env.Home), false
}

func checkLogger(env *Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log
	}
	return slog.Default()
}

// runCheckUncached runs a probe and converts a panic into an unavailable verdict
func runCheckUncached(env *Env, fn func(*Env) bool) (value bool, panicked any) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = rec
			value = false
		}
	}()
	return fn(env), nil
}

// checkCached returns a tool's availability through the TTL cache with transient failure grace
func (r *Registry) checkCached(ctx context.Context, env *Env, fn func(*Env) bool) bool {
	ptr := fnPointer(fn)
	checkMu.Lock()
	uncached := noCacheChecks[ptr] != nil
	checkMu.Unlock()
	scope, bypass := CheckFnCacheScope(env)
	if uncached || bypass {
		return runCheckLogged(env, fn)
	}
	key := checkKey{ptr, scope}
	window := verdictWindowOf(ctx)
	now := checkNow()
	checkMu.Lock()
	pruneCheckCaches(now)
	if cached, ok := checkCache[key]; ok {
		checkMu.Unlock()
		window.note(cached.at.Add(checkTTL))
		return cached.value
	}
	checkMu.Unlock()
	value, panicked := runCheckUncached(env, fn)
	var coreDropped []string
	if !value {
		coreDropped = r.coreToolsGatedBy(ctx, scopeOf(env), fn)
	}
	now = checkNow()
	checkMu.Lock()
	defer checkMu.Unlock()
	pruneCheckCaches(now)
	checkPins[ptr] = fn
	if value {
		storeCheckLastGood(key, now)
		checkEverGood[key] = true
		storeCheckVerdict(key, checkVerdict{now, true})
		window.note(now.Add(checkTTL))
		return true
	}
	outcome := "returned False"
	if panicked != nil {
		outcome = "raised"
	}
	label := fnLabel(fn)
	if last, ok := checkLastGood[key]; ok && now.Sub(last) < checkGrace {
		reuse := now.Add(checkGraceReuse)
		if end := last.Add(checkGrace); end.Before(reuse) {
			reuse = end
		}
		window.note(reuse)
		checkLogger(env).Warn(fmt.Sprintf("check_fn %s failed (%s) within %.0fs of last success; treating as transient and keeping tool(s) available",
			label, outcome, checkGrace.Seconds()))
		return true
	}
	if len(coreDropped) > 0 && checkEverGood[key] && !checkDropWarned[key] {
		checkDropWarned[key] = true
		checkLogger(env).Warn(fmt.Sprintf("check_fn %s %s; previously available core tool(s) %s dropped (non-deferrable, so not searchable either); dependent tools will be unavailable this turn",
			label, outcome, strings.Join(coreDropped, ", ")), panicAttrs(panicked)...)
	} else if panicked != nil {
		checkLogger(env).Warn(fmt.Sprintf("check_fn %s %s; dependent tools will be unavailable this turn", label, outcome), panicAttrs(panicked)...)
	} else {
		checkLogger(env).Info(fmt.Sprintf("check_fn %s %s; dependent tools will be unavailable this turn", label, outcome))
	}
	storeCheckVerdict(key, checkVerdict{now, false})
	window.note(now.Add(checkTTL))
	return false
}

// Log attributes carrying a probe panic, none when the probe returned normally
func panicAttrs(panicked any) []any {
	if panicked == nil {
		return nil
	}
	return []any{"panic", panicked}
}

// Runs a probe outside the cache logging an unscoped secret read at debug when no profile scope is bound
func runCheckLogged(env *Env, fn func(*Env) bool) bool {
	v, p := runCheckUncached(env, fn)
	if p == nil {
		return v
	}
	label := fnLabel(fn)
	if err, ok := p.(error); ok && errors.Is(err, ErrUnscopedSecret) {
		if env == nil || env.Secrets == nil {
			checkLogger(env).Debug(fmt.Sprintf("check_fn %s hit the multiplex fail-closed path with no profile secret scope active; dependent tools re-probe on the first scoped turn", label))
		} else {
			checkLogger(env).Warn(fmt.Sprintf("check_fn %s raised UnscopedSecretError while the profile cache scope was resolved; dependent tools will be unavailable this turn", label), "panic", p)
		}
		return false
	}
	checkLogger(env).Warn(fmt.Sprintf("check_fn %s raised; dependent tools will be unavailable this turn", label), "panic", p)
	return false
}

// coreToolsGatedBy names core tools whose registered check is this probe
func (r *Registry) coreToolsGatedBy(ctx context.Context, scope string, fn func(*Env) bool) []string {
	ptr := fnPointer(fn)
	core := map[string]bool{}
	for _, n := range CoreTools {
		core[n] = true
	}
	var out []string
	for _, d := range r.Snapshot(ctx, scope) {
		if d.Check != nil && core[d.Name] && fnPointer(d.Check) == ptr {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

// checkMemo memoizes one definition pass so each distinct probe runs once
type checkMemo map[uintptr]bool

func (r *Registry) memoCheck(ctx context.Context, env *Env, fn func(*Env) bool, memo checkMemo) bool {
	ptr := fnPointer(fn)
	if v, ok := memo[ptr]; ok {
		return v
	}
	v := r.checkCached(ctx, env, fn)
	memo[ptr] = v
	return v
}

// CachedCheckResult returns a probe's live cached verdict without ever running it
func CachedCheckResult(env *Env, fn func(*Env) bool) (bool, bool) {
	scope, bypass := CheckFnCacheScope(env)
	if bypass {
		return false, false
	}
	key := checkKey{fnPointer(fn), scope}
	now := checkNow()
	checkMu.Lock()
	defer checkMu.Unlock()
	v, ok := checkCache[key]
	if !ok || now.Sub(v.at) >= checkTTL {
		return false, false
	}
	return v.value, true
}

// toolsetHasExposableTools reports whether any member of a toolset passes its check
func (r *Registry) toolsetHasExposableTools(ctx context.Context, env *Env, toolset string, entries []*Def) bool {
	memo := checkMemo{}
	for _, d := range entries {
		if d.Toolset != toolset {
			continue
		}
		if d.Check == nil || r.memoCheck(ctx, env, d.Check, memo) {
			return true
		}
	}
	return false
}

// CheckToolsetRequirements maps every visible toolset to whether it exposes a tool
func (r *Registry) CheckToolsetRequirements(ctx context.Context, env *Env) map[string]bool {
	entries := r.Snapshot(ctx, scopeOf(env))
	out := map[string]bool{}
	for ts := range grouped(entries) {
		out[ts] = r.toolsetHasExposableTools(ctx, env, ts, entries)
	}
	return out
}

// farFuture is the expiry used when a definitions pass consulted no cached verdict
var farFuture = time.Unix(math.MaxInt32, 0)
