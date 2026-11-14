package browser

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Temp ICEBATH_HOME with an in memory keychain, the hermes fixture of the vault CLI tests
func vaultCLIHome(t *testing.T) (string, *vault.MemoryKeyring) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	keyring, restore := vault.UseMemoryKeyring()
	t.Cleanup(restore)
	return h, keyring
}

func runVault(t *testing.T, h string, ns map[string]any) string {
	t.Helper()
	var out bytes.Buffer
	code, err := VaultCommand(context.Background(), h, ns, &out)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v output %s", code, err, out.String())
	}
	return out.String()
}

func keyStorageSetting(t *testing.T, h string) string {
	t.Helper()
	cfg, err := config.Load(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	section, _ := cfg["vault"].(map[string]any)
	v, _ := section["key_storage"].(string)
	return v
}

func TestVaultKeyMigrateMovesTheKeyAndRecordsTheChoice(t *testing.T) {
	h, keyring := vaultCLIHome(t)
	store := vault.GetVaultStore(context.Background())
	item, err := store.AddItem("login", "Example", pyjson.ObjectOf("identifier_type", "email", "identifier", "jane@example.com", "password", "dummy-pw-cli-9"), "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	runVault(t, h, map[string]any{"vault_action": "key", "vault_key_action": "migrate", "to": "keychain"})
	if _, err := os.Stat(filepath.Join(h, "vault", "vault.key")); err == nil || keyring.Count() != 1 {
		t.Fatalf("key file present %v entries %d", err == nil, keyring.Count())
	}
	if keyStorageSetting(t, h) != "keychain" {
		t.Fatalf("key_storage %q", keyStorageSetting(t, h))
	}
	if out := runVault(t, h, map[string]any{"vault_action": "key"}); !strings.Contains(out, "OS keychain") {
		t.Fatalf("output %q", out)
	}
	runVault(t, h, map[string]any{"vault_action": "key", "vault_key_action": "migrate", "to": "file"})
	if _, err := os.Stat(filepath.Join(h, "vault", "vault.key")); err != nil || keyring.Count() != 0 {
		t.Fatalf("key file missing %v entries %d", err, keyring.Count())
	}
	if keyStorageSetting(t, h) != "file" {
		t.Fatalf("key_storage %q", keyStorageSetting(t, h))
	}
	secret, err := vault.GetVaultStore(context.Background()).ResolveSecret(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := secret.Get("password"); pw != "dummy-pw-cli-9" {
		t.Fatalf("password %v", pw)
	}
}

func TestMigratingAnEmptyVaultSaysNoKeyExistsYet(t *testing.T) {
	for _, target := range []string{"keychain", "file"} {
		t.Run(target, func(t *testing.T) {
			h, keyring := vaultCLIHome(t)
			out := runVault(t, h, map[string]any{"vault_action": "key", "vault_key_action": "migrate", "to": target})
			if !strings.Contains(out, "No vault key exists yet") || strings.Contains(out, "decrypted") || strings.Contains(out, "moved") {
				t.Fatalf("output %q", out)
			}
			if _, err := os.Stat(filepath.Join(h, "vault", "vault.key")); err == nil || keyring.Count() != 0 {
				t.Fatal("something was moved or verified")
			}
		})
	}
}

func TestSourcesDisableToleratesScalarVaultSection(t *testing.T) {
	h, _ := vaultCLIHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("vault: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runVault(t, h, map[string]any{"vault_action": "sources", "enable": nil, "disable": "bitwarden"})
	loaded, err := config.LoadYAMLFileReadonly(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	section := loaded.(map[string]any)["vault"].(map[string]any)["bitwarden"].(map[string]any)
	if section["enabled"] != false {
		t.Fatalf("config %v", loaded)
	}
}

func TestVaultCommandSurfacesCorruptStoreAsCleanError(t *testing.T) {
	h, _ := vaultCLIHome(t)
	store := vault.NewVaultStore(context.Background(), filepath.Join(h, "vault"))
	if _, err := store.AddItem("login", "site", pyjson.ObjectOf("identifier_type", "email", "identifier", "u", "password", "p"), "https://x.example"); err != nil {
		t.Fatal(err)
	}
	if err := store.EncryptVaultBody([]byte("{not json")); err != nil {
		t.Fatal(err)
	}
	out := runVault(t, h, map[string]any{"vault_action": nil})
	if !strings.Contains(out, "Error") || !strings.Contains(out, "corrupted") {
		t.Fatalf("output %q", out)
	}
}

func TestVaultListShowsItemsAndRmRemovesThem(t *testing.T) {
	h, _ := vaultCLIHome(t)
	if out := runVault(t, h, map[string]any{"vault_action": "list"}); !strings.Contains(out, "Vault is empty. Add an item with `icebath vault add`.") {
		t.Fatalf("output %q", out)
	}
	meta, err := vault.GetVaultStore(context.Background()).AddItem("login", "GitHub", pyjson.ObjectOf("identifier_type", "email", "identifier", "me@example.com", "password", "pw"), "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	out := runVault(t, h, map[string]any{"vault_action": "list"})
	for _, want := range []string{"Vault items (1)", "vault_", "Icebath", "login", "GitHub", "Passwords are never shown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from %q", want, out)
		}
	}
	if out := runVault(t, h, map[string]any{"vault_action": "rm", "handle": meta.ID}); !strings.Contains(out, "Removed "+meta.ID) {
		t.Fatalf("output %q", out)
	}
	if out := runVault(t, h, map[string]any{"vault_action": "rm", "handle": "vault_nope"}); !strings.Contains(out, "No vault item with handle 'vault_nope'") {
		t.Fatalf("output %q", out)
	}
}

func TestVaultAddStoresALoginThroughThePrompts(t *testing.T) {
	h, _ := vaultCLIHome(t)
	lines := []string{"", "GitHub work account", "https://github.com", "", "jane@example.com"}
	secrets := []string{"", "s3cret", ""}
	prevLine, prevSecret := vaultReadLine, vaultReadSecret
	t.Cleanup(func() { vaultReadLine, vaultReadSecret = prevLine, prevSecret })
	var prompts []string
	vaultReadLine = func(prompt string) (string, error) {
		prompts = append(prompts, prompt)
		next := lines[0]
		lines = lines[1:]
		return next, nil
	}
	vaultReadSecret = func(prompt string) (string, error) {
		prompts = append(prompts, prompt)
		next := secrets[0]
		secrets = secrets[1:]
		return next, nil
	}
	out := runVault(t, h, map[string]any{"vault_action": "add", "kind": nil})
	if !strings.Contains(out, "Stored.") || !strings.Contains(out, "kind=login origin=https://github.com") {
		t.Fatalf("output %q", out)
	}
	items, err := vault.GetVaultStore(context.Background()).ListItems()
	if err != nil || len(items) != 1 || items[0].Label != "GitHub work account" || items[0].Identifier != "jane@example.com" {
		t.Fatalf("items %+v err %v", items, err)
	}
	if prompts[0] != "Kind (login/payment/address) [login]: " || prompts[3] != "Identifier type (email/phone/username) [email]: " || prompts[4] != "Email: " {
		t.Fatalf("prompts %q", prompts)
	}
}
