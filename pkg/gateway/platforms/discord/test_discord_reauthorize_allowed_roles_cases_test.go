package discord

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Home with the given config.yaml
func reauthorizeHome(t *testing.T, yaml string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Adapter whose guild 222222 member 42 holds roleID, the member served by the fake API
func reauthorizeAdapter(t *testing.T, roleID string) *DiscordAdapter {
	a := newTestAdapter(t, nil)
	s := attachClient(t, a, "999", []string{"222222"})
	f := newFakeDiscordAPI(t, s)
	f.get = map[string]string{"/guilds/222222/members/42": `{"user":{"id":"42","username":"u"},"roles":["` + roleID + `"]}`}
	return a
}

func reauthorize(t *testing.T, a *DiscordAdapter) any {
	t.Helper()
	got, err := a.ReauthorizeNativeRoles(t.Context(), &gateway.SessionSource{ChatType: "group", ScopeID: strPtr("222222"), UserID: strPtr("42")})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAllowedRolesSettingDecidesReauthorization(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool
	}{{"2", true}, {"3", false}} {
		t.Run(tc.role, func(t *testing.T) {
			reauthorizeHome(t, "discord:\n  allowed_roles: ['2']\nplatforms:\n  discord:\n    enabled: true\n")
			if reauthorize(t, reauthorizeAdapter(t, tc.role)) != tc.want {
				t.Fatalf("role %s", tc.role)
			}
		})
	}
}

func TestReauthorizationFollowsARoleRemovedFromTheSetting(t *testing.T) {
	reauthorizeHome(t, "discord:\n  allowed_roles: ['2']\nplatforms:\n  discord:\n    enabled: true\n")
	a := reauthorizeAdapter(t, "2")
	if reauthorize(t, a) != true {
		t.Fatal("listed role must authorize")
	}
	setDiscord(t, "allowed_roles", "9")
	if reauthorize(t, a) != false {
		t.Fatal("a role taken off the list must stop authorizing")
	}
}
