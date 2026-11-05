package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testSource struct {
	spec      SecretSourceSpec
	secrets   map[string]string
	errText   string
	errKind   SecretErrorKind
	override  bool
	protected []string
	fetch     func(cfg map[string]any, homePath string) *SecretFetchResult
	calls     int
}

func (s *testSource) Spec() SecretSourceSpec                  { return s.spec }
func (s *testSource) ConfigSchema() map[string]map[string]any { return nil }
func (s *testSource) Fetch(cfg map[string]any, homePath string, _ *SourceEnvironment) *SecretFetchResult {
	s.calls++
	if s.fetch != nil {
		return s.fetch(cfg, homePath)
	}
	res := NewSecretFetchResult()
	if s.errText != "" {
		kind := s.errKind
		if kind == "" {
			kind = SecretErrorInternal
		}
		return res.Fail(s.errText, kind)
	}
	for k, v := range s.secrets {
		res.Secrets[k] = v
	}
	return res
}

func makeSource(name, shape string, secrets map[string]string) *testSource {
	return &testSource{spec: SecretSourceSpec{APIVersion: SecretSourceAPIVersion, Name: name, Label: strings.ToUpper(name[:1]) + name[1:], Shape: shape}, secrets: secrets}
}

func cleanRegistry(t *testing.T) {
	t.Helper()
	ResetSecretRegistryForTests()
	secretRegistryMu.Lock()
	builtinSourcesReady = true
	secretRegistryMu.Unlock()
	t.Cleanup(ResetSecretRegistryForTests)
}

func TestRegistersConformingSource(t *testing.T) {
	cleanRegistry(t)
	if !RegisterSecretSource(makeSource("dummy", "mapped", nil), false, false, "") || GetSecretSource("dummy", "") == nil {
		t.Fatal("conforming source registers")
	}
}

func TestRejectsInvalidSources(t *testing.T) {
	cleanRegistry(t)
	if RegisterSecretSource(nil, false, false, "") {
		t.Fatal("nil rejected")
	}
	bad := makeSource("Bad-Name", "mapped", nil)
	if RegisterSecretSource(bad, false, false, "") {
		t.Fatal("invalid name rejected")
	}
	old := makeSource("old", "mapped", nil)
	old.spec.APIVersion = 99
	if RegisterSecretSource(old, false, false, "") {
		t.Fatal("wrong api version rejected")
	}
	shape := makeSource("shape", "weird", nil)
	if RegisterSecretSource(shape, false, false, "") {
		t.Fatal("invalid shape rejected")
	}
	a := makeSource("a", "mapped", nil)
	a.spec.Scheme = "x"
	b := makeSource("b", "mapped", nil)
	b.spec.Scheme = "x"
	if !RegisterSecretSource(a, false, false, "") || RegisterSecretSource(b, false, false, "") {
		t.Fatal("scheme collisions across names are rejected")
	}
	if RegisterSecretSource(makeSource("a", "mapped", nil), false, false, "") {
		t.Fatal("duplicate name without replace rejected")
	}
	if !RegisterSecretSource(makeSource("a", "mapped", nil), true, false, "") {
		t.Fatal("replace allows same name override")
	}
}

func TestSameNameIsIsolatedByProfile(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "secrets-a"), filepath.Join(root, "secrets-b")
	_ = os.MkdirAll(homeA, 0o700)
	_ = os.MkdirAll(homeB, 0o700)
	sourceA := makeSource("profile_secret", "mapped", map[string]string{"A": "a"})
	sourceB := makeSource("profile_secret", "mapped", map[string]string{"B": "b"})
	if !RegisterSecretSource(sourceA, false, false, homeA) || !RegisterSecretSource(sourceB, false, false, homeB) {
		t.Fatal("scoped registrations succeed")
	}
	if GetSecretSource("profile_secret", homeA) != sourceA || GetSecretSource("profile_secret", homeB) != sourceB {
		t.Fatal("each scope sees its own source")
	}
	env := MappingSourceEnvironment(map[string]string{})
	report := ApplyAllSecretSources(map[string]any{"profile_secret": map[string]any{"enabled": true}}, homeB, env)
	if len(report.Sources) != 1 || report.Sources[0].Result.Secrets["B"] != "b" || env.Value("B") != "b" {
		t.Fatalf("home B applies its own source: %+v", report)
	}
}

func TestDisabledSourcesDoNotRun(t *testing.T) {
	cleanRegistry(t)
	src := makeSource("dummy", "mapped", map[string]string{"A": "1"})
	RegisterSecretSource(src, false, false, "")
	env := MappingSourceEnvironment(map[string]string{})
	report := ApplyAllSecretSources(map[string]any{"dummy": map[string]any{"enabled": false}}, t.TempDir(), env)
	if src.calls != 0 || len(report.Sources) != 0 || len(env.Snapshot()) != 0 {
		t.Fatal("disabled sources never run")
	}
}

func TestAppliesSecretsAndRecordsProvenance(t *testing.T) {
	cleanRegistry(t)
	RegisterSecretSource(makeSource("dummy", "mapped", map[string]string{"API_KEY": "v1"}), false, false, "")
	env := MappingSourceEnvironment(map[string]string{})
	report := ApplyAllSecretSources(map[string]any{"dummy": map[string]any{"enabled": true}}, t.TempDir(), env)
	p := report.Provenance["API_KEY"]
	if env.Value("API_KEY") != "v1" || p.Source != "dummy" || p.Shape != "mapped" || p.OverrodeEnv {
		t.Fatalf("provenance wrong: %+v", p)
	}
}

func TestMappedBeatsBulkFirstClaimWinsAndProtected(t *testing.T) {
	cleanRegistry(t)
	bulk := makeSource("bulk", "bulk", map[string]string{"SHARED": "from-bulk", "BULK_ONLY": "b", "TOKEN_ENV": "steal"})
	bulk.spec.TokenEnvKey, bulk.spec.DefaultTokenEnv = "token_env", "TOKEN_ENV"
	mapped := makeSource("mapped", "mapped", map[string]string{"SHARED": "from-mapped", "EXISTING": "override"})
	mapped.spec.OverrideExistingDefault = true
	RegisterSecretSource(bulk, false, false, "")
	RegisterSecretSource(mapped, false, false, "")
	env := MappingSourceEnvironment(map[string]string{"TOKEN_ENV": "boot", "EXISTING": "old"})
	report := ApplyAllSecretSources(map[string]any{"sources": []any{"bulk", "mapped"}, "bulk": map[string]any{"enabled": true}, "mapped": map[string]any{"enabled": true}}, t.TempDir(), env)
	if env.Value("SHARED") != "from-mapped" || env.Value("BULK_ONLY") != "b" || env.Value("TOKEN_ENV") != "boot" || env.Value("EXISTING") != "override" {
		t.Fatalf("precedence wrong: %v", env.Snapshot())
	}
	if report.Provenance["SHARED"].Source != "mapped" || len(report.Conflicts) == 0 || !report.Provenance["EXISTING"].OverrodeEnv {
		t.Fatalf("report wrong: %+v", report)
	}
}

func TestPreserveExistingBeatsOverride(t *testing.T) {
	cleanRegistry(t)
	src := makeSource("vault", "mapped", map[string]string{"KEEP": "new", "REPLACE": "new"})
	src.spec.OverrideExistingDefault = true
	RegisterSecretSource(src, false, false, "")
	env := MappingSourceEnvironment(map[string]string{"KEEP": "old", "REPLACE": "old"})
	ApplyAllSecretSources(map[string]any{"preserve_existing": []any{"KEEP"}, "vault": map[string]any{"enabled": true}}, t.TempDir(), env)
	if env.Value("KEEP") != "old" || env.Value("REPLACE") != "new" {
		t.Fatalf("preserve_existing wrong: %v", env.Snapshot())
	}
}

func TestFailedSourceDoesNotBlockOthers(t *testing.T) {
	cleanRegistry(t)
	broken := makeSource("broken", "mapped", nil)
	broken.errText, broken.errKind = "boom", SecretErrorNetwork
	RegisterSecretSource(broken, false, false, "")
	RegisterSecretSource(makeSource("works", "mapped", map[string]string{"K": "v"}), false, false, "")
	env := MappingSourceEnvironment(map[string]string{})
	report := ApplyAllSecretSources(map[string]any{"broken": map[string]any{"enabled": true}, "works": map[string]any{"enabled": true}}, t.TempDir(), env)
	if env.Value("K") != "v" {
		t.Fatal("the working source applies")
	}
	for _, s := range report.Sources {
		if s.Name == "broken" && s.Result.ErrorKind != SecretErrorNetwork {
			t.Fatal("error kind preserved")
		}
	}
}

func TestFetchTimeoutIsEnforced(t *testing.T) {
	cleanRegistry(t)
	slow := makeSource("slow", "mapped", nil)
	slow.fetch = func(map[string]any, string) *SecretFetchResult {
		time.Sleep(2 * time.Second)
		return NewSecretFetchResult()
	}
	RegisterSecretSource(slow, false, false, "")
	report := ApplyAllSecretSources(map[string]any{"slow": map[string]any{"enabled": true, "timeout_seconds": 0.2}}, t.TempDir(), MappingSourceEnvironment(map[string]string{}))
	if len(report.Sources) != 1 || report.Sources[0].Result.ErrorKind != SecretErrorTimeout {
		t.Fatalf("timeout expected: %+v", report.Sources)
	}
}

func TestMalformedSecretsCfgShapesAreSafe(t *testing.T) {
	cleanRegistry(t)
	RegisterSecretSource(makeSource("dummy", "mapped", map[string]string{"K": "v"}), false, false, "")
	for _, cfg := range []map[string]any{nil, {}, {"dummy": "not-a-dict"}, {"sources": "junk"}} {
		if ApplyAllSecretSources(cfg, t.TempDir(), MappingSourceEnvironment(map[string]string{})) == nil {
			t.Fatal("report expected")
		}
	}
}

func TestProfileAliasHydratesCanonicalName(t *testing.T) {
	cleanRegistry(t)
	RegisterSecretSource(makeSource("vault", "mapped", map[string]string{"OPENAI_API_KEY_TEAM_B": "sk-b"}), false, false, "")
	homePath := filepath.Join(t.TempDir(), "profiles", "team-b")
	_ = os.MkdirAll(homePath, 0o700)
	env := MappingSourceEnvironment(map[string]string{})
	report := ApplyAllSecretSources(map[string]any{"vault": map[string]any{"enabled": true}}, homePath, env)
	if env.Value("OPENAI_API_KEY") != "sk-b" || report.Provenance["OPENAI_API_KEY"].Source != "vault" {
		t.Fatalf("alias expected: %v", env.Snapshot())
	}
}

func TestIsValidEnvName(t *testing.T) {
	for name, want := range map[string]bool{"GOOD_NAME": true, "_LEADING": true, "": false, "1BAD": false, "bad-name": false, "has space": false} {
		if IsValidEnvName(name) != want {
			t.Fatalf("%q: want %t", name, want)
		}
	}
}

func TestScrubANSI(t *testing.T) {
	if ScrubANSI("\x1b[31mred\x1b[0m \x1b]0;title\x07x \x1b]unterminated") != "red x " {
		t.Fatalf("got %q", ScrubANSI("\x1b[31mred\x1b[0m \x1b]0;title\x07x \x1b]unterminated"))
	}
}

// Puts a fake helper binary first on PATH
func fakeBinary(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func TestBitwardenStaleCacheOnlyAfterTransportFailure(t *testing.T) {
	homePath := t.TempDir()
	ClearBitwardenCaches(homePath)
	bws := fakeBinary(t, "bws", `printf '[{"key":"K","value":"v1"}]'`)
	secrets, _, err := FetchBitwardenSecrets(BitwardenFetchOptions{AccessToken: "tok", ProjectID: "proj", Binary: bws, CacheTTL: 300, UseCache: true, HomePath: homePath})
	if err != nil || secrets["K"] != "v1" {
		t.Fatalf("first fetch: %v %v", secrets, err)
	}
	cacheKey := strings.Join([]string{SecretFingerprint("tok"), "proj", ""}, "|")
	bwsStore.Disk.Write(cacheKey, CachedFetch{Secrets: map[string]string{"K": "v1"}, FetchedAt: nowEpoch() - 1000}, 300, homePath)
	bwsStore.mu.Lock()
	bwsStore.memory = map[string]CachedFetch{}
	bwsStore.mu.Unlock()
	if err := os.WriteFile(bws, []byte("#!/bin/sh\necho 'Error: network connection refused' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	secrets, warnings, err := FetchBitwardenSecrets(BitwardenFetchOptions{AccessToken: "tok", ProjectID: "proj", Binary: bws, CacheTTL: 300, UseCache: true, HomePath: homePath})
	if err != nil || secrets["K"] != "v1" || len(warnings) != 1 || !strings.Contains(warnings[0], "stale disk cache") {
		t.Fatalf("stale fallback expected: %v %v %v", secrets, warnings, err)
	}
	if err := os.WriteFile(bws, []byte("#!/bin/sh\necho 'Error: [400 Bad Request] invalid_client' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bwsStore.mu.Lock()
	bwsStore.memory = map[string]CachedFetch{}
	bwsStore.mu.Unlock()
	if _, _, err := FetchBitwardenSecrets(BitwardenFetchOptions{AccessToken: "tok", ProjectID: "proj", Binary: bws, CacheTTL: 300, UseCache: true, HomePath: homePath}); err == nil {
		t.Fatal("an auth failure never serves stale secrets")
	}
}

func TestBitwardenEncryptedCacheRoundTrip(t *testing.T) {
	homePath := t.TempDir()
	entry := CachedFetch{Secrets: map[string]string{"K": "v"}, FetchedAt: nowEpoch()}
	writeEncryptedBwsCache("key|proj|", "tok", entry, homePath)
	if got, ok := readEncryptedBwsCache("key|proj|", "tok", 60, homePath); !ok || got.Secrets["K"] != "v" {
		t.Fatal("encrypted cache must round trip")
	}
	if _, ok := readEncryptedBwsCache("key|proj|", "other-token", 60, homePath); ok {
		t.Fatal("another token must not decrypt the cache")
	}
	raw, _ := os.ReadFile(filepath.Join(homePath, bwsEncryptedCacheBasename))
	if strings.Contains(string(raw), "\"v\"") {
		t.Fatal("the payload must be encrypted at rest")
	}
}

func conformance(t *testing.T, source SecretSource) {
	t.Helper()
	spec := source.Spec()
	if spec.Name == "" || strings.ToLower(spec.Name) != spec.Name || !isAlnumUnderscore(spec.Name) || spec.Label == "" || (spec.Shape != "mapped" && spec.Shape != "bulk") || spec.APIVersion != SecretSourceAPIVersion {
		t.Fatalf("identity wrong: %+v", spec)
	}
	homePath := t.TempDir()
	env := MappingSourceEnvironment(map[string]string{})
	for _, cfg := range []map[string]any{{}, {"enabled": true}, {"enabled": true, "env": "not-a-dict"}, {"enabled": true, "cache_ttl_seconds": "bogus"}, nil} {
		if source.Fetch(cfg, homePath, env) == nil {
			t.Fatal("fetch must return a result")
		}
	}
	result := source.Fetch(map[string]any{"enabled": true}, homePath, env)
	if !result.OK() && (result.ErrorKind == "" || len(result.Secrets) != 0) {
		t.Fatalf("errors carry a kind and no secrets: %+v", result)
	}
	if SecretSourceEnabled(map[string]any{}) || SecretSourceEnabled(map[string]any{"enabled": false}) {
		t.Fatal("disabled by default")
	}
	if SecretSourceFetchTimeout(map[string]any{"enabled": true}) <= 0 || SecretSourceFetchTimeout(map[string]any{"timeout_seconds": "junk"}) <= 0 {
		t.Fatal("timeout positive")
	}
	for v := range SecretSourceProtectedEnvVars(spec, map[string]any{"enabled": true}) {
		if !IsValidEnvName(v) {
			t.Fatal("protected vars are valid names")
		}
	}
	cleanRegistry(t)
	if !RegisterSecretSource(source, false, false, "") {
		t.Fatal("registers")
	}
	report := ApplyAllSecretSources(map[string]any{spec.Name: map[string]any{"enabled": true}}, homePath, MappingSourceEnvironment(map[string]string{}))
	found := false
	for _, s := range report.Sources {
		if s.Name == spec.Name {
			found = true
		}
	}
	if !found {
		t.Fatal("the source must survive an apply pass")
	}
}

func TestBundledSourceConformance(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BWS_ACCESS_TOKEN", "")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	for _, s := range []SecretSource{&BitwardenSource{}, &OnePasswordSource{}, &CommandSource{}} {
		t.Run(s.Spec().Name, func(t *testing.T) { conformance(t, s) })
	}
}

// ---- test_command_secret_source.py ----

func writeHelper(t *testing.T, body, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnquoteStripsOneLayerOfMatchingQuotes(t *testing.T) {
	for in, want := range map[string]string{`"abc"`: "abc", `'abc'`: "abc", `""`: "", `"`: `"`, "  plain  ": "plain"} {
		if UnquoteDotenvValue(in) != want {
			t.Fatalf("%q -> %q", in, UnquoteDotenvValue(in))
		}
	}
}

func TestTimeoutKillsHungHelperAndDegradesToEmpty(t *testing.T) {
	helper := writeHelper(t, "sleep 30", "helper.sh")
	start := time.Now()
	if _, ok := runSecretHelper(helper, "CMDTEST_API_KEY", 2*time.Second, 1024, ProcessSourceEnvironment()); ok {
		t.Fatal("a hung helper resolves no value")
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("helper not killed within the bound")
	}
}

func loaderHome(t *testing.T) string {
	t.Helper()
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	h := newTestHome(t)
	for _, k := range []string{"CMDTEST_API_KEY", "CMDTEST_TOKEN"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	return h
}

func captureStatus(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	prev := secretStatusPrint
	secretStatusPrint = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { secretStatusPrint = prev })
	return &lines
}

func TestRegistryFailingHelperDoesNotBlockStartup(t *testing.T) {
	cleanRegistry(t)
	registerBuiltinSources()
	h := loaderHome(t)
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: true\n    command: exit 9\n")
	ApplyExternalSecretSources(h)
	if SecretSourceOf(h, "CMDTEST_API_KEY") != "" {
		t.Fatal("nothing applied")
	}
}

func TestRegistryHelperErrorPrintsRemediation(t *testing.T) {
	cleanRegistry(t)
	registerBuiltinSources()
	h := loaderHome(t)
	lines := captureStatus(t)
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: true\n    command: ''\n")
	ApplyExternalSecretSources(h)
	if !strings.Contains(strings.Join(*lines, "\n"), "secrets.command.command") {
		t.Fatalf("remediation expected: %v", *lines)
	}
}

func TestSanitizeLoadedCredentialsStripsNonASCII(t *testing.T) {
	lines := captureStatus(t)
	t.Setenv("CMDTEST_API_KEY", "sk-éabc​")
	secretSourceCacheMu.Lock()
	delete(warnedCredentialKeys, "CMDTEST_API_KEY")
	secretSourceCacheMu.Unlock()
	SanitizeLoadedCredentials()
	if os.Getenv("CMDTEST_API_KEY") != "sk-abc" {
		t.Fatalf("got %q", os.Getenv("CMDTEST_API_KEY"))
	}
	if !strings.Contains(strings.Join(*lines, "\n"), "contained 2 non-ASCII characters") {
		t.Fatalf("warning expected: %v", *lines)
	}
}

// Registers the bundled sources into a registry the test emptied
func registerBuiltinSources() {
	for _, s := range []SecretSource{&BitwardenSource{}, &OnePasswordSource{}, &CommandSource{}} {
		RegisterSecretSource(s, false, true, "")
	}
}
