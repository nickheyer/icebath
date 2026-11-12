package tuiserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Fresh profile home for the vault handlers with only the named manager CLIs reachable on PATH
func vaultMethodsHome(t *testing.T, installed ...string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	bin := t.TempDir()
	for _, name := range installed {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return h
}

// Raw envelope of one vault handler
func vaultMethodsEnvelope(id int, method string, params map[string]any) map[string]any {
	return pyjson.Plain(handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})).(map[string]any)
}

// Result of one vault handler, failing the case on an error envelope
func vaultMethodsResult(t *testing.T, id int, method string, params map[string]any) map[string]any {
	t.Helper()
	envelope := vaultMethodsEnvelope(id, method, params)
	if _, failed := envelope["error"]; failed {
		t.Fatalf("%s answered %v", method, envelope)
	}
	return envelope["result"].(map[string]any)
}

// Error member of one vault handler, failing the case on a result
func vaultMethodsError(t *testing.T, id int, method string, params map[string]any) map[string]any {
	t.Helper()
	envelope := vaultMethodsEnvelope(id, method, params)
	failure, failed := envelope["error"].(map[string]any)
	if !failed {
		t.Fatalf("%s answered %v", method, envelope)
	}
	return failure
}

// Login item params of the add cases, a fresh copy per call
func vaultLoginParams() map[string]any {
	return map[string]any{"kind": "login", "label": "Example login", "origin": "https://example.com",
		"secret": map[string]any{"identifier_type": "email", "identifier": "user@example.com", "password": "s3cret-pw-9000"}}
}

// Dumped JSON text of a value
func vaultMethodsDump(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Rows of vault.sources by source name
func vaultSourcesRows(t *testing.T) map[string]map[string]any {
	t.Helper()
	rows := map[string]map[string]any{}
	for _, row := range vaultMethodsResult(t, 90, "vault.sources", map[string]any{})["sources"].([]any) {
		r := row.(map[string]any)
		rows[r["name"].(string)] = r
	}
	return rows
}

func TestAddThenListIsPasswordFree(t *testing.T) {
	vaultMethodsHome(t)
	out := vaultMethodsResult(t, 1, "vault.add", vaultLoginParams())
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "vault_") {
		t.Fatalf("id %v", out["id"])
	}
	if strings.Contains(vaultMethodsDump(t, out), "s3cret-pw-9000") {
		t.Fatal("add echoed the secret")
	}
	listed := vaultMethodsResult(t, 2, "vault.list", map[string]any{})
	items := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items %v", items)
	}
	item := items[0].(map[string]any)
	if item["id"] != out["id"] || item["kind"] != "login" || item["label"] != "Example login" || item["origin"] != "https://example.com" {
		t.Fatalf("item %v", item)
	}
	if !truthy(item["created_at"]) {
		t.Fatalf("created_at %v", item["created_at"])
	}
	if item["identifier"] != "user@example.com" || item["identifier_type"] != "email" {
		t.Fatalf("identifier %v", item)
	}
	dumped := vaultMethodsDump(t, listed)
	if strings.Contains(dumped, "s3cret-pw-9000") || strings.Contains(dumped, "password") {
		t.Fatalf("list leaked %s", dumped)
	}
}

func TestAddValidationErrorsAreClean(t *testing.T) {
	vaultMethodsHome(t)
	failure := vaultMethodsError(t, 1, "vault.add", map[string]any{"kind": "login", "label": "no origin",
		"secret": map[string]any{"identifier_type": "email", "identifier": "user@example.com", "password": "s3cret-pw-9000"}})
	if !pyjson.Equal(failure["code"], 5095) || strings.Contains(vaultMethodsDump(t, failure), "s3cret-pw-9000") {
		t.Fatalf("no origin %v", failure)
	}
	if failure = vaultMethodsError(t, 2, "vault.add", map[string]any{"kind": "login", "label": "x"}); !pyjson.Equal(failure["code"], 5095) {
		t.Fatalf("no secret %v", failure)
	}
	failure = vaultMethodsError(t, 3, "vault.add", map[string]any{"kind": "wat", "label": "x", "secret": map[string]any{"password": "s3cret-pw-9000"}})
	if !pyjson.Equal(failure["code"], 5095) || strings.Contains(vaultMethodsDump(t, failure), "s3cret-pw-9000") {
		t.Fatalf("unknown kind %v", failure)
	}
}

func TestEnablingADetectedManagerReportsItEnabled(t *testing.T) {
	vaultMethodsHome(t, "bw")
	vaultMethodsResult(t, 80, "vault.source.set", map[string]any{"name": "bitwarden", "enabled": true})
	rows := vaultSourcesRows(t)
	if rows["bitwarden"]["installed"] != true || rows["bitwarden"]["enabled"] != true {
		t.Fatalf("bitwarden %v", rows["bitwarden"])
	}
}

func TestDisablingAManagerPersistsTheOptOut(t *testing.T) {
	vaultMethodsHome(t, "bw")
	vaultMethodsResult(t, 81, "vault.source.set", map[string]any{"name": "bitwarden", "enabled": false})
	rows := vaultSourcesRows(t)
	if rows["bitwarden"]["installed"] != true || rows["bitwarden"]["enabled"] != false {
		t.Fatalf("bitwarden %v", rows["bitwarden"])
	}
}

func TestUndetectedManagerStaysOff(t *testing.T) {
	vaultMethodsHome(t)
	if got := vaultSourcesRows(t)["bitwarden"]["installed"]; got != false {
		t.Fatalf("installed %v", got)
	}
}

func TestSourceSetToleratesScalarVaultSection(t *testing.T) {
	h := vaultMethodsHome(t, "bw")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("vault: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vaultMethodsResult(t, 82, "vault.source.set", map[string]any{"name": "bitwarden", "enabled": false})
	if got := vaultSourcesRows(t)["bitwarden"]["enabled"]; got != false {
		t.Fatalf("enabled %v", got)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	vaultMethodsHome(t)
	itemID := vaultMethodsResult(t, 1, "vault.add", vaultLoginParams())["id"]
	if got := vaultMethodsResult(t, 2, "vault.remove", map[string]any{"id": itemID})["removed"]; got != true {
		t.Fatalf("first remove %v", got)
	}
	if got := vaultMethodsResult(t, 3, "vault.remove", map[string]any{"id": itemID})["removed"]; got != false {
		t.Fatalf("second remove %v", got)
	}
	if got := vaultMethodsResult(t, 4, "vault.list", map[string]any{})["items"]; !pyjson.Equal(got, []any{}) {
		t.Fatalf("items %v", got)
	}
}

func TestRemoveRequiresID(t *testing.T) {
	vaultMethodsHome(t)
	if failure := vaultMethodsError(t, 1, "vault.remove", map[string]any{}); !pyjson.Equal(failure["code"], 5095) {
		t.Fatalf("remove without id %v", failure)
	}
}

func TestLaunchProfileVaultRpcsStayScopedOnceTheProcessMultiplexes(t *testing.T) {
	vaultMethodsHome(t, "op")
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	if got := vaultSourcesRows(t)["onepassword"]["enabled"]; got != true {
		t.Fatalf("onepassword enabled %v", got)
	}
}
