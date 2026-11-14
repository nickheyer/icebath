package provider

import (
	"reflect"
	"testing"
)

func TestRegistrationRejectsNonSecretsourceInstance(t *testing.T) {
	cleanRegistry(t)
	object := struct{ Name string }{"not a source"}
	if (secretSourceScopedRegistry{}).Accepts(object) {
		t.Fatal("an object that is not a secret source is accepted by the registry")
	}
	if adapted, ok := adaptSecretSource(object); ok || adapted != nil {
		t.Fatalf("an object that is not a secret source adapted to %v", adapted)
	}
	if RegisterSecretSource(nil, false, false, "") {
		t.Fatal("a missing source registers")
	}
}

// Bitwarden binary and fetch doubles recording the fetch options, restored when the test ends
func fakeBitwardenFetch(t *testing.T, secrets map[string]string, warnings []string) *BitwardenFetchOptions {
	t.Helper()
	captured := &BitwardenFetchOptions{}
	prevFind, prevFetch := findBws, fetchBitwardenSecrets
	findBws = func(bool) string { return "/fake/bws" }
	fetchBitwardenSecrets = func(opts BitwardenFetchOptions) (map[string]string, []string, error) {
		*captured = opts
		return secrets, warnings, nil
	}
	t.Cleanup(func() { findBws, fetchBitwardenSecrets = prevFind, prevFetch })
	return captured
}

func TestBitwardenSourceFetchDelegatesToFetchBitwardenSecrets(t *testing.T) {
	captured := fakeBitwardenFetch(t, map[string]string{"MY_KEY": "val"}, []string{"a warning"})
	h := t.TempDir()
	env := MappingSourceEnvironment(map[string]string{"BWS_ACCESS_TOKEN": "0.token"})
	result := (&BitwardenSource{}).Fetch(map[string]any{"enabled": true, "project_id": "proj", "server_url": " https://vault.bitwarden.eu "}, h, env)
	if !result.OK() || !reflect.DeepEqual(result.Secrets, map[string]string{"MY_KEY": "val"}) || !reflect.DeepEqual(result.Warnings, []string{"a warning"}) {
		t.Fatalf("result %+v", result)
	}
	if captured.ProjectID != "proj" || captured.ServerURL != "https://vault.bitwarden.eu" || captured.HomePath != h {
		t.Fatalf("fetch options %+v", captured)
	}
}

func TestBitwardenSourceE2eThroughOrchestrator(t *testing.T) {
	cleanRegistry(t)
	fakeBitwardenFetch(t, map[string]string{"ANTHROPIC_API_KEY": "sk-ant", "BWS_ACCESS_TOKEN": "steal"}, nil)
	if !RegisterSecretSource(&BitwardenSource{}, false, false, "") {
		t.Fatal("the bitwarden source registers")
	}
	env := MappingSourceEnvironment(map[string]string{"BWS_ACCESS_TOKEN": "0.token"})
	report := applyAllSecretSourcesForTest(t, map[string]any{"bitwarden": map[string]any{"enabled": true, "project_id": "proj"}}, t.TempDir(), env)
	if env.Value("ANTHROPIC_API_KEY") != "sk-ant" {
		t.Fatalf("ANTHROPIC_API_KEY %q", env.Value("ANTHROPIC_API_KEY"))
	}
	if env.Value("BWS_ACCESS_TOKEN") != "0.token" {
		t.Fatalf("the bootstrap token was replaced with %q", env.Value("BWS_ACCESS_TOKEN"))
	}
	if report.Provenance["ANTHROPIC_API_KEY"].Source != "bitwarden" {
		t.Fatalf("provenance %+v", report.Provenance["ANTHROPIC_API_KEY"])
	}
}
