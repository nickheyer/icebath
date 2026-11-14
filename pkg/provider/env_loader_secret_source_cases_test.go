package provider

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const bitwardenTestConfig = "secrets:\n  bitwarden:\n    enabled: true\n    project_id: test-project\n    access_token_env: BWS_ACCESS_TOKEN\n"

// Clean source map, snapshots and applied home guard with the process env restored afterwards
func secretSourceFixture(t *testing.T) string {
	t.Helper()
	root := envLoaderRoot(t)
	restoreProcessEnv(t)
	captureSecretStatus(t)
	return root
}

// Empty registry whose builtin sources register again on the next apply pass
func freshSecretRegistry(t *testing.T) {
	t.Helper()
	ResetSecretRegistryForTests()
	t.Cleanup(ResetSecretRegistryForTests)
}

func writeProfileConfig(t *testing.T, h, text string) {
	t.Helper()
	writeEnvBytes(t, filepath.Join(h, "config.yaml"), []byte(text))
	config.ResetCache()
}

// Fake bws binary and fetch layer under the Bitwarden source adapter
func fakeBitwarden(t *testing.T, fetch func(opts BitwardenFetchOptions) (map[string]string, []string, error)) {
	t.Helper()
	prevFind, prevFetch := findBws, fetchBitwardenSecrets
	findBws = func(bool) string { return "/fake/bws" }
	fetchBitwardenSecrets = fetch
	t.Cleanup(func() { findBws, fetchBitwardenSecrets = prevFind, prevFetch })
}

// Fake op binary and fetch layer under the 1Password source adapter
func fakeOnePassword(t *testing.T, fetch func(opts OnePasswordFetchOptions) (map[string]string, []string, error)) {
	t.Helper()
	prevFind, prevFetch := findOp, fetchOnePasswordSecrets
	findOp = func(string) string { return "/fake/op" }
	fetchOnePasswordSecrets = fetch
	t.Cleanup(func() { findOp, fetchOnePasswordSecrets = prevFind, prevFetch })
}

// Replaces the orchestrated apply pass the loader and the hydrator run
func fakeApplyAll(t *testing.T, apply func(cfg map[string]any, homePath string, environ *SourceEnvironment) (*ApplyReport, error)) {
	t.Helper()
	prev := applyAllSecretSourcesFn
	applyAllSecretSourcesFn = apply
	t.Cleanup(func() { applyAllSecretSourcesFn = prev })
}

func expectValues(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values %v, want %v", got, want)
	}
}

func TestGetSecretSourceReturnsNoneForUntrackedVar(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	if got := SecretSourceOf(h, "ANTHROPIC_API_KEY"); got != "" {
		t.Fatalf("source %q", got)
	}
}

func TestGetSecretSourceValuesReturnsHomeSnapshotCopy(t *testing.T) {
	root := secretSourceFixture(t)
	homeA := envLoaderProfile(t, root, "profile-a")
	homeB := envLoaderProfile(t, root, "profile-b")
	secretSourceCacheMu.Lock()
	secretValuesByHome[homeKey(homeA)] = map[string]string{"ANTHROPIC_API_KEY": "sk-profile-a"}
	secretSourceCacheMu.Unlock()
	snapshot := SecretSourceValues(homeA)
	expectValues(t, snapshot, map[string]string{"ANTHROPIC_API_KEY": "sk-profile-a"})
	expectValues(t, SecretSourceValues(homeB), map[string]string{})
	snapshot["ANTHROPIC_API_KEY"] = "mutated"
	expectValues(t, SecretSourceValues(homeA), map[string]string{"ANTHROPIC_API_KEY": "sk-profile-a"})
}

func TestFormatSecretSourceSuffixEmptyForUntracked(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	if got := FormatSecretSourceSuffix(h, "ANTHROPIC_API_KEY"); got != "" {
		t.Fatalf("suffix %q", got)
	}
}

func TestApplyExternalSecretSourcesRecordsBitwardenOrigin(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("BWS_ACCESS_TOKEN", "0.test-token")
	envLoaderUnset(t, "ANTHROPIC_API_KEY")
	writeProfileConfig(t, h, bitwardenTestConfig)
	fakeBitwarden(t, func(BitwardenFetchOptions) (map[string]string, []string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}, nil, nil
	})
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	if got := SecretSourceOf(h, "ANTHROPIC_API_KEY"); got != "bitwarden" {
		t.Fatalf("source %q", got)
	}
	if got := FormatSecretSourceSuffix(h, "ANTHROPIC_API_KEY"); got != " (from Bitwarden)" {
		t.Fatalf("suffix %q", got)
	}
}

func TestColdProfileBitwardenUsesProfileBootstrapWithoutGlobalEnv(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	envLoaderUnset(t, "BWS_ACCESS_TOKEN", "ANTHROPIC_API_KEY")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("BWS_ACCESS_TOKEN=profile-bootstrap\n"))
	writeProfileConfig(t, h, bitwardenTestConfig)
	var captured BitwardenFetchOptions
	fakeBitwarden(t, func(opts BitwardenFetchOptions) (map[string]string, []string, error) {
		captured = opts
		return map[string]string{"ANTHROPIC_API_KEY": "profile-provider-key"}, nil, nil
	})
	freshSecretRegistry(t)
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{"ANTHROPIC_API_KEY": "profile-provider-key"})
	if captured.AccessToken != "profile-bootstrap" {
		t.Fatalf("access token %q", captured.AccessToken)
	}
	expectUnset(t, "BWS_ACCESS_TOKEN")
	expectUnset(t, "ANTHROPIC_API_KEY")
}

func TestSingleProfileScopedLoadKeepsOverrideBehavior(t *testing.T) {
	root := secretSourceFixture(t)
	envLoaderUnset(t, "ICEBATH_TEST_SHARED_ADAPTER_CONFIG")
	other := envLoaderProfile(t, root, "other")
	envFile := filepath.Join(other, ".env")
	writeEnvBytes(t, envFile, []byte("ICEBATH_TEST_SHARED_ADAPTER_CONFIG=second\n"))
	resetMultiplex(t)
	ctx := home.WithOverride(context.Background(), other)
	loaded, err := LoadIcebathDotenv(ctx, DotenvLoadOptions{Home: other})
	if err != nil {
		t.Fatal(err)
	}
	expectEnv(t, "ICEBATH_TEST_SHARED_ADAPTER_CONFIG", "second")
	if !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
}

func TestMultiplexDotenvLoadHydratesSourcesWithoutGlobalEnv(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	envLoaderUnset(t, "BWS_ACCESS_TOKEN", "ANTHROPIC_API_KEY")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("BWS_ACCESS_TOKEN=profile-bootstrap\n"))
	writeProfileConfig(t, h, bitwardenTestConfig)
	fakeBitwarden(t, func(BitwardenFetchOptions) (map[string]string, []string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "profile-provider-key"}, nil, nil
	})
	freshSecretRegistry(t)
	resetMultiplex(t)
	SetMultiplexActive(true)
	ctx := home.WithOverride(context.Background(), h)
	loaded, err := LoadIcebathDotenv(ctx, DotenvLoadOptions{Home: h})
	if err != nil || len(loaded) != 0 {
		t.Fatalf("loaded %v %v", loaded, err)
	}
	expectValues(t, SecretSourceValues(h), map[string]string{"ANTHROPIC_API_KEY": "profile-provider-key"})
	expectUnset(t, "BWS_ACCESS_TOKEN")
	expectUnset(t, "ANTHROPIC_API_KEY")
}

// Hydration of a home whose apply pass only records the environment it was handed
func hydrationSeenEnv(t *testing.T, h string) map[string]string {
	t.Helper()
	seen := map[string]string{}
	fakeApplyAll(t, func(_ map[string]any, _ string, environ *SourceEnvironment) (*ApplyReport, error) {
		for k, v := range environ.Mapping() {
			seen[k] = v
		}
		return &ApplyReport{Provenance: map[string]AppliedVar{}}, nil
	})
	freshSecretRegistry(t)
	HydrateProfileSecretSources(h)
	return seen
}

func TestColdProfileHydrationSeedsOpEnvBootstrap(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	envLoaderUnset(t, "OP_SERVICE_ACCOUNT_TOKEN")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("UNRELATED=x\n"))
	writeEnvBytes(t, filepath.Join(h, ".op.env"), []byte("OP_SERVICE_ACCOUNT_TOKEN=ops_from-op-env\n"))
	writeProfileConfig(t, h, "secrets:\n  onepassword:\n    enabled: true\n")
	seen := hydrationSeenEnv(t, h)
	if seen["OP_SERVICE_ACCOUNT_TOKEN"] != "ops_from-op-env" {
		t.Fatalf("seen %v", seen)
	}
	expectUnset(t, "OP_SERVICE_ACCOUNT_TOKEN")
}

func TestColdProfileHydrationDotenvWinsOverOpEnv(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	envLoaderUnset(t, "OP_SERVICE_ACCOUNT_TOKEN")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("OP_SERVICE_ACCOUNT_TOKEN=ops_from-dotenv\n"))
	writeEnvBytes(t, filepath.Join(h, ".op.env"), []byte("OP_SERVICE_ACCOUNT_TOKEN=ops_from-op-env\n"))
	writeProfileConfig(t, h, "secrets:\n  onepassword:\n    enabled: true\n")
	seen := hydrationSeenEnv(t, h)
	if seen["OP_SERVICE_ACCOUNT_TOKEN"] != "ops_from-dotenv" {
		t.Fatalf("seen %v", seen)
	}
}

// Report of one applied bulk var from source with every other field empty
func appliedReport(source, label, name, shape string, overrode bool) *ApplyReport {
	return &ApplyReport{
		Sources:    []*SourceReport{{Name: source, Label: label, Result: NewSecretFetchResult(), Applied: []string{name}}},
		Provenance: map[string]AppliedVar{name: {Name: name, Source: source, Shape: shape, OverrodeEnv: overrode}},
	}
}

func TestColdProfileHydrationRetriesFailedSource(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	writeProfileConfig(t, h, "secrets:\n  command:\n    enabled: true\n")
	attempts := 0
	fakeApplyAll(t, func(_ map[string]any, _ string, environ *SourceEnvironment) (*ApplyReport, error) {
		attempts++
		if attempts == 1 {
			failed := NewSecretFetchResult().Fail("helper exited 1", SecretErrorAuthFailed)
			return &ApplyReport{Sources: []*SourceReport{{Name: "command", Label: "command", Result: failed}}, Provenance: map[string]AppliedVar{}}, nil
		}
		environ.Set("OPENAI_API_KEY", "recovered-key")
		report := appliedReport("command", "command", "OPENAI_API_KEY", "bulk", false)
		report.Sources[0].Result.Secrets["OPENAI_API_KEY"] = "recovered-key"
		return report, nil
	})
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{})
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{"OPENAI_API_KEY": "recovered-key"})
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{"OPENAI_API_KEY": "recovered-key"})
	if attempts != 2 {
		t.Fatalf("attempts %d", attempts)
	}
}

func TestColdProfileHydrationClearsPartialSnapshotWhenSourcesAreRemoved(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	writeProfileConfig(t, h, "secrets:\n  command:\n    enabled: true\n")
	fakeApplyAll(t, func(_ map[string]any, _ string, environ *SourceEnvironment) (*ApplyReport, error) {
		environ.Set("OPENAI_API_KEY", "partial-key")
		report := appliedReport("onepassword", "1Password", "OPENAI_API_KEY", "bulk", false)
		report.Sources[0].Result.Secrets["OPENAI_API_KEY"] = "partial-key"
		failed := NewSecretFetchResult().Fail("helper exited 1", SecretErrorAuthFailed)
		report.Sources = append(report.Sources, &SourceReport{Name: "command", Label: "command", Result: failed})
		return report, nil
	})
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{"OPENAI_API_KEY": "partial-key"})
	if got := BuildProfileSecretScope(h)["OPENAI_API_KEY"]; got != "partial-key" {
		t.Fatalf("scope %q", got)
	}
	writeProfileConfig(t, h, "{}\n")
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{})
	expectValues(t, SecretSourceValues(h), map[string]string{})
	if _, ok := BuildProfileSecretScope(h)["OPENAI_API_KEY"]; ok {
		t.Fatal("revoked value still in the profile scope")
	}
}

func TestApplyExternalSecretSourcesNoopWhenDisabled(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	writeProfileConfig(t, h, "secrets:\n  bitwarden:\n    enabled: false\n")
	ApplyExternalSecretSources(h)
	if got := SecretSourceOf(h, "ANTHROPIC_API_KEY"); got != "" {
		t.Fatalf("source %q", got)
	}
}

func TestApplyExternalSecretSourcesDedupesWithinProcess(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("BWS_ACCESS_TOKEN", "0.test-token")
	envLoaderUnset(t, "ANTHROPIC_API_KEY")
	writeProfileConfig(t, h, bitwardenTestConfig)
	calls := 0
	fakeBitwarden(t, func(BitwardenFetchOptions) (map[string]string, []string, error) {
		calls++
		return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}, nil, nil
	})
	freshSecretRegistry(t)
	for range 5 {
		ApplyExternalSecretSources(h)
	}
	if calls != 1 {
		t.Fatalf("Bitwarden backend was called %d time(s); expected exactly 1", calls)
	}
	if got := SecretSourceOf(h, "ANTHROPIC_API_KEY"); got != "bitwarden" {
		t.Fatalf("source %q", got)
	}
	expectValues(t, SecretSourceValues(h), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"})
	ResetSecretSourceCache("")
	ApplyExternalSecretSources(h)
	if calls != 2 {
		t.Fatalf("calls after reset %d", calls)
	}
}

func TestApplyExternalSecretSourcesStatusLineSuppressesSecretNames(t *testing.T) {
	root := secretSourceFixture(t)
	lines := captureSecretStatus(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("BWS_ACCESS_TOKEN", "0.test-token")
	envLoaderUnset(t, "LEAK_THIS_API_KEY", "LEAK_THIS_TOKEN")
	writeProfileConfig(t, h, bitwardenTestConfig)
	fakeBitwarden(t, func(BitwardenFetchOptions) (map[string]string, []string, error) {
		return map[string]string{"LEAK_THIS_API_KEY": "sk-test", "LEAK_THIS_TOKEN": "tok-test"}, nil, nil
	})
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	stderr := strings.Join(*lines, "\n")
	if strings.Contains(stderr, "LEAK_THIS_API_KEY") || strings.Contains(stderr, "LEAK_THIS_TOKEN") {
		t.Fatalf("status output names secrets %q", stderr)
	}
}

func TestExternalSecretValuesAreIsolatedBetweenHomes(t *testing.T) {
	root := secretSourceFixture(t)
	homeA := envLoaderProfile(t, root, "profile-a")
	homeB := envLoaderProfile(t, root, "profile-b")
	for _, h := range []string{homeA, homeB} {
		writeProfileConfig(t, h, "secrets:\n  test-source:\n    enabled: true\n")
	}
	values := map[string]string{homeKey(homeA): "value-a", homeKey(homeB): "value-b"}
	envLoaderUnset(t, "SHARED_API_KEY")
	fakeApplyAll(t, func(_ map[string]any, homePath string, environ *SourceEnvironment) (*ApplyReport, error) {
		environ.Set("SHARED_API_KEY", values[homeKey(homePath)])
		return appliedReport("test-source", "Test Source", "SHARED_API_KEY", "mapped", true), nil
	})
	ApplyExternalSecretSources(homeA)
	ApplyExternalSecretSources(homeB)
	expectEnv(t, "SHARED_API_KEY", "value-b")
	expectValues(t, SecretSourceValues(homeA), map[string]string{"SHARED_API_KEY": "value-a"})
	expectValues(t, SecretSourceValues(homeB), map[string]string{"SHARED_API_KEY": "value-b"})
	expectValues(t, BuildProfileSecretScope(homeA), map[string]string{"SHARED_API_KEY": "value-a"})
	expectValues(t, BuildProfileSecretScope(homeB), map[string]string{"SHARED_API_KEY": "value-b"})
}

func TestApplyExternalSecretSourcesRecordsOnepasswordOrigin(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	envLoaderUnset(t, "ANTHROPIC_API_KEY")
	writeProfileConfig(t, h, "secrets:\n  onepassword:\n    enabled: true\n    env:\n      ANTHROPIC_API_KEY: 'op://Private/Anthropic/credential'\n")
	fakeOnePassword(t, func(OnePasswordFetchOptions) (map[string]string, []string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"}, nil, nil
	})
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	if got := SecretSourceOf(h, "ANTHROPIC_API_KEY"); got != "onepassword" {
		t.Fatalf("source %q", got)
	}
	if got := FormatSecretSourceSuffix(h, "ANTHROPIC_API_KEY"); got != " (from 1Password)" {
		t.Fatalf("suffix %q", got)
	}
}

func TestApplyExternalSecretSourcesSurvivesNonDictSection(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	writeProfileConfig(t, h, "secrets:\n  bitwarden: true\n  onepassword: true\n")
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	if got := SecretSourceOf(h, "ANYTHING"); got != "" {
		t.Fatalf("source %q", got)
	}
}

func TestApplyExternalSecretSourcesBadTtlDoesNotCrash(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_HOME", h)
	writeProfileConfig(t, h, "secrets:\n  onepassword:\n    enabled: true\n    cache_ttl_seconds: not-a-number\n    env:\n      K: 'op://V/I/F'\n")
	var captured OnePasswordFetchOptions
	fakeOnePassword(t, func(opts OnePasswordFetchOptions) (map[string]string, []string, error) {
		captured = opts
		return map[string]string{}, nil, nil
	})
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	if captured.CacheTTL != 300 {
		t.Fatalf("cache ttl %v", captured.CacheTTL)
	}
}

// Bulk source named fakebulk resolving GLM_API_KEY per home through valueForHome
func registerFakeBulkSourcePerHome(t *testing.T, valueForHome func(h string) string) {
	t.Helper()
	cleanRegistry(t)
	src := makeSource("fakebulk", "bulk", nil)
	src.fetch = func(_ map[string]any, homePath string) *SecretFetchResult {
		res := NewSecretFetchResult()
		res.Secrets["GLM_API_KEY"] = valueForHome(homePath)
		return res
	}
	if !RegisterSecretSource(src, true, false, "") {
		t.Fatal("fake source refused")
	}
}

func TestEnvShadowedReapplyKeepsHomeSnapshot(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeProfileConfig(t, h, "secrets:\n  fakebulk:\n    enabled: true\n")
	t.Setenv("ICEBATH_HOME", h)
	envLoaderUnset(t, "GLM_API_KEY")
	registerFakeBulkSourcePerHome(t, func(string) string { return "vault-value" })
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectValues(t, SecretSourceValues(h), map[string]string{"GLM_API_KEY": "vault-value"})
	ResetSecretSourceCache("")
	mustLoad(t, DotenvLoadOptions{Home: h})
	if !SecretHomeApplied(h) {
		t.Fatal("home not marked applied after the re-apply")
	}
	expectValues(t, HydrateProfileSecretSources(h), map[string]string{"GLM_API_KEY": "vault-value"})
	if got := BuildProfileSecretScope(h)["GLM_API_KEY"]; got != "vault-value" {
		t.Fatalf("scope %q", got)
	}
}

func TestHomeScopedResetPreservesSiblingSnapshot(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	sibling := envLoaderProfile(t, root, filepath.Join(".icebath", "profiles", "b"))
	for _, p := range []string{h, sibling} {
		writeProfileConfig(t, p, "secrets:\n  fakebulk:\n    enabled: true\n")
	}
	t.Setenv("ICEBATH_HOME", h)
	envLoaderUnset(t, "GLM_API_KEY")
	registerFakeBulkSourcePerHome(t, func(p string) string { return "vault-" + filepath.Base(p) })
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectValues(t, HydrateProfileSecretSources(sibling), map[string]string{"GLM_API_KEY": "vault-b"})
	ResetSecretSourceCache(h)
	expectValues(t, SecretSourceValues(h), map[string]string{})
	expectValues(t, SecretSourceValues(sibling), map[string]string{"GLM_API_KEY": "vault-b"})
	if !SecretHomeApplied(sibling) {
		t.Fatal("sibling snapshot lost its applied mark")
	}
}

const enabledBitwardenConfig = "secrets:\n  bitwarden:\n    enabled: true\n    project_id: proj\n"

var errBwsNetwork = errors.New("bws exited 1: network unreachable")

// Full dotenv load under ctx that must succeed, returning the loaded paths
func mustLoadCtx(t *testing.T, ctx context.Context, opts DotenvLoadOptions) []string {
	t.Helper()
	loaded, err := LoadIcebathDotenv(ctx, opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return loaded
}

// Counting Bitwarden fetch returning secrets, or failing with err when it is set
func countingBitwarden(t *testing.T, secrets map[string]string, err error) *int {
	t.Helper()
	calls := 0
	fakeBitwarden(t, func(BitwardenFetchOptions) (map[string]string, []string, error) {
		calls++
		if err != nil {
			return nil, nil, err
		}
		return secrets, nil, nil
	})
	return &calls
}

func TestMalformedConfigDoesNotPermanentlySkip(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeProfileConfig(t, h, "secrets: [unclosed")
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	if SecretHomeApplied(h) {
		t.Fatal("malformed config marked the home applied")
	}
	writeProfileConfig(t, h, enabledBitwardenConfig)
	t.Setenv("BWS_ACCESS_TOKEN", "0.t")
	calls := countingBitwarden(t, map[string]string{"NEW_KEY_40597": "val"}, nil)
	envLoaderUnset(t, "NEW_KEY_40597")
	ResetSecretRegistryForTests()
	ApplyExternalSecretSources(h)
	if *calls != 1 || !SecretHomeApplied(h) {
		t.Fatalf("calls %d applied %v", *calls, SecretHomeApplied(h))
	}
}

func TestNoSecretsSectionDoesNotMarkApplied(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeProfileConfig(t, h, "model:\n  provider: openrouter\n")
	ApplyExternalSecretSources(h)
	if SecretHomeApplied(h) {
		t.Fatal("home without a secrets section marked applied")
	}
}

func TestDisabledSourcesDoNotMarkApplied(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeProfileConfig(t, h, "secrets:\n  bitwarden:\n    enabled: false\n    project_id: p\n")
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	if SecretHomeApplied(h) {
		t.Fatal("disabled sources marked the home applied")
	}
}

func TestFetchErrorStillMarksApplied(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeProfileConfig(t, h, enabledBitwardenConfig)
	t.Setenv("BWS_ACCESS_TOKEN", "0.dead")
	calls := countingBitwarden(t, nil, errBwsNetwork)
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	ApplyExternalSecretSources(h)
	if *calls != 1 || !SecretHomeApplied(h) {
		t.Fatalf("calls %d applied %v", *calls, SecretHomeApplied(h))
	}
}

func TestSuccessMarksAppliedAndSecondCallNoop(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeProfileConfig(t, h, enabledBitwardenConfig)
	t.Setenv("BWS_ACCESS_TOKEN", "0.t")
	envLoaderUnset(t, "KEY_OK_40597")
	calls := countingBitwarden(t, map[string]string{"KEY_OK_40597": "v"}, nil)
	freshSecretRegistry(t)
	ApplyExternalSecretSources(h)
	ApplyExternalSecretSources(h)
	if *calls != 1 {
		t.Fatalf("calls %d", *calls)
	}
}

// Routed profile turn context like the gateway binds it, the home override and the hydrated scope
func profileTurnContext(profile string) context.Context {
	ctx := home.WithOverride(context.Background(), profile)
	return WithSecretScope(ctx, BuildProfileSecretScope(profile), profile)
}

func expectScopedSecret(t *testing.T, ctx context.Context, name, want string) {
	t.Helper()
	got, ok, err := GetSecret(ctx, name)
	if err != nil || !ok || got != want {
		t.Fatalf("%s = %q %v %v, want %q", name, got, ok, err, want)
	}
}

func TestTurnScopedDotenvReloadDoesNotPolluteProcessEnv(t *testing.T) {
	root := secretSourceFixture(t)
	profileA := envLoaderProfile(t, root, filepath.Join("profiles", "a"))
	profileB := envLoaderProfile(t, root, filepath.Join("profiles", "b"))
	writeEnvBytes(t, filepath.Join(profileA, ".env"), []byte("PROFILE_SCOPED_API_KEY=secret-a\nDISCORD_ALLOWED_CHANNELS=profile-a-only\n"))
	writeEnvBytes(t, filepath.Join(profileB, ".env"), []byte("PROFILE_SCOPED_API_KEY=secret-b\nDISCORD_ALLOWED_CHANNELS=profile-b-only\n"))
	envLoaderUnset(t, "PROFILE_SCOPED_API_KEY")
	t.Setenv("DISCORD_ALLOWED_CHANNELS", "all-channels")
	resetMultiplex(t)
	SetMultiplexActive(true)
	for _, tc := range []struct{ profile, suffix string }{{profileA, "a"}, {profileB, "b"}} {
		ctx := profileTurnContext(tc.profile)
		expectScopedSecret(t, ctx, "PROFILE_SCOPED_API_KEY", "secret-"+tc.suffix)
		expectScopedSecret(t, ctx, "DISCORD_ALLOWED_CHANNELS", "profile-"+tc.suffix+"-only")
		loaded, err := LoadIcebathDotenv(ctx, DotenvLoadOptions{Home: home.Dir(ctx)})
		if err != nil || len(loaded) != 0 {
			t.Fatalf("loaded %v %v", loaded, err)
		}
		expectUnset(t, "PROFILE_SCOPED_API_KEY")
		expectEnv(t, "DISCORD_ALLOWED_CHANNELS", "all-channels")
	}
}

func TestLaunchHomeDotenvStillLoadsUnderMultiplex(t *testing.T) {
	root := secretSourceFixture(t)
	launch := envLoaderProfile(t, root, "launch")
	routed := envLoaderProfile(t, root, filepath.Join("profiles", "routed"))
	launchEnv := filepath.Join(launch, ".env")
	writeEnvBytes(t, launchEnv, []byte("LAUNCH_ONLY_FALLBACK_KEY=launch-key\n"))
	writeEnvBytes(t, filepath.Join(routed, ".env"), []byte("LAUNCH_ONLY_FALLBACK_KEY=routed-secret\n"))
	t.Setenv("ICEBATH_HOME", launch)
	envLoaderUnset(t, "LAUNCH_ONLY_FALLBACK_KEY")
	resetMultiplex(t)
	SetMultiplexActive(true)
	load := func(h string) []string {
		return mustLoadCtx(t, home.WithOverride(context.Background(), h), DotenvLoadOptions{Home: h})
	}
	if !sameStrings(load(launch), launchEnv) {
		t.Fatal("launch home load skipped under its own override")
	}
	expectEnv(t, "LAUNCH_ONLY_FALLBACK_KEY", "launch-key")
	if got := load(routed); len(got) != 0 {
		t.Fatalf("routed load %v", got)
	}
	expectEnv(t, "LAUNCH_ONLY_FALLBACK_KEY", "launch-key")
	if !sameStrings(load(launch), launchEnv) {
		t.Fatal("second launch home load skipped")
	}
	expectEnv(t, "LAUNCH_ONLY_FALLBACK_KEY", "launch-key")
}

func TestLaunchHomeLoadInsideForeignTurnKeepsRoutedCwdOutOfProcessEnv(t *testing.T) {
	root := secretSourceFixture(t)
	launch := envLoaderProfile(t, root, "launch")
	routed := envLoaderProfile(t, root, filepath.Join("profiles", "routed"))
	launchWork := envLoaderProfile(t, root, "work-launch")
	routedWork := envLoaderProfile(t, root, "work-routed")
	writeEnvBytes(t, filepath.Join(launch, ".env"), []byte("LAUNCH_ONLY_FALLBACK_KEY=launch-key\n"))
	writeProfileConfig(t, launch, "terminal:\n  backend: local\n  cwd: "+launchWork+"\n")
	writeProfileConfig(t, routed, "terminal:\n  backend: local\n  cwd: "+routedWork+"\n")
	t.Setenv("ICEBATH_HOME", launch)
	t.Setenv("TERMINAL_CWD", launchWork)
	terminalConfigBridgeFn = applyTerminalConfigToEnv
	resetMultiplex(t)
	SetMultiplexActive(true)
	ctx := home.WithOverride(context.Background(), routed)
	loaded, err := LoadIcebathDotenv(ctx, DotenvLoadOptions{Home: launch})
	if err != nil || len(loaded) != 0 {
		t.Fatalf("loaded %v %v", loaded, err)
	}
	expectEnv(t, "TERMINAL_CWD", launchWork)
}

func TestDotenvLoadingIsPreservedWhenExternalSecretsAreSkipped(t *testing.T) {
	for _, external := range []bool{true, false} {
		root := secretSourceFixture(t)
		h := envLoaderProfile(t, root, "hermes-home")
		envFile := filepath.Join(h, ".env")
		writeEnvBytes(t, envFile, []byte("UPDATE_TEST_VALUE=from-dotenv\n"))
		envLoaderUnset(t, "UPDATE_TEST_VALUE")
		var applied []string
		applyExternalSecretSourcesFn = func(p string) { applied = append(applied, p) }
		loaded := mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: !external})
		if !sameStrings(loaded, envFile) {
			t.Fatalf("loaded %v", loaded)
		}
		expectEnv(t, "UPDATE_TEST_VALUE", "from-dotenv")
		if external && !sameStrings(applied, h) || !external && len(applied) != 0 {
			t.Fatalf("external=%v applied %v", external, applied)
		}
	}
}
