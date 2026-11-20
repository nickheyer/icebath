package agent

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	envDefaultBase = "https://api.openai.com/v1"
	envLocalBase   = "http://127.0.0.1:39080"
	envCustomBase  = "https://api.longcat.example/openai/v1"
)

var envWatchedKeys = []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "LONGCAT_API_KEY"}

// Env refresh fixture over a temp home whose .env and process env hold exactly the given values
type envRefreshFixture struct {
	t       *testing.T
	home    string
	values  map[string]string
	rebuild *[]*fakeOpenAI
	failNew bool
}

func newEnvRefreshFixture(t *testing.T, customKeyEnv bool) *envRefreshFixture {
	t.Helper()
	installFakeResponseHooks(t)
	f := &envRefreshFixture{t: t, home: t.TempDir(), values: map[string]string{}}
	t.Setenv("ICEBATH_HOME", f.home)
	for _, k := range envWatchedKeys {
		t.Setenv(k, "")
	}
	block := "custom_providers:\n  - name: longcat\n    base_url: " + envCustomBase + "\n"
	if customKeyEnv {
		block += "    key_env: LONGCAT_API_KEY\n"
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.yaml"), []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	var constructed []*fakeOpenAI
	f.rebuild = &constructed
	prev := openAIClientConstructor
	openAIClientConstructor = func(kwargs map[string]any) (any, error) {
		if f.failNew {
			return nil, errors.New("rebuild failed")
		}
		c := &fakeOpenAI{kwargs: kwargs}
		constructed = append(constructed, c)
		return c, nil
	}
	t.Cleanup(func() { openAIClientConstructor = prev })
	f.sync()
	return f
}

func (f *envRefreshFixture) set(key, value string) {
	f.values[key] = value
	f.sync()
}

// Writes the values to the home .env and mirrors them into the process environment
func (f *envRefreshFixture) sync() {
	var lines []string
	for _, k := range envWatchedKeys {
		if v, ok := f.values[k]; ok {
			lines = append(lines, k+"="+v)
			os.Setenv(k, v)
		} else {
			os.Unsetenv(k)
		}
	}
	if err := os.WriteFile(filepath.Join(f.home, ".env"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func envAgent(providerName, baseURL, apiKey string) *AIAgent {
	a := &AIAgent{provider: providerName, requestedProvider: providerName, apiMode: "chat_completions", apiKey: apiKey}
	a.setBaseURL(baseURL)
	a.clientKwargs = map[string]any{"base_url": baseURL, "api_key": apiKey}
	return a
}

func refreshEnv(t *testing.T, a *AIAgent) bool {
	t.Helper()
	ok, err := a.tryRefreshEnvClientCredentials(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestAdoptsEnvEditsBootDefaultAdoptsOverrideOnFirstLook(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	f.set("OPENAI_BASE_URL", envLocalBase)
	if !refreshEnv(t, a) || a.baseURL != envLocalBase || a.clientKwargs["base_url"] != envLocalBase || len(*f.rebuild) != 1 {
		t.Fatalf("base %q kwargs %v rebuilds %d", a.baseURL, a.clientKwargs, len(*f.rebuild))
	}
}

func TestAdoptsEnvEditsEditBetweenTurnsIsAdopted(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	if refreshEnv(t, a) {
		t.Fatal("unchanged env adopted")
	}
	f.set("OPENAI_BASE_URL", envLocalBase)
	if !refreshEnv(t, a) || a.baseURL != envLocalBase {
		t.Fatalf("base %q", a.baseURL)
	}
}

func TestAdoptsEnvEditsKeyRotationInEnvIsAdopted(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	if refreshEnv(t, a) {
		t.Fatal("unchanged env adopted")
	}
	f.set("OPENAI_API_KEY", "sk-new")
	if !refreshEnv(t, a) || a.apiKey != "sk-new" || a.clientKwargs["api_key"] != "sk-new" {
		t.Fatalf("key %q kwargs %v", a.apiKey, a.clientKwargs)
	}
}

func TestLeavesNonEnvStateAloneUnchangedEnvIsANoop(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	if refreshEnv(t, a) || len(*f.rebuild) != 0 {
		t.Fatal("noop rebuilt the client")
	}
}

func TestLeavesNonEnvStateAlonePoolRotationIsNotStomped(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	if refreshEnv(t, a) {
		t.Fatal("unchanged env adopted")
	}
	a.apiKey = "sk-rotated-pool-entry"
	if refreshEnv(t, a) || a.apiKey != "sk-rotated-pool-entry" {
		t.Fatalf("rotated key stomped to %q", a.apiKey)
	}
}

func TestLeavesNonEnvStateAloneFirstLookDoesNotStompRotatedPoolKey(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-backup")
	a.credentialPool = &provider.Pool{}
	a.credentialPoolEntryID = "entry-backup"
	f.set("OPENAI_API_KEY", "sk-primary")
	if refreshEnv(t, a) || a.apiKey != "sk-backup" || a.credentialPoolEntryID != "entry-backup" {
		t.Fatalf("key %q entry %q", a.apiKey, a.credentialPoolEntryID)
	}
}

func TestLeavesNonEnvStateAloneAdoptedKeyRebindsPoolEntryID(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	if refreshEnv(t, a) {
		t.Fatal("unchanged env adopted")
	}
	a.credentialPoolEntryID = "stale-rotated-id"
	f.set("OPENAI_API_KEY", "sk-new")
	if !refreshEnv(t, a) || a.apiKey != "sk-new" || a.credentialPoolEntryID == "stale-rotated-id" {
		t.Fatalf("key %q entry %q", a.apiKey, a.credentialPoolEntryID)
	}
}

func TestLeavesNonEnvStateAloneCustomEndpointWinsOverEnvEdit(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", "https://my-proxy.corp.example/v1", "sk-old")
	f.set("OPENAI_API_KEY", "sk-old")
	f.set("OPENAI_BASE_URL", envLocalBase)
	if refreshEnv(t, a) || a.baseURL != "https://my-proxy.corp.example/v1" {
		t.Fatalf("base %q", a.baseURL)
	}
}

func TestLeavesNonEnvStateAloneSkippedWhileFailedOver(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	a.fallbackActivated = true
	f.set("OPENAI_API_KEY", "sk-old")
	f.set("OPENAI_BASE_URL", envLocalBase)
	if refreshEnv(t, a) {
		t.Fatal("adopted while failed over")
	}
}

func TestLeavesNonEnvStateAloneSkippedForNonAPIKeyProvider(t *testing.T) {
	newEnvRefreshFixture(t, true)
	if refreshEnv(t, envAgent("openai-codex", envDefaultBase, "sk-old")) {
		t.Fatal("oauth provider adopted")
	}
}

func TestLeavesNonEnvStateAloneSkippedForNonChatCompletionsAPIMode(t *testing.T) {
	newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	a.apiMode = "anthropic_messages"
	if refreshEnv(t, a) {
		t.Fatal("non chat wire adopted")
	}
}

func TestLeavesNonEnvStateAloneSkippedWhenNoKeyResolves(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	f.set("OPENAI_BASE_URL", envLocalBase)
	if refreshEnv(t, a) {
		t.Fatal("adopted without a key")
	}
}

func TestFailedRebuildRetriesFailedRebuildRollsBackAndRetriesNextTurn(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := envAgent("openai-api", envDefaultBase, "sk-old")
	before := maps.Clone(a.clientKwargs)
	f.set("OPENAI_API_KEY", "sk-old")
	if refreshEnv(t, a) {
		t.Fatal("unchanged env adopted")
	}
	f.set("OPENAI_BASE_URL", envLocalBase)
	f.failNew = true
	if refreshEnv(t, a) {
		t.Fatal("failed rebuild reported adoption")
	}
	if a.baseURL != envDefaultBase || a.apiKey != "sk-old" || !maps.Equal(a.clientKwargs, before) {
		t.Fatalf("rollback base %q key %q kwargs %v", a.baseURL, a.apiKey, a.clientKwargs)
	}
	f.failNew = false
	if !refreshEnv(t, a) || a.baseURL != envLocalBase || a.clientKwargs["base_url"] != envLocalBase {
		t.Fatalf("retry base %q", a.baseURL)
	}
}

func customEnvAgent(apiKey string) *AIAgent {
	a := envAgent("custom", envCustomBase, apiKey)
	a.requestedProvider = "longcat"
	return a
}

func TestNamedCustomProvidersKeyAddedMidSessionIsAdopted(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := customEnvAgent("no-key-required")
	f.set("LONGCAT_API_KEY", "lc-fresh")
	if !refreshEnv(t, a) || a.apiKey != "lc-fresh" || a.clientKwargs["api_key"] != "lc-fresh" || len(*f.rebuild) != 1 {
		t.Fatalf("key %q rebuilds %d", a.apiKey, len(*f.rebuild))
	}
}

func TestNamedCustomProvidersKeyRotationBetweenTurnsIsAdopted(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := customEnvAgent("lc-old")
	f.set("LONGCAT_API_KEY", "lc-old")
	if refreshEnv(t, a) {
		t.Fatal("unchanged key adopted")
	}
	f.set("LONGCAT_API_KEY", "lc-new")
	if !refreshEnv(t, a) || a.apiKey != "lc-new" {
		t.Fatalf("key %q", a.apiKey)
	}
}

func TestNamedCustomProvidersUnchangedEnvIsANoop(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := customEnvAgent("lc-old")
	f.set("LONGCAT_API_KEY", "lc-old")
	if refreshEnv(t, a) || len(*f.rebuild) != 0 {
		t.Fatal("noop rebuilt the client")
	}
}

func TestNamedCustomProvidersSkippedWithoutKeyEnv(t *testing.T) {
	f := newEnvRefreshFixture(t, false)
	a := customEnvAgent("no-key-required")
	f.set("LONGCAT_API_KEY", "lc-fresh")
	if refreshEnv(t, a) {
		t.Fatal("adopted without key_env")
	}
}

func TestNamedCustomProvidersSkippedForUnknownCustomProvider(t *testing.T) {
	f := newEnvRefreshFixture(t, true)
	a := customEnvAgent("no-key-required")
	a.requestedProvider = "someone-else"
	f.set("LONGCAT_API_KEY", "lc-fresh")
	if refreshEnv(t, a) {
		t.Fatal("unknown custom provider adopted")
	}
}
