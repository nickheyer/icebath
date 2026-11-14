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
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
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
				CheckpointMaxSnapshots: new(1), CheckpointMaxTotalSizeMB: new(1), CheckpointMaxFileSizeMB: new(1)}); err != nil {
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

func exhaustedCredential(providerName string, code int, message string) provider.PooledCredential {
	return provider.PooledCredential{Provider: providerName, ID: "e1", Label: "test-cred", AuthType: "api_key", Source: "manual",
		AccessToken: "sk-test-0000000000", LastStatus: provider.StatusExhausted, LastStatusAt: float64(time.Now().UnixNano()) / 1e9,
		LastErrorCode: code, LastErrorMessage: message}
}

// Router answering no client for every provider and the pool loader returning the given pool
func noRoutedClient(t *testing.T, pool func(ctx context.Context) initCredentialPool) {
	t.Helper()
	prevResolve, prevKey, prevPool := initResolveProviderClient, initResolveEntryAPIKey, initLoadPool
	initResolveProviderClient = func(context.Context, string, string, provider.ProviderClientOptions) (provider.AuxClient, string, error) {
		return nil, "", nil
	}
	initResolveEntryAPIKey = func(context.Context, map[string]any) string { return "" }
	initLoadPool = func(ctx context.Context, _ string) initCredentialPool { return pool(ctx) }
	t.Cleanup(func() {
		initResolveProviderClient, initResolveEntryAPIKey, initLoadPool = prevResolve, prevKey, prevPool
	})
}

func TestExhaustedPoolRaisesTypedBillingError(t *testing.T) {
	for _, c := range [][2]string{{"openrouter", "z-ai/glm-5.3"}, {"custom", "qwen3.5:4b"}, {"zai", "glm-5.3"}} {
		t.Run(c[0], func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			noRoutedClient(t, func(ctx context.Context) initCredentialPool {
				return provider.NewPool(agentPoolEnv(ctx), c[0], []provider.PooledCredential{
					exhaustedCredential(c[0], 402, "You requested up to 65536 tokens, but can only afford 4282.")})
			})
			timeout := 60.0
			_, err := routedClientKwargs(context.Background(), &AIAgent{provider: c[0], model: c[1]}, nil, &timeout)
			var exhausted *provider.ProviderCredentialsExhaustedError
			if !errors.As(err, &exhausted) {
				t.Fatalf("error %T %v", err, err)
			}
			msg := err.Error()
			if exhausted.Provider != c[0] || !strings.Contains(msg, "'"+c[0]+"'") || !strings.Contains(msg, c[1]) || !strings.Contains(msg, "402") ||
				!strings.Contains(msg, "exhausted") || strings.Contains(msg, "No LLM provider configured") || strings.Contains(msg, "no API key was found") {
				t.Fatalf("message %q", msg)
			}
		})
	}
}

func TestHealthyPoolKeepsTheExistingDiagnostic(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	noRoutedClient(t, func(ctx context.Context) initCredentialPool {
		return provider.NewPool(agentPoolEnv(ctx), "openrouter", []provider.PooledCredential{{Provider: "openrouter", ID: "e1", Label: "test-cred",
			AuthType: "api_key", Source: "manual", AccessToken: "sk-test-0000000000", LastStatus: provider.StatusOK}})
	})
	timeout := 60.0
	if _, err := routedClientKwargs(context.Background(), &AIAgent{provider: "openrouter", model: "z-ai/glm-5.3"}, nil, &timeout); err == nil || !strings.Contains(err.Error(), "No LLM provider configured") {
		t.Fatalf("error %v", err)
	}
}

func TestEmptyPoolKeepsTheExistingDiagnostic(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	noRoutedClient(t, func(ctx context.Context) initCredentialPool {
		return provider.NewPool(agentPoolEnv(ctx), "openrouter", nil)
	})
	timeout := 60.0
	if _, err := routedClientKwargs(context.Background(), &AIAgent{provider: "openrouter", model: "z-ai/glm-5.3"}, nil, &timeout); err == nil || !strings.Contains(err.Error(), "No LLM provider configured") {
		t.Fatalf("error %v", err)
	}
}

func TestOpenrouterQuotaCooldownStillReportsTheCooldown(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENROUTER_API_KEY", "")
	os.Unsetenv("OPENROUTER_API_KEY")
	now := float64(time.Now().UnixNano()) / 1e9
	auth := map[string]any{"credential_pool": map[string]any{"openrouter": []any{map[string]any{"provider": "openrouter", "id": "or1", "label": "or1",
		"auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-test-0000000000", "last_status": provider.StatusExhausted,
		"last_status_at": now, "last_error_code": 429, "last_error_reset_at": now + 3*3600}}}}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  provider: openrouter\n  default: z-ai/glm-5.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noRoutedClient(t, func(ctx context.Context) initCredentialPool {
		return provider.NewPool(agentPoolEnv(ctx), "openrouter", []provider.PooledCredential{exhaustedCredential("openrouter", 429, "rate limited")})
	})
	timeout := 60.0
	_, err := routedClientKwargs(context.Background(), &AIAgent{provider: "openrouter", model: "z-ai/glm-5.3"}, nil, &timeout)
	if err == nil || !strings.Contains(err.Error(), "cooling down") || strings.Contains(err.Error(), "No LLM provider configured") {
		t.Fatalf("error %v", err)
	}
}

func TestAgentInitRaisesTheNamedError(t *testing.T) {
	t.Run("test_no_provider_configured_at_all", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		prev := initResolveProviderClient
		initResolveProviderClient = func(context.Context, string, string, provider.ProviderClientOptions) (provider.AuxClient, string, error) {
			return nil, "", nil
		}
		t.Cleanup(func() { initResolveProviderClient = prev })
		timeout := 60.0
		_, err := routedClientKwargs(context.Background(), &AIAgent{provider: "auto", model: "m"}, nil, &timeout)
		var named *provider.ProviderNotConfiguredError
		if !errors.As(err, &named) || !strings.Contains(err.Error(), "No LLM provider configured") {
			t.Fatalf("error %T %v", err, err)
		}
	})
	t.Run("test_explicit_provider_without_credentials", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		timeout := 60.0
		_, err := routedClientKwargs(context.Background(), &AIAgent{provider: "minimax-oauth", model: "m"}, nil, &timeout)
		var named *provider.ProviderNotConfiguredError
		if !errors.As(err, &named) || !strings.Contains(err.Error(), "icebath auth add minimax-oauth") {
			t.Fatalf("error %T %v", err, err)
		}
	})
}

func TestAgentInitRefusesCheckpointRequiredOnCodexAppServer(t *testing.T) {
	if err := refuseCheckpointRequiredOnCodexAppServer(true, "codex_app_server"); err == nil || !strings.Contains(err.Error(), "BLOCKED_MISSING_PREREQUISITE") {
		t.Fatalf("error %v", err)
	}
	for _, c := range []struct {
		required bool
		mode     string
	}{{true, "chat_completions"}, {true, "codex_responses"}, {false, "codex_app_server"}, {false, ""}} {
		if err := refuseCheckpointRequiredOnCodexAppServer(c.required, c.mode); err != nil {
			t.Fatalf("%v %q refused %v", c.required, c.mode, err)
		}
	}
}

// Error carrying the Python exception class a failing config read raised
type pyClassError struct{ class, msg string }

func (e pyClassError) Error() string      { return e.msg }
func (e pyClassError) PyTypeName() string { return e.class }

// Openrouter agent of the config warning fixture with memory on, review fork off and real log files
func configWarningAgent(t *testing.T) *AIAgent {
	t.Helper()
	prevSetup := agentSetupLogging
	agentSetupLogging = realAgentSetupLogging
	t.Cleanup(func() { agentSetupLogging = prevSetup })
	a, err := NewAIAgent(context.Background(), AIAgentOptions{Model: "test-model", Provider: "openrouter", APIMode: "chat_completions",
		APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", EnabledToolsets: []string{}, QuietMode: true, SkipContextFiles: true,
		SkipBackgroundReview: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestConfigFailureWarnsWithoutExposingValuesAndPreservesDefaults(t *testing.T) {
	const configured = "prompt_caching:\n  cache_ttl: 1h\nmodel:\n  reasoning_echo: true\ncompression:\n  enabled: false\nskills:\n  creation_nudge_interval: 99\n"
	sections := map[string]string{"cache": "prompt_caching.cache_ttl", "reasoning": "model.reasoning_echo", "sections": "memory"}
	for _, failureAt := range []string{"import", "load"} {
		for _, reader := range []string{"cache", "reasoning", "sections", "all"} {
			t.Run(failureAt+"_"+reader, func(t *testing.T) {
				h := t.TempDir()
				t.Setenv("ICEBATH_HOME", h)
				t.Setenv("OPENROUTER_API_KEY", "test-key")
				configPath := filepath.Join(h, "config.yaml")
				if err := os.MkdirAll(filepath.Join(h, "memories"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h, "memories", "MEMORY.md"), []byte("The notebook is blue."), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, []byte(configured), 0o600); err != nil {
					t.Fatal(err)
				}
				affected := map[string]bool{reader: true}
				if reader == "all" {
					affected = map[string]bool{"cache": true, "reasoning": true, "sections": true}
				}
				const secret = "private-config-value-do-not-log"
				class := "RuntimeError"
				if failureAt == "import" {
					class = "ModuleNotFoundError"
				}
				failure := pyClassError{class: class, msg: "Could not load config.yaml: api_key: " + secret}
				prevCache, prevEcho, prevSections := promptCacheConfigLoader, reasoningEchoConfigLoader, agentConfigLoader
				if affected["cache"] {
					promptCacheConfigLoader = func(context.Context) (map[string]any, error) { return nil, failure }
				}
				if affected["reasoning"] {
					reasoningEchoConfigLoader = func(context.Context) (config.Tree, error) { return nil, failure }
				}
				if affected["sections"] {
					agentConfigLoader = func(context.Context) (map[string]any, error) { return nil, failure }
				}
				fallback := configWarningAgent(t)
				promptCacheConfigLoader, reasoningEchoConfigLoader, agentConfigLoader = prevCache, prevEcho, prevSections
				if raw, _ := os.ReadFile(configPath); string(raw) != configured {
					t.Fatalf("config rewritten %q", raw)
				}
				logging.FlushLogQueue()
				persisted, _ := os.ReadFile(filepath.Join(h, "logs", "errors.log"))
				var warnings []string
				for _, line := range strings.Split(string(persisted), "\n") {
					lower := strings.ToLower(line)
					if strings.Contains(line, "run_agent") && strings.Contains(line, "WARNING") && strings.Contains(lower, "config") && strings.Contains(lower, "default") {
						warnings = append(warnings, line)
					}
				}
				if len(warnings) != len(affected) {
					t.Fatalf("warnings %d want %d\n%s", len(warnings), len(affected), persisted)
				}
				for scope := range affected {
					if !strings.Contains(string(persisted), sections[scope]) {
						t.Fatalf("errors.log lacks %s\n%s", sections[scope], persisted)
					}
				}
				for _, w := range warnings {
					if !strings.Contains(w, class) {
						t.Fatalf("warning %q lacks %s", w, class)
					}
				}
				if strings.Contains(string(persisted), secret) {
					t.Fatal("secret persisted")
				}
				healthy := configWarningAgent(t)
				if err := os.WriteFile(configPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				config.ResetCache()
				defaults := configWarningAgent(t)
				if healthy.cacheTTL == defaults.cacheTTL || healthy.reasoningEchoFlag == defaults.reasoningEchoFlag ||
					healthy.compressionEnabled == defaults.compressionEnabled || healthy.skillNudgeInterval == defaults.skillNudgeInterval {
					t.Fatal("healthy and default agents agree on a configured value")
				}
				ref := func(scope string) *AIAgent {
					if affected[scope] {
						return defaults
					}
					return healthy
				}
				if r := ref("cache"); fallback.cacheTTL != r.cacheTTL || fallback.cacheDisabled != r.cacheDisabled || fallback.usePromptCaching != r.usePromptCaching {
					t.Fatal("cache settings")
				}
				if fallback.reasoningEchoFlag != ref("reasoning").reasoningEchoFlag {
					t.Fatal("reasoning echo flag")
				}
				if r := ref("sections"); fallback.compressionEnabled != r.compressionEnabled || fallback.skillNudgeInterval != r.skillNudgeInterval {
					t.Fatal("section settings")
				}
				if fallback.memoryManager != nil || healthy.memoryManager != nil || defaults.memoryManager != nil {
					t.Fatal("memory manager built")
				}
				for _, x := range []*AIAgent{healthy, defaults} {
					if fallback.memoryEnabled != x.memoryEnabled || fallback.userProfileEnabled != x.userProfileEnabled || fallback.memoryNudgeInterval != x.memoryNudgeInterval ||
						fallback.compressionInPlace != x.compressionInPlace || fallback.maxCompressionAttempts != x.maxCompressionAttempts ||
						fallback.contextCompressor.Base().ThresholdPercent != x.contextCompressor.Base().ThresholdPercent {
						t.Fatal("unaffected settings differ")
					}
					if fallback.memoryStore == nil || x.memoryStore == nil || !reflect.DeepEqual(fallback.memoryStore.Entries("memory"), x.memoryStore.Entries("memory")) {
						t.Fatal("memory entries differ")
					}
				}
			})
		}
	}
}

func TestEmptyConfigInitializesMemoryWithoutALoadWarning(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	_ = os.MkdirAll(filepath.Join(h, "memories"), 0o700)
	_ = os.WriteFile(filepath.Join(h, "memories", "MEMORY.md"), []byte("The notebook is blue."), 0o600)
	_ = os.WriteFile(filepath.Join(h, "config.yaml"), nil, 0o600)
	a := configWarningAgent(t)
	if a.memoryManager != nil || a.memoryStore == nil || !reflect.DeepEqual(a.memoryStore.Entries("memory"), []string{"The notebook is blue."}) {
		t.Fatalf("memory %v %v", a.memoryManager, a.memoryStore)
	}
	if raw, _ := os.ReadFile(filepath.Join(h, "config.yaml")); len(raw) != 0 {
		t.Fatalf("config rewritten %q", raw)
	}
	logging.FlushLogQueue()
	persisted, _ := os.ReadFile(filepath.Join(h, "logs", "errors.log"))
	for _, line := range strings.Split(string(persisted), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(line, "run_agent") && strings.Contains(lower, "config") && strings.Contains(lower, "default") {
			t.Fatalf("load warning %q", line)
		}
	}
}

// Fresh logging state for the rest of the test, the hermes reset of _logging_initialized and the queued handlers
func resetLoggingState(t *testing.T) {
	t.Helper()
	logging.ResetQueuedHandlers()
	prevSetup := agentSetupLogging
	agentSetupLogging = realAgentSetupLogging
	t.Cleanup(func() {
		agentSetupLogging = prevSetup
		logging.ResetQueuedHandlers()
		logging.ClearSessionContext()
	})
}

func readLog(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func TestSetupLoggingProfileScope(t *testing.T) {
	t.Run("test_routes_agent_records_to_profile_scope", func(t *testing.T) {
		resetLoggingState(t)
		base := t.TempDir()
		homeA, homeB := filepath.Join(base, "home-a"), filepath.Join(base, "home-b")
		for _, d := range []string{homeA, homeB} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("ICEBATH_HOME", homeA)
		quiet := &AIAgent{quietMode: true}
		if err := setupAgentLogging(home.WithOverride(context.Background(), homeA), quiet); err != nil {
			t.Fatal(err)
		}
		ctxB := home.WithOverride(context.Background(), homeB)
		if err := setupAgentLogging(ctxB, quiet); err != nil {
			t.Fatal(err)
		}
		logging.GetLogger("agent.test_profile_scope_125974").LogContext(ctxB, logging.LevelInfo, "scoped-witness-b")
		logging.FlushLogQueue()
		if !strings.Contains(readLog(filepath.Join(homeB, "logs", "agent.log")), "scoped-witness-b") {
			t.Fatal("the profile scoped agent's records did not reach its own logs")
		}
		if strings.Contains(readLog(filepath.Join(homeA, "logs", "agent.log")), "scoped-witness-b") {
			t.Fatal("record leaked into the launch home")
		}
	})
	t.Run("test_under_routing_adds_no_bare_handler", func(t *testing.T) {
		resetLoggingState(t)
		base := t.TempDir()
		homeA, homeB := filepath.Join(base, "home-a"), filepath.Join(base, "home-b")
		for _, d := range []string{homeA, homeB} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("ICEBATH_HOME", homeA)
		if _, err := logging.SetupLogging(context.Background(), logging.Options{Home: homeA}); err != nil {
			t.Fatal(err)
		}
		if ok, err := logging.EnableProfileLogRouting([]string{homeA, homeB}); err != nil || !ok {
			t.Fatalf("routing %v %v", ok, err)
		}
		ctxB := home.WithOverride(context.Background(), homeB)
		if err := setupAgentLogging(ctxB, &AIAgent{quietMode: true}); err != nil {
			t.Fatal(err)
		}
		logging.GetLogger("agent.test_routing_guard_125974").LogContext(ctxB, logging.LevelInfo, "routing-guard-witness-b")
		logging.GetLogger("agent.test_routing_guard_125974").LogContext(home.WithOverride(context.Background(), homeA), logging.LevelInfo, "routing-guard-witness-a")
		logging.FlushLogQueue()
		logB, logA := readLog(filepath.Join(homeB, "logs", "agent.log")), readLog(filepath.Join(homeA, "logs", "agent.log"))
		if !strings.Contains(logB, "routing-guard-witness-b") {
			t.Fatal("home b record missing")
		}
		if strings.Contains(logB, "routing-guard-witness-a") {
			t.Fatal("a bare handler beside the routers took another home's record")
		}
		if !strings.Contains(logA, "routing-guard-witness-a") || strings.Contains(logA, "routing-guard-witness-b") {
			t.Fatal("the routers stopped routing by home")
		}
	})
}

func TestSafeWorkerBypassesRuleReadersEvenWithoutCliFlags(t *testing.T) {
	for _, mode := range []string{"safe", "config", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			config.ResetWorkerPolicyForTests()
			t.Cleanup(config.ResetWorkerPolicyForTests)
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			project := filepath.Join(h, "project")
			sub := filepath.Join(project, "sub")
			for _, f := range [][2]string{
				{filepath.Join(project, "AGENTS.md"), "PROJECT_SENTINEL"}, {filepath.Join(sub, "AGENTS.md"), "SUB_SENTINEL"},
				{filepath.Join(h, "SOUL.md"), "SOUL_SENTINEL"}, {filepath.Join(h, "memories", "MEMORY.md"), "MEMORY_SENTINEL"},
				{filepath.Join(h, "skills", "probe", "SKILL.md"), "---\nname: probe\ndescription: SKILL_SENTINEL\n---\nSKILL_SENTINEL\n"},
			} {
				if err := os.MkdirAll(filepath.Dir(f[0]), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f[0], []byte(f[1]), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "ordinary" {
				if _, err := config.BindSafeWorkerPolicy(mode == "safe", true, map[string]any{}); err != nil {
					t.Fatal(err)
				}
			}
			var reads []string
			prevRead := tools.ReadContextFileText
			tools.ReadContextFileText = func(path string) (string, error) {
				reads = append(reads, filepath.Base(path))
				return prevRead(path)
			}
			t.Cleanup(func() { tools.ReadContextFileText = prevRead })
			ctx := context.Background()
			env := &tools.Env{Home: h, Values: &sync.Map{}}
			contextText, err := BuildContextFilesPrompt(ctx, env, ContextFilesOptions{Cwd: &project})
			if err != nil {
				t.Fatal(err)
			}
			soul, _ := LoadSoulMD(ctx, env, nil, "")
			scope := skills.NewScope(h, nil)
			skillsText, err := scope.BuildSkillsSystemPrompt(ctx, skills.SkillsPromptOptions{})
			if err != nil {
				t.Fatal(err)
			}
			preloaded, _, missing := scope.BuildPreloadedSkillsPrompt(ctx, []string{"probe"}, "", nil)
			hints, _ := tools.NewSubdirectoryHintTrackerFor(env, project, true).CheckToolCall("read_file", map[string]any{"path": filepath.Join(sub, "file.py")})
			a := &AIAgent{env: env, enabledToolsets: []string{"memory"}, disabledToolsets: []string{}}
			initMemory(ctx, a, map[string]any{"memory": map[string]any{"memory_enabled": true, "user_profile_enabled": true}}, false, "cli", nil)
			var engines []string
			prevLoad := loadContextEngineFn
			loadContextEngineFn = func(_ context.Context, name string) *plugins.HostedObject {
				engines = append(engines, name)
				return nil
			}
			t.Cleanup(func() { loadContextEngineFn = prevLoad })
			selectContextEngine(ctx, map[string]any{"context": map[string]any{"engine": "sentinel"}})
			if mode == "safe" {
				if contextText != "" || soul != "" || skillsText != "" || preloaded != "" || !reflect.DeepEqual(missing, []string{"probe"}) ||
					hints != "" || len(reads) != 0 || len(engines) != 0 || a.memoryStore != nil {
					t.Fatalf("safe worker read rules: context %q soul %q skills %q preloaded %q missing %v hints %q reads %v engines %v memory %v",
						contextText, soul, skillsText, preloaded, missing, hints, reads, engines, a.memoryStore != nil)
				}
				return
			}
			if !strings.Contains(contextText, "PROJECT_SENTINEL") || !strings.Contains(soul, "SOUL_SENTINEL") || !strings.Contains(skillsText, "SKILL_SENTINEL") ||
				!strings.Contains(preloaded, "SKILL_SENTINEL") || !strings.Contains(hints, "SUB_SENTINEL") {
				t.Fatalf("rules not read: context %q soul %q skills %q preloaded %q hints %q", contextText, soul, skillsText, preloaded, hints)
			}
			seen := map[string]bool{}
			for _, r := range reads {
				seen[r] = true
			}
			if !seen["AGENTS.md"] || !seen["SOUL.md"] || a.memoryStore == nil || !reflect.DeepEqual(a.memoryStore.Entries("memory"), []string{"MEMORY_SENTINEL"}) {
				t.Fatalf("reads %v memory %v", reads, a.memoryStore != nil)
			}
			if !reflect.DeepEqual(engines, []string{"sentinel"}) {
				t.Fatalf("engines %v", engines)
			}
		})
	}
}

func TestCredentialPoolPreservedOnAutoDetect(t *testing.T) {
	t.Run("test_anthropic_pool_preserved_with_url_auto_detect", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		pool := provider.NewPool(agentPoolEnv(context.Background()), "anthropic", nil)
		a, err := NewAIAgent(context.Background(), AIAgentOptions{BaseURL: "https://api.anthropic.com", APIKey: "test-key", Model: "test-model",
			CredentialPool: pool, SkipContextFiles: true, SkipMemory: true, QuietMode: true})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if a.provider != "anthropic" || a.apiMode != "anthropic_messages" || a.credentialPool != pool {
			t.Fatalf("provider %q api_mode %q pool kept %v", a.provider, a.apiMode, a.credentialPool == pool)
		}
	})
}

func TestMissingRequirementsKeepsToolInventory(t *testing.T) {
	for _, setting := range []any{nil, false, true} {
		t.Run(pyReprForCase(setting), func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(h, "managed"))
			body := "{}"
			if setting != nil {
				body = `{"display": {"suppress_warning_notifications": ` + strings.ToLower(pyReprForCase(setting)) + `}}`
			}
			if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			prevDefs, prevReqs, prevDiscover := loadToolDefinitions, loadToolsetRequirements, discoverPluginsAtLoad
			loadToolDefinitions = func(context.Context, *tools.Env, tools.DefinitionOptions) []map[string]any {
				return []map[string]any{{"function": map[string]any{"name": "offline_tool"}}}
			}
			loadToolsetRequirements = func(context.Context, *tools.Env) map[string]bool { return map[string]bool{"fixture_dependency": false} }
			discoverPluginsAtLoad = func(context.Context) {}
			t.Cleanup(func() {
				loadToolDefinitions, loadToolsetRequirements, discoverPluginsAtLoad = prevDefs, prevReqs, prevDiscover
			})
			out := captureStatusStdout(t)
			var printed []string
			a := &AIAgent{platform: "cli", model: "fixture", maxIterations: 1, env: &tools.Env{Home: h, Values: &sync.Map{}},
				printFn: func(s string) { printed = append(printed, s) }}
			loadTools(context.Background(), a, nil, nil)
			output := out.String() + strings.Join(printed, "\n")
			if strings.Contains(output, "Some tools may not work due to missing requirements") == (setting == true) {
				t.Fatalf("policy %v output %q", setting, output)
			}
			if !strings.Contains(output, "Loaded 1 tools") || !reflect.DeepEqual(a.validToolNames, map[string]bool{"offline_tool": true}) {
				t.Fatalf("output %q names %v", output, a.validToolNames)
			}
		})
	}
}

// Plugin engine offered by the general plugin system, cloned as itself
type offeredEngine struct{ stubEngine }

func (e *offeredEngine) Name() string                 { return "offered" }
func (e *offeredEngine) CloneForAgent() ContextEngine { return e }

func TestDefaultCompressorDoesNotActivateAnOfferedPlugin(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	candidate := &offeredEngine{stubEngine{ContextEngineBase: NewContextEngineBase()}}
	prevLoad, prevPlugin := loadContextEngineFn, pluginContextEngineFn
	loadContextEngineFn = func(context.Context, string) *plugins.HostedObject { return nil }
	pluginContextEngineFn = func(context.Context) any { return candidate }
	t.Cleanup(func() { loadContextEngineFn, pluginContextEngineFn = prevLoad, prevPlugin })
	ctx := context.Background()
	if got := selectContextEngine(ctx, map[string]any{"context": map[string]any{"engine": "compressor"}}); got != nil {
		t.Fatalf("compressor activated %v", got)
	}
	if got := selectContextEngine(ctx, map[string]any{"context": map[string]any{"engine": "offered"}}); got == nil || got.Name() != "offered" {
		t.Fatalf("offered engine %v", got)
	}
}

func TestKeyCmdOauthRouteBuildsClaudeCodeIdentityOnMainAndAuxClients(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	const oauth = "sk-ant-oat01-keycmd-token"
	keyCmd := func(context.Context) (string, error) { return oauth, nil }
	var builtKey any
	var builtBase string
	prev := anthropicClientBuilder
	t.Cleanup(func() { anthropicClientBuilder = prev })
	anthropicClientBuilder = func(ctx context.Context, key any, base string, timeout float64, drop bool) (any, error) {
		builtKey, builtBase = key, base
		return prev(ctx, key, base, timeout, drop)
	}
	ctx := context.Background()
	a := &AIAgent{provider: "enterprise-anthropic", model: "claude-opus-5", quietMode: true}
	if err := initAnthropicClient(ctx, a, keyCmd, "https://api.anthropic.com", nil); err != nil {
		t.Fatal(err)
	}
	if !a.isAnthropicOAuth || a.anthropicClient == nil || builtBase != "https://api.anthropic.com" {
		t.Fatalf("oauth=%v client=%v base=%q", a.isAnthropicOAuth, a.anthropicClient, builtBase)
	}
	src, ok := builtKey.(*provider.TaggedTokenSource)
	if !ok || src != a.anthropicTokenSource || a.anthropicKeyValue() != src {
		t.Fatalf("callable key not handed to the client: %T", builtKey)
	}
	if minted, err := src.Mint(ctx); err != nil || minted != oauth {
		t.Fatalf("minted %q %v", minted, err)
	}
	t.Run("runtime_token_source_reaches_the_client", func(t *testing.T) {
		rt := &provider.Runtime{Provider: "custom", APIMode: provider.ModeAnthropicMessages, BaseURL: "https://api.anthropic.com",
			TokenSource: provider.TagCredentialSource(keyCmd, provider.CredentialSourceKeyCmd).Func()}
		built, err := NewAIAgent(ctx, AIAgentOptions{Runtime: rt, Model: "claude-opus-5", EnabledToolsets: []string{}, QuietMode: true,
			SkipContextFiles: true, SkipMemory: true, SkipBackgroundReview: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { built.Close() })
		if !built.isAnthropicOAuth || built.anthropicTokenSource == nil || provider.CredentialLabel(built.anthropicTokenSource) == "" {
			t.Fatalf("key_cmd runtime lost its key: oauth=%v source=%v", built.isAnthropicOAuth, built.anthropicTokenSource)
		}
	})
}

type instanceScopedProvider struct {
	memory.BaseProvider
	home string
}

func (p *instanceScopedProvider) Name() string                   { return "instancescoped" }
func (p *instanceScopedProvider) Available(context.Context) bool { return true }
func (p *instanceScopedProvider) ToolSchemas(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"name": "instance_tool", "description": "x", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}, nil
}
func (p *instanceScopedProvider) Initialize(_ context.Context, _ string, o memory.InitOptions) error {
	p.home = o.Home
	return nil
}

func TestAgentWhoseProviderInstanceIsRefusedIsToldAndRunsWithoutIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	alpha, beta := filepath.Join(root, "profiles", "alpha"), filepath.Join(root, "profiles", "beta")
	for _, h := range []string{alpha, beta} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", beta)
	shared := &instanceScopedProvider{}
	ctx := provider.WithMultiplexContext(context.Background(), true)
	alphaCtx := home.WithOverride(ctx, alpha)
	owner := memory.NewMemoryManager(alphaCtx, nil, nil)
	if err := owner.AddProvider(alphaCtx, shared); err != nil {
		t.Fatal(err)
	}
	owner.InitializeAll(alphaCtx, "s-alpha", memory.InitOptions{Platform: "api_server", Home: alpha})
	memory.RegisterProvider(memory.ProviderRegistration{Name: "instancescoped", Source: memory.SourceUser, Home: beta,
		New: func(context.Context, *tools.Env, memory.LoadOptions) (memory.Provider, error) { return shared, nil }})
	t.Cleanup(func() { memory.UnregisterProviders(memory.SourceUser, beta) })
	records := captureLogger(t, "run_agent", slog.LevelDebug)
	a, _ := startupAgent(t, "api_server")
	a.sessionID = "s-beta"
	initMemory(home.WithOverride(ctx, beta), a, map[string]any{"memory": map[string]any{"provider": "instancescoped"}}, false, "api_server", nil)
	if a.memoryManager != nil {
		t.Fatal("the refused provider still built a memory manager")
	}
	for _, r := range records() {
		if strings.Contains(r.msg, "activated") {
			t.Fatalf("refused provider claimed activation: %q", r.msg)
		}
	}
	var notices []provider.AgentNotice
	a.noticeCallback = func(n provider.AgentNotice) { notices = append(notices, n) }
	a.replayStartupWarnings()
	if len(notices) != 1 || notices[0].Level != "warn" || !strings.Contains(notices[0].Text, "instancescoped") {
		t.Fatalf("notices %+v", notices)
	}
	if strings.Contains(notices[0].Text, alpha) || strings.Contains(notices[0].Text, beta) {
		t.Fatalf("notice names a home: %q", notices[0].Text)
	}
	runtime.KeepAlive(shared)
}

func TestColdRefineBuildsNoMemoryProvider(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if config.SafeWorkerEnabled() {
		t.Fatal("safe worker policy is active in the test process")
	}
	var loaded []string
	memory.RegisterProvider(memory.ProviderRegistration{Name: "fake", Source: memory.SourceUser, Home: h,
		New: func(context.Context, *tools.Env, memory.LoadOptions) (memory.Provider, error) {
			loaded = append(loaded, "fake")
			return &instanceScopedProvider{}, nil
		}})
	t.Cleanup(func() { memory.UnregisterProviders(memory.SourceUser, h) })
	ctx := home.WithOverride(context.Background(), h)
	if memory.IsCoreMemoryProvider("fake") {
		t.Fatal("fake reads as a core provider")
	}
	if memory.LoadMemoryProvider(ctx, nil, "fake") == nil || len(loaded) != 1 {
		t.Fatalf("the registered provider does not load: %v", loaded)
	}
	loaded = nil
	a := &AIAgent{sessionID: "refine-memory-parent", validToolNames: map[string]bool{}}
	initMemory(ctx, a, map[string]any{"memory": map[string]any{"provider": "fake"}}, true, "local", nil)
	if len(loaded) != 0 {
		t.Fatalf("the review side agent loaded a memory provider: %v", loaded)
	}
	if a.memoryManager != nil {
		t.Fatal("the review side agent built a memory manager")
	}
}

func TestASessionOffTheDesktopNeverHearsOfTheToolEvenThroughToolSearch(t *testing.T) {
	for _, platform := range []string{"desktop", "cli", "subagent"} {
		t.Run(platform, func(t *testing.T) {
			env := promptTestEnv(t)
			ctx := home.WithOverride(context.Background(), env.Home)
			prev := discoverPluginsAtLoad
			t.Cleanup(func() { discoverPluginsAtLoad = prev })
			discoverPluginsAtLoad = func(context.Context) {}
			enabled, disabled := []string{"catalog", "web"}, []string(nil)
			if platform == "subagent" {
				parent := &AIAgent{enabledToolsets: []string{"catalog", "web"}, env: env}
				enabled, disabled = resolveChildToolsets(ctx, parent, nil, "leaf")
			}
			if !slices.Contains(enabled, "catalog") {
				t.Fatalf("catalog not enabled: %v", enabled)
			}
			a := &AIAgent{quietMode: true, platform: platform, enabledToolsets: enabled, disabledToolsets: disabled, env: env}
			loadTools(ctx, a, enabled, disabled)
			found := tools.DispatcherOf(ctx).DispatchWith(ctx, &tools.Call{Name: "tool_search", Env: env,
				Args: map[string]any{"queries": []any{"catalog plugin install"}}},
				tools.DispatchOptions{EnabledToolsets: a.enabledToolsets, DisabledToolsets: a.disabledToolsets})
			surface, err := json.Marshal(a.tools)
			if err != nil {
				t.Fatal(err)
			}
			carried := strings.Contains(string(surface), "manage_catalog") || strings.Contains(found, "manage_catalog")
			if carried != (platform == "desktop") {
				t.Fatalf("platform %s carried=%v search=%s", platform, carried, found)
			}
		})
	}
}
