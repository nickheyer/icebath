package gatewayrun

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Bare runner with an ordered agent cache and its lock, the cache infrastructure of a real runner
func cacheRunner() *GatewayRunner {
	r := &GatewayRunner{}
	r.agentCache = pydict.New[string, agentCacheEntry]()
	r.agentCacheLock = &sync.Mutex{}
	return r
}

// Real agent pointed at a dead loopback endpoint
func quietAgent(t *testing.T) *agent.AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{
		Model: "anthropic/claude-sonnet-4", APIKey: "test", BaseURL: "http://127.0.0.1:9/v1", Provider: "openrouter",
		MaxIterations: 5, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Optional string of a literal
func strp(s string) *string { return &s }

// Session context of a Discord thread with one home channel, the prompt freeze fixture
func freezeContext(platform gateway.Platform, threadID, parentChatID *string) *gateway.SessionContext {
	homes := pydict.New[gateway.Platform, *gateway.HomeChannel]()
	homes.Set(gateway.PlatformDiscord, &gateway.HomeChannel{Platform: gateway.PlatformDiscord, ChatID: "111222333", Name: "general"})
	return &gateway.SessionContext{
		Source: &gateway.SessionSource{
			Platform: platform, ChatID: "111222333", ChatName: strp("general"), ChatType: "channel",
			UserID: strp("9001"), UserName: strp("pix"), ThreadID: threadID, ChatTopic: strp("ops chatter"),
			ParentChatID: parentChatID, ScopeID: strp("777888999"), MessageID: strp("1357"),
		},
		ConnectedPlatforms: []gateway.Platform{gateway.PlatformDiscord, gateway.PlatformTelegram},
		HomeChannels:       homes,
	}
}

// test_prompt_tail_freeze.py TestEphemeralChangeKeyParity test_redact_pii_flip_changes_key
func TestRedactPiiFlipChangesKey(t *testing.T) {
	ctx := context.Background()
	sc := freezeContext(gateway.PlatformTelegram, nil, nil)
	if gateway.BuildSessionContextPrompt(ctx, sc, false) == gateway.BuildSessionContextPrompt(ctx, sc, true) {
		t.Fatal("redaction left the render unchanged")
	}
	if ephemeralChangeKey(ctx, sc, false) == ephemeralChangeKey(ctx, sc, true) {
		t.Fatal("redaction left the key unchanged")
	}
}

// test_prompt_tail_freeze.py TestSessionContextPin test_pin_hit_returns_identical_object
func TestPinHitReturnsIdenticalObject(t *testing.T) {
	ctx := context.Background()
	r := &GatewayRunner{}
	first := r.pinnedSessionContextPrompt(ctx, freezeContext(gateway.PlatformDiscord, strp("444555666"), strp("111222333")), false, "sk", false)
	second := r.pinnedSessionContextPrompt(ctx, freezeContext(gateway.PlatformDiscord, strp("444555666"), strp("111222333")), false, "sk", false)
	if second != first || r.peekSessionState("sk").conversation.ephemeralPin.text != first {
		t.Fatal("the pin was not reused verbatim")
	}
}

// test_prompt_tail_freeze.py TestSidecarNoteStaging test_set_then_consume_once
func TestSetThenConsumeOnce(t *testing.T) {
	r := &GatewayRunner{}
	r.setPendingTurnSidecarNotes("sk", []string{"[System note: reset]"})
	if got := r.consumePendingTurnSidecarNotes("sk"); !reflect.DeepEqual(got, []string{"[System note: reset]"}) {
		t.Fatalf("first consume %v", got)
	}
	if got := r.consumePendingTurnSidecarNotes("sk"); len(got) != 0 {
		t.Fatalf("second consume %v", got)
	}
}

// test_agent_cache.py TestAgentCacheLifecycle test_evict_on_session_reset
func TestEvictOnSessionReset(t *testing.T) {
	r := cacheRunner()
	a := quietAgent(t)
	key := "telegram:12345"
	r.agentCacheLock.Lock()
	r.agentCache.Set(key, agentCacheEntry{agent: a, signature: "sig123"})
	r.agentCacheLock.Unlock()
	r.evictCachedAgent(context.Background(), key, false, false)
	r.agentCacheLock.Lock()
	defer r.agentCacheLock.Unlock()
	if r.agentCache.Has(key) {
		t.Fatal("the cached agent survived eviction")
	}
}

// test_agent_cache_pressure.py TestConfiguredBoundsReachTheCache test_unset_config_keeps_the_built_in_defaults
func TestUnsetConfigKeepsTheBuiltInDefaults(t *testing.T) {
	r := &GatewayRunner{}
	r.agentCacheBoundsCache = &gateway.AgentCacheBounds{}
	if r.agentCacheCap(context.Background()) != agentCacheMaxSize || r.agentCacheIdleTTL(context.Background()) != agentCacheIdleTTLSecs {
		t.Fatalf("cap %d ttl %v", r.agentCacheCap(context.Background()), r.agentCacheIdleTTL(context.Background()))
	}
}

// test_agent_cache_pressure.py TestConfiguredBoundsReachTheCache test_configured_cap_bounds_the_real_enforcer
func TestConfiguredCapBoundsTheRealEnforcer(t *testing.T) {
	r := cacheRunner()
	two := 2
	r.agentCacheBoundsCache = &gateway.AgentCacheBounds{MaxSize: &two}
	r.agentCacheLock.Lock()
	for _, key := range []string{"s0", "s1", "s2", "s3", "s4"} {
		r.agentCache.Set(key, agentCacheEntry{agent: quietAgent(t), signature: "sig"})
	}
	r.enforceAgentCacheCap(context.Background())
	r.agentCacheLock.Unlock()
	if got := r.agentCache.Keys(); !reflect.DeepEqual(got, []string{"s3", "s4"}) {
		t.Fatalf("cache keys %v", got)
	}
}

// test_agent_cache_pressure.py TestSalvageFollowups test_config_read_failure_still_resolves_auto_budget
func TestConfigReadFailureStillResolvesAutoBudget(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	r := &GatewayRunner{}
	if bounds := r.agentCacheBounds(context.Background()); bounds.MemoryHighMB == nil {
		t.Fatal("an unreadable config disabled the pressure valve")
	}
}

// Running slot of a session holding the agent, the pending sentinel or nothing
func TestRunningAgentReportsSlotAndSentinel(t *testing.T) {
	r := &GatewayRunner{}
	if a, pending := r.RunningAgent("sk"); a != nil || pending {
		t.Fatal("an idle key reported a running agent")
	}
	r.sessionState("sk").turn.agent = agentPendingSentinel
	if a, pending := r.RunningAgent("sk"); a != nil || !pending {
		t.Fatal("the pending sentinel was not reported as pending")
	}
	running := quietAgent(t)
	r.sessionState("sk").turn.agent = running
	if a, pending := r.RunningAgent("sk"); a != running || pending {
		t.Fatal("the running agent was not reported")
	}
}
