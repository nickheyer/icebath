package provider

import "testing"

func TestConnectorTokenPathOptOutHidesTheFreeTierFromConnectorsIncludingCachedTokens(t *testing.T) {
	_, env := installAnonPortal(t)
	if _, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if token, err := ReadNousAccessToken(env); err != nil || token == "" {
		t.Fatalf("%q %v", token, err)
	}
	anonWriteNousConfig(t, env, "  guest: false\n")
	if peek := PeekNousAccessToken(env); peek != "" {
		t.Fatal(peek)
	}
	if token, err := ReadNousAccessToken(env); err != nil || token != "" {
		t.Fatalf("%q %v", token, err)
	}
}

func TestConnectorTokenPathReplacesADeadCredentialOnce(t *testing.T) {
	portal, env := installAnonPortal(t)
	first, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	stale := anonJWT(map[string]any{"exp": 1})
	anonPatchNous(t, env, map[string]any{"expires_at": "2000-01-01T00:00:00+00:00", "access_token": stale})
	portal.set(func(p *anonFakePortal) { p.deadTokens[anyString(first["anon_token"])] = true })
	token, err := ReadNousAccessToken(env)
	if err != nil || token == "" || token == stale {
		t.Fatalf("%q %v", token, err)
	}
	nous, _ := anonProviders(t, env)["nous"].(map[string]any)
	if nous["anon_token"] == first["anon_token"] || portal.minted != 2 {
		t.Fatalf("%v minted %d", nous, portal.minted)
	}
}
