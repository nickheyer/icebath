package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	declaredRun1    = "gc_run_room7_default_Reviewer_11111111111141118111111111111111"
	declaredRun2    = "gc_run_room7_default_Reviewer_22222222222242228222222222222222"
	declaredChatKey = "agent:main:telegram:group:-100123:456"
)

func declaredAgent(db any, sessionID, key string) *CacheScopeAgent {
	a := &CacheScopeAgent{SessionID: sessionID, GatewaySessionKey: key}
	switch store := db.(type) {
	case nil:
	case *state.SessionDB:
		a.SessionDB = CacheScopeSessionStore{DB: store}
	default:
		a.SessionDB = db
	}
	return a
}

// OpenRouter sticky session id the profile sends for the session under the context
func declaredStickyKey(t *testing.T, ctx context.Context, sessionID string) string {
	t.Helper()
	p := getProviderProfile(ctx, "openrouter")
	if p == nil {
		t.Fatal("openrouter profile missing")
	}
	return stringOf(p.BuildExtraBody(sessionID, map[string]any{hookCtxKey: ctx})["session_id"])
}

// Grok conversation header the OpenRouter profile pins for the session under the context
func declaredGrokHeaders(t *testing.T, ctx context.Context, sessionID string) map[string]string {
	t.Helper()
	p := getProviderProfile(ctx, "openrouter")
	if p == nil {
		t.Fatal("openrouter profile missing")
	}
	_, top := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "x-ai/grok-4", "session_id": sessionID, hookCtxKey: ctx})
	headers, _ := top["extra_headers"].(map[string]string)
	return headers
}

func TestDeclaredConversationScopePerResponseIdsResolveToOneDeclaredScope(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	scopeCreate(t, db, state.NewSession{ID: declaredRun2, Source: "api_server"})
	first := ResolvePromptCacheScope(declaredAgent(db, declaredRun1, declaredChatKey))
	second := ResolvePromptCacheScope(declaredAgent(db, declaredRun2, declaredChatKey))
	if first != second || first == declaredRun1 || first == declaredRun2 {
		t.Fatalf("%q %q", first, second)
	}
}

func TestDeclaredConversationScopeDistinctDeclarationsStayIsolated(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	if ResolvePromptCacheScope(declaredAgent(db, declaredRun1, declaredChatKey)) == ResolvePromptCacheScope(declaredAgent(db, declaredRun1, "agent:main:telegram:group:-100123:999")) {
		t.Fatal("distinct declarations merged")
	}
}

func TestDeclaredConversationScopeScopeNeverCarriesTheRawKey(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	scope := ResolvePromptCacheScope(declaredAgent(db, declaredRun1, declaredChatKey))
	if !strings.HasPrefix(scope, "gwk_") || strings.Contains(scope, "telegram") || strings.Contains(scope, "-100123") || len(scope) > 64 {
		t.Fatalf("%q", scope)
	}
}

func TestDeclaredConversationScopeNoDeclarationKeepsLineageBehavior(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "webui"})
	scopeEnd(t, db, "root-sess", "compression")
	scopeCreate(t, db, state.NewSession{ID: "rotated-1", Source: "webui", ParentSessionID: "root-sess"})
	if got := ResolvePromptCacheScope(declaredAgent(db, "rotated-1", "")); got != "root-sess" {
		t.Fatalf("%q", got)
	}
	if got := DeclaredConversationScope(declaredAgent(db, "rotated-1", "")); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestDeclaredConversationScopeDeclarationOutranksTheLineageRoot(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "webui"})
	scopeEnd(t, db, "root-sess", "compression")
	scopeCreate(t, db, state.NewSession{ID: "rotated-1", Source: "webui", ParentSessionID: "root-sess"})
	scope := ResolvePromptCacheScope(declaredAgent(db, "rotated-1", declaredChatKey))
	scopeCreate(t, db, state.NewSession{ID: "rotated-2", Source: "webui"})
	if scope != ResolvePromptCacheScope(declaredAgent(db, "rotated-2", declaredChatKey)) || scope == "root-sess" || !strings.HasPrefix(scope, "gwk_") {
		t.Fatalf("%q", scope)
	}
}

func TestDeclaredConversationScopeBranchChildIgnoresTheSharedChatKey(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "root-sess", Source: "telegram"})
	scopeCreate(t, db, state.NewSession{ID: "branch-child", Source: "telegram", ParentSessionID: "root-sess", ModelConfig: map[string]any{"_branched_from": "root-sess"}})
	if got := ResolvePromptCacheScope(declaredAgent(db, "branch-child", declaredChatKey)); got != "branch-child" {
		t.Fatalf("%q", got)
	}
	if got := ResolvePromptCacheScope(declaredAgent(db, "root-sess", declaredChatKey)); got == "branch-child" {
		t.Fatalf("%q", got)
	}
}

func TestDeclaredConversationScopeDelegateChildIgnoresTheDeclaration(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "parent-sess", Source: "telegram"})
	scopeCreate(t, db, state.NewSession{ID: "delegate-child", Source: "telegram", ParentSessionID: "parent-sess", ModelConfig: map[string]any{"_delegate_from": "parent-sess"}})
	if got := ResolvePromptCacheScope(declaredAgent(db, "delegate-child", declaredChatKey)); got != "delegate-child" {
		t.Fatalf("%q", got)
	}
}

func TestDeclaredConversationScopeToolChildIgnoresTheDeclaration(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "parent-sess", Source: "telegram"})
	scopeCreate(t, db, state.NewSession{ID: "tool-child", Source: "tool", ParentSessionID: "parent-sess"})
	if got := ResolvePromptCacheScope(declaredAgent(db, "tool-child", declaredChatKey)); got != "tool-child" {
		t.Fatalf("%q", got)
	}
}

func TestDeclaredConversationScopeBackgroundReviewForkIgnoresTheDeclaration(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "live-sess", Source: "telegram"})
	a := declaredAgent(db, "review-fork", declaredChatKey)
	a.PersistDisabled = true
	if DeclaredConversationScope(a) != "" || ResolvePromptCacheScope(a) != "review-fork" {
		t.Fatal("review fork adopted the declaration")
	}
}

// Store whose fork check fails while the lineage still answers
type scopeForkBoomDB struct{}

func (scopeForkBoomDB) IsExplicitForkChild(string) (bool, error) {
	return false, errors.New("db exploded")
}
func (scopeForkBoomDB) GetCompressionLineage(sid string) ([]string, error) {
	return []string{sid}, nil
}

func TestDeclaredConversationScopeForkCheckFailureDegradesToThePhysicalScope(t *testing.T) {
	a := declaredAgent(scopeForkBoomDB{}, "maybe-fork", declaredChatKey)
	if DeclaredConversationScope(a) != "" || ResolvePromptCacheScope(a) != "maybe-fork" {
		t.Fatal("a failing fork check merged the fork")
	}
}

func TestDeclaredConversationScopeDeclarationAppliesBeforeTheRowLands(t *testing.T) {
	db := scopeRealDB(t)
	if got := ResolvePromptCacheScope(declaredAgent(db, declaredRun1, declaredChatKey)); !strings.HasPrefix(got, "gwk_") {
		t.Fatalf("%q", got)
	}
}

func TestDeclaredConversationScopeBlankDeclarationsAreNoDeclaration(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	for _, blank := range []string{"", "   "} {
		if got := DeclaredConversationScope(declaredAgent(db, declaredRun1, blank)); got != "" {
			t.Fatalf("%q -> %q", blank, got)
		}
	}
}

func TestDeclaredConversationScopeSafeVariantNeverRaises(t *testing.T) {
	if got := DeclaredConversationScopeSafe(&CacheScopeAgent{GatewaySessionKey: declaredChatKey, Platform: "cli", SessionID: "s", SessionDB: scopeHostileDB{}}); got != "" {
		t.Fatalf("%q", got)
	}
	if got := DeclaredConversationScopeSafe(declaredAgent(nil, "sess", declaredChatKey)); !strings.HasPrefix(got, "gwk_") {
		t.Fatalf("%q", got)
	}
}

// Real store wrapper recording every sessions row read by session id
type countingScopeStore struct {
	CacheScopeSessionStore
	reads *[]string
}

func (c countingScopeStore) DeclaredScopeIdentity(sessionID string) (bool, string, error) {
	*c.reads = append(*c.reads, sessionID)
	return c.CacheScopeSessionStore.DeclaredScopeIdentity(sessionID)
}

func (c countingScopeStore) GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	*c.reads = append(*c.reads, sessionID)
	return c.CacheScopeSessionStore.GetSession(ctx, sessionID)
}

func TestOneIdentityReadPerResolutionTheIdentityRowIsReadOnce(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	var reads []string
	a := &CacheScopeAgent{SessionID: declaredRun1, GatewaySessionKey: declaredChatKey, SessionDB: countingScopeStore{CacheScopeSessionStore{DB: db}, &reads}}
	scope := DeclaredConversationScope(a)
	if !strings.HasPrefix(scope, "gwk_") || fmt.Sprint(reads) != fmt.Sprint([]string{declaredRun1}) {
		t.Fatalf("scope %q reads %v", scope, reads)
	}
}

// Store predating the combined identity view, the fork check and the row read apart
type legacyScopeDB struct{ calls *[]string }

func (d legacyScopeDB) IsExplicitForkChild(sid string) (bool, error) {
	*d.calls = append(*d.calls, "fork:"+sid)
	return false, nil
}

func (d legacyScopeDB) GetSession(_ context.Context, sid string) (*pyjson.Object, error) {
	*d.calls = append(*d.calls, "row:"+sid)
	return pyjson.ObjectOf("source", "telegram"), nil
}

func (legacyScopeDB) LatestConversationBoundary(string, string) (int64, bool, error) {
	return 0, false, nil
}

func TestOneIdentityReadPerResolutionADbWithoutTheCombinedViewKeepsTheTwoCallPath(t *testing.T) {
	var calls []string
	scope := DeclaredConversationScope(declaredAgent(legacyScopeDB{&calls}, "sess-legacy", declaredChatKey))
	if !strings.HasPrefix(scope, "gwk_") || fmt.Sprint(calls) != fmt.Sprint([]string{"fork:sess-legacy", "row:sess-legacy"}) {
		t.Fatalf("scope %q calls %v", scope, calls)
	}
}

// Store whose combined identity read fails while the lineage still answers
type scopeIdentityBoomDB struct{}

func (scopeIdentityBoomDB) DeclaredScopeIdentity(string) (bool, string, error) {
	return false, "", errors.New("db exploded")
}
func (scopeIdentityBoomDB) GetCompressionLineage(sid string) ([]string, error) {
	return []string{sid}, nil
}

func TestOneIdentityReadPerResolutionTheCombinedReadStillFailsClosed(t *testing.T) {
	a := declaredAgent(scopeIdentityBoomDB{}, "maybe-fork", declaredChatKey)
	if DeclaredConversationScope(a) != "" || ResolvePromptCacheScope(a) != "maybe-fork" {
		t.Fatal("a failing identity read merged the fork")
	}
}

func TestOneIdentityReadPerResolutionTheCombinedReadStillRefusesAFork(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "tool-child", Source: "tool"})
	if got := DeclaredConversationScope(declaredAgent(db, "tool-child", declaredChatKey)); got != "" {
		t.Fatalf("%q", got)
	}
}

const stabilityInstructions = "You are Reviewer in room7."

func stabilityKeyFor(a *CacheScopeAgent) string {
	return contentCacheKey(stabilityInstructions, []map[string]any{{"type": "function", "name": "terminal"}}, cacheScopeFromSessionID(ResolvePromptCacheScope(a)))
}

func stabilityDB(t *testing.T) *state.SessionDB {
	t.Helper()
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	scopeCreate(t, db, state.NewSession{ID: declaredRun2, Source: "api_server"})
	return db
}

func TestPromptCacheKeyStabilityKeySurvivesAPerResponseId(t *testing.T) {
	db := stabilityDB(t)
	if stabilityKeyFor(declaredAgent(db, declaredRun1, declaredChatKey)) != stabilityKeyFor(declaredAgent(db, declaredRun2, declaredChatKey)) {
		t.Fatal("key churned across responses")
	}
}

func TestPromptCacheKeyStabilityKeyStillChurnsWithoutADeclaration(t *testing.T) {
	db := stabilityDB(t)
	if stabilityKeyFor(declaredAgent(db, declaredRun1, "")) == stabilityKeyFor(declaredAgent(db, declaredRun2, "")) {
		t.Fatal("undeclared ids merged")
	}
}

func TestPromptCacheKeyStabilityCodexTransportKeyMatchesAcrossResponses(t *testing.T) {
	db := stabilityDB(t)
	key := func(sid string) any {
		scope := ResolvePromptCacheScope(declaredAgent(db, sid, declaredChatKey))
		return scopeCodexKwargs(t, "gpt-5.5", []*state.Message{cm("system", stabilityInstructions), cm("user", "hi")}, codexParams{sessionID: sid, cacheScopeID: scope})["prompt_cache_key"]
	}
	if key(declaredRun1) != key(declaredRun2) {
		t.Fatal("codex keys differ across responses")
	}
}

func TestPromptCacheKeyStabilityChatCompletionsKeyMatchesAcrossResponses(t *testing.T) {
	db := stabilityDB(t)
	key := func(sid string) string {
		return scopeChatKey(t, sid, ResolvePromptCacheScope(declaredAgent(db, sid, declaredChatKey)))
	}
	if key(declaredRun1) != key(declaredRun2) {
		t.Fatal("chat completions keys differ across responses")
	}
}

func TestPromptCacheKeyStabilityTranscriptIdentityIsNotRewritten(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: declaredRun1, Source: "api_server"})
	kw := scopeCodexKwargs(t, "gpt-5.5", []*state.Message{cm("system", stabilityInstructions)}, codexParams{sessionID: declaredRun1,
		cacheScopeID: ResolvePromptCacheScope(declaredAgent(db, declaredRun1, declaredChatKey)), isCodexBackend: true})
	if scopeHeader(kw, "session_id") != declaredRun1 {
		t.Fatalf("%#v", kw["extra_headers"])
	}
}

func TestProviderStickyKeysDeclaredScopePinsTheStickyKey(t *testing.T) {
	scope := DeclaredConversationScope(declaredAgent(nil, declaredRun1, declaredChatKey))
	ctx := WithPortalAffinityScope(context.Background(), scope)
	first, second := declaredStickyKey(t, ctx, declaredRun1), declaredStickyKey(t, ctx, declaredRun2)
	if first != second || first != scope {
		t.Fatalf("%q %q %q", first, second, scope)
	}
}

func TestProviderStickyKeysWithoutADeclarationTheConversationIdStillWins(t *testing.T) {
	ctx := WithPortalConversationContext(context.Background(), "parent-root")
	if PortalAffinityScope(ctx) != "" {
		t.Fatal("affinity scope set")
	}
	if got := declaredStickyKey(t, ctx, "delegate-child"); got != "parent-root" {
		t.Fatalf("%q", got)
	}
}

func TestProviderStickyKeysGrokConvIdFollowsTheDeclaredScope(t *testing.T) {
	scope := DeclaredConversationScope(declaredAgent(nil, declaredRun1, declaredChatKey))
	ctx := WithPortalAffinityScope(context.Background(), scope)
	headers, next := declaredGrokHeaders(t, ctx, declaredRun1), declaredGrokHeaders(t, ctx, declaredRun2)
	if headers["x-grok-conv-id"] != scope || next["x-grok-conv-id"] != scope {
		t.Fatalf("%v %v", headers, next)
	}
}

func TestProviderStickyKeysNousStickyKeyFollowsTheDeclaredScope(t *testing.T) {
	scope := DeclaredConversationScope(declaredAgent(nil, declaredRun1, declaredChatKey))
	ctx := WithPortalAffinityScope(context.Background(), scope)
	p := getProviderProfile(ctx, "nous")
	if p == nil {
		t.Fatal("nous profile missing")
	}
	body := p.BuildExtraBody(declaredRun1, map[string]any{hookCtxKey: ctx})
	next := p.BuildExtraBody(declaredRun2, map[string]any{hookCtxKey: ctx})
	if stringOf(body["session_id"]) != scope || stringOf(next["session_id"]) != scope {
		t.Fatalf("%v %v", body, next)
	}
}

const generationKey = "agent:main:telegram:dm:123"

// Keyed telegram row for the declared conversation and its agent
func generationKeyed(t *testing.T, db *state.SessionDB, sessionID string) *CacheScopeAgent {
	t.Helper()
	scopeCreate(t, db, state.NewSession{ID: sessionID, Source: "telegram", SessionKey: generationKey})
	return declaredAgent(db, sessionID, generationKey)
}

func TestConversationGenerationRotatesNewRotatesTheDeclaredScope(t *testing.T) {
	db := scopeRealDB(t)
	before := ResolvePromptCacheScope(generationKeyed(t, db, "sess-A"))
	scopeEnd(t, db, "sess-A", "session_reset")
	after := ResolvePromptCacheScope(generationKeyed(t, db, "sess-B"))
	if !strings.HasPrefix(before, "gwk_") || !strings.HasPrefix(after, "gwk_") || after == before {
		t.Fatalf("%q %q", before, after)
	}
}

func TestConversationGenerationRotatesEveryResetBoundaryRotates(t *testing.T) {
	for _, reason := range []string{"session_reset", "session_switch", "idle", "daily", "suspended", "resume_pending_expired"} {
		t.Run(reason, func(t *testing.T) {
			db := scopeRealDB(t)
			first := ResolvePromptCacheScope(generationKeyed(t, db, "sess-"+reason+"-1"))
			scopeEnd(t, db, "sess-"+reason+"-1", reason)
			if ResolvePromptCacheScope(generationKeyed(t, db, "sess-"+reason+"-2")) == first {
				t.Fatalf("%s did not rotate", reason)
			}
		})
	}
}

func TestConversationGenerationRotatesGenerationsNeverRollBack(t *testing.T) {
	db := scopeRealDB(t)
	scopes := map[string]bool{}
	for i := range 3 {
		sid := fmt.Sprintf("sess-gen%d", i)
		scopes[ResolvePromptCacheScope(generationKeyed(t, db, sid))] = true
		scopeEnd(t, db, sid, "session_reset")
	}
	if len(scopes) != 3 {
		t.Fatalf("%v", scopes)
	}
}

func TestConversationGenerationRotatesPerResponseIdsStillShareOneScope(t *testing.T) {
	db := scopeRealDB(t)
	scopes := map[string]bool{}
	for i := range 4 {
		scopes[ResolvePromptCacheScope(generationKeyed(t, db, fmt.Sprintf("gc_run_%d", i)))] = true
	}
	if len(scopes) != 1 {
		t.Fatalf("%v", scopes)
	}
	for s := range scopes {
		if !strings.HasPrefix(s, "gwk_") {
			t.Fatalf("%q", s)
		}
	}
}

func TestConversationGenerationRotatesAnAccidentalEndIsNotABoundary(t *testing.T) {
	db := scopeRealDB(t)
	first := ResolvePromptCacheScope(generationKeyed(t, db, "sess-live"))
	scopeEnd(t, db, "sess-live", "agent_close")
	if ResolvePromptCacheScope(generationKeyed(t, db, "sess-resumed")) != first {
		t.Fatal("a crash close rotated the scope")
	}
}

func TestConversationGenerationRotatesAnotherChatsResetDoesNotRotateThisOne(t *testing.T) {
	db := scopeRealDB(t)
	mine := ResolvePromptCacheScope(generationKeyed(t, db, "sess-mine"))
	scopeCreate(t, db, state.NewSession{ID: "sess-other", Source: "telegram", SessionKey: "agent:main:telegram:dm:999"})
	scopeEnd(t, db, "sess-other", "session_reset")
	if ResolvePromptCacheScope(generationKeyed(t, db, "sess-mine-2")) != mine {
		t.Fatal("another chat's reset rotated this one")
	}
}

func TestConversationGenerationRotatesScopeNeverCarriesTheRawKeyOrBoundary(t *testing.T) {
	db := scopeRealDB(t)
	generationKeyed(t, db, "sess-A")
	scopeEnd(t, db, "sess-A", "session_reset")
	scope := ResolvePromptCacheScope(generationKeyed(t, db, "sess-B"))
	if strings.Contains(scope, generationKey) || strings.Contains(scope, "telegram") || !strings.HasPrefix(scope, "gwk_") || len(scope) != len("gwk_")+24 {
		t.Fatalf("%q", scope)
	}
}

// Store whose boundary lookup is down while the fork check and lineage answer
type scopeBoundaryBoomDB struct{}

func (scopeBoundaryBoomDB) IsExplicitForkChild(string) (bool, error) { return false, nil }
func (scopeBoundaryBoomDB) LatestConversationBoundary(string, string) (int64, bool, error) {
	return 0, false, errors.New("db down")
}
func (scopeBoundaryBoomDB) GetCompressionLineage(string) ([]string, error) { return []string{}, nil }

func TestConversationGenerationRotatesGenerationReadFailureDegradesToThePhysicalScope(t *testing.T) {
	if got := DeclaredConversationScope(declaredAgent(scopeBoundaryBoomDB{}, "sess-A", generationKey)); got != "" {
		t.Fatalf("%q", got)
	}
}

// Store without the boundary lookup, no boundary means no generation
type scopeNoBoundaryDB struct{}

func (scopeNoBoundaryDB) IsExplicitForkChild(string) (bool, error)       { return false, nil }
func (scopeNoBoundaryDB) GetCompressionLineage(string) ([]string, error) { return []string{}, nil }

func TestConversationGenerationRotatesADbWithoutTheLookupKeepsTheDeclaration(t *testing.T) {
	if got := DeclaredConversationScope(declaredAgent(scopeNoBoundaryDB{}, "sess-A", generationKey)); !strings.HasPrefix(got, "gwk_") {
		t.Fatalf("%q", got)
	}
}

// Moves the session's end stamp before every other boundary, the clock running backwards
func scopeRewindEndedAt(t *testing.T, db *state.SessionDB, sessionID string) {
	t.Helper()
	err := db.ExecuteWrite(context.Background(), func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(context.Background(), "UPDATE sessions SET ended_at = (SELECT MIN(ended_at) FROM sessions WHERE ended_at IS NOT NULL) - 60 WHERE id = ?", sessionID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConversationGenerationRotatesABackwardsClockDoesNotReuseAGeneration(t *testing.T) {
	db := scopeRealDB(t)
	first := ResolvePromptCacheScope(generationKeyed(t, db, "sess-clock-1"))
	scopeEnd(t, db, "sess-clock-1", "session_reset")
	second := ResolvePromptCacheScope(generationKeyed(t, db, "sess-clock-2"))
	scopeEnd(t, db, "sess-clock-2", "session_reset")
	scopeRewindEndedAt(t, db, "sess-clock-2")
	third := ResolvePromptCacheScope(generationKeyed(t, db, "sess-clock-3"))
	if first == second || second == third || first == third {
		t.Fatalf("%q %q %q", first, second, third)
	}
}

const peerKey = "shared-key-string"

func peerRow(t *testing.T, db *state.SessionDB, sessionID, source string) *CacheScopeAgent {
	t.Helper()
	scopeCreate(t, db, state.NewSession{ID: sessionID, Source: source, SessionKey: peerKey})
	a := declaredAgent(db, sessionID, peerKey)
	a.Platform = source
	return a
}

func TestPeerIdentityIsSourceQualifiedAForeignSourcesResetDoesNotRotateThisConversation(t *testing.T) {
	db := scopeRealDB(t)
	before := ResolvePromptCacheScope(peerRow(t, db, "api-1", "api_server"))
	peerRow(t, db, "tg-1", "telegram")
	scopeEnd(t, db, "tg-1", "session_reset")
	if ResolvePromptCacheScope(peerRow(t, db, "api-2", "api_server")) != before {
		t.Fatal("a foreign source's reset rotated this conversation")
	}
}

func TestPeerIdentityIsSourceQualifiedOurOwnResetStillRotates(t *testing.T) {
	db := scopeRealDB(t)
	before := ResolvePromptCacheScope(peerRow(t, db, "api-1", "api_server"))
	scopeEnd(t, db, "api-1", "session_reset")
	if ResolvePromptCacheScope(peerRow(t, db, "api-2", "api_server")) == before {
		t.Fatal("our own reset did not rotate")
	}
}

func TestPeerIdentityIsSourceQualifiedEqualKeysUnderDifferentSourcesNeverShareAScope(t *testing.T) {
	db := scopeRealDB(t)
	mine := ResolvePromptCacheScope(peerRow(t, db, "api-1", "api_server"))
	theirs := ResolvePromptCacheScope(peerRow(t, db, "tg-1", "telegram"))
	if mine == theirs || !strings.HasPrefix(mine, "gwk_") || !strings.HasPrefix(theirs, "gwk_") {
		t.Fatalf("%q %q", mine, theirs)
	}
}

func TestPeerIdentityIsSourceQualifiedTheBoundaryReadIsPeerScoped(t *testing.T) {
	db := scopeRealDB(t)
	ctx := context.Background()
	scopeCreate(t, db, state.NewSession{ID: "tg-1", Source: "telegram", SessionKey: peerKey})
	scopeEnd(t, db, "tg-1", "session_reset")
	if _, ok, err := db.LatestConversationBoundary(ctx, peerKey, "telegram"); err != nil || !ok {
		t.Fatalf("telegram boundary %v %v", ok, err)
	}
	for _, source := range []string{"api_server", ""} {
		if _, ok, err := db.LatestConversationBoundary(ctx, peerKey, source); err != nil || ok {
			t.Fatalf("%q boundary %v %v", source, ok, err)
		}
	}
}

const pruneKey = "agent:main:telegram:dm:777"

func pruneKeyed(t *testing.T, db *state.SessionDB, sessionID string) *CacheScopeAgent {
	t.Helper()
	scopeCreate(t, db, state.NewSession{ID: sessionID, Source: "telegram", SessionKey: pruneKey})
	a := declaredAgent(db, sessionID, pruneKey)
	a.Platform = "telegram"
	return a
}

// Generation counter of the pruning key, -1 when the store records none
func pruneGeneration(t *testing.T, db *state.SessionDB) int64 {
	t.Helper()
	gen, ok, err := db.LatestConversationBoundary(context.Background(), pruneKey, "telegram")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return -1
	}
	return gen
}

func pruneDelete(t *testing.T, db *state.SessionDB, sessionID string) {
	t.Helper()
	if _, err := db.DeleteSession(context.Background(), sessionID, state.DeleteSessionOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSurvivesPruningDeletingTheNewestBoundaryDoesNotRollBack(t *testing.T) {
	db := scopeRealDB(t)
	scopeA := ResolvePromptCacheScope(pruneKeyed(t, db, "s-a"))
	scopeEnd(t, db, "s-a", "session_reset")
	scopeB := ResolvePromptCacheScope(pruneKeyed(t, db, "s-b"))
	scopeEnd(t, db, "s-b", "session_reset")
	if pruneGeneration(t, db) != 2 {
		t.Fatalf("generation %d", pruneGeneration(t, db))
	}
	pruneDelete(t, db, "s-b")
	if pruneGeneration(t, db) != 2 {
		t.Fatalf("generation rolled back to %d", pruneGeneration(t, db))
	}
	scopeC := ResolvePromptCacheScope(pruneKeyed(t, db, "s-c"))
	if scopeA == scopeB || scopeB == scopeC || scopeA == scopeC {
		t.Fatalf("%q %q %q", scopeA, scopeB, scopeC)
	}
}

func TestGenerationSurvivesPruningDeletingEveryBoundaryDoesNotRollBack(t *testing.T) {
	db := scopeRealDB(t)
	scopeA := ResolvePromptCacheScope(pruneKeyed(t, db, "s-a"))
	scopeEnd(t, db, "s-a", "session_reset")
	pruneDelete(t, db, "s-a")
	if ResolvePromptCacheScope(pruneKeyed(t, db, "s-b")) == scopeA {
		t.Fatal("deleting the boundary reused the scope")
	}
}

func TestGenerationSurvivesPruningABackwardsClockThenAPruneStillCannotRepeat(t *testing.T) {
	db := scopeRealDB(t)
	scopeA := ResolvePromptCacheScope(pruneKeyed(t, db, "s-a"))
	scopeEnd(t, db, "s-a", "session_reset")
	scopeB := ResolvePromptCacheScope(pruneKeyed(t, db, "s-b"))
	scopeEnd(t, db, "s-b", "session_reset")
	scopeRewindEndedAt(t, db, "s-b")
	pruneDelete(t, db, "s-b")
	scopeC := ResolvePromptCacheScope(pruneKeyed(t, db, "s-c"))
	if scopeA == scopeB || scopeB == scopeC || scopeA == scopeC {
		t.Fatalf("%q %q %q", scopeA, scopeB, scopeC)
	}
}

func TestGenerationSurvivesPruningOnlyRealBoundariesAdvanceIt(t *testing.T) {
	db := scopeRealDB(t)
	pruneKeyed(t, db, "s-a")
	scopeEnd(t, db, "s-a", "compression")
	if pruneGeneration(t, db) != -1 {
		t.Fatal("compression advanced the generation")
	}
	scopeCreate(t, db, state.NewSession{ID: "s-b", Source: "telegram", SessionKey: pruneKey})
	scopeEnd(t, db, "s-b", "agent_close")
	if pruneGeneration(t, db) != -1 {
		t.Fatal("agent close advanced the generation")
	}
}

func TestGenerationSurvivesPruningARepeatedEndDoesNotDoubleCount(t *testing.T) {
	db := scopeRealDB(t)
	pruneKeyed(t, db, "s-a")
	scopeEnd(t, db, "s-a", "session_reset")
	scopeEnd(t, db, "s-a", "session_reset")
	scopeEnd(t, db, "s-a", "idle")
	if pruneGeneration(t, db) != 1 {
		t.Fatalf("generation %d", pruneGeneration(t, db))
	}
}

func TestGenerationSurvivesPruningAnUnkeyedRowAdvancesNothing(t *testing.T) {
	db := scopeRealDB(t)
	scopeCreate(t, db, state.NewSession{ID: "s-bare", Source: "telegram"})
	scopeEnd(t, db, "s-bare", "session_reset")
	if pruneGeneration(t, db) != -1 {
		t.Fatal("an unkeyed row advanced the generation")
	}
}

func TestGenerationSurvivesPruningTheCounterIsPeerScoped(t *testing.T) {
	db := scopeRealDB(t)
	pruneKeyed(t, db, "s-a")
	scopeEnd(t, db, "s-a", "session_reset")
	if pruneGeneration(t, db) != 1 {
		t.Fatalf("generation %d", pruneGeneration(t, db))
	}
	if _, ok, err := db.LatestConversationBoundary(context.Background(), pruneKey, "api_server"); err != nil || ok {
		t.Fatalf("api_server boundary %v %v", ok, err)
	}
}

const overrideKey = "agent:main:telegram:dm:888"

func TestSourceOverrideDomainThePreRowSourceMatchesPersistence(t *testing.T) {
	db := scopeRealDB(t)
	t.Setenv("ICEBATH_SESSION_SOURCE", "override-src")
	a := declaredAgent(db, "s-none", overrideKey)
	a.Platform = "telegram"
	if got := cacheScopeAgentSource(a, "", a.SessionDB, nil); got != "override-src" {
		t.Fatalf("%q", got)
	}
}

func TestSourceOverrideDomainNewRotatesUnderASourceOverride(t *testing.T) {
	db := scopeRealDB(t)
	t.Setenv("ICEBATH_SESSION_SOURCE", "override-src")
	keyed := func(sid string) *CacheScopeAgent {
		scopeCreate(t, db, state.NewSession{ID: sid, Source: "override-src", SessionKey: overrideKey})
		a := declaredAgent(db, sid, overrideKey)
		a.Platform = "telegram"
		return a
	}
	before := ResolvePromptCacheScope(keyed("s-a"))
	scopeEnd(t, db, "s-a", "session_reset")
	if ResolvePromptCacheScope(keyed("s-b")) == before {
		t.Fatal("a reset under a source override did not rotate")
	}
}
