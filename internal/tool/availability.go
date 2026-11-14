package tool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/secret"
)

const (
	checkTTL        = 30 * time.Second
	checkGrace      = 60 * time.Second
	checkGraceReuse = 5 * time.Second
	checkCacheMax   = 512
)

type cacheKey struct {
	check   *Check
	profile string
}

type verdict struct {
	at time.Time
	ok bool
}

type orderedCache[V any] struct {
	order []cacheKey
	vals  map[cacheKey]V
}

func (c *orderedCache[V]) get(k cacheKey) (V, bool) {
	v, ok := c.vals[k]
	return v, ok
}

func (c *orderedCache[V]) set(k cacheKey, v V) {
	if c.vals == nil {
		c.vals = map[cacheKey]V{}
	}
	if _, ok := c.vals[k]; !ok {
		c.order = append(c.order, k)
	}
	c.vals[k] = v
}

func (c *orderedCache[V]) prune(expired func(V) bool) {
	kept := c.order[:0]
	for _, k := range c.order {
		if expired(c.vals[k]) {
			delete(c.vals, k)
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
	for len(c.order) >= checkCacheMax {
		delete(c.vals, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *orderedCache[V]) clear() {
	c.order = nil
	c.vals = nil
}

type checkCache struct {
	mu       sync.Mutex
	verdicts orderedCache[verdict]
	lastGood orderedCache[time.Time]
	everGood map[cacheKey]bool
	warned   map[cacheKey]bool
	epoch    uint64
}

func newCheckCache() checkCache {
	return checkCache{everGood: map[cacheKey]bool{}, warned: map[cacheKey]bool{}}
}

func (c *checkCache) prune(now time.Time) {
	c.verdicts.prune(func(v verdict) bool { return now.Sub(v.at) >= checkTTL })
	c.lastGood.prune(func(t time.Time) bool { return now.Sub(t) >= checkGrace })
}

type checkPass struct {
	r       *Registry
	at      Scope
	entries []*Entry
	memo    map[*Check]bool
	expiry  time.Time
}

func (r *Registry) newPass(at Scope, entries []*Entry) *checkPass {
	return &checkPass{r: r, at: at, entries: entries, memo: map[*Check]bool{}}
}

func (p *checkPass) note(deadline time.Time) {
	if p.expiry.IsZero() || deadline.Before(p.expiry) {
		p.expiry = deadline
	}
}

func (p *checkPass) ok(ctx context.Context, c *Check) bool {
	if v, seen := p.memo[c]; seen {
		return v
	}
	v := p.r.probe(ctx, c, p)
	p.memo[c] = v
	return v
}

func checkLabel(c *Check) string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("%p", c)
}

func (r *Registry) probeUncached(ctx context.Context, c *Check) bool {
	ok, err := c.Probe(ctx)
	if err == nil {
		return ok
	}
	label := checkLabel(c)
	switch {
	case errors.Is(err, secret.ErrUnscoped) && (r.secrets == nil || !r.secrets.Bound(ctx)):
		slog.Debug(fmt.Sprintf("check_fn %s hit the multiplex fail-closed path with no profile secret scope active; dependent tools re-probe on the first scoped turn", label))
	case errors.Is(err, secret.ErrUnscoped):
		slog.Warn(fmt.Sprintf("check_fn %s raised an unscoped secret read while the profile cache scope was resolved; dependent tools will be unavailable this turn: %v", label, err))
	default:
		slog.Warn(fmt.Sprintf("check_fn %s raised; dependent tools will be unavailable this turn: %v", label, err))
	}
	return false
}

func (r *Registry) probe(ctx context.Context, c *Check, p *checkPass) bool {
	if c.Uncached {
		return r.probeUncached(ctx, c)
	}
	key := cacheKey{check: c, profile: p.at.Profile}
	cc := &r.checks
	cc.mu.Lock()
	cc.prune(time.Now())
	if v, ok := cc.verdicts.get(key); ok {
		cc.mu.Unlock()
		p.note(v.at.Add(checkTTL))
		return v.ok
	}
	cc.mu.Unlock()
	value, err := c.Probe(ctx)
	if err != nil {
		value = false
	}
	outcome := "returned false"
	if err != nil {
		outcome = "raised: " + err.Error()
	}
	var coreDropped []string
	if !value {
		coreDropped = p.coreGatedBy(c)
	}
	label := checkLabel(c)
	now := time.Now()
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.prune(now)
	if value {
		cc.lastGood.set(key, now)
		cc.everGood[key] = true
		cc.verdicts.set(key, verdict{at: now, ok: true})
		p.note(now.Add(checkTTL))
		return true
	}
	if last, ok := cc.lastGood.get(key); ok && now.Sub(last) < checkGrace {
		reuse := now.Add(checkGraceReuse)
		if end := last.Add(checkGrace); end.Before(reuse) {
			reuse = end
		}
		p.note(reuse)
		slog.Warn(fmt.Sprintf("check_fn %s failed (%s) within %.0fs of last success; treating as transient and keeping tool(s) available", label, outcome, checkGrace.Seconds()))
		return true
	}
	if len(coreDropped) > 0 && cc.everGood[key] && !cc.warned[key] {
		cc.warned[key] = true
		slog.Warn(fmt.Sprintf("check_fn %s %s; previously available core tool(s) %s dropped (non-deferrable, so not searchable either); dependent tools will be unavailable this turn", label, outcome, strings.Join(coreDropped, ", ")))
	} else if err != nil {
		slog.Warn(fmt.Sprintf("check_fn %s %s; dependent tools will be unavailable this turn", label, outcome))
	} else {
		slog.Info(fmt.Sprintf("check_fn %s %s; dependent tools will be unavailable this turn", label, outcome))
	}
	cc.verdicts.set(key, verdict{at: now, ok: false})
	p.note(now.Add(checkTTL))
	return false
}

func (p *checkPass) coreGatedBy(c *Check) []string {
	core := CoreTools()
	var out []string
	for _, e := range p.entries {
		if e.Tool.Check == c && slices.Contains(core, e.Tool.Name) {
			out = append(out, e.Tool.Name)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Registry) InvalidateChecks() {
	cc := &r.checks
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.epoch++
	cc.verdicts.clear()
	cc.lastGood.clear()
	cc.everGood = map[cacheKey]bool{}
	cc.warned = map[cacheKey]bool{}
}

func (r *Registry) checkEpoch() uint64 {
	cc := &r.checks
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.epoch
}

func (r *Registry) CachedVerdict(c *Check, at Scope) (bool, bool) {
	cc := &r.checks
	cc.mu.Lock()
	defer cc.mu.Unlock()
	v, ok := cc.verdicts.get(cacheKey{check: c, profile: at.Profile})
	if !ok || time.Since(v.at) >= checkTTL {
		return false, false
	}
	return v.ok, true
}

func (r *Registry) Availability(ctx context.Context, at Scope) []ToolsetStatus {
	r.mu.RLock()
	entries := r.merged(at)
	checks := make(map[string]bool, len(r.toolsetChecks))
	for name, c := range r.toolsetChecks {
		checks[name] = c != nil
	}
	r.mu.RUnlock()
	var order []string
	groups := map[string][]*Entry{}
	for _, e := range entries {
		if _, seen := groups[e.Tool.Toolset]; !seen {
			order = append(order, e.Tool.Toolset)
		}
		groups[e.Tool.Toolset] = append(groups[e.Tool.Toolset], e)
	}
	pass := r.newPass(at, entries)
	out := make([]ToolsetStatus, 0, len(order))
	for _, name := range order {
		members := groups[name]
		st := ToolsetStatus{Name: name, HasCheck: checks[name], LeadEnv: slices.Clone(members[0].Tool.RequiresEnv)}
		for _, e := range members {
			st.Tools = append(st.Tools, e.Tool.Name)
			for _, v := range e.Tool.RequiresEnv {
				if !slices.Contains(st.Requirements, v) {
					st.Requirements = append(st.Requirements, v)
				}
			}
			if e.Tool.Check != nil {
				st.HasCheck = true
			}
		}
		for _, e := range members {
			if e.Tool.Check == nil || pass.ok(ctx, e.Tool.Check) {
				st.Available = true
				break
			}
		}
		out = append(out, st)
	}
	return out
}
