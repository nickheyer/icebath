//go:build linux

package stage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func terminalNousState() map[string]any {
	return map[string]any{"portal_base_url": "https://portal.example.com", "client_id": "hermes-cli-vps",
		"last_auth_error": map[string]any{"provider": "nous", "code": "invalid_grant", "relogin_required": true}}
}

func healthyNousState() map[string]any {
	return map[string]any{"portal_base_url": "https://portal.example.com", "client_id": "hermes-cli-vps",
		"access_token": "live-at", "refresh_token": "live-rt"}
}

func writeAuth(t *testing.T, dir string, providers map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"version": 1, "providers": providers})
	p := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const freshSeed = `{"version": 1, "providers": {"nous": {"portal_base_url": "https://portal.example.com", "client_id": "hermes-cli-vps", "access_token": "FRESH-at", "refresh_token": "FRESH-rt"}}}`

func nousOf(t *testing.T, auth string) map[string]any {
	t.Helper()
	raw, _ := os.ReadFile(auth)
	var store struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	return store.Providers["nous"]
}

func reseed(t *testing.T, auth, seed string) string {
	t.Helper()
	got, err := reseedIfTerminal(auth, seed)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReseedsTerminalEntry(t *testing.T) {
	auth := writeAuth(t, t.TempDir(), map[string]any{"nous": terminalNousState()})
	if got := reseed(t, auth, freshSeed); got != "reseeded" {
		t.Fatalf("result = %s", got)
	}
	nous := nousOf(t, auth)
	if nous["refresh_token"] != "FRESH-rt" {
		t.Fatalf("nous = %v", nous)
	}
	if _, ok := nous["last_auth_error"]; ok {
		t.Fatal("last_auth_error kept")
	}
}

func TestDoesNotClobberHealthyEntry(t *testing.T) {
	auth := writeAuth(t, t.TempDir(), map[string]any{"nous": healthyNousState()})
	if got := reseed(t, auth, freshSeed); got != "not_terminal" {
		t.Fatalf("result = %s", got)
	}
	if nousOf(t, auth)["refresh_token"] != "live-rt" {
		t.Fatal("healthy entry clobbered")
	}
}

func TestMarkerButLiveTokenIsNotTerminal(t *testing.T) {
	state := terminalNousState()
	state["refresh_token"] = "somehow-live"
	auth := writeAuth(t, t.TempDir(), map[string]any{"nous": state})
	if got := reseed(t, auth, freshSeed); got != "not_terminal" {
		t.Fatalf("result = %s", got)
	}
}

func TestTimezoneLessLocalTimestampIsIncomparable(t *testing.T) {
	local := healthyNousState()
	local["obtained_at"] = "2026-07-14T19:00:00"
	auth := writeAuth(t, t.TempDir(), map[string]any{"nous": local})
	seed := `{"providers": {"nous": {"client_id": "hermes-cli-vps", "access_token": "FRESH-at", "refresh_token": "FRESH-rt", "obtained_at": "2026-07-14T19:05:00Z"}}}`
	if got := reseed(t, auth, seed); got != "not_terminal" {
		t.Fatalf("result = %s", got)
	}
}

func TestTerminalEntryMissingMarkerIsNotTerminal(t *testing.T) {
	auth := writeAuth(t, t.TempDir(), map[string]any{"nous": map[string]any{"client_id": "hermes-cli-vps"}})
	if got := reseed(t, auth, freshSeed); got != "not_terminal" {
		t.Fatalf("result = %s", got)
	}
}

func TestStaleTempFromAKilledPriorRunDoesNotBlockReseed(t *testing.T) {
	home := t.TempDir()
	auth := writeAuth(t, home, map[string]any{"nous": terminalNousState()})
	stale := fmt.Sprintf("%s.rebootstrap.%d.tmp", auth, os.Getpid())
	os.WriteFile(stale, []byte("{torn"), 0o600)
	if got := reseed(t, auth, freshSeed); got != "reseeded" {
		t.Fatalf("result = %s", got)
	}
	if nousOf(t, auth)["refresh_token"] != "FRESH-rt" {
		t.Fatal("not reseeded")
	}
	entries, _ := os.ReadDir(home)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"auth.json", filepath.Base(stale)}
	sort.Strings(want)
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("dir = %v, want %v", names, want)
	}
}
