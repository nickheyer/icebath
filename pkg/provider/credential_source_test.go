package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func neverCalled(context.Context) (string, error) {
	panic("a display path invoked the credential callable")
}

func TestCallableKeysAreLabelledByTheirSource(t *testing.T) {
	cases := map[any]string{
		TagCredentialSource(neverCalled, CredentialSourceEntraID):      "Microsoft Entra ID",
		TagCredentialSource(neverCalled, CredentialSourceVertexOAuth):  "Google OAuth (Vertex AI)",
		TagCredentialSource(neverCalled, CredentialSourceMinimaxOAuth): "MiniMax OAuth",
		&CommandTokenSource{Command: "print-token"}:                    "key_cmd token (command)",
	}
	for key, want := range cases {
		if got := CredentialLabel(key); got != want {
			t.Fatalf("%v: got %q want %q", key, got, want)
		}
	}
	var untagged func(context.Context) (string, error) = neverCalled
	if got := CredentialLabel(untagged); got != "Token provider" {
		t.Fatalf("untagged callable gets the neutral label, got %q", got)
	}
	if CredentialLabel("sk-live-abcdefghijklmnop3456") != "" || CredentialLabel(nil) != "" {
		t.Fatal("string keys keep the masked form, never a source label")
	}
}

func TestBothSurfacesShareOneSourceTable(t *testing.T) {
	seen := map[string]bool{}
	for source := range credentialSourceLabels {
		seen[CredentialLabel(TagCredentialSource(neverCalled, source))] = true
	}
	if len(seen) != len(credentialSourceLabels) || seen["Token provider"] {
		t.Fatal("every source has its own label and none collides with the neutral one")
	}
}

func TestAdoptExternalLoginsEnabledDefaultsTrueAndLogsOnce(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	if !AdoptExternalLoginsEnabled(env) {
		t.Fatal("default true")
	}
	writeConfigYAML(t, h, "auth:\n  adopt_external_logins: false\n")
	sink := &recordingHandler{}
	env.Log = newRecordingLogger(sink)
	externalLoginsNoticeMu.Lock()
	externalLoginsNoticeLogged = false
	externalLoginsNoticeMu.Unlock()
	if AdoptExternalLoginsEnabled(env) || AdoptExternalLoginsEnabled(env) {
		t.Fatal("opt out honoured")
	}
	if sink.count(ExternalLoginsNotAdoptedNotice) != 1 {
		t.Fatal("the notice is logged once per process")
	}
}

func TestRemovalStepsCleanTheRightSource(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeEnvFile(t, h, "OPENROUTER_API_KEY=sk-or\n")
	step, ok := FindRemovalStep("openrouter", "env:OPENROUTER_API_KEY")
	if !ok {
		t.Fatal("env step expected")
	}
	result := step.Remove(env, "openrouter", PooledCredential{Source: "env:OPENROUTER_API_KEY"})
	if len(result.Cleaned) != 1 || !result.Suppress {
		t.Fatalf("got %+v", result)
	}
	raw, _ := os.ReadFile(filepath.Join(h, ".env"))
	if string(raw) != "" && contains(string(raw), "OPENROUTER_API_KEY=") {
		t.Fatal(".env entry must be cleared")
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "a"}}}})
	step, _ = FindRemovalStep("openai-codex", "manual:device_code")
	result = step.Remove(env, "openai-codex", PooledCredential{Source: "manual:device_code"})
	if len(result.Cleaned) != 1 || !env.Store.IsSourceSuppressed("openai-codex", "device_code") {
		t.Fatalf("codex removal must clear the block and suppress device_code: %+v", result)
	}
	if err := WriteHermesOAuthCredentials(env, "a", "r", 1, ""); err != nil {
		t.Fatal(err)
	}
	step, _ = FindRemovalStep("anthropic", "hermes_pkce")
	if result = step.Remove(env, "anthropic", PooledCredential{Source: "hermes_pkce"}); len(result.Cleaned) != 1 || fileExists(HermesOAuthFile(env)) {
		t.Fatal("the PKCE file is ours to delete")
	}
	step, _ = FindRemovalStep("anthropic", "claude_code")
	if result = step.Remove(env, "anthropic", PooledCredential{Source: "claude_code"}); len(result.Cleaned) != 0 || len(result.Hints) != 3 {
		t.Fatal("borrowed files are only suppressed")
	}
	if _, ok := FindRemovalStep("openrouter", "manual"); ok {
		t.Fatal("manual rows have nothing external to clean")
	}
	registerHookedProfile(t, "plug", ProviderProfileHooks{RemoveCredentialSource: func(source string) ([]string, []string, bool) {
		return []string{source, source + "-alt"}, []string{"hint"}, true
	}})
	step, ok = FindRemovalStep("plug", "env:X")
	if !ok {
		t.Fatal("profile claim expected")
	}
	result = step.Remove(env, "plug", PooledCredential{Source: "env:X"})
	if !env.Store.IsSourceSuppressed("plug", "env:X-alt") || len(result.Hints) != 1 {
		t.Fatal("a profile claim suppresses every variant")
	}
}

func TestReloginCommands(t *testing.T) {
	h := newTestHome(t)
	if OAuthReloginCommand("nous") != "icebath portal" || OAuthReloginCommand("openai-codex") != "icebath auth add openai-codex --type oauth" {
		t.Fatal("classic home has no selector")
	}
	profile := filepath.Join(h, "profiles", "work")
	_ = os.MkdirAll(profile, 0o700)
	if oauthReloginCommandIn(profile, "xai-oauth") != "icebath -p work auth add xai-oauth --type oauth" {
		t.Fatalf("got %q", oauthReloginCommandIn(profile, "xai-oauth"))
	}
	if ReloginCommandHint(h, "") != "icebath auth add <provider>" || ReloginCommandHint(h, "deepseek") != "icebath auth add deepseek" {
		t.Fatal("api key slugs and unknown providers")
	}
}
