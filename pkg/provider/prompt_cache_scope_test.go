package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// In memory compression lineage store recording every lineage walk
type scopeFakeDB struct {
	lineage map[string][]string
	calls   []string
}

func (d *scopeFakeDB) GetCompressionLineage(sid string) ([]string, error) {
	d.calls = append(d.calls, sid)
	if l, ok := d.lineage[sid]; ok {
		return l, nil
	}
	return []string{}, nil
}

// Records a compression rotation from parent to child the way the session store lineage reports it
func (d *scopeFakeDB) rotate(parent, child string) {
	root := parent
	if l, ok := d.lineage[parent]; ok && len(l) > 0 {
		root = l[0]
	}
	chain := append([]string{}, d.lineage[root]...)
	if len(chain) == 0 {
		chain = []string{root}
	}
	chain = append(chain, child)
	for _, id := range chain {
		d.lineage[id] = chain
	}
}

func (d *scopeFakeDB) create(id string) { d.lineage[id] = []string{id} }

type scopeBoomDB struct{}

func (scopeBoomDB) GetCompressionLineage(string) ([]string, error) {
	return nil, errors.New("db exploded")
}

type scopeWeirdDB struct{ lineage []string }

func (d scopeWeirdDB) GetCompressionLineage(string) ([]string, error) { return d.lineage, nil }

func newScopeFakeDB() *scopeFakeDB { return &scopeFakeDB{lineage: map[string][]string{}} }

func TestCacheScopeResolvePromptCacheScope(t *testing.T) {
	t.Run("no_session_id_returns_empty", func(t *testing.T) {
		if ResolvePromptCacheScope(&CacheScopeAgent{}) != "" {
			t.Fatal("empty session id must give an empty scope")
		}
	})
	t.Run("no_db_falls_back_to_physical_id", func(t *testing.T) {
		if ResolvePromptCacheScope(&CacheScopeAgent{SessionID: "root-sess"}) != "root-sess" {
			t.Fatal("physical id expected")
		}
	})
	t.Run("memoized_per_segment", func(t *testing.T) {
		db := newScopeFakeDB()
		db.create("root-sess")
		db.rotate("root-sess", "rotated-1")
		a := &CacheScopeAgent{SessionID: "rotated-1", SessionDB: db}
		if ResolvePromptCacheScope(a) != "root-sess" {
			t.Fatal("rotation child must resolve to the root")
		}
		db.calls = nil
		if ResolvePromptCacheScope(a) != "root-sess" || len(db.calls) != 0 {
			t.Fatalf("memo miss, walks %v", db.calls)
		}
		db.rotate("rotated-1", "rotated-2")
		a.SessionID = "rotated-2"
		if ResolvePromptCacheScope(a) != "root-sess" || fmt.Sprint(db.calls) != "[rotated-2]" {
			t.Fatalf("rotation must re walk once, walks %v", db.calls)
		}
	})
	t.Run("db_failure_falls_back_to_physical_id", func(t *testing.T) {
		if ResolvePromptCacheScope(&CacheScopeAgent{SessionID: "sess-x", SessionDB: scopeBoomDB{}}) != "sess-x" {
			t.Fatal("physical id expected on a failing store")
		}
	})
	t.Run("failed_walk_is_not_pinned", func(t *testing.T) {
		db := newScopeFakeDB()
		a := &CacheScopeAgent{SessionID: "late-row", SessionDB: db}
		if ResolvePromptCacheScope(a) != "late-row" {
			t.Fatal("pre persist fallback must be the physical id")
		}
		db.create("late-root")
		db.rotate("late-root", "late-row")
		if ResolvePromptCacheScope(a) != "late-root" {
			t.Fatal("a failed walk was memoized")
		}
	})
	t.Run("persist_disabled_agent_is_memoized_despite_missing_row", func(t *testing.T) {
		db := newScopeFakeDB()
		a := &CacheScopeAgent{SessionID: "review-fork", SessionDB: db, PersistDisabled: true}
		if ResolvePromptCacheScope(a) != "review-fork" {
			t.Fatal("physical id expected")
		}
		db.calls = nil
		if ResolvePromptCacheScope(a) != "review-fork" || len(db.calls) != 0 {
			t.Fatalf("persist disabled fallback not memoized, walks %v", db.calls)
		}
	})
	t.Run("db_attached_later_re_resolves", func(t *testing.T) {
		db := newScopeFakeDB()
		db.create("root-sess")
		db.rotate("root-sess", "rotated-1")
		a := &CacheScopeAgent{SessionID: "rotated-1"}
		if ResolvePromptCacheScope(a) != "rotated-1" {
			t.Fatal("store less agent must use the physical id")
		}
		a.SessionDB = db
		if ResolvePromptCacheScope(a) != "root-sess" {
			t.Fatal("lazy store attach must re resolve")
		}
	})
	t.Run("bogus_lineage_shape_falls_back", func(t *testing.T) {
		for _, l := range [][]string{nil, {}, {""}} {
			if ResolvePromptCacheScope(&CacheScopeAgent{SessionID: "sess-y", SessionDB: scopeWeirdDB{l}}) != "sess-y" {
				t.Fatalf("lineage %q must fall back", l)
			}
		}
	})
	t.Run("safe_variant_never_raises", func(t *testing.T) {
		hostile := &CacheScopeAgent{SessionID: "sess-h", PromptCacheForkTag: "review", Provider: "xai",
			CompressionCount: func() int { panic("hostile property") }}
		if ResolvePromptCacheScopeSafe(hostile) != "" || DeclaredConversationScopeSafe(&CacheScopeAgent{GatewaySessionKey: "k", Platform: "cli",
			SessionID: "s", SessionDB: scopeHostileDB{}}) != "" {
			t.Fatal("safe variants must swallow the panic")
		}
		if ResolvePromptCacheScopeSafe(&CacheScopeAgent{SessionID: "sess-ok"}) != "sess-ok" || ResolvePromptCacheScopeSafe(&CacheScopeAgent{}) != "" {
			t.Fatal("safe variant must resolve through to the plain one")
		}
	})
}

type scopeHostileDB struct{}

func (scopeHostileDB) IsExplicitForkChild(string) (bool, error) { panic("hostile store") }

func scopeKeyFor(a *CacheScopeAgent) string {
	return contentCacheKey("You are a helpful assistant.", []map[string]any{{"type": "function", "name": "terminal"}}, cacheScopeFromSessionID(ResolvePromptCacheScope(a)))
}

func scopeCodexKwargs(t *testing.T, model string, msgs []*state.Message, p codexParams) map[string]any {
	return build(t, newCodexTransport(), model, msgs, nil, p)
}

func scopeSysHi() []*state.Message { return []*state.Message{cm("system", "sys"), cm("user", "hi")} }

func scopeHeader(kw map[string]any, name string) string {
	switch h := kw["extra_headers"].(type) {
	case map[string]string:
		return h[name]
	case map[string]any:
		return stringOf(h[name])
	}
	return ""
}

// Chat completions build on a direct OpenAI route returning the prompt cache key
func scopeChatKey(t *testing.T, sessionID, scope string) string {
	c := newCCRoute(Route{Provider: "openai", Model: "gpt-5.5", BaseURL: "https://api.openai.com/v1", Mode: ModeChatCompletions})
	b := c.build(context.Background(), &Request{Model: "gpt-5.5", Messages: []*state.Message{cm("system", "sys")}, SessionID: sessionID, CacheScope: scope}, wireSettings{})
	v, _ := b.body.Get("prompt_cache_key")
	return stringOf(v)
}

func TestAnthropicPromptCachePolicyWrapsUnexported(t *testing.T) {
	ctx := context.Background()
	disabled := false
	prov, base, mode, model := "anthropic", "https://api.anthropic.com", "anthropic_messages", "claude-sonnet-4"
	dest := CacheDestination{Provider: &prov, BaseURL: &base, APIMode: &mode, Model: &model}
	wantCache, wantNative := anthropicPromptCachePolicy(ctx, BlankCachePolicyAgent(ctx, &disabled), dest)
	gotCache, gotNative := AnthropicPromptCachePolicy(ctx, BlankCachePolicyAgent(ctx, &disabled), dest)
	if gotCache != wantCache || gotNative != wantNative {
		t.Fatalf("got (%v, %v) want (%v, %v)", gotCache, gotNative, wantCache, wantNative)
	}
}
