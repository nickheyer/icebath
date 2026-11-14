package provider

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Real session store under a temp home, the hermes SessionDB(db_path=tmp_path / "state.db") fixture
func scopeRealDB(t *testing.T) *state.SessionDB {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func scopeCreate(t *testing.T, db *state.SessionDB, n state.NewSession) {
	t.Helper()
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
}

func scopeEnd(t *testing.T, db *state.SessionDB, id, reason string) {
	t.Helper()
	if err := db.EndSession(id, reason); err != nil {
		t.Fatal(err)
	}
}

// Legacy mode compression rotation parent to child
func scopeRotate(t *testing.T, db *state.SessionDB, parent, child string) {
	t.Helper()
	scopeEnd(t, db, parent, "compression")
	scopeCreate(t, db, state.NewSession{ID: child, Source: "webui", ParentSessionID: parent})
}

func scopeAgentOn(db *state.SessionDB, sessionID string) *CacheScopeAgent {
	a := &CacheScopeAgent{SessionID: sessionID}
	if db != nil {
		a.SessionDB = CacheScopeSessionStore{DB: db}
	}
	return a
}

func TestResolvePromptCacheScopeRotationChildInheritsRootScope(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "webui"})
	scopeRotate(t, db, "root-sess", "rotated-1")
	if got := ResolvePromptCacheScope(scopeAgentOn(db, "rotated-1")); got != "root-sess" {
		t.Fatalf("%q", got)
	}
}

func TestResolvePromptCacheScopeChainedRotationsShareOneScope(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "webui"})
	scopeRotate(t, db, "root-sess", "rotated-1")
	scopeRotate(t, db, "rotated-1", "rotated-2")
	if got := ResolvePromptCacheScope(scopeAgentOn(db, "rotated-2")); got != "root-sess" {
		t.Fatalf("%q", got)
	}
}

func TestResolvePromptCacheScopeNewSessionGetsFreshScope(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "old-conv", Source: "webui"})
	scopeRotate(t, db, "old-conv", "old-rotated")
	scopeCreate(t, db, state.NewSession{ID: "new-conv", Source: "webui"})
	if got := ResolvePromptCacheScope(scopeAgentOn(db, "new-conv")); got != "new-conv" {
		t.Fatalf("%q", got)
	}
}

func TestResolvePromptCacheScopeBranchChildStaysIsolated(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "webui"})
	scopeEnd(t, db, "root-sess", "compression")
	scopeCreate(t, db, state.NewSession{ID: "branch-child", Source: "webui", ParentSessionID: "root-sess", ModelConfig: map[string]any{"_branched_from": "root-sess"}})
	if got := ResolvePromptCacheScope(scopeAgentOn(db, "branch-child")); got != "branch-child" {
		t.Fatalf("%q", got)
	}
}

func TestResolvePromptCacheScopeDelegateChildStaysIsolated(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "parent-sess", Source: "webui"})
	scopeEnd(t, db, "parent-sess", "compression")
	scopeCreate(t, db, state.NewSession{ID: "delegate-child", Source: "webui", ParentSessionID: "parent-sess", ModelConfig: map[string]any{"_delegate_from": "parent-sess"}})
	if got := ResolvePromptCacheScope(scopeAgentOn(db, "delegate-child")); got != "delegate-child" {
		t.Fatalf("%q", got)
	}
}

func TestResolvePromptCacheScopeToolChildStaysIsolated(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "parent-sess", Source: "webui"})
	scopeEnd(t, db, "parent-sess", "compression")
	scopeCreate(t, db, state.NewSession{ID: "tool-child", Source: "tool", ParentSessionID: "parent-sess"})
	if got := ResolvePromptCacheScope(scopeAgentOn(db, "tool-child")); got != "tool-child" {
		t.Fatalf("%q", got)
	}
}

func TestRotationContinuityEndToEndRotationKeepsPromptCacheKeyStable(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "webui"})
	before := scopeKeyFor(scopeAgentOn(db, "root-sess"))
	scopeRotate(t, db, "root-sess", "rotated-1")
	if after := scopeKeyFor(scopeAgentOn(db, "rotated-1")); after != before {
		t.Fatal("rotation moved the prompt cache key")
	}
}

func TestRotationContinuityEndToEndSiblingForksKeepDistinctKeys(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "parent-sess", Source: "webui"})
	scopeEnd(t, db, "parent-sess", "compression")
	for _, child := range []string{"delegate-a", "delegate-b"} {
		scopeCreate(t, db, state.NewSession{ID: child, Source: "webui", ParentSessionID: "parent-sess", ModelConfig: map[string]any{"_delegate_from": "parent-sess"}})
	}
	if scopeKeyFor(scopeAgentOn(db, "delegate-a")) == scopeKeyFor(scopeAgentOn(db, "delegate-b")) {
		t.Fatal("sibling forks share a key")
	}
}

func TestTransportWiringCodexBuildKwargsPrefersCacheScopeId(t *testing.T) {
	k1 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: "root-sess", cacheScopeID: "root-sess"})
	k2 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: "rotated-1", cacheScopeID: "root-sess"})
	k3 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: "rotated-1"})
	if k1["prompt_cache_key"] != k2["prompt_cache_key"] || k3["prompt_cache_key"] == k1["prompt_cache_key"] {
		t.Fatalf("codex keys %v %v %v", k1["prompt_cache_key"], k2["prompt_cache_key"], k3["prompt_cache_key"])
	}
}

func TestTransportWiringCodexSessionHeaderKeepsPhysicalId(t *testing.T) {
	kw := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: "rotated-1", cacheScopeID: "root-sess", isCodexBackend: true})
	if scopeHeader(kw, "session_id") != "rotated-1" || scopeHeader(kw, "x-client-request-id") != stringOf(kw["prompt_cache_key"]) {
		t.Fatalf("codex backend headers %#v key %v", kw["extra_headers"], kw["prompt_cache_key"])
	}
}

func TestTransportWiringXaiConvIdUsesLogicalScope(t *testing.T) {
	kw := scopeCodexKwargs(t, "grok-4", scopeSysHi(), codexParams{sessionID: "rotated-1", cacheScopeID: "root-sess", isXAI: true})
	if scopeHeader(kw, "x-grok-conv-id") != "root-sess" {
		t.Fatalf("xai conv id %#v", kw["extra_headers"])
	}
}

func TestTransportWiringChatCompletionsPrefersCacheScopeId(t *testing.T) {
	if scopeChatKey(t, "root-sess", "root-sess") != scopeChatKey(t, "rotated-1", "root-sess") || scopeChatKey(t, "rotated-1", "") == scopeChatKey(t, "rotated-1", "root-sess") {
		t.Fatal("chat completions must prefer the cache scope")
	}
}

func TestTransportWiringCronNormalizationStillAppliesToScope(t *testing.T) {
	c1 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: "cron_backup_20260814_120000", cacheScopeID: "cron_backup_20260814_120000"})
	c2 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: "cron_backup_20260815_120000", cacheScopeID: "cron_backup_20260815_120000"})
	if c1["prompt_cache_key"] != c2["prompt_cache_key"] {
		t.Fatal("cron fires must share a key")
	}
}

const nonceRun = "gc_run_room42_default_Worker"
const nonceResponse1 = nonceRun + "_5f2c1ab9d4e34f7a8b0c6d1e2f3a4b5c"
const nonceResponse2 = nonceRun + "_9a7e3b1c05d24e6fb83a1c7d9e0f2a4b"

func noncePromptCacheKey(t *testing.T, sid string) any {
	return scopeCodexKwargs(t, "gpt-5.5", []*state.Message{cm("system", "same")}, codexParams{sessionID: sid})["prompt_cache_key"]
}

func TestPerResponseRunNonceIsolationExternalUuidIdentityRemainsIsolated(t *testing.T) {
	first, second := "customer_chat_11111111111141118111111111111111", "customer_chat_22222222222242228222222222222222"
	if cacheScopeFromSessionID(first) == cacheScopeFromSessionID(second) || noncePromptCacheKey(t, first) == noncePromptCacheKey(t, second) {
		t.Fatal("external uuid identities merged")
	}
}

func TestPerResponseRunNonceIsolationStudioTruncationDoesNotMergeMembers(t *testing.T) {
	common := strings.Repeat("A", 70)
	m1 := ("gc_run_mabc1234qwerty_default_" + common + "Worker")[:96] + "_11111111111141118111111111111111"
	m2 := ("gc_run_mabc1234qwerty_default_" + common + "Reviewer")[:96] + "_22222222222242228222222222222222"
	if m1 == m2 || cacheScopeFromSessionID(m1) == cacheScopeFromSessionID(m2) {
		t.Fatal("studio members merged")
	}
}

func TestPerResponseRunNonceIsolationCronNormalizationStaysTheOnlyCarveOut(t *testing.T) {
	if cacheScopeFromSessionID("cron_backup_20260814_120000") != "cron_backup" || cacheScopeFromSessionID(nonceResponse1) != nonceResponse1 {
		t.Fatal("cron must stay the only carve out")
	}
}

func TestPerResponseRunNonceIsolationParentlessRowsResolveToTheirOwnScope(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: nonceResponse1, Source: "studio"})
	scopeCreate(t, db, state.NewSession{ID: nonceResponse2, Source: "studio"})
	if ResolvePromptCacheScope(scopeAgentOn(db, nonceResponse1)) != nonceResponse1 || ResolvePromptCacheScope(scopeAgentOn(db, nonceResponse2)) != nonceResponse2 {
		t.Fatal("parentless rows must resolve to themselves")
	}
}

func TestPerResponseRunNonceIsolationDistinctIdsKeepDistinctAffinityKeys(t *testing.T) {
	base1 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: nonceResponse1})
	base2 := scopeCodexKwargs(t, "gpt-5.5", scopeSysHi(), codexParams{sessionID: nonceResponse2})
	x1 := scopeCodexKwargs(t, "grok-4", scopeSysHi(), codexParams{sessionID: nonceResponse1, isXAI: true})
	x2 := scopeCodexKwargs(t, "grok-4", scopeSysHi(), codexParams{sessionID: nonceResponse2, isXAI: true})
	if base1["prompt_cache_key"] == base2["prompt_cache_key"] || scopeHeader(x1, "x-grok-conv-id") == scopeHeader(x2, "x-grok-conv-id") || scopeChatKey(t, nonceResponse1, "") == scopeChatKey(t, nonceResponse2, "") {
		t.Fatal("affinity keys merged")
	}
}

func TestPerResponseRunNonceIsolationDistinctIdsKeepDistinctProviderStickyKeys(t *testing.T) {
	for _, p := range []struct{ provider, base string }{{"openrouter", "https://openrouter.ai/api/v1"}, {"nous", "https://inference-api.nousresearch.com/v1"}} {
		t.Run(p.provider, func(t *testing.T) {
			var keys []any
			for _, sid := range []string{nonceResponse1, nonceResponse2} {
				c := newCCRoute(Route{Provider: p.provider, Model: "anthropic/claude-sonnet-4.6", BaseURL: p.base, Mode: ModeChatCompletions})
				b := c.build(context.Background(), &Request{Model: "anthropic/claude-sonnet-4.6", Messages: scopeSysHi(), SessionID: sid}, wireSettings{})
				v, ok := b.extraBody.Get("session_id")
				if !ok {
					v, ok = b.body.Get("session_id")
				}
				if !ok || stringOf(v) != sid {
					t.Fatalf("%s sticky session id %#v", p.provider, v)
				}
				keys = append(keys, v)
			}
			if keys[0] == keys[1] {
				t.Fatalf("%s sticky keys merged", p.provider)
			}
		})
	}
}

// Review fork double from the hermes test, the parent scope inherited and the tag applied after compaction
func reviewForkAgent(provider, model, tag string, compactions int) *CacheScopeAgent {
	count := compactions
	return &CacheScopeAgent{SessionID: "parent-sess", Provider: provider, Model: model, PromptCacheForkTag: tag,
		CompressionCount: func() int { return count }}
}

func TestInheritedParentScopeIsDerivedOnXaiOnlyAfterForkCompaction(t *testing.T) {
	fork := reviewForkAgent("xai-oauth", "grok-4.3", "review", 0)
	fork.InheritedCacheScope = "gwk_parentscope"
	if got := ResolvePromptCacheScope(fork); got != "gwk_parentscope" {
		t.Fatalf("%q", got)
	}
	fork.CompressionCount = func() int { return 1 }
	if got := ResolvePromptCacheScope(fork); got != "gwk_parentscope::review" {
		t.Fatalf("%q", got)
	}
	fork.Provider, fork.Model = "anthropic", "claude-opus-4-8"
	if got := ResolvePromptCacheScope(fork); got != "gwk_parentscope" {
		t.Fatalf("%q", got)
	}
}

func TestXaiForkSendsDistinctConvIdAndCacheKey(t *testing.T) {
	buildFor := func(a *CacheScopeAgent) map[string]any {
		return scopeCodexKwargs(t, "grok-4.3", scopeSysHi(), codexParams{sessionID: a.SessionID, cacheScopeID: ResolvePromptCacheScope(a), isXAI: true})
	}
	parent := buildFor(reviewForkAgent("xai-oauth", "grok-4.3", "", 0))
	fork := buildFor(reviewForkAgent("xai-oauth", "grok-4.3", "review", 1))
	if scopeHeader(parent, "x-grok-conv-id") != "parent-sess" || scopeHeader(fork, "x-grok-conv-id") != "parent-sess::review" {
		t.Fatalf("conv ids %#v %#v", parent["extra_headers"], fork["extra_headers"])
	}
	parentBody, _ := parent["extra_body"].(map[string]any)
	forkBody, _ := fork["extra_body"].(map[string]any)
	if parentBody["prompt_cache_key"] == forkBody["prompt_cache_key"] {
		t.Fatalf("cache keys shared %v", parentBody["prompt_cache_key"])
	}
}
