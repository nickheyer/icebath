package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const pureMemoMax = 512

type stickyKey struct {
	identity any
	name     string
	hook     Hook
}

type Scope struct {
	mu     sync.Mutex
	failed map[stickyKey]bool
}

func NewScope() *Scope { return &Scope{failed: map[stickyKey]bool{}} }

func (s *Scope) has(k stickyKey) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed[k]
}

func (s *Scope) add(k stickyKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[k] = true
}

type policy struct {
	mu          sync.Mutex
	memo        map[string]any
	warned      map[string]bool
	errorWarned map[string]bool
	modeWarned  map[string]bool
}

func newPolicy() *policy {
	return &policy{memo: map[string]any{}, warned: map[string]bool{}, errorWarned: map[string]bool{}, modeWarned: map[string]bool{}}
}

func (r *Registry) pol() *policy { return r.policy }

func identityOf(p Profile) any {
	if p == nil {
		return nil
	}
	if t := reflect.TypeOf(p); t.Comparable() {
		return p
	}
	return fmt.Sprintf("%T", p)
}

func stickyOf(p Profile, hook Hook) stickyKey {
	return stickyKey{identity: identityOf(p), name: p.Spec().Name, hook: hook}
}

func HookFailed(scope *Scope, p Profile, hook Hook) bool {
	return p != nil && scope.has(stickyOf(p, hook))
}

func (r *Registry) RouteProfiles(ctx context.Context, hook Hook, provider, baseURL string) []Profile {
	var named Profile
	if provider != "" {
		named, _ = r.Get(ctx, provider)
	}
	ordered := []Profile{}
	if named != nil && Overrides(named, hook) {
		ordered = append(ordered, named)
	}
	if baseURL != "" {
		if hosted, ok := r.ForHost(ctx, baseURL, false); ok && identityOf(hosted) != identityOf(named) && Overrides(hosted, hook) {
			ordered = append(ordered, hosted)
		}
	}
	return ordered
}

func freezeArgs(args ...any) string {
	var b strings.Builder
	for _, a := range args {
		switch v := a.(type) {
		case map[string]string:
			keys := slices.Sorted(maps.Keys(v))
			b.WriteString("{")
			for _, k := range keys {
				b.WriteString(strconv.Quote(k) + ":" + strconv.Quote(v[k]) + ",")
			}
			b.WriteString("}")
		case *wire.Reasoning:
			if v == nil {
				b.WriteString("nil")
			} else {
				b.WriteString(strconv.FormatBool(v.Enabled) + strconv.Quote(v.Effort) + strconv.Quote(string(v.Payload)))
			}
		case string:
			b.WriteString(strconv.Quote(v))
		default:
			fmt.Fprintf(&b, "%v", v)
		}
		b.WriteString("|")
	}
	return b.String()
}

func cloneAnswer(v any) any {
	switch x := v.(type) {
	case map[string]string:
		return maps.Clone(x)
	case json.RawMessage:
		return slices.Clone(x)
	case []string:
		return slices.Clone(x)
	}
	return v
}

func askProfile[T any](ctx context.Context, r *Registry, scope *Scope, p Profile, hook Hook, pure bool, fallback T, args []any, call func() (T, error)) T {
	sticky := stickyOf(p, hook)
	if scope.has(sticky) {
		return fallback
	}
	pol := r.pol()
	key := ""
	if pure {
		key = fmt.Sprintf("%d|%v|%T|%s|%s", r.Generation(), sticky.identity, p, hook, freezeArgs(args...))
		pol.mu.Lock()
		cached, ok := pol.memo[key]
		pol.mu.Unlock()
		if ok {
			return cloneAnswer(cached).(T)
		}
	}
	answer, err := call()
	if err != nil {
		scope.add(sticky)
		warnKey := sticky.name + "|" + string(hook)
		pol.mu.Lock()
		first := !pol.warned[warnKey]
		pol.warned[warnKey] = true
		pol.mu.Unlock()
		if scope != nil || first {
			slog.Warn(fmt.Sprintf("provider %s: %s hook failed (%v); using the generic answer for the rest of the session", sticky.name, hook, err))
		}
		return fallback
	}
	if pure {
		pol.mu.Lock()
		if len(pol.memo) >= pureMemoMax {
			clear(pol.memo)
		}
		pol.memo[key] = cloneAnswer(answer)
		pol.mu.Unlock()
	}
	return answer
}

func askOnErrorPath[T any](r *Registry, p Profile, hook Hook, fallback T, call func() (T, error)) T {
	answer, err := call()
	if err == nil {
		return answer
	}
	pol := r.pol()
	key := p.Spec().Name + "|" + string(hook)
	pol.mu.Lock()
	first := !pol.errorWarned[key]
	pol.errorWarned[key] = true
	pol.mu.Unlock()
	if first {
		slog.Warn(fmt.Sprintf("provider %s: %s hook failed; handling the API error without it", p.Spec().Name, hook), "error", err)
	}
	return fallback
}

func (r *Registry) ClientHeaders(ctx context.Context, scope *Scope, provider, baseURL string, apiKey auth.Secret, images bool) map[string]string {
	for _, p := range r.RouteProfiles(ctx, HookClientHeaders, provider, baseURL) {
		h := p.(ClientHeaderer)
		answer := askProfile(ctx, r, scope, p, HookClientHeaders, false, map[string]string(nil), nil, func() (map[string]string, error) {
			headers, ok, err := h.ClientHeaders(ctx, baseURL, apiKey, images)
			if !ok {
				return nil, err
			}
			return headers, err
		})
		if len(answer) > 0 {
			return answer
		}
	}
	return nil
}

func (r *Registry) RequestHeaders(ctx context.Context, scope *Scope, provider, baseURL, model string, userInitiated bool) map[string]string {
	for _, p := range r.RouteProfiles(ctx, HookRequestHeaders, provider, baseURL) {
		h := p.(RequestHeaderer)
		answer := askProfile(ctx, r, scope, p, HookRequestHeaders, true, map[string]string(nil),
			[]any{baseURL, model, userInitiated}, func() (map[string]string, error) {
				return h.RequestHeaders(ctx, baseURL, model, userInitiated)
			})
		if len(answer) > 0 {
			return answer
		}
	}
	return nil
}

func (r *Registry) MaxTokensField(ctx context.Context, scope *Scope, provider, baseURL, model string) (wire.MaxTokensField, bool) {
	for _, p := range r.RouteProfiles(ctx, HookMaxTokensParam, provider, baseURL) {
		h := p.(MaxTokensFielder)
		answer := askProfile(ctx, r, scope, p, HookMaxTokensParam, true, wire.MaxTokensField(0),
			[]any{baseURL, model}, func() (wire.MaxTokensField, error) {
				field, ok, err := h.MaxTokensField(ctx, baseURL, model)
				if !ok {
					return 0, err
				}
				return field, err
			})
		if answer != 0 {
			return answer, true
		}
	}
	return 0, false
}

func (r *Registry) ReasoningPayload(ctx context.Context, scope *Scope, p Profile, model string, rc *wire.Reasoning, baseURL string) (json.RawMessage, bool) {
	h, ok := p.(ReasoningPayloader)
	if !ok || !Overrides(p, HookReasoningPayload) {
		return nil, false
	}
	answer := askProfile(ctx, r, scope, p, HookReasoningPayload, true, json.RawMessage(nil),
		[]any{model, rc, baseURL}, func() (json.RawMessage, error) {
			return h.ReasoningPayload(ctx, model, rc, baseURL)
		})
	return answer, len(answer) > 0
}

func (r *Registry) ReplaysFunctionSignature(ctx context.Context, scope *Scope, p Profile, model, baseURL string) bool {
	h, ok := p.(SignatureReplayer)
	if !ok || !Overrides(p, HookReplaysSignature) {
		return false
	}
	return askProfile(ctx, r, scope, p, HookReplaysSignature, true, false, []any{model, baseURL}, func() (bool, error) {
		return h.ReplaysFunctionSignature(ctx, model, baseURL)
	})
}

func (r *Registry) ReplayCarriers(ctx context.Context, scope *Scope, p Profile, values map[string]string, model, baseURL string) (map[string]string, bool) {
	h, ok := p.(CarrierReplayer)
	if !ok || !Overrides(p, HookReplayCarriers) {
		return nil, false
	}
	type result struct {
		values map[string]string
		ok     bool
	}
	answer := askProfile(ctx, r, scope, p, HookReplayCarriers, false, result{}, nil, func() (result, error) {
		out, err := h.ReplayCarriers(ctx, values, model, baseURL)
		return result{values: out, ok: err == nil}, err
	})
	return answer.values, answer.ok
}

func (r *Registry) RefreshRuntime(ctx context.Context, p Profile, reason string) (auth.Secret, string, bool) {
	h, ok := p.(RuntimeRefresher)
	if !ok || !Overrides(p, HookRefreshRuntime) {
		return auth.Secret{}, "", false
	}
	type result struct {
		key     auth.Secret
		baseURL string
		ok      bool
	}
	answer := askOnErrorPath(r, p, HookRefreshRuntime, result{}, func() (result, error) {
		key, baseURL, ok, err := h.RefreshRuntime(ctx, reason)
		return result{key: key, baseURL: baseURL, ok: ok}, err
	})
	return answer.key, answer.baseURL, answer.ok
}

func (r *Registry) RefreshRuntimeRoute(ctx context.Context, provider, baseURL, reason string) (auth.Secret, string, bool) {
	for _, p := range r.RouteProfiles(ctx, HookRefreshRuntime, strings.ToLower(strings.TrimSpace(provider)), baseURL) {
		if key, url, ok := r.RefreshRuntime(ctx, p, reason); ok {
			return key, url, true
		}
	}
	return auth.Secret{}, "", false
}

func (r *Registry) IsStale(ctx context.Context, p Profile, status int, message string) bool {
	h, ok := p.(StaleDetector)
	if !ok || !Overrides(p, HookIsStale) {
		return false
	}
	return askOnErrorPath(r, p, HookIsStale, false, func() (bool, error) { return h.IsStale(status, message) })
}

func (r *Registry) RefreshNotice(ctx context.Context, p Profile, reason string) string {
	h, ok := p.(RefreshNoticer)
	if !ok || !Overrides(p, HookRefreshNotice) {
		return ""
	}
	return askOnErrorPath(r, p, HookRefreshNotice, "", func() (string, error) { return h.RefreshNotice(reason) })
}

func (r *Registry) RejectionHint(ctx context.Context, p Profile, status int, baseURL string) []string {
	h, ok := p.(RejectionHinter)
	if !ok || !Overrides(p, HookRejectionHint) {
		return nil
	}
	return askOnErrorPath(r, p, HookRejectionHint, []string(nil), func() ([]string, error) { return h.RejectionHint(status, baseURL) })
}

func (r *Registry) ModeForModel(ctx context.Context, p Profile, model string, apiKey *auth.Secret) (wire.Mode, bool) {
	h, ok := p.(ModeChooser)
	if !ok || !Overrides(p, HookModeForModel) {
		return "", false
	}
	mode, ok, err := h.ModeForModel(ctx, model, apiKey)
	if err == nil {
		return mode, ok
	}
	pol := r.pol()
	name := p.Spec().Name
	pol.mu.Lock()
	first := !pol.modeWarned[name]
	pol.modeWarned[name] = true
	pol.mu.Unlock()
	if first {
		slog.Warn(fmt.Sprintf("provider %s: api_mode_for_model failed; using its declared api_mode", name), "error", err)
	}
	return p.Spec().ModeOrDefault(), true
}
