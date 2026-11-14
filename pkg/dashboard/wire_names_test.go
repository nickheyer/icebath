package dashboard

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// The icebath session header wins and the hermes header plugins still send is accepted when alone
func TestPresentedSessionHeaderReadsBothNames(t *testing.T) {
	cases := []struct {
		headers []Header
		want    string
	}{
		{[]Header{{"x-icebath-session-token", "new"}}, "new"},
		{[]Header{{"x-hermes-session-token", "old"}}, "old"},
		{[]Header{{"x-hermes-session-token", "old"}, {"x-icebath-session-token", "new"}}, "new"},
		{nil, ""},
	}
	for _, c := range cases {
		list := c.headers
		if got := presentedSessionHeader(Headers{list: &list}); got != c.want {
			t.Errorf("headers %v gave %q, want %q", c.headers, got, c.want)
		}
	}
}

// The served index names every global under icebath and aliases the token plugins read under hermes
func TestDashboardGlobalsUseIcebathNames(t *testing.T) {
	body := string(initialProfileApp(t, "worker_x").get("/").Body)
	for _, want := range []string{"window.__ICEBATH_SESSION_TOKEN__=", "window.__HERMES_SESSION_TOKEN__=window.__ICEBATH_SESSION_TOKEN__;",
		"window.__ICEBATH_BASE_PATH__=", "window.__ICEBATH_AUTH_REQUIRED__=", "window.__ICEBATH_DASHBOARD_PROFILE__="} {
		if !strings.Contains(body, want) {
			t.Errorf("index misses %s", want)
		}
	}
	if strings.Contains(body, "__HERMES_BASE_PATH__") || strings.Contains(body, "__HERMES_AUTH_REQUIRED__") {
		t.Error("index still serves a renamed hermes global")
	}
}

// A store a hermes Desktop wrote serves its keys under icebath names, an icebath key winning a clash
func TestLegacyHermesPreferenceKeysReadUnderIcebathNames(t *testing.T) {
	stored := pyjson.ObjectOf("hermes.desktop.keybinds", "{}", "hermes-desktop-mode-v1", "dark",
		"icebath-desktop-mode-v1", "light", "hermes-onboarding-skipped-v1", "1", "other", "x")
	got := sanitizeUIPreferences(stored)
	want := pyjson.ObjectOf("icebath-desktop-mode-v1", "light", "other", "x", "icebath.desktop.keybinds", "{}", "icebath-onboarding-skipped-v1", "1")
	if !pyjson.Equal(got, want) || strings.Join(got.Keys, ",") != strings.Join(want.Keys, ",") {
		t.Fatalf("sanitized %v, want %v", got.Keys, want.Keys)
	}
	if err := validateUIPreferenceChanges(pyjson.ObjectOf("icebath.desktop.keybinds", "{}", "hermes.desktop.keybinds", "{}")); err != nil {
		t.Fatalf("both prefix families must be writable: %v", err)
	}
}

// Session cookies are written under icebath names and logout clears the hermes names a migrated browser still holds
func TestSessionCookiesWriteIcebathAndClearHermesNames(t *testing.T) {
	set := strings.Join(cookieEndpoint(t, "set", false, ""), "\n")
	if !strings.Contains(set, "icebath_session_at=AT") || !strings.Contains(set, "icebath_session_rt=RT") || strings.Contains(set, "hermes_session_") {
		t.Fatalf("set cookies %s", set)
	}
	cleared := strings.Join(cookieEndpoint(t, "clear", false, ""), "\n")
	for _, name := range []string{"icebath_session_at=", "hermes_session_at=", "icebath_session_rt=", "hermes_session_rt=", "hermes_session_provider="} {
		if !strings.Contains(cleared, name) {
			t.Errorf("logout does not clear %s in %s", name, cleared)
		}
	}
}

// A browser holding only hermes cookies stays signed in and an icebath cookie wins once written
func TestSessionCookiesReadHermesNamesUntilReplaced(t *testing.T) {
	legacy := &HTTPConnection{Scope: &Scope{Headers: []Header{{"cookie", "__Host-hermes_session_at=old; hermes_session_rt=oldrt"}}}}
	if at, rt := readSessionCookies(legacy, ""); at != "old" || rt != "oldrt" {
		t.Fatalf("legacy cookies read %q %q", at, rt)
	}
	both := &HTTPConnection{Scope: &Scope{Headers: []Header{{"cookie", "hermes_session_at=old; icebath_session_at=new"}}}}
	if at, _ := readSessionCookies(both, ""); at != "new" {
		t.Fatalf("icebath cookie must win, read %q", at)
	}
}
