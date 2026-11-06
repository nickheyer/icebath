package provider

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Transport counting the closes the async client sends it
type auxCountingCloseTransport struct{ closes atomic.Int32 }

func (t *auxCountingCloseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrNotSupported
}
func (t *auxCountingCloseTransport) Close() error { t.closes.Add(1); return nil }

// Home with a config file bound to the returned context
func auxResolveHome(t *testing.T, configYAML string) context.Context {
	t.Helper()
	ctx := modelsTestHome(t)
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config.ResetCache()
	return ctx
}

func TestNeuterAsyncHTTPXDelMakesCollectionCloseANoOp(t *testing.T) {
	t.Cleanup(func() { auxAsyncHTTPXDelNeutered.Store(false) })
	live := &auxCountingCloseTransport{}
	auxAsyncTransportCollected(&auxAsyncTransport{http: &http.Client{Transport: live}, owns: true})
	if live.closes.Load() != 1 {
		t.Fatalf("collection close before neuter ran %d times", live.closes.Load())
	}
	NeuterAsyncHTTPXDel()
	neutered := &auxCountingCloseTransport{}
	state := &auxAsyncTransport{http: &http.Client{Transport: neutered}, owns: true}
	auxAsyncTransportCollected(state)
	if neutered.closes.Load() != 0 || state.closed.Load() {
		t.Fatal("neutered collection close touched the transport")
	}
	collected := &auxCountingCloseTransport{}
	func() {
		c := newAuxAsyncOpenAIClient("k", "http://127.0.0.1:1/v1", auxOpenAIOptions{HTTPClient: &http.Client{Transport: collected}})
		runtime.KeepAlive(c)
	}()
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if collected.closes.Load() != 0 {
		t.Fatal("collected async client closed its transport after neuter")
	}
}

func TestAsyncOpenAIClientAwaitedAndForcedCloses(t *testing.T) {
	tr := &auxCountingCloseTransport{}
	c := newAuxAsyncOpenAIClient("k", "http://127.0.0.1:1/v1", auxOpenAIOptions{HTTPClient: &http.Client{Transport: tr}})
	if err := c.CloseAwaitable()(); err != nil || tr.closes.Load() != 1 || !c.IsClosed() {
		t.Fatalf("awaited close %v closes %d", err, tr.closes.Load())
	}
	_ = c.Close()
	if tr.closes.Load() != 1 {
		t.Fatal("second close reached the transport")
	}
	forced := &auxCountingCloseTransport{}
	f := newAuxAsyncOpenAIClient("k", "http://127.0.0.1:1/v1", auxOpenAIOptions{HTTPClient: &http.Client{Transport: forced}})
	f.ForceCloseTransport()
	auxAsyncTransportCollected(f.transport)
	_ = f.CloseAwaitable()()
	if forced.closes.Load() != 0 || !f.IsClosed() {
		t.Fatalf("force closed transport closed %d times", forced.closes.Load())
	}
}

func TestToAsyncClientKeepsCredentialHeadersAndWrappers(t *testing.T) {
	ctx := auxResolveHome(t, "")
	mint := func() (string, error) { return "minted", nil }
	sync := auxCreateOpenAIClient(ctx, mint, "https://gw.example.com/v1", auxOpenAIOptions{DefaultHeaders: map[string]string{"X-Route": "blue"}})
	async, model := auxToAsyncClient(ctx, sync, "m", false)
	ac, ok := async.(*auxAsyncOpenAIClient)
	if !ok || model != "m" {
		t.Fatalf("async client %T model %q", async, model)
	}
	if key, err := ac.refreshAPIKey(); err != nil || key != "minted" {
		t.Fatalf("async credential %q %v", key, err)
	}
	if ac.customHeaders["X-Route"] != "blue" || ac.maxRetries != 0 || ac.BaseURL() != "https://gw.example.com/v1/" {
		t.Fatalf("async headers %v retries %d base %q", ac.customHeaders, ac.maxRetries, ac.BaseURL())
	}
	probe := &auxProbeClient{baseURL: "x"}
	if got, _ := auxToAsyncClient(ctx, probe, "m", false); got != AuxClient(probe) {
		t.Fatal("probe stub was rewrapped")
	}
	codex := NewCodexAuxiliaryClient(sync.(*auxOpenAIClient), "gpt-5-codex")
	if got, _ := auxToAsyncClient(ctx, codex, "m", false); func() bool { _, ok := got.(*AsyncCodexAuxiliaryClient); return !ok }() {
		t.Fatalf("codex async %T", got)
	}
	gem, err := newAuxGeminiNativeClient("gk", "https://generativelanguage.googleapis.com/v1beta", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := auxToAsyncClient(ctx, gem, "gemini-3-flash", false)
	ag, ok := got.(*auxAsyncGeminiNativeClient)
	if !ok || ag.auxRealClient() != AuxClient(gem) || ag.APIKey() != "gk" {
		t.Fatalf("gemini async %T", got)
	}
	if err := ag.Close(); err != nil || !gem.IsClosed() {
		t.Fatal("async gemini close did not close the sync client")
	}
}

func TestEffectiveProviderStampSurvivesOnEveryClientType(t *testing.T) {
	ctx := auxResolveHome(t, "")
	plain := auxCreateOpenAIClient(ctx, "k", "https://x.example.com/v1", auxOpenAIOptions{})
	clients := []AuxClient{plain, NewCodexAuxiliaryClient(plain.(*auxOpenAIClient), "m"), &auxProbeClient{},
		NewBedrockAuxiliaryClient("us-east-1", "m"), NewAsyncCodexAuxiliaryClient(NewCodexAuxiliaryClient(plain.(*auxOpenAIClient), "m"))}
	for _, c := range clients {
		if !auxStampEffectiveProvider(c, "zai") || auxEffectiveProviderForClient(c, "auto") != "zai" {
			t.Fatalf("%T lost the stamp", c)
		}
	}
	if auxStampEffectiveProvider(auxNoResponsesClient{plain}, "zai") {
		t.Fatal("value wrapper claimed a stamp")
	}
}

func TestUnknownProviderLogsDebugOnceNotWarning(t *testing.T) {
	ctx := auxResolveHome(t, "")
	auxLoggedUnknownProviderKeys.clear()
	t.Cleanup(auxLoggedUnknownProviderKeys.clear)
	logs := auxCaptureLogs(t)
	for i := 0; i < 3; i++ {
		client, model, err := ResolveProviderClient(ctx, "no_such_provider_xyz", "", ProviderClientOptions{})
		if err != nil || client != nil || model != "" {
			t.Fatalf("unknown provider resolved %v %q %v", client, model, err)
		}
	}
	recs := logs.matching("unknown provider")
	if len(recs) != 1 || recs[0].level != slog.LevelDebug {
		t.Fatalf("unknown provider records %+v", recs)
	}
}

func TestV1TailAppliesOnlyToBareAliasHostsAndNeverDoubles(t *testing.T) {
	ctx := auxResolveHome(t, "")
	custom, _, err := ResolveProviderClient(ctx, "custom", "m", ProviderClientOptions{ExplicitBaseURL: "http://127.0.0.1:11434", ExplicitAPIKey: "test-key-123"})
	if err != nil || custom == nil || strings.TrimRight(custom.BaseURL(), "/") != "http://127.0.0.1:11434" {
		t.Fatalf("custom %v %v", custom, err)
	}
	withPath, _, err := ResolveProviderClient(ctx, "vllm", "m", ProviderClientOptions{ExplicitBaseURL: "http://127.0.0.1:8000/v1", ExplicitAPIKey: "test-key-123"})
	if err != nil || withPath == nil || strings.TrimRight(withPath.BaseURL(), "/") != "http://127.0.0.1:8000/v1" || withPath.APIKey() != "test-key-123" {
		t.Fatalf("vllm %v %v", withPath, err)
	}
	bare, _, err := ResolveProviderClient(ctx, "ollama", "m", ProviderClientOptions{ExplicitBaseURL: "http://127.0.0.1:11434"})
	if err != nil || bare == nil || strings.TrimRight(bare.BaseURL(), "/") != "http://127.0.0.1:11434/v1" || bare.APIKey() != "no-key-required" {
		t.Fatalf("bare ollama alias %v %v", bare, err)
	}
}

func TestLocalAliasNeverBorrowsOpenAIKey(t *testing.T) {
	ctx := auxResolveHome(t, "")
	t.Setenv("OPENAI_API_KEY", "sk-openai-secret")
	client, _, err := ResolveProviderClient(ctx, "ollama", "m", ProviderClientOptions{ExplicitBaseURL: "http://10.1.1.1:11434/v1"})
	if err != nil || client == nil || client.APIKey() != "no-key-required" {
		t.Fatalf("local alias key %v %v", client, err)
	}
	custom, _, err := ResolveProviderClient(ctx, "custom", "m", ProviderClientOptions{ExplicitBaseURL: "http://10.1.1.1:11434/v1"})
	if err != nil || custom == nil || custom.APIKey() != "sk-openai-secret" {
		t.Fatalf("bare custom key %v %v", custom, err)
	}
}

func TestNamedCustomProviderComposesUnderExplicitEndpoint(t *testing.T) {
	ctx := auxResolveHome(t, "custom_providers:\n  - name: my-proxy\n    base_url: https://proxy.example.com/v1\n    api_key: sk-proxy\n    model: proxy-model\n    extra_headers:\n      X-Route: blue\n")
	client, model, err := ResolveProviderClient(ctx, "my-proxy", "", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("named custom %v %v", client, err)
	}
	oc, ok := client.(*auxOpenAIClient)
	if !ok || model != "proxy-model" || oc.BaseURL() != "https://proxy.example.com/v1/" || oc.APIKey() != "sk-proxy" || oc.customHeaders["X-Route"] != "blue" {
		t.Fatalf("named custom client %T model %q headers %v", client, model, oc.customHeaders)
	}
	mainCtx := auxResolveHome(t, "model:\n  default: main-model\ncustom_providers:\n  - name: my-proxy\n    base_url: https://proxy.example.com/v1\n    api_key: sk-proxy\n    model: proxy-model\n")
	if _, mainModel, err := ResolveProviderClient(mainCtx, "my-proxy", "", ProviderClientOptions{}); err != nil || mainModel != "main-model" {
		t.Fatalf("configured main model did not prefill the request %q %v", mainModel, err)
	}
	over, _, err := ResolveProviderClient(ctx, "my-proxy", "m2", ProviderClientOptions{ExplicitBaseURL: "https://other.example.com/v1"})
	if err != nil || over == nil || over.BaseURL() != "https://other.example.com/v1/" || over.APIKey() != "sk-proxy" {
		t.Fatalf("explicit base composition %v %v", over, err)
	}
	codex, _, err := ResolveProviderClient(ctx, "my-proxy", "m", ProviderClientOptions{APIMode: "codex_responses"})
	if _, ok := codex.(*CodexAuxiliaryClient); err != nil || !ok {
		t.Fatalf("codex api mode %T %v", codex, err)
	}
	async, _, err := ResolveProviderClient(ctx, "my-proxy", "m", ProviderClientOptions{AsyncMode: true})
	if ac, ok := async.(*auxAsyncOpenAIClient); err != nil || !ok || ac.customHeaders["X-Route"] != "blue" || ac.APIKey() != "sk-proxy" {
		t.Fatalf("async named custom %T %v", async, err)
	}
}

func TestOpenAICodexBranchNeedsModelAndToken(t *testing.T) {
	ctx := auxResolveHome(t, "")
	logs := auxCaptureLogs(t)
	if c, m, err := ResolveProviderClient(ctx, "openai-codex", "gpt-5.5", ProviderClientOptions{RawCodex: true}); err != nil || c != nil || m != "" {
		t.Fatalf("codex without token %v %q %v", c, m, err)
	}
	if len(logs.matching("no Codex OAuth token found (run: icebath model)")) == 0 {
		t.Fatal("missing codex token warning")
	}
}

func TestUnhandledAuthTypeLogsDebugOnceNotWarning(t *testing.T) {
	ctx := auxResolveHome(t, "")
	auxLoggedUnhandledAuthTypeKeys.clear()
	t.Cleanup(auxLoggedUnhandledAuthTypeKeys.clear)
	registerTestProfile(t, Profile{Name: "bogus_authtype", DisplayName: "Bogus", AuthType: "totally_unhandled_scheme"})
	logs := auxCaptureLogs(t)
	for i := 0; i < 2; i++ {
		client, model, err := ResolveProviderClient(ctx, "bogus_authtype", "", ProviderClientOptions{})
		if err != nil || client != nil || model != "" {
			t.Fatalf("unhandled auth type resolved %v %q %v", client, model, err)
		}
	}
	recs := logs.matching("unhandled auth_type")
	if len(recs) != 1 || recs[0].level != slog.LevelDebug {
		t.Fatalf("unhandled auth_type records %+v", recs)
	}
}
