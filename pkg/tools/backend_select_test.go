package tools

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

type tbhAccount struct {
	NousAccountSource
	enabled bool
	msg     string
	pool    map[string]string
	seen    []string
	supp    map[string]bool
}

func (a *tbhAccount) ManagedToolsEnabled(env *Env, forceFresh bool) bool { return a.enabled }

func (a *tbhAccount) UnavailableMessage(env *Env, capability string, forceFresh bool) string {
	if a.msg == "" {
		return ""
	}
	return a.msg + " " + capability
}

func (a *tbhAccount) PoolAPIKey(env *Env, providerID string) string {
	a.seen = append(a.seen, providerID)
	return a.pool[providerID]
}

func (a *tbhAccount) SourceSuppressed(env *Env, providerID, source string) bool {
	return a.supp[providerID+"|"+source]
}

func tbhInstall(t *testing.T, a NousAccountSource) {
	prev := NousAccount
	NousAccount = a
	t.Cleanup(func() { NousAccount = prev })
}

func tbhEnv(t *testing.T, dotenv string, cfg map[string]any) *Env {
	dir := t.TempDir()
	if dotenv != "" {
		os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv), 0o600)
	}
	if cfg != nil {
		var b strings.Builder
		for section, v := range cfg {
			b.WriteString(section + ":\n")
			for k, val := range v.(map[string]any) {
				b.WriteString("  " + k + ": " + val.(string) + "\n")
			}
		}
		os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(b.String()), 0o600)
	}
	return &Env{Home: dir, Secrets: config.NewSecrets(dir), Config: config.Tree(cfg)}
}

func tbhClearVoiceEnv(t *testing.T) {
	for _, v := range []string{"GROQ_API_KEY", "MISTRAL_API_KEY", "ELEVENLABS_API_KEY", "DEEPINFRA_API_KEY", "MINIMAX_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY", "OPENAI_API_KEY", "VOICE_TOOLS_OPENAI_KEY", "MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "FAL_KEY"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
}

func tbhMultiplex(t *testing.T) {
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
}

func TestScopedCredentialReadsTheEnvSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A2A_PORT= 9911 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &Env{Home: dir, Secrets: config.NewSecrets(dir)}
	if got := ScopedCredential(env, "A2A_PORT"); got != scopedCredential(env, "A2A_PORT") || got != "9911" {
		t.Fatalf("ScopedCredential = %q", got)
	}
}

func TestManagedNousToolsEnabledGate(t *testing.T) {
	tbhInstall(t, &tbhAccount{enabled: false})
	if managedNousToolsEnabled(nil, false) {
		t.Fatal("logged out account enabled the gateway")
	}
	tbhInstall(t, nil)
	if managedNousToolsEnabled(nil, false) {
		t.Fatal("missing account source enabled the gateway")
	}
}

func TestNousToolGatewayUnavailableMessageUsesAccountGuidance(t *testing.T) {
	tbhInstall(t, &tbhAccount{msg: "credits are exhausted at https://portal.example.test/billing for"})
	msg := nousToolGatewayUnavailableMessage(nil, "managed image generation", false)
	if !strings.Contains(msg, "credits are exhausted") || !strings.Contains(msg, "managed image generation") || !strings.Contains(msg, "https://portal.example.test/billing") {
		t.Fatalf("message %q", msg)
	}
	tbhInstall(t, &tbhAccount{})
	if got := nousToolGatewayUnavailableMessage(nil, "", false); got != "the Nous Tool Gateway is unavailable. Run `icebath model` to refresh your Nous Portal login and billing status." {
		t.Fatalf("fallback %q", got)
	}
}

func TestNormalizeBrowserCloudProviderAndCoerceModalMode(t *testing.T) {
	if NormalizeBrowserCloudProvider("") != "local" || NormalizeBrowserCloudProvider(" BrowserBase ") != "browserbase" {
		t.Fatal("browser provider normalization")
	}
	for _, v := range []string{"auto", "direct", "managed"} {
		if CoerceModalMode(v) != v || NormalizeModalMode(v) != v {
			t.Fatalf("mode %s", v)
		}
	}
	if CoerceModalMode("") != "auto" || CoerceModalMode("  managed  ") != "managed" || CoerceModalMode("bogus") != "auto" {
		t.Fatal("modal mode coercion")
	}
}

func TestHasDirectModalCredentials(t *testing.T) {
	tbhClearVoiceEnv(t)
	home := t.TempDir()
	prev := modalHomeDir
	t.Cleanup(func() { modalHomeDir = prev })
	modalHomeDir = func() (string, error) { return home, nil }
	if HasDirectModalCredentials(nil) {
		t.Fatal("no env no file")
	}
	t.Setenv("MODAL_TOKEN_SECRET", "sec-456")
	if HasDirectModalCredentials(nil) {
		t.Fatal("secret alone is not enough")
	}
	t.Setenv("MODAL_TOKEN_ID", "id-123")
	os.WriteFile(filepath.Join(home, ".modal.toml"), nil, 0o600)
	if !HasDirectModalCredentials(nil) {
		t.Fatal("token pair ignored")
	}
	modalHomeDir = func() (string, error) { return "", errors.New("denied") }
	if !HasDirectModalCredentials(nil) {
		t.Fatal("permission denied home with env vars")
	}
	os.Unsetenv("MODAL_TOKEN_ID")
	if HasDirectModalCredentials(nil) {
		t.Fatal("permission denied home without env vars")
	}
	os.Unsetenv("MODAL_TOKEN_SECRET")
	modalHomeDir = func() (string, error) { return home, nil }
	if !HasDirectModalCredentials(nil) {
		t.Fatal("modal.toml ignored")
	}
}

func TestHasDirectModalCredentialsIgnoresDefaultProfileEnvironUnderMultiplex(t *testing.T) {
	tbhClearVoiceEnv(t)
	home := t.TempDir()
	prev := modalHomeDir
	t.Cleanup(func() { modalHomeDir = prev })
	modalHomeDir = func() (string, error) { return home, nil }
	t.Setenv("MODAL_TOKEN_ID", "default-id")
	t.Setenv("MODAL_TOKEN_SECRET", "default-secret")
	tbhMultiplex(t)
	if HasDirectModalCredentials(tbhEnv(t, "UNRELATED=x\n", nil)) {
		t.Fatal("secondary profile borrowed the default profile Modal tokens")
	}
	if !HasDirectModalCredentials(tbhEnv(t, "MODAL_TOKEN_ID=a\nMODAL_TOKEN_SECRET=b\n", nil)) {
		t.Fatal("profile scoped tokens ignored")
	}
}

func TestPrefersGatewayQuotedBooleans(t *testing.T) {
	if PrefersGateway(&Env{Config: config.Tree{"web": map[string]any{"use_gateway": "false"}}}, "web") {
		t.Fatal("quoted false preferred the gateway")
	}
	if !PrefersGateway(&Env{Config: config.Tree{"web": map[string]any{"use_gateway": "true"}}}, "web") {
		t.Fatal("quoted true ignored")
	}
	if PrefersGateway(&Env{Config: config.Tree{"web": map[string]any{"use_gateway": "y"}}}, "web") {
		t.Fatal("y is outside the truthy set")
	}
}

func TestResolveModalBackendStateMatrix(t *testing.T) {
	on, off := true, false
	r := ResolveModalBackendState(nil, "auto", true, true, &on)
	if r.SelectedBackend != "managed" {
		t.Fatalf("auto %+v", r)
	}
	if r := ResolveModalBackendState(nil, "direct", true, true, &on); r.SelectedBackend != "direct" {
		t.Fatalf("direct %+v", r)
	}
	if r := ResolveModalBackendState(nil, "direct", false, true, &on); r.SelectedBackend != "" {
		t.Fatalf("direct without credentials %+v", r)
	}
	if r := ResolveModalBackendState(nil, "managed", true, true, &off); r.SelectedBackend != "" || !r.ManagedModeBlocked {
		t.Fatalf("managed blocked %+v", r)
	}
	tbhInstall(t, &tbhAccount{enabled: false})
	if r := ResolveModalBackendState(nil, "bogus", true, false, nil); r.RequestedMode != "auto" || r.Mode != "auto" || r.SelectedBackend != "direct" {
		t.Fatalf("invalid mode %+v", r)
	}
}

func TestResolveOpenAIAudioAPIKeyPriority(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{})
	t.Setenv("VOICE_TOOLS_OPENAI_KEY", "voice-key")
	t.Setenv("OPENAI_API_KEY", "general-key")
	if got := ResolveOpenAIAudioAPIKey(nil); got != "voice-key" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("VOICE_TOOLS_OPENAI_KEY", "  voice-key  ")
	os.Unsetenv("OPENAI_API_KEY")
	if got := ResolveOpenAIAudioAPIKey(nil); got != "voice-key" {
		t.Fatalf("strip %q", got)
	}
	os.Unsetenv("VOICE_TOOLS_OPENAI_KEY")
	t.Setenv("OPENAI_API_KEY", "sk-plain")
	if got := ResolveOpenAIAudioAPIKey(nil); got != "sk-plain" {
		t.Fatalf("single profile %q", got)
	}
}

func TestResolveOpenAIAudioAPIKeyIsProfileScoped(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{pool: map[string]string{"openai-api": "pool-key"}})
	t.Setenv("OPENAI_API_KEY", "sk-other-profile")
	tbhMultiplex(t)
	if got := ResolveOpenAIAudioAPIKey(tbhEnv(t, "OPENAI_API_KEY=sk-this-profile\n", nil)); got != "sk-this-profile" {
		t.Fatalf("voice used another profile key %q", got)
	}
	if got := ResolveOpenAIAudioAPIKey(tbhEnv(t, "UNRELATED=x\n", nil)); got != "" {
		t.Fatalf("scope miss borrowed %q", got)
	}
}

func TestResolveProviderSecretPoolFallback(t *testing.T) {
	pairs := [][2]string{{"GROQ_API_KEY", "groq"}, {"MISTRAL_API_KEY", "mistral"}, {"ELEVENLABS_API_KEY", "elevenlabs"},
		{"DEEPINFRA_API_KEY", "deepinfra"}, {"MINIMAX_API_KEY", "minimax"}, {"GEMINI_API_KEY", "gemini"}, {"XAI_API_KEY", "xai"},
		{"OPENAI_API_KEY", "openai-api"}}
	for _, p := range pairs {
		tbhClearVoiceEnv(t)
		acct := &tbhAccount{pool: map[string]string{p[1]: "pool-key-" + p[1]}}
		tbhInstall(t, acct)
		if got := ResolveProviderSecret(tbhEnv(t, "", nil), p[0], p[1], "", nil); got != "pool-key-"+p[1] {
			t.Fatalf("%s got %q", p[0], got)
		}
		if acct.seen[0] != p[1] {
			t.Fatalf("pool keys %v", acct.seen)
		}
	}
	acct := &tbhAccount{pool: map[string]string{"custom:mistral": "custom-key"}}
	tbhInstall(t, acct)
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "custom-key" {
		t.Fatalf("custom pool %q seen %v", got, acct.seen)
	}
	tbhInstall(t, &tbhAccount{})
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "" {
		t.Fatalf("empty pool %q", got)
	}
}

func TestResolveProviderSecretPrecedence(t *testing.T) {
	tbhClearVoiceEnv(t)
	acct := &tbhAccount{pool: map[string]string{"elevenlabs": "pool-key", "mistral": "pool-key"}}
	tbhInstall(t, acct)
	t.Setenv("ELEVENLABS_API_KEY", "env-key")
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "ELEVENLABS_API_KEY", "elevenlabs", "", nil); got != "env-key" || len(acct.seen) != 0 {
		t.Fatalf("env %q pool %v", got, acct.seen)
	}
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "GROQ_API_KEY", "groq", "", func(string) string { return "dotenv-key" }); got != "dotenv-key" {
		t.Fatalf("getter %q", got)
	}
	t.Setenv("MISTRAL_API_KEY", "env-key")
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "MISTRAL_API_KEY", "mistral", "cfg-key", nil); got != "cfg-key" {
		t.Fatalf("config %q", got)
	}
}

func TestResolveProviderSecretMultiplexScope(t *testing.T) {
	tbhClearVoiceEnv(t)
	acct := &tbhAccount{pool: map[string]string{"mistral": "pool-key"}}
	tbhInstall(t, acct)
	t.Setenv("MISTRAL_API_KEY", "sk-other-profile")
	tbhMultiplex(t)
	if got := ResolveProviderSecret(tbhEnv(t, "MISTRAL_API_KEY=sk-this-profile\n", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "sk-this-profile" {
		t.Fatalf("scope %q", got)
	}
	if got := ResolveProviderSecret(tbhEnv(t, "UNRELATED=x\n", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "" || len(acct.seen) != 0 {
		t.Fatalf("scope miss %q pool %v", got, acct.seen)
	}
}

func TestResolveProviderSecretHonorsSuppressedEnvSource(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{supp: map[string]bool{"xai|env:XAI_API_KEY": true}, pool: map[string]string{"xai": "oauth-pool"}})
	t.Setenv("XAI_API_KEY", "stale-env")
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "XAI_API_KEY", "xai", "", nil); got != "oauth-pool" {
		t.Fatalf("suppressed env still used %q", got)
	}
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "XAI_API_KEY", "grok", "", nil); got != "stale-env" {
		t.Fatalf("suppression leaked across providers %q", got)
	}
}

func TestRemovedBackendNoteAndSelectionError(t *testing.T) {
	note := "the LegacySearch backend was removed in v9.9.9 (pick another backend)"
	RemovedBackends["web"] = map[string]string{"legacysearch": note}
	t.Cleanup(func() { delete(RemovedBackends, "web") })
	for _, name := range []string{"legacysearch", "'LegacySearch'", `  "LEGACYSEARCH" `} {
		if RemovedBackendNote("web", name) != note {
			t.Fatalf("note lookup %q", name)
		}
	}
	if RemovedBackendNote("web", "exa") != "" || RemovedBackendNote("web", "") != "" || RemovedBackendNote("stt", "legacysearch") != "" {
		t.Fatal("unknown names must miss")
	}
	msg := SelectionError("web", "'legacysearch'", "no registered web search provider has that name")
	if !strings.Contains(msg, note) || strings.Contains(msg, "no registered web search provider") {
		t.Fatalf("removed %q", msg)
	}
	msg = SelectionError("web", "'exa'", "EXA_API_KEY is not set")
	if msg != "web is configured to use 'exa' (set via icebath tools), but EXA_API_KEY is not set. Run 'icebath tools' to change it." {
		t.Fatalf("live %q", msg)
	}
}

func TestReadSelectionAndSelectionConfigKeys(t *testing.T) {
	env := tbhEnv(t, "", map[string]any{
		"browser":   map[string]any{"backend": "local", "cloud_provider": "Browserbase"},
		"tts":       map[string]any{"use_gateway": "true", "provider": "openai"},
		"stt":       map[string]any{"use_gateway": "false"},
		"web":       map[string]any{"search_backend": "Exa"},
		"image_gen": map[string]any{"backend": "fal"},
	})
	if ReadSelection(env, "browser") != "browserbase" || ReadSelection(env, "tts") != NousManagedProvider || ReadSelection(env, "stt") != "" {
		t.Fatal("read selection")
	}
	if ReadSelection(env, "image_gen") != "fal" || ReadSelection(env, "video_gen") != "" {
		t.Fatal("default name keys")
	}
	if ReadWebCapabilitySelection(env, "search") != "exa" || ReadWebCapabilitySelection(env, "extract") != "" {
		t.Fatal("web capability selection")
	}
	if !SelectionExists(env, "web") || SelectionExists(env, "stt") || !SelectionExists(env, "tts") {
		t.Fatal("selection exists")
	}
	keys := SelectionConfigKeys()
	for _, k := range []string{"browser.cloud_provider", "browser.use_gateway", "web.backend", "web.search_backend", "web.extract_backend",
		"tts.provider", "stt.cloud_provider", "image_gen.backend", "video_gen.use_gateway"} {
		if !keys[k] {
			t.Fatalf("missing %s", k)
		}
	}
	if keys["browser.backend"] || len(keys) != 2+4+4*4 {
		t.Fatalf("keys %v", keys)
	}
}

func TestFalKeyIsConfigured(t *testing.T) {
	tbhClearVoiceEnv(t)
	if FalKeyIsConfigured(tbhEnv(t, "", nil)) {
		t.Fatal("no key")
	}
	if !FalKeyIsConfigured(tbhEnv(t, "FAL_KEY=abc\n", nil)) {
		t.Fatal(".env key ignored")
	}
	t.Setenv("FAL_KEY", "   ")
	if FalKeyIsConfigured(tbhEnv(t, "", nil)) {
		t.Fatal("blank key counted")
	}
}

func TestManagedNousGatewayExportsMatchTheBackendGate(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	env := &Env{Home: t.TempDir(), Config: config.Tree{}}
	if ManagedNousToolsEnabled(env, false) != managedNousToolsEnabled(env, false) {
		t.Fatal("managed nous gate diverges")
	}
	if NousToolGatewayUnavailableMessage(env, "image generation", false) != nousToolGatewayUnavailableMessage(env, "image generation", false) {
		t.Fatal("gateway message diverges")
	}
}

func TestPoolFallbackPoolEntryResolvesWhenEnvEmpty(t *testing.T) {
	for _, p := range [][2]string{{"GROQ_API_KEY", "groq"}, {"MISTRAL_API_KEY", "mistral"}, {"ELEVENLABS_API_KEY", "elevenlabs"},
		{"DEEPINFRA_API_KEY", "deepinfra"}, {"MINIMAX_API_KEY", "minimax"}, {"GEMINI_API_KEY", "gemini"}, {"XAI_API_KEY", "xai"},
		{"OPENAI_API_KEY", "openai-api"}} {
		tbhClearVoiceEnv(t)
		acct := &tbhAccount{pool: map[string]string{p[1]: "pool-key-" + p[1]}}
		tbhInstall(t, acct)
		if got := ResolveProviderSecret(tbhEnv(t, "", nil), p[0], p[1], "", nil); got != "pool-key-"+p[1] || !slices.Contains(acct.seen, p[1]) {
			t.Fatalf("%s got %q seen %v", p[0], got, acct.seen)
		}
	}
}

func TestPoolFallbackPoolReadFailureNeverRaises(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{})
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestEnvPrecedenceEnvWinsOverPool(t *testing.T) {
	tbhClearVoiceEnv(t)
	acct := &tbhAccount{pool: map[string]string{"elevenlabs": "pool-key"}}
	tbhInstall(t, acct)
	t.Setenv("ELEVENLABS_API_KEY", "env-key")
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "ELEVENLABS_API_KEY", "elevenlabs", "", nil); got != "env-key" || len(acct.seen) != 0 {
		t.Fatalf("env %q pool %v", got, acct.seen)
	}
}

func TestEnvPrecedenceEnvGetterIsConsulted(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{})
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "GROQ_API_KEY", "groq", "", func(string) string { return "dotenv-key" }); got != "dotenv-key" {
		t.Fatalf("getter %q", got)
	}
}

func TestConfigPrecedenceConfigValueWinsOverEnvAndPool(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{pool: map[string]string{"mistral": "pool-key"}})
	t.Setenv("MISTRAL_API_KEY", "env-key")
	if got := ResolveProviderSecret(tbhEnv(t, "", nil), "MISTRAL_API_KEY", "mistral", "cfg-key", nil); got != "cfg-key" {
		t.Fatalf("config %q", got)
	}
}

func TestMultiplexScopeScopeValueWins(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{pool: map[string]string{"mistral": "pool-key"}})
	t.Setenv("MISTRAL_API_KEY", "sk-other-profile")
	tbhMultiplex(t)
	if got := ResolveProviderSecret(tbhEnv(t, "MISTRAL_API_KEY=sk-this-profile\n", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "sk-this-profile" {
		t.Fatalf("scope %q", got)
	}
}

func TestMultiplexScopeScopeMissDoesNotFallThroughToPool(t *testing.T) {
	tbhClearVoiceEnv(t)
	acct := &tbhAccount{pool: map[string]string{"mistral": "pool-key"}}
	tbhInstall(t, acct)
	tbhMultiplex(t)
	if got := ResolveProviderSecret(tbhEnv(t, "UNRELATED=x\n", nil), "MISTRAL_API_KEY", "mistral", "", nil); got != "" || len(acct.seen) != 0 {
		t.Fatalf("scope miss %q pool %v", got, acct.seen)
	}
}

func TestToolWiringOpenaiAudioKeyFallsBackToPool(t *testing.T) {
	tbhClearVoiceEnv(t)
	tbhInstall(t, &tbhAccount{pool: map[string]string{"openai-api": "oai-pool-key"}})
	if got := ResolveOpenAIAudioAPIKey(tbhEnv(t, "", nil)); got != "oai-pool-key" {
		t.Fatalf("got %q", got)
	}
}
