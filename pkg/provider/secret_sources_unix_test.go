//go:build !windows

package provider

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBitwardenE2EThroughOrchestrator(t *testing.T) {
	cleanRegistry(t)
	fakeBinary(t, "bws", `printf '[{"key":"ANTHROPIC_API_KEY","value":"sk-ant"},{"key":"BWS_ACCESS_TOKEN","value":"steal"}]'`)
	RegisterSecretSource(&BitwardenSource{}, false, false, "")
	env := MappingSourceEnvironment(map[string]string{"BWS_ACCESS_TOKEN": "0.token"})
	report := ApplyAllSecretSources(map[string]any{"bitwarden": map[string]any{"enabled": true, "project_id": "proj", "cache_ttl_seconds": 0}}, t.TempDir(), env)
	if env.Value("ANTHROPIC_API_KEY") != "sk-ant" || env.Value("BWS_ACCESS_TOKEN") != "0.token" || report.Provenance["ANTHROPIC_API_KEY"].Source != "bitwarden" {
		t.Fatalf("got %v %+v", env.Snapshot(), report)
	}
}

func TestBitwardenFetchDelegatesToBws(t *testing.T) {
	fakeBinary(t, "bws", `[ "$BWS_SERVER_URL" = "https://vault.bitwarden.eu" ] || exit 3
printf '[{"key":"MY_KEY","value":"val"},{"key":"bad name","value":"x"}]'`)
	homePath := t.TempDir()
	env := MappingSourceEnvironment(map[string]string{"BWS_ACCESS_TOKEN": "0.token"})
	result := (&BitwardenSource{}).Fetch(map[string]any{"enabled": true, "project_id": "proj", "server_url": " https://vault.bitwarden.eu ", "cache_ttl_seconds": 0}, homePath, env)
	if !result.OK() || result.Secrets["MY_KEY"] != "val" || len(result.Warnings) != 1 {
		t.Fatalf("got %+v", result)
	}
}

func TestFetchParsesDotenvBlob(t *testing.T) {
	helper := writeHelper(t, `printf 'CMDTEST_API_KEY=sk-applied\nCMDTEST_TOKEN=tok-applied\n'`, "helper.sh")
	result := (&CommandSource{}).Fetch(map[string]any{"enabled": true, "command": helper}, t.TempDir(), ProcessSourceEnvironment())
	if !result.OK() || result.Secrets["CMDTEST_API_KEY"] != "sk-applied" || result.Secrets["CMDTEST_TOKEN"] != "tok-applied" {
		t.Fatalf("got %+v", result)
	}
}

func TestHelperStdoutIsReturnedVerbatim(t *testing.T) {
	helper := writeHelper(t, "printf 'sk-test-bare-12345'", "helper.sh")
	if out, ok := runSecretHelper(helper, "CMDTEST_API_KEY", 3*time.Second, 1024, ProcessSourceEnvironment()); !ok || out != "sk-test-bare-12345" {
		t.Fatalf("got %q", out)
	}
}

func TestHydrateProfileSecretSourcesNeverTouchesProcessEnv(t *testing.T) {
	cleanRegistry(t)
	registerBuiltinSources()
	h := loaderHome(t)
	helper := writeHelper(t, `printf 'CMDTEST_API_KEY=sk-hydrated\n'`, "helper.sh")
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: true\n    command: "+helper+"\n")
	values := HydrateProfileSecretSources(h)
	if values["CMDTEST_API_KEY"] != "sk-hydrated" || os.Getenv("CMDTEST_API_KEY") != "" {
		t.Fatalf("hydration is private: %v env=%q", values, os.Getenv("CMDTEST_API_KEY"))
	}
	if SecretSourceValues(h)["CMDTEST_API_KEY"] != "sk-hydrated" || BuildProfileSecretScope(h)["CMDTEST_API_KEY"] != "sk-hydrated" {
		t.Fatal("the snapshot must feed the profile scope")
	}
}

func TestMappedOpBeatsBulkBitwardenThroughOrchestrator(t *testing.T) {
	cleanRegistry(t)
	fakeBinary(t, "bws", `printf '[{"key":"SHARED_KEY","value":"from-bitwarden"},{"key":"BW_ONLY","value":"bw-val"}]'`)
	fakeBinary(t, "op", `printf 'from-1password'`)
	RegisterSecretSource(&BitwardenSource{}, false, false, "")
	RegisterSecretSource(&OnePasswordSource{}, false, false, "")
	env := MappingSourceEnvironment(map[string]string{"BWS_ACCESS_TOKEN": "0.token"})
	report := ApplyAllSecretSources(map[string]any{"sources": []any{"bitwarden", "onepassword"},
		"bitwarden":   map[string]any{"enabled": true, "project_id": "proj", "cache_ttl_seconds": 0},
		"onepassword": map[string]any{"enabled": true, "env": map[string]any{"SHARED_KEY": "op://V/I/F"}, "cache_ttl_seconds": 0}}, t.TempDir(), env)
	if env.Value("SHARED_KEY") != "from-1password" || env.Value("BW_ONLY") != "bw-val" {
		t.Fatalf("mapped must beat bulk: %v", env.Snapshot())
	}
	if report.Provenance["SHARED_KEY"].Source != "onepassword" || report.Provenance["BW_ONLY"].Source != "bitwarden" || len(report.Conflicts) == 0 {
		t.Fatalf("provenance wrong: %+v", report)
	}
}

func TestOnePasswordValidationAndEmptyValues(t *testing.T) {
	valid, warnings := validateOpReferences(map[string]any{"GOOD": "op://v/i/f ", "bad name": "op://v/i/f", "NOTREF": "plain", "NUM": 3})
	if len(valid) != 1 || valid["GOOD"] != "op://v/i/f" || len(warnings) != 3 {
		t.Fatalf("got %v %v", valid, warnings)
	}
	fakeBinary(t, "op", `printf ''`)
	homePath := t.TempDir()
	ClearOnePasswordCaches(homePath)
	secrets, warnings, err := FetchOnePasswordSecrets(OnePasswordFetchOptions{References: map[string]any{"EMPTY": "op://v/i/f"}, UseCache: false, HomePath: homePath})
	if err != nil || len(secrets) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "empty value") {
		t.Fatalf("an empty op value must become a warning: %v %v %v", secrets, warnings, err)
	}
}

func TestProfileHelperDoesNotInheritProcessSecret(t *testing.T) {
	t.Setenv("LEAK_CANARY", "global-secret")
	out, ok := runSecretHelper(`printf "%s|%s" "${LEAK_CANARY-unset}" "$PROFILE_ONLY"`, "", time.Second, 1024, MappingSourceEnvironment(map[string]string{"PROFILE_ONLY": "profile-value"}))
	if !ok || out != "unset|profile-value" {
		t.Fatalf("got %q %t", out, ok)
	}
}

func TestRegistryCommandSourceAppliesAndRecordsSource(t *testing.T) {
	cleanRegistry(t)
	registerBuiltinSources()
	h := loaderHome(t)
	helper := writeHelper(t, `printf 'CMDTEST_API_KEY=sk-dispatch\nCMDTEST_TOKEN=tok-dispatch\n'`, "helper.sh")
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: true\n    command: "+helper+"\n")
	ApplyExternalSecretSources(h)
	if os.Getenv("CMDTEST_API_KEY") != "sk-dispatch" || SecretSourceOf(h, "CMDTEST_API_KEY") != "command" || SecretSourceOf(h, "CMDTEST_TOKEN") != "command" {
		t.Fatal("the helper secrets must land with provenance")
	}
	if FormatSecretSourceSuffix(h, "CMDTEST_API_KEY") != " (from Command helper)" {
		t.Fatalf("got %q", FormatSecretSourceSuffix(h, "CMDTEST_API_KEY"))
	}
}

func TestRegistryStatusLinePrintedOncePerHome(t *testing.T) {
	cleanRegistry(t)
	registerBuiltinSources()
	h := loaderHome(t)
	lines := captureStatus(t)
	helper := writeHelper(t, `printf 'CMDTEST_API_KEY=sk-once\n'`, "helper.sh")
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: true\n    command: "+helper+"\n")
	for i := 0; i < 3; i++ {
		ApplyExternalSecretSources(h)
	}
	count := 0
	for _, l := range *lines {
		if strings.Contains(l, "Command helper: applied 1 secret") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("status line printed %d times", count)
	}
}

func TestRevokedSourceWritesLeaveTheEnvironment(t *testing.T) {
	cleanRegistry(t)
	registerBuiltinSources()
	h := loaderHome(t)
	helper := writeHelper(t, `printf 'CMDTEST_API_KEY=sk-first\n'`, "helper.sh")
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: true\n    command: "+helper+"\n")
	ApplyExternalSecretSources(h)
	if os.Getenv("CMDTEST_API_KEY") != "sk-first" {
		t.Fatal("applied first")
	}
	ResetSecretSourceCache(h)
	writeConfigYAML(t, h, "secrets:\n  command:\n    enabled: false\n")
	ApplyExternalSecretSources(h)
	if _, still := os.LookupEnv("CMDTEST_API_KEY"); still {
		t.Fatal("a disabled source's write must be revoked")
	}
	if SecretSourceOf(h, "CMDTEST_API_KEY") != "" {
		t.Fatal("provenance cleared")
	}
}

func TestRunSecretCLIMinimalEnv(t *testing.T) {
	t.Setenv("LEAKY_API_KEY", "secret")
	res, err := RunSecretCLI([]string{"/bin/sh", "-c", "env"}, nil, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		name, _, _ := strings.Cut(line, "=")
		if strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_TOKEN") || strings.HasSuffix(name, "_SECRET") {
			t.Fatalf("credential leaked to the helper: %s", name)
		}
	}
	if !strings.Contains(res.Stdout, "NO_COLOR=1") {
		t.Fatal("NO_COLOR expected")
	}
}
