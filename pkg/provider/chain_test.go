package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func ident(provider, model, baseURL string) BackendIdentity {
	return BuildBackendIdentity(provider, model, baseURL)
}

func withFirstClass(t *testing.T, names ...string) {
	t.Helper()
	old := providerIsFirstClass
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	providerIsFirstClass = func(p string) bool { return set[p] }
	t.Cleanup(func() { providerIsFirstClass = old })
}

func TestSameDeployment(t *testing.T) {
	failed := ident("custom", "zai-org/glm-5.2", "")
	sibling := ident("custom", "mindai/macaron-v1-venti", "")
	if SameDeployment(sibling, failed) || ShouldSkipCandidate(sibling, failed, ScopeModel) {
		t.Fatal("sibling models are independent deployments")
	}
	a := ident("custom", "qwen-3", "http://box1:1234/v1")
	b := ident("custom", "qwen-3", "http://box2:1234/v1")
	if SameDeployment(a, b) || ShouldSkipCandidate(a, b, ScopeModel) {
		t.Fatal("same model on two explicit urls is a pool")
	}
	a = ident("claude-cli", "claude-opus-4.7", "http://127.0.0.1:7891/v1")
	b = ident("claude-cli-alt", "claude-opus-4.7", "http://127.0.0.1:7891/v1")
	if !SameDeployment(a, b) {
		t.Fatal("shim aliases at one url with one model are the same deployment")
	}
	withFirstClass(t, "xai", "xai-oauth")
	a = ident("xai-oauth", "grok-4.5", "https://api.x.ai/v1")
	b = ident("xai", "grok-4.5", "https://api.x.ai/v1")
	if SameDeployment(a, b) || ShouldSkipCandidate(a, b, ScopeModel) {
		t.Fatal("first class pair on one host is not the same")
	}
}

func TestSameCredentialSurface(t *testing.T) {
	failed := ident("custom", "zai-org/glm-5.2", "")
	sibling := ident("custom", "mindai/macaron-v1-venti", "")
	if !SameCredentialSurface(sibling, failed) || !ShouldSkipCandidate(sibling, failed, ScopeCredential) {
		t.Fatal("same provider label shares the credential")
	}
	withFirstClass(t, "xai", "xai-oauth")
	a := ident("xai", "grok-4.5", "https://api.x.ai/v1")
	b := ident("xai-oauth", "grok-4.5", "https://api.x.ai/v1")
	if SameCredentialSurface(a, b) || ShouldSkipCandidate(a, b, ScopeCredential) {
		t.Fatal("distinct first class providers differ")
	}
	if SameCredentialSurface(ident("proxy-a", "m", "http://gw:9000/v1"), ident("proxy-b", "m", "http://gw:9000/v1")) {
		t.Fatal("distinct custom labels on one url are not assumed shared")
	}
}

func TestSameEndpoint(t *testing.T) {
	a := ident("a", "m1", "http://host:8000/v1/")
	b := ident("b", "m2", "http://HOST:8000/v1")
	if !SameEndpoint(a, b) || !ShouldSkipCandidate(a, b, ScopeEndpoint) {
		t.Fatal("trailing slash and host case are the same endpoint")
	}
	failed := ident("custom", "m", "https://Example.test/API/v1?token=AbC")
	if ShouldSkipCandidate(ident("custom", "m", "https://example.test/api/v1?token=AbC"), failed, ScopeEndpoint) {
		t.Fatal("path case distinguishes endpoints")
	}
	if ShouldSkipCandidate(ident("custom", "m", "https://example.test/API/v1?token=abc"), failed, ScopeEndpoint) {
		t.Fatal("query case distinguishes endpoints")
	}
	if SameEndpoint(ident("a", "m", "http://h1/v1"), ident("a2", "m", "http://h2/v1")) {
		t.Fatal("different urls differ")
	}
	if !SameEndpoint(ident("openrouter", "m1", ""), ident("openrouter", "m2", "")) || SameEndpoint(ident("openrouter", "m", ""), ident("nous", "m", "")) {
		t.Fatal("unknown url falls back to the provider default")
	}
}

func TestUnknownAxesNeverStrand(t *testing.T) {
	failed := ident("custom", "glm", "http://h/v1")
	for _, scope := range FailureScopes {
		if ShouldSkipCandidate(ident("", "", ""), failed, scope) {
			t.Fatalf("empty candidate skipped under %s", scope)
		}
	}
	if ShouldSkipCandidate(ident("custom", "some-model", ""), ident("custom", "", ""), ScopeModel) {
		t.Fatal("model scope requires model evidence")
	}
}

func TestReadFallbackChainReturnsCopies(t *testing.T) {
	cfg := config.Tree{"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "anthropic/claude-sonnet-4.6"}, map[string]any{"provider": "nous", "model": "Hermes-4-Llama-3.1-405B"}}}
	chain := ReadFallbackChain(cfg)
	if len(chain) != 2 || chain[0]["provider"] != "openrouter" || chain[1]["model"] != "Hermes-4-Llama-3.1-405B" {
		t.Fatalf("chain %v", chain)
	}
	chain[0]["provider"] = "mutated"
	if cfg["fallback_providers"].([]any)[0].(map[string]any)["provider"] != "openrouter" {
		t.Fatal("chain must not alias the config")
	}
}

func TestExtractFallbackFromModelCfg(t *testing.T) {
	entry, ok := ExtractFallbackFromModelCfg(map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6"})
	if !ok || entry["provider"] != "openrouter" || entry["model"] != "anthropic/claude-sonnet-4.6" || len(entry) != 2 {
		t.Fatalf("entry %v", entry)
	}
	if _, ok := ExtractFallbackFromModelCfg(map[string]any{"provider": "openrouter"}); ok {
		t.Fatal("missing model must give none")
	}
	if _, ok := ExtractFallbackFromModelCfg("string"); ok {
		t.Fatal("non map must give none")
	}
}

func writeConfig(t *testing.T, h string, doc map[string]any) {
	t.Helper()
	if err := config.SaveUser(h, doc); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	doc, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestFallbackListWithEntries(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "openrouter", "default": "gpt-5.4"}, "fallback_providers": []any{map[string]any{"provider": "nous", "model": "Hermes-4"}}})
	var out bytes.Buffer
	if err := FallbackList(&out, h); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Primary:   gpt-5.4  (via openrouter)") || !strings.Contains(text, "Fallback chain (1 entry):") || !strings.Contains(text, "1. Hermes-4  (via nous)") {
		t.Fatalf("list output %q", text)
	}
	empty := t.TempDir()
	writeConfig(t, empty, map[string]any{})
	out.Reset()
	if err := FallbackList(&out, empty); err != nil || !strings.Contains(out.String(), "No fallback providers configured.") || !strings.Contains(out.String(), "icebath fallback add") {
		t.Fatalf("empty list %q %v", out.String(), err)
	}
}

func pickerWriting(h string, model map[string]any, extra map[string]any) func() error {
	return func() error {
		doc, err := config.ReadUser(h)
		if err != nil {
			return err
		}
		doc["model"] = model
		for k, v := range extra {
			doc[k] = v
		}
		return config.SaveUser(h, doc)
	}
}

func TestFallbackAddAppendsNewEntry(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6"}})
	store := NewStore(h)
	picked := map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6", "base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}
	var out bytes.Buffer
	if err := FallbackAdd(&out, h, store, pickerWriting(h, picked, nil)); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, h)
	model := cfg["model"].(map[string]any)
	if model["provider"] != "anthropic" || model["default"] != "claude-sonnet-4-6" {
		t.Fatalf("primary not preserved: %v", model)
	}
	chain := cfg["fallback_providers"].([]any)
	if len(chain) != 1 {
		t.Fatalf("chain %v", chain)
	}
	entry := chain[0].(map[string]any)
	if entry["provider"] != "openrouter" || entry["model"] != "anthropic/claude-sonnet-4.6" || entry["base_url"] != "https://openrouter.ai/api/v1" || entry["api_mode"] != "chat_completions" {
		t.Fatalf("entry %v", entry)
	}
	if !strings.Contains(out.String(), "Added fallback: anthropic/claude-sonnet-4.6  (via openrouter)  [https://openrouter.ai/api/v1]") {
		t.Fatalf("output %q", out.String())
	}
}

func TestFallbackAddRejectsSameAsPrimary(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "openrouter", "default": "gpt-5.4"}})
	var out bytes.Buffer
	if err := FallbackAdd(&out, h, NewStore(h), pickerWriting(h, map[string]any{"provider": "openrouter", "default": "gpt-5.4"}, nil)); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, h)
	if list, has := cfg["fallback_providers"]; has && len(list.([]any)) != 0 {
		t.Fatalf("self fallback written: %v", list)
	}
	if !strings.Contains(out.String(), "A provider cannot be a fallback for itself") {
		t.Fatalf("output %q", out.String())
	}
}

func TestFallbackAddPreservesPrimaryWhenPickerChangesIt(t *testing.T) {
	h := t.TempDir()
	primary := map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"}
	writeConfig(t, h, map[string]any{"model": primary})
	custom := []any{map[string]any{"name": "Picker-created endpoint", "base_url": "https://picker.example/v1", "model": "picker-model"}}
	picked := map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6", "base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}
	var out bytes.Buffer
	if err := FallbackAdd(&out, h, NewStore(h), pickerWriting(h, picked, map[string]any{"custom_providers": custom})); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, h)
	model := cfg["model"].(map[string]any)
	for k, v := range primary {
		if model[k] != v {
			t.Fatalf("primary field %s = %v want %v", k, model[k], v)
		}
	}
	if len(cfg["fallback_providers"].([]any)) != 1 || cfg["fallback_providers"].([]any)[0].(map[string]any)["provider"] != "openrouter" {
		t.Fatalf("fallback %v", cfg["fallback_providers"])
	}
	got := cfg["custom_providers"].([]any)[0].(map[string]any)
	if got["name"] != "Picker-created endpoint" || got["base_url"] != "https://picker.example/v1" || got["model"] != "picker-model" {
		t.Fatalf("custom providers %v", cfg["custom_providers"])
	}
}

func TestRestorePreservesAbsentActiveProvider(t *testing.T) {
	h := t.TempDir()
	store := NewStore(h)
	if err := store.Save(AuthData{"version": 1, "providers": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	value, present, err := SnapshotAuthActiveProvider(store)
	if err != nil || present {
		t.Fatalf("snapshot %v %v %v", value, present, err)
	}
	if err := RestoreAuthActiveProvider(store, value, present); err != nil {
		t.Fatal(err)
	}
	data, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, has := data["active_provider"]; has {
		t.Fatal("active_provider must stay absent")
	}
	if err := RestoreAuthActiveProvider(store, "nous", true); err != nil {
		t.Fatal(err)
	}
	if active, _ := store.ActiveProvider(); active != "nous" {
		t.Fatalf("active provider %q", active)
	}
}

func TestPickerFailureRestoresPersistedPrimaryWithoutMaskingError(t *testing.T) {
	h := t.TempDir()
	primary := map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-6", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"}
	writeConfig(t, h, map[string]any{"model": primary, "theme": "midnight"})
	pickerErr := errors.New("picker failed")
	failing := func() error {
		if err := pickerWriting(h, map[string]any{"provider": "openrouter", "default": "anthropic/claude-sonnet-4.6"}, nil)(); err != nil {
			return err
		}
		return pickerErr
	}
	var out bytes.Buffer
	err := FallbackAdd(&out, h, NewStore(h), failing)
	if !errors.Is(err, pickerErr) {
		t.Fatalf("picker error must surface: %v", err)
	}
	cfg := readConfig(t, h)
	model := cfg["model"].(map[string]any)
	for k, v := range primary {
		if model[k] != v {
			t.Fatalf("primary field %s = %v", k, model[k])
		}
	}
	if cfg["theme"] != "midnight" {
		t.Fatalf("theme lost: %v", cfg["theme"])
	}
}

func TestFallbackRemoveSelectedEntry(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, map[string]any{"fallback_providers": []any{
		map[string]any{"provider": "openrouter", "model": "gpt-5.4"}, map[string]any{"provider": "nous", "model": "Hermes-4"}, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6"}}})
	var out bytes.Buffer
	if err := FallbackRemove(&out, h, func(prompt string, options []string, def int) int {
		if len(options) != 4 || options[3] != "Cancel" {
			t.Fatalf("options %v", options)
		}
		return 1
	}); err != nil {
		t.Fatal(err)
	}
	chain := readConfig(t, h)["fallback_providers"].([]any)
	if len(chain) != 2 || chain[0].(map[string]any)["model"] != "gpt-5.4" || chain[1].(map[string]any)["provider"] != "anthropic" {
		t.Fatalf("chain %v", chain)
	}
	out.Reset()
	if err := FallbackRemove(&out, h, func(string, []string, int) int { return -1 }); err != nil || !strings.Contains(out.String(), "Cancelled — no change.") {
		t.Fatalf("cancel %q %v", out.String(), err)
	}
}

func TestFallbackClearWithConfirmation(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, map[string]any{"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "gpt-5.4"}, map[string]any{"provider": "nous", "model": "Hermes-4"}}})
	var out bytes.Buffer
	if err := FallbackClear(&out, h, func(string) (string, error) { return "y", nil }); err != nil {
		t.Fatal(err)
	}
	if chain := readConfig(t, h)["fallback_providers"].([]any); len(chain) != 0 {
		t.Fatalf("chain %v", chain)
	}
	writeConfig(t, h, map[string]any{"fallback_providers": []any{map[string]any{"provider": "openrouter", "model": "gpt-5.4"}}})
	out.Reset()
	if err := FallbackClear(&out, h, func(string) (string, error) { return "", errors.New("EOF") }); err != nil || !strings.Contains(out.String(), "Cancelled.") {
		t.Fatalf("eof cancel %q %v", out.String(), err)
	}
	if chain := readConfig(t, h)["fallback_providers"].([]any); len(chain) != 1 {
		t.Fatalf("chain changed on cancel: %v", chain)
	}
}

func TestUnknownFallbackSubcommand(t *testing.T) {
	err := RunFallbackCommand("nope", FallbackCommandIO{Out: &bytes.Buffer{}, Home: t.TempDir()})
	var unknown *UnknownFallbackSubcommandError
	if !errors.As(err, &unknown) || unknown.Sub != "nope" || !strings.Contains(err.Error(), "Use one of: list, add, remove, clear") {
		t.Fatalf("err %v", err)
	}
}

func TestChainSkipsSameBackendAndUnusableEntries(t *testing.T) {
	h := t.TempDir()
	primary := Route{Provider: "openrouter", Model: "gpt-5.4", BaseURL: "https://openrouter.ai/api/v1"}
	store := NewStore(h)
	pools := map[string]PoolProbe{"zai": &fakePool{provider: "zai", entries: 1, available: false, nextAt: time.Now().Add(time.Hour), hasNext: true}}
	chain := NewChain(ChainOptions{Primary: primary, Store: store, LoadPool: func(ctx context.Context, provider string) PoolProbe { return pools[provider] },
		Fallbacks: []Route{
			{Provider: "openrouter", Model: "gpt-5.4", BaseURL: "https://openrouter.ai/api/v1/"},
			{Provider: "nous", Model: "Hermes-4"},
			{Provider: "zai", Model: "glm-5.2"},
			{Provider: "", Model: "x"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		}})
	if !chain.TryActivateFallback(context.Background(), ReasonServerError, nil) {
		t.Fatal("expected the anthropic entry to activate")
	}
	if cur := chain.Current(); cur.Provider != "anthropic" || chain.Index() != 5 {
		t.Fatalf("current %+v index %d", cur, chain.Index())
	}
	if chain.TryActivateFallback(context.Background(), ReasonServerError, nil) {
		t.Fatal("chain exhausted")
	}
	if !chain.Cooldown().Active() {
		t.Fatal("non rate limit exhaustion arms the short cooldown")
	}
}

func TestChainRotateImplementsRotator(t *testing.T) {
	primary := Route{Provider: "openrouter", Model: "gpt-5.4"}
	chain := NewChain(ChainOptions{Primary: primary, Fallbacks: []Route{{Provider: "openai", Model: "gpt-4o"}}})
	var rotator Rotator = chain
	next, ok := rotator.Rotate(context.Background(), primary, &APIError{StatusCode: 429, Message: "rate limited", Kind: ErrRateLimit, Provider: "openrouter", Model: "gpt-5.4"})
	if !ok || next.Provider != "openai" {
		t.Fatalf("rotate %+v %v", next, ok)
	}
	if _, ok := rotator.Rotate(context.Background(), next, &APIError{StatusCode: 500}); ok {
		t.Fatal("exhausted chain must not rotate")
	}
	notices := chain.TakePendingNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "⚠️ Model fallback: gpt-5.4 via openrouter unavailable (rate limit); using "+NormalizeModelForProvider("gpt-4o", "openai")+" via openai.") {
		t.Fatalf("notices %q", notices)
	}
	if len(chain.TakePendingNotices()) != 0 {
		t.Fatal("notices must be cleared once taken")
	}
}

func TestRestorePrimaryGatesOnPoolReset(t *testing.T) {
	now := time.Unix(5000, 0)
	primary := Route{Provider: "openai-codex", Model: "gpt-5.5"}
	pool := &fakePool{provider: "openai-codex", entries: 1, available: false, nextAt: now.Add(time.Hour), hasNext: true}
	restores := 0
	chain := NewChain(ChainOptions{Primary: primary, Fallbacks: []Route{{Provider: "zai", Model: "glm-5.2"}}, Pool: pool, Now: func() time.Time { return now },
		LoadPool: func(ctx context.Context, provider string) PoolProbe {
			if provider == "openai-codex" {
				return pool
			}
			return nil
		},
		OnRestore: func(ctx context.Context, r Route, p PoolProbe) error { restores++; return nil }})
	chain.Cooldown().now = func() time.Time { return now }
	if !chain.TryActivateFallback(context.Background(), ReasonRateLimit, nil) {
		t.Fatal("activation")
	}
	chain.Cooldown().Clear()
	if _, restored := chain.RestorePrimary(context.Background()); restored || restores != 0 {
		t.Fatal("a future pool reset must block the restore")
	}
	pool.lifted = true
	chain.codexReopenChecked = false
	if _, restored := chain.RestorePrimary(context.Background()); restored {
		t.Fatal("the lifted cooldown clears the primary cooldown but the pool reset still gates")
	}
	pool.hasNext = false
	chain.Cooldown().Clear()
	if _, restored := chain.RestorePrimary(context.Background()); !restored || restores != 1 || chain.Activated() {
		t.Fatalf("restore expected: %v %d", restored, restores)
	}
}

func TestFallbackEntryFormatting(t *testing.T) {
	if FormatFallbackEntry(map[string]any{"model": "m", "provider": "p", "base_url": "http://x"}) != "m  (via p)  [http://x]" || FormatFallbackEntry(map[string]any{}) != "?  (via ?)" {
		t.Fatal("format")
	}
	if EntriesLabel(1) != "1 entry" || EntriesLabel(3) != "3 entries" {
		t.Fatal("entries label")
	}
	if DescribePrimary(map[string]any{"model": "bare"}) != "bare" || DescribePrimary(map[string]any{"model": map[string]any{"provider": "p"}}) != "?  (via p)" || DescribePrimary(map[string]any{}) != "" {
		t.Fatal("describe primary")
	}
	if FallbackReasonText(ReasonBilling) != "billing or quota exhausted" || FallbackReasonText("custom_thing") != "custom thing" || FallbackReasonText("") != "provider failure" {
		t.Fatal("reason text")
	}
}

func TestRestoreModelCfgWritesUserFile(t *testing.T) {
	h := t.TempDir()
	writeConfig(t, h, map[string]any{"model": "changed", "theme": "x"})
	if err := RestoreModelCfg(h, map[string]any{"provider": "nous", "default": "Hermes-4"}); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig(t, h)
	if cfg["model"].(map[string]any)["provider"] != "nous" || cfg["theme"] != "x" {
		t.Fatalf("cfg %v", cfg)
	}
	if err := RestoreModelCfg(h, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := readConfig(t, h)["model"]; has {
		t.Fatal("nil snapshot removes the model section")
	}
	if _, err := os.Stat(filepath.Join(h, filepath.Base(home.ConfigPath(h)))); err != nil {
		t.Fatal(err)
	}
}
