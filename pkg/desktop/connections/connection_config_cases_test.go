package connections

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("ICEBATH_FAKE_SSH"); mode != "" {
		os.Exit(fakeSSH(mode, os.Args[1:]))
	}
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func cfg(profiles map[string]any) map[string]any { return map[string]any{"profiles": profiles} }

func TestConnectionScopeKeyTrimsToANameOrNullForTheGlobalScope(t *testing.T) {
	eq(t, ConnectionScopeKey("  coder "), "coder")
	eq(t, ConnectionScopeKey(""), "")
	eq(t, ConnectionScopeKey(nil), "")
}

func TestResolveRemoteSshDashboardProfileNeverSendsAConnPoolKeyToTheRemote(t *testing.T) {
	eq(t, ResolveRemoteSSHDashboardProfile("", "conn:mac-mini::default"), "")
	eq(t, ResolveRemoteSSHDashboardProfile(nil, "conn:spark::default"), "")
	eq(t, ResolveRemoteSSHDashboardProfile("", "conn:mac-mini::dixie"), "dixie")
	eq(t, ResolveRemoteSSHDashboardProfile("", "bob"), "bob")
	eq(t, ResolveRemoteSSHDashboardProfile("", "default"), "")
	eq(t, ResolveRemoteSSHDashboardProfile("writer", "conn:mac-mini::default"), "writer")
	eq(t, ResolveRemoteSSHDashboardProfile("default", "mac-mini"), "")
}

func TestNormAuthModeCoercesToTokenUnlessExplicitlyOauth(t *testing.T) {
	eq(t, NormAuthMode("oauth"), "oauth")
	eq(t, NormAuthMode("token"), "token")
	eq(t, NormAuthMode(nil), "token")
	eq(t, NormAuthMode("weird"), "token")
}

func TestNormalizeRemoteHeadersKeepsSafeProxyHeadersAndDropsTransportAuthHeaders(t *testing.T) {
	plain := func(v string) map[string]any { return map[string]any{"encoding": "plain", "value": v} }
	eq(t, NormalizeRemoteHeaders(map[string]any{
		" CF-Access-Client-Id ":   plain("id"),
		"CF-Access-Client-Secret": "secret",
		"Authorization":           plain("bearer"),
		"Cookie":                  plain("a=b"),
		"Host":                    plain("example.com"),
		"X-Hermes-Session-Token":  plain("token"),
		"X-Icebath-Session-Token": plain("token"),
		"Bad Header":              plain("bad"),
		"Empty":                   plain(""),
	}), map[string]SecretEnvelope{
		"CF-Access-Client-Id":     {Encoding: "plain", Value: "id"},
		"CF-Access-Client-Secret": {Encoding: "plain", Value: "secret"},
	})
}

func TestSanitizeRemoteHeaderValueStripsCRLFSoAPastedTokenCannotSplitARequest(t *testing.T) {
	eq(t, SanitizeRemoteHeaderValue("client-secret\r\n"), "client-secret")
	eq(t, SanitizeRemoteHeaderValue("  client-secret\r  "), "client-secret")
	eq(t, SanitizeRemoteHeaderValue("a\r\nX-Injected: evil"), "aX-Injected: evil")
	eq(t, SanitizeRemoteHeaderValue(nil), "")
}

func TestNormalizeRemoteHeadersSanitizesPlaintextValuesAtIngest(t *testing.T) {
	eq(t, NormalizeRemoteHeaders(map[string]any{"CF-Access-Client-Secret": "secret\r\nX-Injected: evil"}),
		map[string]SecretEnvelope{"CF-Access-Client-Secret": {Encoding: "plain", Value: "secretX-Injected: evil"}})
}

func TestRemoteRequestMatchesBaseUrlTreatsHTTPSAndWSSAsTheSameGatewayOrigin(t *testing.T) {
	eq(t, RemoteRequestMatchesBaseURL("wss://hermes.example.com/gateway/api/ws?ticket=abc", "https://hermes.example.com/gateway"), true)
	eq(t, RemoteRequestMatchesBaseURL("ws://hermes.example.com/api/ws", "http://hermes.example.com"), true)
	eq(t, RemoteRequestMatchesBaseURL("wss://hermes.example.com/other/api/ws", "https://hermes.example.com/gateway"), false)
	eq(t, RemoteRequestMatchesBaseURL("wss://other.example.com/gateway/api/ws", "https://hermes.example.com/gateway"), false)
}

func TestModeIsRemoteLikeIsTrueForRemoteAndCloudFalseOtherwise(t *testing.T) {
	eq(t, ModeIsRemoteLike("remote"), true)
	eq(t, ModeIsRemoteLike("cloud"), true)
	eq(t, ModeIsRemoteLike("local"), false)
	eq(t, ModeIsRemoteLike(nil), false)
	eq(t, ModeIsRemoteLike("weird"), false)
}

func TestProfileRemoteOverrideReturnsNullWhenNoProfileIsGiven(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "remote", "url": "https://x"}})
	eq(t, ProfileRemoteOverride(c, ""), (*RemoteOverride)(nil))
	eq(t, ProfileRemoteOverride(c, nil), (*RemoteOverride)(nil))
}

func TestProfileRemoteOverrideReturnsNullWhenTheProfileHasNoEntry(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "remote", "url": "https://x"}})
	eq(t, ProfileRemoteOverride(c, "writer"), (*RemoteOverride)(nil))
}

func TestProfileRemoteOverrideIgnoresLocalOrUrlLessProfileEntries(t *testing.T) {
	eq(t, ProfileRemoteOverride(cfg(map[string]any{"p": map[string]any{"mode": "local", "url": "https://x"}}), "p"), (*RemoteOverride)(nil))
	eq(t, ProfileRemoteOverride(cfg(map[string]any{"p": map[string]any{"mode": "remote", "url": ""}}), "p"), (*RemoteOverride)(nil))
	eq(t, ProfileRemoteOverride(cfg(map[string]any{"p": map[string]any{"mode": "remote"}}), "p"), (*RemoteOverride)(nil))
}

func TestProfileRemoteOverrideReturnsThePerProfileRemoteWithDefaultedAuthMode(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "remote", "url": "  https://coder.example.com/hermes  ", "token": map[string]any{"value": "sek"}}})
	eq(t, ProfileRemoteOverride(c, "coder"), &RemoteOverride{URL: "https://coder.example.com/hermes", AuthMode: "token", Token: map[string]any{"value": "sek"}})
}

func TestProfileRemoteOverridePreservesAnExplicitOauthAuthMode(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "remote", "url": "https://x", "authMode": "oauth"}})
	eq(t, ProfileRemoteOverride(c, "coder").AuthMode, "oauth")
}

func TestProfileRemoteOverridePreservesNormalizedRemoteHeaders(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "remote", "url": "https://x", "headers": map[string]any{
		"CF-Access-Client-Id": map[string]any{"encoding": "safeStorage", "value": "encrypted-id"},
		"Authorization":       map[string]any{"encoding": "plain", "value": "blocked"},
	}}})
	eq(t, ProfileRemoteOverride(c, "coder"), &RemoteOverride{URL: "https://x", AuthMode: "token",
		Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": {Encoding: "safeStorage", Value: "encrypted-id"}}})
}

func TestProfileRemoteOverrideTreatsACloudEntryAsARemoteOverride(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "cloud", "url": "https://agent-1.agents.nousresearch.com", "authMode": "oauth"}})
	eq(t, ProfileRemoteOverride(c, "coder"), &RemoteOverride{URL: "https://agent-1.agents.nousresearch.com", AuthMode: "oauth"})
}

func TestProfileRemoteOverrideToleratesAMissingObjectProfilesMap(t *testing.T) {
	eq(t, ProfileRemoteOverride(map[string]any{}, "coder"), (*RemoteOverride)(nil))
	eq(t, ProfileRemoteOverride(map[string]any{"profiles": nil}, "coder"), (*RemoteOverride)(nil))
	eq(t, ProfileRemoteOverride(nil, "coder"), (*RemoteOverride)(nil))
}

func TestSSHRemainsSeparateFromURLShapedRemoteModesAndPreservesAnExplicitRemoteProfile(t *testing.T) {
	eq(t, ModeIsRemoteLike("ssh"), false)
	c := cfg(map[string]any{"coder": map[string]any{"mode": "ssh", "host": "alice@box:2222", "keyPath": "/key", "remoteProfile": "default"}})
	eq(t, ProfileRemoteOverride(c, "coder"), (*RemoteOverride)(nil))
	eq(t, ProfileSSHOverride(c, "coder"), &SSHConfig{Mode: "ssh", Host: "box", User: "alice", Port: 2222, KeyPath: "/key", RemoteProfile: "default"})
}

func TestProfileRemoteOverridePreservesAnExplicitRemoteProfileMappingOnAURLOverride(t *testing.T) {
	c := cfg(map[string]any{"gris": map[string]any{"mode": "remote", "url": "https://agent.example.com/hermes", "remoteProfile": "main-gris"}})
	eq(t, ProfileRemoteOverride(c, "gris"), &RemoteOverride{URL: "https://agent.example.com/hermes", AuthMode: "token", RemoteProfile: "main-gris"})
}

func TestProfileRemoteOverrideDropsInvalidOrReservedRemoteProfileMappings(t *testing.T) {
	eq(t, ProfileRemoteOverride(cfg(map[string]any{"p": map[string]any{"mode": "remote", "url": "https://x", "remoteProfile": "bad profile"}}), "p").RemoteProfile, "")
	eq(t, ProfileRemoteOverride(cfg(map[string]any{"p": map[string]any{"mode": "cloud", "url": "https://x", "remoteProfile": "root"}}), "p").RemoteProfile, "")
	eq(t, ProfileRemoteOverride(cfg(map[string]any{"p": map[string]any{"mode": "remote", "url": "https://x", "remoteProfile": ""}}), "p").RemoteProfile, "")
}

func ssh(fields map[string]any) map[string]any {
	return jsSpread(map[string]any{"mode": "ssh"}, fields)
}

func TestNormalizeSshConfigRejectsUnsafeRemoteProfileMappings(t *testing.T) {
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "remoteProfile": "writer_2"})), &SSHConfig{Mode: "ssh", Host: "box", RemoteProfile: "writer_2"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "remoteProfile": "bad profile"})), &SSHConfig{Mode: "ssh", Host: "box"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "remoteProfile": ""})), &SSHConfig{Mode: "ssh", Host: "box"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "remoteProfile": "root"})), &SSHConfig{Mode: "ssh", Host: "box"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "remoteProfile": "default"})), &SSHConfig{Mode: "ssh", Host: "box", RemoteProfile: "default"})
}

func TestNormalizeSshConfigHandlesIPv6AndStrictPortBounds(t *testing.T) {
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "::1", "port": 22.0})), &SSHConfig{Mode: "ssh", Host: "::1"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "[::1]:2222"})), &SSHConfig{Mode: "ssh", Host: "::1", Port: 2222})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "port": "2222junk"})), &SSHConfig{Mode: "ssh", Host: "box"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "box", "port": 65536.0})), &SSHConfig{Mode: "ssh", Host: "box"})
}

func TestNormalizeSshConfigStripsAPastedSshCommandPrefix(t *testing.T) {
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "ssh root@box"})), &SSHConfig{Mode: "ssh", Host: "box", User: "root"})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "SSH root@box:2222"})), &SSHConfig{Mode: "ssh", Host: "box", User: "root", Port: 2222})
	eq(t, NormalizeSSHConfig(ssh(map[string]any{"host": "ssh"})), &SSHConfig{Mode: "ssh", Host: "ssh"})
}

func TestLocalProfileEntryPreservesInactiveSSHDraftsButDropsCloudState(t *testing.T) {
	draft := map[string]any{"mode": "ssh", "host": "box", "user": "alice", "remoteHermesPath": "/hermes"}
	eq(t, LocalProfileEntry(draft), map[string]any{"mode": "local", "savedSsh": draft})
	eq(t, LocalProfileEntry(map[string]any{"mode": "local", "savedSsh": draft}), map[string]any{"mode": "local", "savedSsh": draft})
	eq(t, LocalProfileEntry(map[string]any{"mode": "cloud", "url": "https://agent"}), map[string]any(nil))
}

func TestSavedSSHDraftsAreInactiveAndExplicitOverridesTakePrecedence(t *testing.T) {
	c := cfg(map[string]any{"coder": map[string]any{"mode": "local", "savedSsh": map[string]any{"mode": "ssh", "host": "saved"}}})
	eq(t, SavedProfileSSH(c, "coder"), &SSHConfig{Mode: "ssh", Host: "saved"})
	eq(t, ProfileSSHOverride(c, "coder"), (*SSHConfig)(nil))
	eq(t, ProfileHasRemoteConnection(c, "coder"), false)
	jsObject(c["profiles"])["coder"] = map[string]any{"mode": "ssh", "host": "active"}
	eq(t, ProfileSSHOverride(c, "coder"), &SSHConfig{Mode: "ssh", Host: "active"})
	eq(t, ProfileHasRemoteConnection(c, "coder"), true)
}

type routeCase struct {
	name     string
	profile  string
	opts     ProfileRouteOptions
	expected ProfileBackendRoute
}

var (
	primaryUntagged = ProfileBackendRoute{Backend: "primary"}
	pooled          = ProfileBackendRoute{Backend: "pool"}
)

func primaryTagged(p string) ProfileBackendRoute {
	return ProfileBackendRoute{Backend: "primary", DescriptorProfile: p, ScopePath: true}
}

var routes = []routeCase{
	{"the primary profile owns the window backend", "default", ProfileRouteOptions{PrimaryProfile: "default"}, primaryUntagged},
	{"the primary profile names itself on a scopable local REST request", "nash", ProfileRouteOptions{PrimaryProfile: "nash", RequestMethod: "POST", RequestPath: "/api/model/set"}, primaryTagged("nash")},
	{"the primary profile stays unscoped on a route the server cannot scope", "nash", ProfileRouteOptions{PrimaryProfile: "nash", RequestMethod: "POST", RequestPath: "/api/files/upload"}, primaryUntagged},
	{"a renamed primary profile on a global remote is still scoped on the wire", " coder ", ProfileRouteOptions{PrimaryProfile: "coder", GlobalRemote: true}, primaryTagged("coder")},
	{"an unset profile resolves to the primary", "", ProfileRouteOptions{PrimaryProfile: "default", GlobalRemote: true}, primaryUntagged},
	{"a profile inheriting the app-global remote shares the primary backend, scoped per request", "coder", ProfileRouteOptions{PrimaryProfile: "default", GlobalRemote: true}, primaryTagged("coder")},
	{"a profile with its own remote override gets a pooled descriptor for that host", "coder", ProfileRouteOptions{PrimaryProfile: "default", GlobalRemote: true, ProfileRemoteOverride: true}, pooled},
	{"a mutating local request the server cannot scope keeps its pooled backend", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "POST", RequestPath: "/api/files/upload"}, pooled},
	{"a read on an unscopable route still shares the host backend", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "GET", RequestPath: "/api/files/upload"}, primaryTagged("coder")},
	{"a destructive local request the server can scope shares the host backend", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "POST", RequestPath: "/api/memory/reset"}, primaryTagged("coder")},
	{"a remote sub-profile without a local entry routes through the primary remote gateway", "pm", ProfileRouteOptions{PrimaryProfile: "default", PrimaryRemoteActive: true}, primaryTagged("pm")},
	{"a sub-profile with its own local entry still pools locally under a remote primary", "pm", ProfileRouteOptions{PrimaryProfile: "default", PrimaryRemoteActive: true, OwnEntry: true}, pooled},
	{"a profile-aware local REST request reuses the primary backend", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "GET", RequestPath: "/api/config"}, primaryTagged("coder")},
	{"a read-only local session request reuses the primary backend", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "GET", RequestPath: "/api/sessions/session-1/messages?limit=20"}, primaryTagged("coder")},
	{"a local session write shares the host backend, scoped by its body", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "PATCH", RequestPath: "/api/sessions/session-1"}, primaryUntagged},
	{"a profile-management request uses the primary without a query scope", "coder", ProfileRouteOptions{PrimaryProfile: "default", RequestMethod: "DELETE", RequestPath: "/api/profiles/worker"}, primaryUntagged},
	{"a stored local profile never reuses a remote primary for an eligible REST route", "coder", ProfileRouteOptions{PrimaryProfile: "default", PrimaryRemoteActive: true, OwnEntry: true, RequestMethod: "GET", RequestPath: "/api/config"}, pooled},
	{"ICEBATH_DESKTOP_ISOLATED_BACKEND keeps a local profile on its own pooled backend", "coder", ProfileRouteOptions{PrimaryProfile: "default", IsolatedBackend: true}, pooled},
}

func TestResolveProfileBackendRouteRoute(t *testing.T) {
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) { eq(t, ResolveProfileBackendRoute(r.profile, r.opts), r.expected) })
	}
}

func TestResolveProfileBackendRouteOnlyTagsADescriptorWhenTheBackendIsShared(t *testing.T) {
	for _, r := range routes {
		resolved := ResolveProfileBackendRoute(r.profile, r.opts)
		eq(t, resolved.DescriptorProfile != "", resolved.ScopePath)
		if resolved.DescriptorProfile != "" && resolved.Backend != "primary" {
			t.Fatalf("%s tagged a pooled descriptor", r.name)
		}
	}
}

func TestApiRequestRegistryConnectionIdExtractsAGenuinelyNonLocalConnectionId(t *testing.T) {
	eq(t, APIRequestRegistryConnectionID(map[string]any{"connectionId": "gw-tailscale", "path": "/api/cron/jobs"}), "gw-tailscale")
	eq(t, APIRequestRegistryConnectionID(map[string]any{"connectionId": "  gw-1  ", "path": "/x"}), "gw-1")
}

func TestApiRequestRegistryConnectionIdPreservesAnExplicitLocalRegistryRoute(t *testing.T) {
	eq(t, APIRequestRegistryConnectionID(map[string]any{"connectionId": "local", "path": "/x"}), "local")
}

func TestApiRequestRegistryConnectionIdResolvesNullForUnscopedLegacyRoutes(t *testing.T) {
	eq(t, APIRequestRegistryConnectionID(map[string]any{"path": "/api/cron/jobs"}), "")
	eq(t, APIRequestRegistryConnectionID(map[string]any{"connectionId": "", "path": "/x"}), "")
	eq(t, APIRequestRegistryConnectionID(map[string]any{"connectionId": nil, "path": "/x"}), "")
	eq(t, APIRequestRegistryConnectionID(nil), "")
}

func TestPathWithProfileScopeScopesSharedRemoteRequestsToTheProfileUnconditionally(t *testing.T) {
	eq(t, PathWithProfileScope("/api/cron/jobs/job-1/runs?limit=20", "research", ""), "/api/cron/jobs/job-1/runs?limit=20&profile=research")
}

func TestPathWithProfileScopeKeepsAnExplicitProfileQueryAndNoOpsOnEmptyProfile(t *testing.T) {
	eq(t, PathWithProfileScope("/api/cron/jobs?profile=all", "research", ""), "/api/cron/jobs?profile=all")
	eq(t, PathWithProfileScope("/api/cron/jobs", "", ""), "/api/cron/jobs")
	eq(t, PathWithProfileScope("/api/cron/jobs", nil, ""), "/api/cron/jobs")
}

func TestPathForRegistryBackendRequestUsesTheResolvedRegistryBackendScope(t *testing.T) {
	eq(t, PathForRegistryBackendRequest("/api/fs/read-data-url?path=%2Fsrv%2Fimage.png", "research", RegistryBackendRequestScope{SharedRemote: true}, ""),
		"/api/fs/read-data-url?path=%2Fsrv%2Fimage.png&profile=research")
	eq(t, PathForRegistryBackendRequest("/api/fs/download?path=%2Fsrv%2Freport.pdf&profile=mara", "mara", RegistryBackendRequestScope{RemoteProfile: "default"}, ""),
		"/api/fs/download?path=%2Fsrv%2Freport.pdf&profile=default")
	eq(t, PathForRegistryBackendRequest("/api/fs/download?path=%2Fsrv%2Freport.pdf", "mara", RegistryBackendRequestScope{RemoteProfile: "default"}, ""),
		"/api/fs/download?path=%2Fsrv%2Freport.pdf")
	eq(t, PathForRegistryBackendRequest("/api/profiles/sessions/sidebar?recents_profile=research&recents_exclude=cron%2Cdesktop", "research", RegistryBackendRequestScope{RemoteProfile: "remote-research"}, ""),
		"/api/profiles/sessions/sidebar?recents_profile=remote-research&recents_exclude=cron%2Cdesktop")
}

func TestAApiProfilesNamePathAlreadyNamesItsProfileNoAmbientScopeIsAppended140791(t *testing.T) {
	for _, b := range []RegistryBackendRequestScope{{Mode: "local"}, {SharedPrimary: true}, {SharedRemote: true}} {
		for _, ambient := range []string{"default", "__all__", "beta"} {
			for _, p := range []string{"/api/profiles/p2/soul", "/api/profiles/p2", "/api/profiles/p2/model", "/api/profiles/projects/soul", "/api/profiles/sessions/soul"} {
				eq(t, PathForRegistryBackendRequest(p, ambient, b, ""), p)
			}
			for _, p := range []string{"/api/profiles/sessions", "/api/profiles/projects/tree", "/api/profiles"} {
				eq(t, PathForRegistryBackendRequest(p, ambient, b, ""), p+"?profile="+ambient)
			}
		}
	}
	eq(t, PathWithProfileScope("/api/profiles/p2/soul", "default", ""), "/api/profiles/p2/soul")
	for _, pm := range [][2]string{{"/api/profiles/active", "PATCH"}, {"/api/profiles/import", "DELETE"}, {"/api/profiles/sessions", "DELETE"}} {
		eq(t, PathForRegistryBackendRequest(pm[0], "beta", RegistryBackendRequestScope{Mode: "local"}, pm[1]), pm[0])
	}
	eq(t, PathForRegistryBackendRequest("/api/profiles/active", "beta", RegistryBackendRequestScope{Mode: "local"}, "GET"), "/api/profiles/active?profile=beta")
}

func TestRegistryModelReadsAndWritesRetainEachProfileOnASharedLocalBackend(t *testing.T) {
	for _, b := range []RegistryBackendRequestScope{{Mode: "local"}, {SharedPrimary: true}} {
		for _, profile := range []string{"research", "default", "research"} {
			for _, p := range []string{"/api/model/info", "/api/model/options", "/api/model/set"} {
				eq(t, PathForRegistryBackendRequest(p, profile, b, ""), p+"?profile="+profile)
			}
		}
	}
}

func TestPathWithGlobalRemoteProfileAppendsProfileInGlobalRemoteMode(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/model/info", "iris", ProfileRouteOptions{GlobalRemote: true}), "/api/model/info?profile=iris")
}

func TestPathWithGlobalRemoteProfileScopesThePrimaryLabelBecauseTheDashboardLaunchHomeMayDiffer(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/model/info", "coder", ProfileRouteOptions{GlobalRemote: true, PrimaryProfile: "coder"}), "/api/model/info?profile=coder")
}

func TestPathWithGlobalRemoteProfilePreservesExistingQueryParams(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/model/options?force=1", "iris", ProfileRouteOptions{GlobalRemote: true}), "/api/model/options?force=1&profile=iris")
}

func TestPathWithGlobalRemoteProfileDoesNotReplaceAnExplicitProfileQuery(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/model/info?profile=default", "iris", ProfileRouteOptions{GlobalRemote: true}), "/api/model/info?profile=default")
}

func TestPathWithGlobalRemoteProfileScopesASharedHostLocalPathAndSkipsPerProfileRemoteOverrides(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/model/info", "iris", ProfileRouteOptions{}), "/api/model/info?profile=iris")
	eq(t, PathWithGlobalRemoteProfile("/api/model/info", "iris", ProfileRouteOptions{GlobalRemote: true, ProfileRemoteOverride: true}), "/api/model/info")
}

func TestPathWithGlobalRemoteProfileTranslatesADesktopSSHAliasInAnExplicitProfileQuery(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/cron/jobs?profile=mara", "mara", ProfileRouteOptions{ProfileRemoteOverride: true, BackendProfile: "default"}), "/api/cron/jobs?profile=default")
}

func TestPathWithGlobalRemoteProfileTranslatesAURLRemoteOverrideAliasViaBackendProfile(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/cron/jobs?profile=gris", "gris", ProfileRouteOptions{ProfileRemoteOverride: true, BackendProfile: "main-gris"}), "/api/cron/jobs?profile=main-gris")
}

func TestPathWithGlobalRemoteProfileKeepsTheLocalLabelWhenAURLOverrideHasNoMapping(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/cron/jobs?profile=gris", "gris", ProfileRouteOptions{ProfileRemoteOverride: true}), "/api/cron/jobs?profile=gris")
}

func TestPathWithGlobalRemoteProfilePreservesCrossProfileSelectorsWhenTranslatingAnSSHAlias(t *testing.T) {
	o := ProfileRouteOptions{ProfileRemoteOverride: true, BackendProfile: "default"}
	eq(t, PathWithGlobalRemoteProfile("/api/cron/jobs?profile=all", "mara", o), "/api/cron/jobs?profile=all")
	eq(t, PathWithGlobalRemoteProfile("/api/cron/jobs?profile=worker", "mara", o), "/api/cron/jobs?profile=worker")
}

func TestTranslateSelfProfileQueryRewritesTheSelfProfileFilterIntoTheBackendNamespace(t *testing.T) {
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs?profile=mara", "mara", "default"), "/api/cron/jobs?profile=default")
	eq(t, TranslateSelfProfileQuery("/api/cron/blueprints/instantiate?profile=mara", "mara", "default"), "/api/cron/blueprints/instantiate?profile=default")
}

func TestTranslateSelfProfileQueryRewritesSidebarRecentsProfileAliasesForManagedSSH(t *testing.T) {
	eq(t, TranslateSelfProfileQuery("/api/profiles/sessions/sidebar?recents_profile=research&recents_limit=20&cron_limit=50&messaging_limit=100", "research", "remote-research"),
		"/api/profiles/sessions/sidebar?recents_profile=remote-research&recents_limit=20&cron_limit=50&messaging_limit=100")
}

func TestTranslateSelfProfileQueryLeavesCrossProfileAndUnfilteredPathsUntouched(t *testing.T) {
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs?profile=all", "mara", "default"), "/api/cron/jobs?profile=all")
	eq(t, TranslateSelfProfileQuery("/api/profiles/sessions/sidebar?recents_profile=all", "mara", "default"), "/api/profiles/sessions/sidebar?recents_profile=all")
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs?profile=worker", "mara", "default"), "/api/cron/jobs?profile=worker")
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs", "mara", "default"), "/api/cron/jobs")
}

func TestTranslateSelfProfileQueryNoOpsWhenAliasAndBackendProfileAgreeOrAreMissing(t *testing.T) {
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs?profile=mara", "mara", "mara"), "/api/cron/jobs?profile=mara")
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs?profile=mara", "mara", ""), "/api/cron/jobs?profile=mara")
	eq(t, TranslateSelfProfileQuery("/api/cron/jobs?profile=mara", "", "default"), "/api/cron/jobs?profile=mara")
}

func TestPathWithGlobalRemoteProfileAppendsTheProfileScopeOnTheSharedHostBackend(t *testing.T) {
	o := func(method, path string) ProfileRouteOptions {
		return ProfileRouteOptions{RequestMethod: method, RequestPath: path}
	}
	eq(t, PathWithGlobalRemoteProfile("/api/config", "iris", o("GET", "/api/config")), "/api/config?profile=iris")
	eq(t, PathWithGlobalRemoteProfile("/api/memory/reset", "iris", o("POST", "/api/memory/reset")), "/api/memory/reset?profile=iris")
	eq(t, PathWithGlobalRemoteProfile("/api/files/upload", "iris", o("POST", "/api/files/upload")), "/api/files/upload")
	eq(t, PathWithGlobalRemoteProfile("/api/profiles/worker", "iris", o("DELETE", "/api/profiles/worker")), "/api/profiles/worker")
}

func TestPathWithGlobalRemoteProfileSkipsEmptyProfilePathSafely(t *testing.T) {
	eq(t, PathWithGlobalRemoteProfile("/api/model/info", "", ProfileRouteOptions{GlobalRemote: true}), "/api/model/info")
	eq(t, PathWithGlobalRemoteProfile("", "iris", ProfileRouteOptions{GlobalRemote: true}), "")
}

func apiRoute(backendProfile, path string) ProfileAPIRequestRoute {
	return ProfileAPIRequestRoute{BackendProfile: backendProfile, RequestPath: path}
}

func TestResolveProfileApiRequestKeepsEligibleLocalRESTOnThePrimaryBackend(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/config?view=desktop", ProfileRouteOptions{RequestMethod: "GET"}), apiRoute("", "/api/config?view=desktop&profile=iris"))
}

func TestResolveProfileApiRequestScopesReadOnlySessionProbesWithoutSpawningAProfileBackend(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/sessions/stored-session?include_compacted=true", ProfileRouteOptions{RequestMethod: "GET"}),
		apiRoute("", "/api/sessions/stored-session?include_compacted=true&profile=iris"))
}

func TestResolveProfileApiRequestScopesDestructiveProfileOwnedRoutesToTheSharedBackend(t *testing.T) {
	for _, mp := range [][2]string{{"POST", "/api/memory/reset"}, {"POST", "/api/curator/run"}, {"PUT", "/api/curator/paused"}, {"POST", "/api/webhooks"}, {"DELETE", "/api/webhooks/alerts"}, {"DELETE", "/api/ops/hooks"}, {"POST", "/api/ops/checkpoints/prune"}} {
		eq(t, ResolveProfileAPIRequest("iris", mp[1], ProfileRouteOptions{RequestMethod: mp[0]}), apiRoute("", mp[1]+"?profile=iris"))
	}
}

func TestResolveProfileApiRequestKeepsAnUnscopableMutatingRouteOnAProcessScopedBackend(t *testing.T) {
	for _, mp := range [][2]string{{"POST", "/api/files/upload"}, {"DELETE", "/api/files/managed"}, {"POST", "/api/not-a-real-route/destroy"}} {
		eq(t, ResolveProfileAPIRequest("iris", mp[1], ProfileRouteOptions{RequestMethod: mp[0]}), apiRoute("iris", mp[1]))
	}
	eq(t, ResolveProfileAPIRequest("iris", "/api/files/managed", ProfileRouteOptions{RequestMethod: "GET"}), apiRoute("", "/api/files/managed?profile=iris"))
}

func TestResolveProfileApiRequestLeavesABodyScopedSessionWriteUnqueriedOnTheSharedBackend(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/sessions/session-1", ProfileRouteOptions{RequestMethod: "PATCH"}), apiRoute("", "/api/sessions/session-1"))
}

func TestResolveProfileApiRequestUsesExactMethodAndPathEligibilityForMixedFamilies(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/skills", ProfileRouteOptions{RequestMethod: "GET"}), apiRoute("", "/api/skills?profile=iris"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/skills", ProfileRouteOptions{RequestMethod: "POST"}), apiRoute("iris", "/api/skills"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/config/defaults", ProfileRouteOptions{RequestMethod: "GET"}), apiRoute("", "/api/config/defaults?profile=iris"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/model/recommended-default?provider=nous", ProfileRouteOptions{RequestMethod: "GET"}),
		apiRoute("", "/api/model/recommended-default?provider=nous&profile=iris"))
}

func TestResolveProfileApiRequestScopesCompleteSafeFamiliesAccordingToTheirContracts(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/tools/toolsets/image_gen/config", ProfileRouteOptions{RequestMethod: "GET"}),
		apiRoute("", "/api/tools/toolsets/image_gen/config?profile=iris"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/profiles/worker", ProfileRouteOptions{RequestMethod: "DELETE"}), apiRoute("", "/api/profiles/worker"))
}

func TestResolveProfileApiRequestKeepsGatewayLifecycleVerbsOnThePrimaryWithTheProfileScope(t *testing.T) {
	for _, verb := range []string{"restart", "start", "stop"} {
		eq(t, ResolveProfileAPIRequest("iris", "/api/gateway/"+verb, ProfileRouteOptions{RequestMethod: "POST"}), apiRoute("", "/api/gateway/"+verb+"?profile=iris"))
	}
	eq(t, ResolveProfileAPIRequest("iris", "/api/actions/gateway-restart/status?lines=200", ProfileRouteOptions{RequestMethod: "GET"}),
		apiRoute("", "/api/actions/gateway-restart/status?lines=200&profile=iris"))
}

func TestResolveProfileApiRequestRoutesActionStatusPollsWithTheActionSpawningRoutes(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/actions/skills-install-ascii-art-dd7bccf1/status?lines=200", ProfileRouteOptions{RequestMethod: "GET"}),
		apiRoute("", "/api/actions/skills-install-ascii-art-dd7bccf1/status?lines=200&profile=iris"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/skills/hub/install", ProfileRouteOptions{RequestMethod: "POST"}), apiRoute("", "/api/skills/hub/install?profile=iris"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/mcp/catalog/install", ProfileRouteOptions{RequestMethod: "POST"}), apiRoute("", "/api/mcp/catalog/install?profile=iris"))
}

func TestResolveProfileApiRequestPreservesRemoteRoutingPrecedence(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/memory/reset", ProfileRouteOptions{GlobalRemote: true, RequestMethod: "POST"}), apiRoute("", "/api/memory/reset?profile=iris"))
	eq(t, ResolveProfileAPIRequest("iris", "/api/config", ProfileRouteOptions{GlobalRemote: true, ProfileRemoteOverride: true, RequestMethod: "GET"}), apiRoute("iris", "/api/config"))
}

func TestResolveProfileApiRequestKeepsAStoredLocalProfileOffARemotePrimary(t *testing.T) {
	eq(t, ResolveProfileAPIRequest("iris", "/api/config", ProfileRouteOptions{PrimaryRemoteActive: true, OwnEntry: true, RequestMethod: "GET"}), apiRoute("iris", "/api/config"))
}

func mustNormalize(t *testing.T, raw string) string {
	t.Helper()
	out, err := NormalizeRemoteBaseURL(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out
}

func normalizeFails(t *testing.T, raw, fragment string) {
	t.Helper()
	if _, err := NormalizeRemoteBaseURL(raw); err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("%q: want error containing %q, got %v", raw, fragment, err)
	}
}

func TestNormalizeRemoteBaseUrlStripsTrailingSlashesHashAndQuery(t *testing.T) {
	eq(t, mustNormalize(t, "https://gw.example.com/"), "https://gw.example.com")
	eq(t, mustNormalize(t, "https://gw.example.com/hermes/"), "https://gw.example.com/hermes")
	eq(t, mustNormalize(t, "https://gw.example.com/hermes?x=1#frag"), "https://gw.example.com/hermes")
}

func TestNormalizeRemoteBaseUrlPreservesAPathPrefix(t *testing.T) {
	eq(t, mustNormalize(t, "https://host/hermes"), "https://host/hermes")
}

func TestNormalizeRemoteBaseUrlRejectsEmptyInput(t *testing.T) {
	normalizeFails(t, "", "required")
	normalizeFails(t, "   ", "required")
}

func TestNormalizeRemoteBaseUrlRejectsNonHttpSProtocols(t *testing.T) {
	normalizeFails(t, "ftp://host", "http:// or https://")
	normalizeFails(t, "file:///etc/passwd", "http:// or https://")
}

func TestNormalizeRemoteBaseUrlRejectsGarbage(t *testing.T) {
	normalizeFails(t, "not a url", "not valid")
}

func TestNormalizeRemoteBaseUrlAutoPrependsHttpForSchemeLessHostPortInput(t *testing.T) {
	eq(t, mustNormalize(t, "100.64.0.1:9119"), "http://100.64.0.1:9119")
	eq(t, mustNormalize(t, "mini.tailnet-1234.ts.net:9119"), "http://mini.tailnet-1234.ts.net:9119")
	eq(t, mustNormalize(t, "localhost:9119"), "http://localhost:9119")
	eq(t, mustNormalize(t, "gw.example.com"), "http://gw.example.com")
	eq(t, mustNormalize(t, "gw.example.com/hermes/"), "http://gw.example.com/hermes")
}

func TestNormalizeRemoteBaseUrlStillRejectsExplicitNonHttpSSchemesAfterSchemeLessHandling(t *testing.T) {
	normalizeFails(t, "ws://host:9119", "http:// or https://")
	normalizeFails(t, "ftp://host:21", "http:// or https://")
}

func wsURL(t *testing.T, f func(string, string) (string, error), base, secret string) string {
	t.Helper()
	out, err := f(base, secret)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildGatewayWsUrlUsesWssForHttpsAndBakesTheToken(t *testing.T) {
	eq(t, wsURL(t, BuildGatewayWsURL, "https://gw.example.com", "tok123"), "wss://gw.example.com/api/ws?token=tok123")
}

func TestBuildGatewayWsUrlUsesWsForHttp(t *testing.T) {
	eq(t, wsURL(t, BuildGatewayWsURL, "http://127.0.0.1:9119", "abc"), "ws://127.0.0.1:9119/api/ws?token=abc")
}

func TestBuildGatewayWsUrlHonorsAPathPrefix(t *testing.T) {
	eq(t, wsURL(t, BuildGatewayWsURL, "https://host/hermes", "t"), "wss://host/hermes/api/ws?token=t")
}

func TestBuildGatewayWsUrlUrlEncodesTheToken(t *testing.T) {
	eq(t, wsURL(t, BuildGatewayWsURL, "https://host", "a/b c+d"), "wss://host/api/ws?token=a%2Fb%20c%2Bd")
}

func TestBuildGatewayWsUrlWithTicketUsesTicketNotToken(t *testing.T) {
	u := wsURL(t, BuildGatewayWsURLWithTicket, "https://gw.example.com/hermes", "tkt-9")
	eq(t, u, "wss://gw.example.com/hermes/api/ws?ticket=tkt-9")
	eq(t, strings.Contains(u, "token="), false)
}

func TestBuildGatewayWsUrlWithTicketUrlEncodesTheTicket(t *testing.T) {
	eq(t, wsURL(t, BuildGatewayWsURLWithTicket, "https://host", "a+b/c"), "wss://host/api/ws?ticket=a%2Bb%2Fc")
}

func TestAuthModeFromStatusReturnsOauthWhenAuthRequiredIsTrue(t *testing.T) {
	eq(t, AuthModeFromStatus(map[string]any{"auth_required": true, "auth_providers": []any{"nous"}}), "oauth")
}

func TestAuthModeFromStatusReturnsTokenWhenAuthRequiredIsFalseMissing(t *testing.T) {
	eq(t, AuthModeFromStatus(map[string]any{"auth_required": false}), "token")
	eq(t, AuthModeFromStatus(map[string]any{}), "token")
	eq(t, AuthModeFromStatus(nil), "token")
}

func TestResolveAuthModeExplicitInputWinsOverExisting(t *testing.T) {
	eq(t, ResolveAuthMode("oauth", "token"), "oauth")
	eq(t, ResolveAuthMode("token", "oauth"), "token")
}

func TestResolveAuthModeFallsBackToExistingWhenInputAbsent(t *testing.T) {
	eq(t, ResolveAuthMode(nil, "oauth"), "oauth")
	eq(t, ResolveAuthMode(nil, "token"), "token")
	eq(t, ResolveAuthMode("", "oauth"), "oauth")
}

func TestResolveAuthModeDefaultsToTokenWhenNothingIsSet(t *testing.T) {
	eq(t, ResolveAuthMode(nil, nil), "token")
}

func TestResolveAuthModeIgnoresUnknownValuesDefaultsToToken(t *testing.T) {
	eq(t, ResolveAuthMode("bogus", "also-bogus"), "token")
}

func ck(name, value string) []Cookie { return []Cookie{{Name: name, Value: value}} }

func TestCookiesHaveSessionDetectsTheBareAccessTokenCookie(t *testing.T) {
	eq(t, CookiesHaveSession(ck("hermes_session_at", "x")), true)
	eq(t, CookiesHaveSession(ck("icebath_session_at", "x")), true)
}

func TestCookiesHaveSessionDetectsTheHostAndSecurePrefixedVariants(t *testing.T) {
	eq(t, CookiesHaveSession(ck("__Host-hermes_session_at", "x")), true)
	eq(t, CookiesHaveSession(ck("__Secure-hermes_session_at", "x")), true)
	eq(t, CookiesHaveSession(ck("__Host-icebath_session_at", "x")), true)
}

func TestCookiesHaveSessionIsFalseForAnEmptyValue(t *testing.T) {
	eq(t, CookiesHaveSession(ck("hermes_session_at", "")), false)
}

func TestCookiesHaveSessionIgnoresUnrelatedCookiesATOnlyByDesign(t *testing.T) {
	eq(t, CookiesHaveSession(ck("hermes_session_rt", "x")), false)
	eq(t, CookiesHaveSession(ck("other", "x")), false)
}

func TestCookiesHaveSessionHandlesNonArrays(t *testing.T) {
	eq(t, CookiesHaveSession(nil), false)
	eq(t, CookiesHaveSession([]Cookie{}), false)
}

func TestAGatewayWebappSignsInWithCookiesNamedForItsGatewayAndTheyCountAsASession(t *testing.T) {
	gateway := "0123456789abcdef"
	eq(t, CookiesHaveSession(ck("hermes_session_at_"+gateway, "x")), true)
	eq(t, CookiesHaveSession(ck("__Host-hermes_session_at_"+gateway, "x")), true)
	eq(t, CookiesHaveSession(ck("hermes_session_rt_"+gateway, "x")), false)
	eq(t, CookiesHaveLiveSession(ck("__Secure-hermes_session_rt_"+gateway, "x")), true)
	eq(t, CookiesHaveSession(ck("hermes_session_at_other", "x")), false)
	eq(t, CookiesHaveLiveSession(ck("hermes_webapp_signin_"+gateway, "x")), false)
}

func TestCookiesHaveLiveSessionIsTrueForALiveAccessTokenCookie(t *testing.T) {
	for _, n := range []string{"hermes_session_at", "__Host-hermes_session_at", "__Secure-hermes_session_at"} {
		eq(t, CookiesHaveLiveSession(ck(n, "x")), true)
	}
}

func TestCookiesHaveLiveSessionIsTrueForAnRTCookieEvenWithNOAccessTokenCookie(t *testing.T) {
	for _, n := range []string{"hermes_session_rt", "__Host-hermes_session_rt", "__Secure-hermes_session_rt", "icebath_session_rt"} {
		eq(t, CookiesHaveLiveSession(ck(n, "x")), true)
	}
}

func TestCookiesHaveLiveSessionIsTrueWhenBothATAndRTArePresent(t *testing.T) {
	eq(t, CookiesHaveLiveSession([]Cookie{{"hermes_session_at", "a"}, {"hermes_session_rt", "r"}}), true)
}

func TestCookiesHaveLiveSessionIsFalseForEmptyValues(t *testing.T) {
	eq(t, CookiesHaveLiveSession(ck("hermes_session_at", "")), false)
	eq(t, CookiesHaveLiveSession(ck("hermes_session_rt", "")), false)
	eq(t, CookiesHaveLiveSession([]Cookie{{"hermes_session_at", ""}, {"hermes_session_rt", ""}}), false)
}

func TestCookiesHaveLiveSessionIsFalseForUnrelatedCookiesAndNonArrays(t *testing.T) {
	eq(t, CookiesHaveLiveSession(ck("other", "x")), false)
	eq(t, CookiesHaveLiveSession(nil), false)
	eq(t, CookiesHaveLiveSession([]Cookie{}), false)
}

func TestTokenPreviewReturnsNullForEmpty(t *testing.T) {
	eq(t, TokenPreview(""), "")
	eq(t, TokenPreview(nil), "")
}

func TestTokenPreviewReturnsSetForShortTokens(t *testing.T) {
	eq(t, TokenPreview("12345678"), "set")
}

func TestTokenPreviewReturnsAMaskedSuffixForLongTokens(t *testing.T) {
	eq(t, TokenPreview("abcdefghijklmnop"), "...klmnop")
}

func TestResolveTestWsUrlTokenModeBuildsATokenURLTheWSProbeCanUse(t *testing.T) {
	u, err := ResolveTestWsURL("https://gw.example.com", "token", "tok123", nil)
	eq(t, err, nil)
	eq(t, u, "wss://gw.example.com/api/ws?token=tok123")
}

func TestResolveTestWsUrlTokenModeNoTokenReturnsNullGenuineSkip(t *testing.T) {
	u, err := ResolveTestWsURL("https://gw.example.com", "token", "", nil)
	eq(t, err, nil)
	eq(t, u, "")
}

func TestResolveTestWsUrlOauthMintOkBuildsATicketURL(t *testing.T) {
	u, err := ResolveTestWsURL("https://gw.example.com", "oauth", "", func(string) (string, error) { return "tkt-9", nil })
	eq(t, err, nil)
	eq(t, u, "wss://gw.example.com/api/ws?ticket=tkt-9")
}

func mintFails(cause error) func(string) (string, error) {
	return func(string) (string, error) { return "", cause }
}

func TestResolveTestWsUrlOauthAuthRejectedRequestsSignInAndDoesNotSkipWSValidation(t *testing.T) {
	cause := &GatewayError{Message: "ticket mint failed", Status: 401}
	_, err := ResolveTestWsURL("https://gw.example.com", "oauth", "", mintFails(cause))
	g := err.(*GatewayError)
	eq(t, strings.Contains(strings.ToLower(g.Message), "websocket ticket"), true)
	eq(t, strings.Contains(strings.ToLower(g.Message), "sign in again"), true)
	eq(t, g.NeedsOauth, true)
	eq(t, g.Cause != nil, true)
}

func TestResolveTestWsUrlOauthStaleAppBearerNamesTheAppTokenNotTheServerOAuthSession(t *testing.T) {
	staleBearer := "stale-app-bearer-do-not-log"
	cause := &GatewayError{Message: `401: {"reason":"invalid_or_expired_session"}`, Status: 401, AppTokenRejected: true}
	_, err := ResolveTestWsURL("https://gw.example.com", "oauth", "", mintFails(cause))
	g := err.(*GatewayError)
	lower := strings.ToLower(g.Message)
	eq(t, strings.Contains(lower, "app token is invalid"), true)
	eq(t, strings.Contains(lower, "saved gateway bearer"), true)
	eq(t, strings.Contains(lower, "oauth session"), false)
	eq(t, strings.Contains(lower, "re-authenticate"), false)
	eq(t, strings.Contains(g.Message, staleBearer), false)
	eq(t, g.NeedsOauth, true)
}

func TestResolveTestWsUrlOauthTransportFailureRemainsARetryableConnectionError(t *testing.T) {
	cause := errors.New("socket timed out")
	_, err := ResolveTestWsURL("https://gw.example.com", "oauth", "", mintFails(cause))
	g := err.(*GatewayError)
	eq(t, strings.Contains(strings.ToLower(g.Message), "could not mint a websocket ticket"), true)
	eq(t, g.NeedsOauth, false)
	eq(t, g.Cause, cause)
}

func TestGatewayTicketFailuresClassifyOnlyExplicitAuthRejectionStatusesAsReauth(t *testing.T) {
	eq(t, IsGatewayAuthRejection(&GatewayError{Status: 401}), true)
	eq(t, IsGatewayAuthRejection(&GatewayError{Status: 403}), true)
	eq(t, IsGatewayAuthRejection(&GatewayError{NeedsOauth: true}), true)
	eq(t, IsGatewayAuthRejection(&GatewayError{Status: 500}), false)
	eq(t, IsGatewayAuthRejection(errors.New("network timeout")), false)
	failure := GatewayTicketFailure(errors.New("network timeout"), "sign in", "retry connection")
	eq(t, failure.Message, "retry connection")
	eq(t, failure.NeedsOauth, false)
}

func TestWithTransientRetriesRetriesTransportBlipsButNotAuthRejections(t *testing.T) {
	var sleeps []int
	transportAttempts := 0
	ticket, err := WithTransientRetries(func() (string, error) {
		transportAttempts++
		if transportAttempts < 3 {
			return "", &GatewayError{Message: "500: unavailable", Status: 500}
		}
		return "tkt-ok", nil
	}, RetryOptions{DelaysMs: []int{10, 10}, Sleep: func(ms int) { sleeps = append(sleeps, ms) }})
	eq(t, err, nil)
	eq(t, ticket, "tkt-ok")
	eq(t, transportAttempts, 3)
	eq(t, sleeps, []int{10, 10})
	authAttempts := 0
	_, err = WithTransientRetries(func() (string, error) {
		authAttempts++
		return "", &GatewayError{Message: "401: rejected", Status: 401}
	}, RetryOptions{DelaysMs: []int{10}, Sleep: func(int) {}})
	status, _ := readStatusCode(err)
	eq(t, status, 401)
	eq(t, authAttempts, 1)
}

func TestGatewayWSURLIPCResultSerializesSuccessAndTheAuthVsTransportMatrix(t *testing.T) {
	eq(t, GatewayWsURLIpcResult(func() (string, error) { return "wss://gateway.example.com/api/ws?ticket=fresh", nil }),
		GatewayWsURLResult{OK: true, WsURL: "wss://gateway.example.com/api/ws?ticket=fresh"})
	for _, status := range []int{401, 403} {
		e := &GatewayError{Message: fmt.Sprintf("%d: rejected", status), Status: status}
		eq(t, GatewayWsURLIpcResult(func() (string, error) { return "", e }), GatewayWsURLResult{Error: e.Message, NeedsOauthLogin: true})
	}
	for _, e := range []error{
		&GatewayError{Message: "500: unavailable", Status: 500},
		errors.New("Timed out connecting to Icebath backend after 8000ms"),
		&GatewayError{Message: "socket reset", Code: "ECONNRESET"},
	} {
		eq(t, GatewayWsURLIpcResult(func() (string, error) { return "", e }), GatewayWsURLResult{Error: e.Error()})
	}
}

func TestResolveTestWsUrlOauthRequiresAMintTicketFunction(t *testing.T) {
	_, err := ResolveTestWsURL("https://gw.example.com", "oauth", "", nil)
	eq(t, strings.Contains(err.Error(), "mintTicket function is required"), true)
}

func TestGatewayTicketFailurePreservesAStructured503StatusCodeAsATransportFailure(t *testing.T) {
	source := &GatewayError{Message: "upstream unavailable", Status: 503}
	wrapped := GatewayTicketFailure(source, "auth message", "transport message")
	eq(t, wrapped.Message, "transport message")
	eq(t, wrapped.Status, 503)
	eq(t, wrapped.NeedsOauth, false)
	eq(t, wrapped.Cause, error(source))
}

func TestGatewayTicketFailureOnlyCopiesAnIntegerStatusCodeNotAMessagePrefix(t *testing.T) {
	wrapped := GatewayTicketFailure(errors.New("503: Service Unavailable"), "auth message", "transport message")
	eq(t, wrapped.Status, 0)
	eq(t, wrapped.NeedsOauth, false)
}

func TestOAuthTicketMint503SurfacesTheCloudDownErrorStartupBoundary(t *testing.T) {
	ticketErr := &backend.HTTPError{StatusCode: 503, Message: "503: upstream unavailable"}
	cloudError := backend.MakeNousCloudBackendDownError("https://ares-3009.agents.nousresearch.com", ticketErr)
	down, ok := cloudError.(*backend.CloudBackendDownError)
	if !ok {
		t.Fatalf("expected Cloud-down classification, got wrapper: %s", GatewayTicketFailure(ticketErr, "auth", "transport").Message)
	}
	eq(t, down.StatusCode, 503)
}

func TestOAuthTicketMint401StaysOnTheReauthPathNeverCloudDown(t *testing.T) {
	ticketErr := &backend.HTTPError{StatusCode: 401, Message: "Unauthorized"}
	eq(t, backend.MakeNousCloudBackendDownError("https://ares-3009.agents.nousresearch.com", ticketErr), nil)
	wrapped := GatewayTicketFailure(ticketErr, "auth message", "transport message")
	eq(t, wrapped.Message, "auth message")
	eq(t, wrapped.NeedsOauth, true)
	eq(t, wrapped.Status, 401)
}

func TestFIX95701AConfirmed401403TicketRejectionIsTaggedIsReauthRequiredSoStartHermesLatchesIt(t *testing.T) {
	for _, status := range []int{401, 403} {
		wrapped := GatewayTicketFailure(&GatewayError{Message: fmt.Sprintf("%d: rejected", status), Status: status}, "auth copy", "transport copy")
		eq(t, wrapped.Message, "auth copy")
		eq(t, wrapped.NeedsOauth, true)
		eq(t, wrapped.IsReauthRequired, true)
		eq(t, wrapped.Status, status)
	}
	eq(t, GatewayTicketFailure(&GatewayError{NeedsOauth: true}, "auth copy", "transport copy").IsReauthRequired, true)
}

func TestFIX95701TransportAndServerFailuresAtTheTicketMintStayRetryableNeverReauth(t *testing.T) {
	for _, source := range []error{
		&GatewayError{Message: "503: unavailable", Status: 503},
		errors.New("Timed out connecting to Icebath backend after 8000ms"),
		fmt.Errorf("read: %w", syscall.ECONNRESET),
	} {
		wrapped := GatewayTicketFailure(source, "auth copy", "transport copy")
		eq(t, wrapped.Message, "transport copy")
		eq(t, wrapped.NeedsOauth, false)
		eq(t, wrapped.IsReauthRequired, false)
	}
}
