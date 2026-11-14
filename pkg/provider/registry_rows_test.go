package provider

import (
	"context"
	"strings"
	"testing"
)

func tokenProfile(name string, ambient bool) Profile {
	return Profile{Name: name, Aliases: []string{name + "-alias", name + "-other"}, KeyEnv: []string{strings.ToUpper(name) + "_TOKEN"},
		BaseURL: "https://" + name + ".example/v1", AuthType: name + "_flow", RegistryAuthType: "api_key", AmbientCredentials: ambient}
}

func TestAmbientPluginIsNotEnvAutoDetectedUnderItsAliases(t *testing.T) {
	h := newTestHome(t)
	for _, p := range Profiles() {
		for _, v := range p.KeyEnvNames() {
			t.Setenv(v, "")
		}
	}
	registerTestProfile(t, tokenProfile("ambientco", true))
	RegisterProvider(tokenProfile("keyedco", false))
	ctx := context.Background()
	if row := registryLookup(ctx, "ambientco-alias"); row == nil || row.Name != "ambientco" {
		t.Fatalf("alias row: %+v", row)
	}
	t.Setenv("AMBIENTCO_TOKEN", "tok-ambient-1234567890")
	r := authResolver(testEnv(t, h))
	if got := r.envKeyAutoDetected(""); got != "" {
		t.Fatalf("an ambient plugin is never auto detected: %q", got)
	}
	t.Setenv("KEYEDCO_TOKEN", "tok-keyed-1234567890")
	if got := authResolver(testEnv(t, h)).envKeyAutoDetected(""); got != "keyedco" {
		t.Fatalf("keyed plugin detected: %q", got)
	}
}

func TestProfileDeclaresItsRegistryRowShapeAndPlace(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	registerTestProfile(t, Profile{Name: "acme", DisplayName: "Acme Cloud", Aliases: []string{"acme-alias"}, KeyEnv: []string{"ACME_TOKEN", "ACME_KEY"},
		BaseURL: "https://acme.example/v1", AuthType: "acme_flow", RegistryAuthType: "api_key", BaseURLEnv: "ACME_ENDPOINT", RegistryAfter: "lmstudio"})
	ctx := context.Background()
	row := registryLookup(ctx, "acme")
	if row == nil || row.Name != "acme" || row.Label() != "Acme Cloud" || row.AuthType != AuthTypeAPIKey || row.BaseURL != "https://acme.example/v1" {
		t.Fatalf("row shape: %+v", row)
	}
	if strings.Join(row.KeyEnv, ",") != "ACME_TOKEN,ACME_KEY" || row.BaseURLEnv != "ACME_ENDPOINT" {
		t.Fatalf("row env: %v %q", row.KeyEnv, row.BaseURLEnv)
	}
	var ids []string
	for _, r := range canonicalRegistryRows(ctx) {
		ids = append(ids, r.Name)
	}
	for i, id := range ids {
		if id == "lmstudio" && (i+1 >= len(ids) || ids[i+1] != "acme") {
			t.Fatalf("acme not right after lmstudio: %v", ids)
		}
	}
	if !containsString(ids, "lmstudio") {
		t.Fatalf("lmstudio row missing: %v", ids)
	}
}

func TestCanonicalRegistryRowsExportMatchesTheUnexportedRows(t *testing.T) {
	ctx := context.Background()
	got, want := CanonicalRegistryRows(ctx), canonicalRegistryRows(ctx)
	if len(got) == 0 || len(got) != len(want) {
		t.Fatalf("rows %d vs %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d differs: %s vs %s", i, got[i].Name, want[i].Name)
		}
	}
}
