package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestExplicitClientKwargsInjectsCommandForAnyExternalProcessProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "test-process-provider", AuthType: provider.AuthTypeExternalProcess})
	a := &AIAgent{provider: "test-process-provider", acpCommand: "/tmp/test-process", acpArgs: []string{"--acp", "--stdio"}}
	kwargs, err := explicitClientKwargs(context.Background(), a, "process-placeholder", "acp://test-process", nil)
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["command"] != "/tmp/test-process" || !reflect.DeepEqual(kwargs["args"], []string{"--acp", "--stdio"}) {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

func TestFallbackActivationKeepsExternalProcessProviderOnChatCompletions(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	for _, name := range []string{"copilot-acp", "acme-acp"} {
		provider.RegisterProvider(provider.Profile{Name: name, AuthType: provider.AuthTypeExternalProcess})
	}
	ctx := context.Background()
	a := &AIAgent{}
	for _, name := range []string{"copilot-acp", "acme-acp"} {
		if got := a.fallbackAPIModeResolved(ctx, name, "gpt-5.6", "https://proxy.example.invalid/v1"); got != "chat_completions" {
			t.Fatalf("%s -> %s", name, got)
		}
	}
	if got := a.fallbackAPIModeResolved(ctx, "acme-http", "gpt-5.6", "https://proxy.example.invalid/v1"); got != "codex_responses" {
		t.Fatalf("acme-http -> %s", got)
	}
}

func TestShouldStreamIsOffForAnyExternalProcessProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(provider.SnapshotProviderRegistryForTests())
	provider.RegisterProvider(provider.Profile{Name: "acme-acp", AuthType: provider.AuthTypeExternalProcess})
	makeAgent := func(name string) *AIAgent {
		return &AIAgent{provider: name, baseURL: "https://proxy.example.invalid/v1", streamDeltaCallback: func(string) {}}
	}
	ctx := context.Background()
	if shouldStream(ctx, makeAgent("acme-acp")) {
		t.Fatal("an external process profile streams")
	}
	if !shouldStream(ctx, makeAgent("acme-http")) {
		t.Fatal("a plain http provider does not stream")
	}
}

type initLadderPool struct{ available bool }

func (p initLadderPool) HasCredentials() bool                 { return true }
func (p initLadderPool) HasAvailable(string) bool             { return p.available }
func (p initLadderPool) Entries() []provider.PooledCredential { return nil }

type initLadderClient struct{}

func (initLadderClient) BaseURL() string { return "" }
func (initLadderClient) APIKey() string  { return "k" }
func (initLadderClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("unused")
}
func (initLadderClient) Close() error { return nil }

// Replaces the init ladder router, entry key and pool loader for the rest of the test
func installInitLadder(t *testing.T, resolve func(name, model string) (provider.AuxClient, string, error), pool initCredentialPool) {
	t.Helper()
	prevResolve, prevKey, prevPool := initResolveProviderClient, initResolveEntryAPIKey, initLoadPool
	initResolveProviderClient = func(_ context.Context, name, model string, _ provider.ProviderClientOptions) (provider.AuxClient, string, error) {
		return resolve(name, model)
	}
	initResolveEntryAPIKey = func(context.Context, map[string]any) string { return "" }
	initLoadPool = func(context.Context, string) initCredentialPool { return pool }
	t.Cleanup(func() {
		initResolveProviderClient, initResolveEntryAPIKey, initLoadPool = prevResolve, prevKey, prevPool
	})
}

var initLadder = []map[string]any{{"provider": "deepseek", "model": "deepseek-v4-pro"}, {"provider": "kimi", "model": "kimi-k3"}}

func TestFullyRefusingLadderWarnsWithEachReason(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	reasons := []string{"deepseek", "no usable credentials", "kimi", "kimi auth handshake refused"}
	for _, c := range []struct {
		primary       string
		ladder        []map[string]any
		poolAvailable bool
		raiseMatch    string
		expected      []string
	}{
		{"openrouter", initLadder, true, "No LLM provider configured", reasons},
		{"anthropic", initLadder, true, "no API key was found", reasons},
		{"openrouter", nil, false, "No LLM provider configured", []string{"credential pool exhausted"}},
	} {
		t.Run(c.primary, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			installInitLadder(t, func(name, _ string) (provider.AuxClient, string, error) {
				if name == "kimi" {
					return nil, "", errors.New("kimi auth handshake refused")
				}
				return nil, "", nil
			}, initLadderPool{available: c.poolAvailable})
			records := captureLogger(t, "run_agent", slog.LevelWarn)
			timeout := 60.0
			a := &AIAgent{provider: c.primary, model: "z-ai/glm-5.3-flash"}
			_, err := routedClientKwargs(context.Background(), a, c.ladder, &timeout)
			if err == nil || !strings.Contains(err.Error(), c.raiseMatch) {
				t.Fatalf("error %v, want %q", err, c.raiseMatch)
			}
			var notConfigured *provider.ProviderNotConfiguredError
			if !errors.As(err, &notConfigured) {
				t.Fatalf("error %T is not the missing provider error", err)
			}
			warnings := records()
			if len(warnings) != 1 {
				t.Fatalf("warnings %v", warnings)
			}
			text := warnings[0].msg
			if !strings.Contains(text, c.primary) {
				t.Fatalf("warning %q misses the primary", text)
			}
			for _, fragment := range c.expected {
				if !strings.Contains(text, fragment) {
					t.Fatalf("warning %q misses %q", text, fragment)
				}
			}
		})
	}
}

func TestRecoveredLadderDoesNotWarn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installInitLadder(t, func(name, model string) (provider.AuxClient, string, error) {
		if name == "kimi" {
			return initLadderClient{}, model, nil
		}
		return nil, "", nil
	}, nil)
	records := captureLogger(t, "run_agent", slog.LevelWarn)
	timeout := 60.0
	a := &AIAgent{provider: "openrouter", model: "z-ai/glm-5.3-flash"}
	kwargs, err := routedClientKwargs(context.Background(), a, initLadder, &timeout)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kwargs, clientKwargsFromRouted(initLadderClient{}, &timeout)) || kwargs["api_key"] != "k" {
		t.Fatalf("kwargs = %v", kwargs)
	}
	if a.provider != "kimi" || !a.fallbackActivated {
		t.Fatalf("provider %q activated %v", a.provider, a.fallbackActivated)
	}
	if w := records(); len(w) != 0 {
		t.Fatalf("warnings %v", w)
	}
}

func TestAgentContextFollowsThePlatform(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for platform, want := range map[string]string{"cron": "cron", "subagent": "subagent", "telegram": "primary", "": "primary"} {
		a := &AIAgent{sessionID: "sess-80646"}
		if got := memoryProviderInitOptions(context.Background(), a, platform).AgentContext; got != want {
			t.Fatalf("platform %q -> %q", platform, got)
		}
	}
}

func TestMissingKeyBannerIsClassifiedWithoutHidingInitialization(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, suppress := range []any{nil, false, true} {
		t.Run(strings.ReplaceAll(pyReprForCase(suppress), " ", "_"), func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			yaml := "{}\n"
			if suppress != nil {
				yaml = "display:\n  suppress_warning_notifications: " + pyReprForCase(suppress) + "\n"
			}
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(strings.ToLower(yaml)), 0o600); err != nil {
				t.Fatal(err)
			}
			out := captureStatusStdout(t)
			a := &AIAgent{provider: "custom", model: "fixture", platform: "cli", quietMode: false}
			a.setBaseURL("http://localhost:1/v1")
			timeout := 30.0
			if err := initOpenAIClient(context.Background(), a, "dummy-key", a.baseURL, nil, &timeout); err != nil {
				t.Fatal(err)
			}
			output := out.String()
			if strings.Contains(output, "API key appears invalid or missing") != (suppress != true) {
				t.Fatalf("suppress %v output %q", suppress, output)
			}
			if !strings.Contains(output, "AI Agent initialized with model: fixture") {
				t.Fatalf("output %q", output)
			}
			if a.client == nil || a.apiKey != "dummy-key" {
				t.Fatalf("client %v key %q", a.client, a.apiKey)
			}
		})
	}
}

// Python repr spelling of the bool or None parameter a case is labelled with
func pyReprForCase(v any) string {
	switch v {
	case nil:
		return "None"
	case true:
		return "True"
	}
	return "False"
}

// Real codex gpt-5.5 agent under a compression config with the autoraise notice gate as given
func makeCodexNoticeAgent(t *testing.T, h string, showNotice bool) (*AIAgent, string) {
	t.Helper()
	body := "compression:\n  enabled: true\n  threshold: 0.50\n  target_ratio: 0.20\n  protect_first_n: 3\n  protect_last_n: 20\n" +
		"  codex_gpt55_autoraise: true\n  codex_gpt55_autoraise_notice: " + strings.ToLower(pyReprForCase(showNotice)) + "\n" +
		"prompt_caching:\n  cache_ttl: 5m\nsessions: {}\nbedrock: {}\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStatusStdout(t)
	db, err := state.Open(filepath.Join(h, "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, err := NewAIAgent(context.Background(), AIAgentOptions{
		BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "test-key", Provider: "openai-codex", Model: "gpt-5.5",
		EnabledToolsets: []string{}, DisabledToolsets: []string{}, QuietMode: false, SkipMemory: true, SessionDB: db, SessionID: "codex-notice-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, out.String()
}

func TestCodexGpt55AutoraiseNoticeDedupedAcrossAgentInits(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	agent1, stdout1 := makeCodexNoticeAgent(t, h, true)
	if !strings.Contains(stdout1, "auto-compaction was raised") || agent1.compressionWarning == nil {
		t.Fatalf("first init stdout %q warning %v", stdout1, agent1.compressionWarning)
	}
	agent2, stdout2 := makeCodexNoticeAgent(t, h, true)
	base := agent2.contextCompressor.Base()
	if ratio := math.Round(float64(base.ThresholdTokens)/float64(base.ContextLength)*100) / 100; ratio != 0.85 {
		t.Fatalf("threshold ratio %v", ratio)
	}
	if strings.Contains(stdout2, "auto-compaction was raised") || agent2.compressionWarning != nil {
		t.Fatalf("second init stdout %q warning %v", stdout2, agent2.compressionWarning)
	}
}

func TestPersistenceNeverSnapshotsButExplicitSaveWorks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, legacyEnabled := range []bool{false, true} {
		t.Run(pyReprForCase(legacyEnabled), func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			body := "sessions:\n  write_json_snapshots: " + strings.ToLower(pyReprForCase(legacyEnabled)) + "\n"
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			db, err := state.Open(filepath.Join(h, "state.db"), state.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			a := &AIAgent{maxIterations: 1}
			if err := initSessionState(ctx, a, AIAgentOptions{SessionID: "snapshot-removal", SessionDB: db,
				CheckpointMaxSnapshots: 1, CheckpointMaxTotalSizeMB: 1, CheckpointMaxFileSizeMB: 1}); err != nil {
				t.Fatal(err)
			}
			a.model = "fixture"
			a.setBaseURL("http://127.0.0.1:1/v1")
			a.platform, a.tools, a.verboseLogging = "cli", []any{}, true
			if err := db.CreateSession(state.NewSession{ID: a.sessionID, Source: "cli", Model: a.model}); err != nil {
				t.Fatal(err)
			}
			a.sessionDBCreated = true
			messages := []*state.Message{
				{Role: "user", Content: "keep this conversation"},
				{Role: "assistant", Content: "durable answer"},
			}
			if err := a.persistSession(&messages, nil); err != nil {
				t.Fatal(err)
			}
			stored, err := db.GetMessagesAsConversation(ctx, a.sessionID, state.ConversationOptions{})
			if err != nil || len(stored) != 2 {
				t.Fatalf("stored %v %v", stored, err)
			}
			if snaps, _ := filepath.Glob(filepath.Join(a.logsDir, "session_*.json")); len(snaps) != 0 {
				t.Fatalf("snapshots written %v", snaps)
			}
			snapshot, err := state.LoadSaveSnapshot(ctx, db, a.sessionID, "json")
			if err != nil || snapshot == nil || snapshot.Len() == 0 {
				t.Fatalf("snapshot %v %v", snapshot, err)
			}
			rendered, err := state.RenderSessionForSave(snapshot, "json")
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(h, "explicit.json")
			if err := os.WriteFile(output, []byte(rendered), 0o600); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(output)
			var saved struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Messages) != 2 || saved.Messages[0].Content != "keep this conversation" || saved.Messages[1].Content != "durable answer" {
				t.Fatalf("saved %+v", saved.Messages)
			}
		})
	}
}
