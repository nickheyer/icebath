package discord

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Home with the given config.yaml and no ambient role allowlist
func reauthorizeHome(t *testing.T, yaml string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("DISCORD_ALLOWED_ROLES", "")
	os.Unsetenv("DISCORD_ALLOWED_ROLES")
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

func TestFlatPlatformsAllowedRolesAuthorize(t *testing.T) {
	reauthorizeHome(t, "platforms:\n  discord:\n    enabled: true\n    allowed_roles: ['2']\n")
	if reauthorize(t, reauthorizeAdapter(t, "2")) != true {
		t.Fatal("flat platforms allowed_roles must authorize")
	}
}

func TestPlatformsExtraAllowedRolesAuthorize(t *testing.T) {
	reauthorizeHome(t, "platforms:\n  discord:\n    enabled: true\n    extra:\n      allowed_roles: ['2']\n")
	if reauthorize(t, reauthorizeAdapter(t, "2")) != true {
		t.Fatal("extra allowed_roles must authorize")
	}
}

func TestPlatformsExtraBeatsTopLevelBlockLikeIngress(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool
	}{{"2", true}, {"3", false}} {
		t.Run(tc.role, func(t *testing.T) {
			reauthorizeHome(t, "discord:\n  allowed_roles: ['3']\nplatforms:\n  discord:\n    enabled: true\n    extra:\n      allowed_roles: ['2']\n")
			if reauthorize(t, reauthorizeAdapter(t, tc.role)) != tc.want {
				t.Fatalf("role %s", tc.role)
			}
		})
	}
}

func TestLegacyTopLevelAllowedRolesStillAuthorize(t *testing.T) {
	reauthorizeHome(t, "discord:\n  allowed_roles: ['2']\nplatforms:\n  discord:\n    enabled: true\n")
	if reauthorize(t, reauthorizeAdapter(t, "2")) != true {
		t.Fatal("legacy top level allowed_roles must authorize")
	}
}

func TestScopedEnvAllowlistWinsOverConfig(t *testing.T) {
	reauthorizeHome(t, "platforms:\n  discord:\n    enabled: true\n    allowed_roles: ['2']\n")
	t.Setenv("DISCORD_ALLOWED_ROLES", "9")
	if reauthorize(t, reauthorizeAdapter(t, "2")) != false {
		t.Fatal("scoped env allowlist must win")
	}
}
