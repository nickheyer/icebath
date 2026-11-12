package connections

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

func emptyRegistry() ConnectionRegistry { return NormalizeRegistry(nil) }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func fails(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want error containing %q, got %v", fragment, err)
	}
}

func byID(r ConnectionRegistry, id string) *RegistryConnection { return r.Find(id) }

func firstKind(r ConnectionRegistry, kind string) *RegistryConnection {
	for i := range r.Connections {
		if r.Connections[i].Kind == kind {
			return &r.Connections[i]
		}
	}
	return nil
}

func countKind(r ConnectionRegistry, kind string) int {
	n := 0
	for _, c := range r.Connections {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

func apply(t *testing.T, r ConnectionRegistry, config map[string]any) ConnectionRegistry {
	t.Helper()
	return must(ReconcileAppliedGlobalConnection(r, config))
}

func drift(t *testing.T, r ConnectionRegistry, v1 any) (ConnectionRegistry, bool) {
	t.Helper()
	out, changed, err := ReconcileRegistryDrift(r, v1)
	if err != nil {
		t.Fatal(err)
	}
	return out, changed
}

func obj(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func cloudApply(url string, extra ...any) map[string]any {
	return obj("mode", "cloud", "remote", jsSpread(obj("url", url, "authMode", "oauth"), obj(extra...)))
}

func remoteApply(url string) map[string]any {
	return obj("mode", "remote", "remote", obj("url", url, "authMode", "oauth"))
}

func TestCloudApplyUpgradesOnlyHostLabelsWithoutChangingConnectionIdentity(t *testing.T) {
	url, name := "https://agent.example.com", "Research cloud"
	first := apply(t, emptyRegistry(), cloudApply(url))
	named := apply(t, first, cloudApply(url, "name", name))
	eq(t, named.Primary, first.Primary)
	eq(t, byID(named, named.Primary).Label, name)
	var decoded any
	b, _ := json.Marshal(named)
	json.Unmarshal(b, &decoded)
	restored := NormalizeRegistry(decoded)
	eq(t, byID(restored, named.Primary).Name, name)
	custom := *byID(restored, named.Primary)
	custom.Label = "My device"
	reapplied := apply(t, UpsertConnection(restored, custom), cloudApply(url, "name", "New portal name"))
	eq(t, reapplied.Primary, first.Primary)
	eq(t, byID(reapplied, first.Primary).Label, "My device")
	other := apply(t, reapplied, cloudApply("https://other.example.com"))
	if other.Primary == first.Primary {
		t.Fatal("a different gateway URL kept the old primary")
	}
	eq(t, byID(other, other.Primary).Name, "")
}

func TestCloudNameSurvivesPartialEditsButNeverInheritsAcrossGatewayURLs(t *testing.T) {
	registry := apply(t, emptyRegistry(), cloudApply("https://agent.example.com", "name", "Research cloud"))
	existing := byID(registry, registry.Primary)
	renamed := must(NormalizeConnectionInput(MergeConnectionInput(obj("id", existing.ID, "kind", "cloud", "label", "Mine"), existing), registry))
	eq(t, renamed.Name, "Research cloud")
	retargeted := must(NormalizeConnectionInput(MergeConnectionInput(obj("id", existing.ID, "kind", "cloud", "label", "Mine", "url", "https://other.example.com"), existing), registry))
	eq(t, retargeted.Name, "")
}

func TestLabelKeyIsCaseInsensitiveAndTrimmed(t *testing.T) {
	eq(t, LabelKey("  Homelab "), "homelab")
	eq(t, LabelKey("HOMELAB"), LabelKey("homelab"))
}

func TestLabelSlugKebabCasesAndNeverReturnsEmptyForNonEmptyInput(t *testing.T) {
	eq(t, LabelSlug("Work Laptop"), "work-laptop")
	eq(t, LabelSlug("Spark Box #2"), "spark-box-2")
	eq(t, LabelSlug("!!!"), "connection")
}

func sshPrimaryRegistry(extra ...any) ConnectionRegistry {
	return MigrateV1ToRegistry(obj("mode", "ssh", "remote", jsSpread(obj("mode", "ssh", "host", "build-host", "user", "alice"), obj(extra...)), "profiles", obj()), nil)
}

func TestMatchingPrimaryDefaultSSHRouteReusesTheExistingDescriptorOnce(t *testing.T) {
	registry := sshPrimaryRegistry()
	source := byID(registry, registry.Primary)
	descriptor := obj("mode", "remote", "remoteKind", "ssh", "ssh", obj("effectiveConfigFingerprint", "same-effective-config", "host", "build-host", "keyPath", "~/.ssh/id_ed25519", "remoteProfile", "default", "user", "alice"))
	ensureCalls, fingerprintCalls := 0, 0
	eq(t, source.Kind, "ssh")
	got := must(ReuseMatchingPrimarySSHBackend(ReuseMatchingPrimarySSHBackendOptions{
		ConnectionID:         registry.Primary,
		EffectiveFingerprint: func(RegistryConnection) (string, error) { fingerprintCalls++; return "same-effective-config", nil },
		EnsurePrimary:        func() (map[string]any, error) { ensureCalls++; return descriptor, nil },
		Profile:              "default", Registry: registry, Source: *source,
	}))
	eq(t, got, descriptor)
	eq(t, ensureCalls, 1)
	eq(t, fingerprintCalls, 1)
}

func TestNonDefaultOrNonPrimarySSHRoutesDoNotResolveThePrimaryBackend(t *testing.T) {
	registry := sshPrimaryRegistry()
	source := *byID(registry, registry.Primary)
	ensureCalls := 0
	opts := func(id, profile string) ReuseMatchingPrimarySSHBackendOptions {
		return ReuseMatchingPrimarySSHBackendOptions{ConnectionID: id, Profile: profile, Registry: registry, Source: source,
			EffectiveFingerprint: func(RegistryConnection) (string, error) { return "same", nil },
			EnsurePrimary:        func() (map[string]any, error) { ensureCalls++; return obj("mode", "remote", "remoteKind", "ssh"), nil }}
	}
	eq(t, must(ReuseMatchingPrimarySSHBackend(opts(registry.Primary, "researcher"))), map[string]any(nil))
	eq(t, must(ReuseMatchingPrimarySSHBackend(opts(LocalConnectionID, "default"))), map[string]any(nil))
	eq(t, ensureCalls, 0)
}

func TestPrimarySSHReuseRejectsADescriptorWithDifferentEffectiveDialingConfig(t *testing.T) {
	registry := sshPrimaryRegistry()
	got := must(ReuseMatchingPrimarySSHBackend(ReuseMatchingPrimarySSHBackendOptions{
		ConnectionID: registry.Primary, Profile: "default", Registry: registry, Source: *byID(registry, registry.Primary),
		EffectiveFingerprint: func(RegistryConnection) (string, error) { return "registry-config", nil },
		EnsurePrimary: func() (map[string]any, error) {
			return obj("mode", "remote", "remoteKind", "ssh", "ssh", obj("effectiveConfigFingerprint", "active-config", "host", "other-host", "remoteProfile", "")), nil
		},
	}))
	eq(t, got, map[string]any(nil))
}

func TestPrimarySSHReuseRejectsADescriptorWithADifferentRemoteIcebathPath(t *testing.T) {
	registry := sshPrimaryRegistry("remoteHermesPath", "/srv/hermes")
	got := must(ReuseMatchingPrimarySSHBackend(ReuseMatchingPrimarySSHBackendOptions{
		ConnectionID: registry.Primary, Profile: "default", Registry: registry, Source: *byID(registry, registry.Primary),
		EffectiveFingerprint: func(RegistryConnection) (string, error) { return "same-effective-config", nil },
		EnsurePrimary: func() (map[string]any, error) {
			return obj("mode", "remote", "remoteKind", "ssh", "ssh", obj("effectiveConfigFingerprint", "same-effective-config", "host", "build-host", "remoteHermesPath", "/opt/hermes", "remoteProfile", "", "user", "alice")), nil
		},
	}))
	eq(t, got, map[string]any(nil))
}

func TestRegistryPrimaryReusesAMatchingPrimaryBackendDescriptor(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "hermes-vps", "launchMode", "primary", "lastUsed", "hermes-vps", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "hermes-vps", "kind", "ssh", "label", "Icebath VPS", "host", "hermes-vps"),
	}))
	descriptor := obj("connectionId", "hermes-vps", "mode", "remote", "remoteKind", "ssh", "ssh", obj("host", "hermes-vps"))
	eq(t, RegistrySourceOwnsPrimaryBackend(registry, "hermes-vps", descriptor), true)
	eq(t, RegistrySourceOwnsPrimaryBackend(registry, LocalConnectionID, descriptor), false)
}

func TestResolvedConnectionIdIdentifiesLocalAndMigratedRemoteDescriptors(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "local", "profiles", obj(
		"personal", obj("mode", "remote", "url", "https://personal.example:9443/", "authMode", "token"),
		"work", obj("mode", "ssh", "host", "work-host", "user", "root"),
	)), []string{"personal", "work"})
	personal, work := firstKind(registry, "remote"), firstKind(registry, "ssh")
	eq(t, ResolvedConnectionID(registry, obj("mode", "local")), LocalConnectionID)
	eq(t, ResolvedConnectionID(registry, obj("baseUrl", "https://personal.example:9443", "mode", "remote", "remoteKind", "url")), personal.ID)
	eq(t, ResolvedConnectionID(registry, obj("baseUrl", "http://127.0.0.1:49152", "mode", "remote", "remoteHost", "ROOT@WORK-HOST", "remoteKind", "ssh")), work.ID)
	ambiguous := registry.clone()
	ambiguous.Connections = append(ambiguous.Connections, RegistryConnection{ID: "local-copy", Kind: "local", Label: "Local copy"})
	eq(t, ResolvedConnectionID(ambiguous, obj("mode", "local")), "")
}

func TestResolvedConnectionIdDoesNotGuessAnUnregisteredRemote(t *testing.T) {
	eq(t, ResolvedConnectionID(emptyRegistry(), obj("baseUrl", "https://unknown.example", "mode", "remote", "remoteKind", "url")), "")
}

func TestAgentHandleBareWhenUniqueNameDeviceShapeWhenDuplicated(t *testing.T) {
	eq(t, AgentHandle("research", "Homelab", false), "research")
	eq(t, AgentHandle("research", "Homelab", true), "research-homelab")
	eq(t, AgentHandle("research", "Work Laptop", true), "research-work-laptop")
	eq(t, AgentHandle("", "Homelab", false), "default")
}

func env(v string) SecretEnvelope { return SecretEnvelope{Encoding: "safeStorage", Value: v} }

func envObj(v string) map[string]any { return obj("encoding", "safeStorage", "value", v) }

func reversed(r ConnectionRegistry) ConnectionRegistry {
	out := r.clone()
	for i, j := 0, len(out.Connections)-1; i < j; i, j = i+1, j-1 {
		out.Connections[i], out.Connections[j] = out.Connections[j], out.Connections[i]
	}
	return out
}

func TestResolvedConnectionIdAcceptsOnlyACurrentExactDescriptorIdAndNeverFallsBack(t *testing.T) {
	registry := ConnectionRegistry{Version: RegistryVersion, Primary: "remote-a", LaunchMode: "primary", LastUsed: "remote-a", Connections: []RegistryConnection{
		{ID: "local", Kind: "local", Label: "This device"},
		{ID: "remote-a", Kind: "remote", Label: "Remote A", URL: "https://shared.example", AuthMode: "token", Token: envObj("token-a"), Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("header-a")}},
		{ID: "remote-b", Kind: "remote", Label: "Remote B", URL: "https://shared.example", AuthMode: "oauth", Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("header-b")}},
	}}
	inferable := func(extra ...any) map[string]any {
		return jsSpread(obj("authMode", "token", "baseUrl", "https://shared.example", "headers", obj("CF-Access-Client-Id", envObj("header-a")), "mode", "remote", "remoteKind", "url", "token", envObj("token-a")), obj(extra...))
	}
	eq(t, ResolvedConnectionID(registry, inferable("connectionId", "remote-b")), "remote-b")
	eq(t, ResolvedConnectionID(registry, inferable()), "remote-a")
	for _, id := range []any{"", "   ", nil, 42.0, map[string]any{}, "unknown-source", "retired-source"} {
		eq(t, ResolvedConnectionID(registry, inferable("connectionId", id)), "")
	}
	eq(t, ResolvedConnectionID(reversed(registry), inferable("connectionId", "remote-b")), "remote-b")
	eq(t, ResolvedConnectionID(reversed(registry), inferable("connectionId", "retired-source")), "")
}

func TestResolvedConnectionIdReusesTheExactURLEnvelopeAndRejectsWeakOrDuplicateMatches(t *testing.T) {
	shared := "https://shared.example/gateway"
	registry := ConnectionRegistry{Version: RegistryVersion, Primary: "remote-token", LaunchMode: "primary", LastUsed: "remote-token", Connections: []RegistryConnection{
		{ID: "local", Kind: "local", Label: "This device"},
		{ID: "remote-token", Kind: "remote", Label: "Token remote", URL: shared, AuthMode: "token", Token: envObj("token-a"), Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("header-a")}},
		{ID: "remote-oauth", Kind: "remote", Label: "OAuth remote", URL: shared + "/", AuthMode: "oauth", Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("header-b")}},
		{ID: "cloud-nous", Kind: "cloud", Label: "Nous cloud", URL: shared, AuthMode: "oauth", Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("header-cloud")}, Org: "nous"},
		{ID: "cloud-labs", Kind: "cloud", Label: "Labs cloud", URL: shared, AuthMode: "oauth", Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("header-cloud")}, Org: "labs"},
	}}
	tokenDescriptor := obj("authMode", "token", "baseUrl", shared, "headers", obj("CF-Access-Client-Id", envObj("header-a")), "mode", "remote", "remoteKind", "url", "token", envObj("token-a"))
	eq(t, ResolvedConnectionID(registry, obj("authMode", "token", "baseUrl", shared, "headers", obj("cf-access-client-id", envObj("header-a")), "mode", "remote", "remoteKind", "url", "token", envObj("token-a"))), "remote-token")
	eq(t, ResolvedConnectionID(registry, obj("authMode", "oauth", "baseUrl", shared, "headers", obj("CF-ACCESS-CLIENT-ID", envObj("header-b")), "mode", "remote", "remoteKind", "url")), "remote-oauth")
	eq(t, ResolvedConnectionID(registry, obj("authMode", "oauth", "baseUrl", shared, "headers", obj("CF-Access-Client-Id", envObj("header-cloud")), "mode", "remote", "org", "nous", "remoteKind", "cloud")), "cloud-nous")
	eq(t, ResolvedConnectionID(registry, obj("authMode", "oauth", "baseUrl", shared, "headers", obj("CF-Access-Client-Id", envObj("header-cloud")), "mode", "remote", "org", "labs", "remoteKind", "cloud")), "cloud-labs")
	eq(t, ResolvedConnectionID(registry, obj("baseUrl", shared, "mode", "remote", "remoteKind", "url")), "")
	eq(t, ResolvedConnectionID(registry, obj("baseUrl", shared, "mode", "remote", "remoteKind", "cloud")), "")
	duplicate := registry.clone()
	copyEntry := *byID(registry, "remote-token")
	copyEntry.ID = "remote-token-copy"
	duplicate.Connections = append(duplicate.Connections, copyEntry)
	eq(t, ResolvedConnectionID(duplicate, tokenDescriptor), "")
	eq(t, ResolvedConnectionID(reversed(duplicate), tokenDescriptor), "")
}

func TestResolvedConnectionIdKeepsSameHostSSHRoutesDistinctByPortKeyPathAndProfile(t *testing.T) {
	base := RegistryConnection{Kind: "ssh", Host: "work-host", KeyPath: "/keys/a", RemoteHermesPath: "/srv/hermes", RemoteProfile: "alpha", User: "root"}
	variant := func(id, label string, edit func(*RegistryConnection)) RegistryConnection {
		c := base
		c.ID, c.Label = id, label
		edit(&c)
		return c
	}
	registry := ConnectionRegistry{Version: RegistryVersion, Primary: "ssh-base", LaunchMode: "primary", LastUsed: "ssh-base", Connections: []RegistryConnection{
		{ID: "local", Kind: "local", Label: "This device"},
		variant("ssh-base", "SSH base", func(*RegistryConnection) {}),
		variant("ssh-port", "SSH port", func(c *RegistryConnection) { c.Port = 2222 }),
		variant("ssh-key", "SSH key", func(c *RegistryConnection) { c.KeyPath = "/keys/b" }),
		variant("ssh-path", "SSH path", func(c *RegistryConnection) { c.RemoteHermesPath = "/opt/hermes" }),
		variant("ssh-profile", "SSH profile", func(c *RegistryConnection) { c.RemoteProfile = "beta" }),
	}}
	baseObj := obj("host", "work-host", "keyPath", "/keys/a", "kind", "ssh", "remoteHermesPath", "/srv/hermes", "remoteProfile", "alpha", "user", "root")
	resolve := func(r ConnectionRegistry, extra ...any) string {
		return ResolvedConnectionID(r, obj("mode", "remote", "remoteKind", "ssh", "ssh", jsSpread(baseObj, obj(extra...))))
	}
	eq(t, resolve(registry), "ssh-base")
	eq(t, resolve(registry, "port", 2222.0), "ssh-port")
	eq(t, resolve(registry, "keyPath", "/keys/b"), "ssh-key")
	eq(t, resolve(registry, "remoteHermesPath", "/opt/hermes"), "ssh-path")
	eq(t, resolve(registry, "remoteProfile", "beta"), "ssh-profile")
	eq(t, ResolvedConnectionID(registry, obj("connectionId", "ssh-port", "mode", "remote", "remoteKind", "ssh", "ssh", jsSpread(baseObj, obj("port", 9999.0)))), "ssh-port")
	eq(t, ResolvedConnectionID(registry, obj("mode", "remote", "remoteHost", "ROOT@WORK-HOST", "remoteKind", "ssh")), "")
	eq(t, ResolvedConnectionID(registry, obj("mode", "remote", "remoteHost", "ROOT@WORK-HOST", "remoteKind", "ssh", "ssh", nil)), "")
	duplicate := registry.clone()
	duplicate.Connections = append(duplicate.Connections, variant("ssh-base-copy", "SSH base copy", func(*RegistryConnection) {}))
	eq(t, resolve(duplicate), "")
	eq(t, resolve(reversed(duplicate)), "")
}

func TestConnectionIdForLabelSuffixesOnCollisionAndNeverMintsLocal(t *testing.T) {
	eq(t, ConnectionIDForLabel("Homelab", nil), "homelab")
	eq(t, ConnectionIDForLabel("Homelab", []string{"homelab"}), "homelab-2")
	eq(t, ConnectionIDForLabel("Homelab", []string{"homelab", "homelab-2"}), "homelab-3")
	eq(t, ConnectionIDForLabel("Local", nil), "local-2")
}

func TestConnectionIdForPendingLoginAnExplicitDraftIdWinsTrimmed(t *testing.T) {
	registry := emptyRegistry()
	eq(t, ConnectionIDForPendingLogin("homelab", nil, registry), "homelab")
	eq(t, ConnectionIDForPendingLogin("  homelab  ", nil, registry), "homelab")
}

func TestConnectionIdForPendingLoginMintsTheSaveTimeIdForAnUnpersistedDraft(t *testing.T) {
	registry := emptyRegistry()
	registry = UpsertConnection(registry, must(NormalizeConnectionInput(obj("kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119"), registry)))
	eq(t, ConnectionIDForPendingLogin(nil, "Mac mini", registry), "mac-mini")
	eq(t, ConnectionIDForPendingLogin(nil, "Homelab", registry), "homelab-2")
	saved := must(NormalizeConnectionInput(obj("kind", "remote", "id", ConnectionIDForPendingLogin(nil, "Mac mini", registry), "label", "Mac mini", "url", "http://10.0.0.6:9119"), registry))
	eq(t, saved.ID, "mac-mini")
}

func TestConnectionIdForPendingLoginAnUnnamedDraftStillGetsAStableUniqueId(t *testing.T) {
	registry := UpsertConnection(emptyRegistry(), RegistryConnection{ID: "connection", Kind: "remote", Label: "Taken", URL: "http://10.0.0.7:9119", AuthMode: "oauth"})
	eq(t, ConnectionIDForPendingLogin(nil, "", registry), "connection-2")
	eq(t, ConnectionIDForPendingLogin(nil, nil, registry), "connection-2")
}

func TestConnectionIdForPendingLoginBlankOrNonStringIdsFallBackToMinting(t *testing.T) {
	for _, junk := range []any{"", "   ", 42.0, nil} {
		eq(t, ConnectionIDForPendingLogin(junk, "Homelab", emptyRegistry()), "homelab")
	}
}

func TestUniqueLabelCountsUpNeverX22AndClampsLongCandidates(t *testing.T) {
	eq(t, UniqueLabel("Homelab", nil), "Homelab")
	eq(t, UniqueLabel("Homelab", []string{"Homelab"}), "Homelab 2")
	eq(t, UniqueLabel("Homelab", []string{"Homelab", "Homelab 2"}), "Homelab 3")
	eq(t, UniqueLabel("homelab", []string{"HOMELAB"}), "homelab 2")
	long := strings.Repeat("x", 300)
	eq(t, len(UniqueLabel(long, nil)) <= 64, true)
	eq(t, len(UniqueLabel(long, []string{UniqueLabel(long, nil)})) <= 64, true)
}

func TestBackendScopeKeyElectronAndSharedImplementationsAgreeEverywhere(t *testing.T) {
	for _, c := range [][3]string{{"", "", "default"}, {"local", "research", "research"}, {"homelab", "research", "conn:homelab::research"},
		{"homelab", "", "conn:homelab::default"}, {"  homelab  ", "  research  ", "conn:homelab::research"}, {"spark-2", "default", "conn:spark-2::default"}} {
		eq(t, backend.BackendScopeKey(c[0], c[1]), c[2])
	}
	eq(t, backend.BackendScopePrefix("homelab"), "conn:homelab::")
	eq(t, LocalConnectionID, backend.LocalConnectionID)
}

func TestBackendScopeKeyLocalEmptyConnectionKeepsTheBareProfileKey(t *testing.T) {
	eq(t, backend.BackendScopeKey("", "research"), "research")
	eq(t, backend.BackendScopeKey(LocalConnectionID, "research"), "research")
	eq(t, backend.BackendScopeKey("local", ""), "default")
	eq(t, backend.BackendScopeKey("", ""), "default")
}

func TestBackendScopeKeyNonLocalConnectionsGetAnUnambiguousComposite(t *testing.T) {
	eq(t, backend.BackendScopeKey("homelab", "research"), "conn:homelab::research")
	eq(t, backend.BackendScopeKey("homelab", ""), "conn:homelab::default")
	eq(t, strings.HasPrefix(backend.BackendScopeKey("homelab", "research"), backend.BackendScopePrefix("homelab")), true)
	eq(t, strings.HasPrefix(backend.BackendScopeKey("homelab-2", "research"), backend.BackendScopePrefix("homelab")), false)
	eq(t, strings.HasPrefix("research", backend.BackendScopePrefix("homelab")), false)
}

func boolPtr(b bool) *bool { return &b }

func TestRegistryLocalRouteDelegatesToTheLegacyPathWhenV1IsLocalSingleSourceUsersByteIdentical(t *testing.T) {
	eq(t, ResolveRegistryLocalRoute("research", LocalRouteOptions{}), RegistryLocalRoute{Delegate: true, PoolKey: "research"})
	eq(t, ResolveRegistryLocalRoute("", LocalRouteOptions{}), RegistryLocalRoute{Delegate: true, PoolKey: "default"})
}

func TestRegistryLocalRouteV1REMOTEGlobalModeForcesAGenuinelyLocalBackendMigrationScenario(t *testing.T) {
	route := ResolveRegistryLocalRoute("default", LocalRouteOptions{GlobalRemote: true})
	eq(t, route.Delegate, false)
	eq(t, route.PoolKey, "conn:local::default")
	eq(t, strings.HasPrefix(route.PoolKey, backend.BackendScopePrefix(LocalConnectionID)), true)
	eq(t, route.PoolKey != backend.BackendScopeKey(LocalConnectionID, "default"), true)
}

func TestRegistryLocalRouteAPerProfileRemoteOverrideDelegatesToTheOverride90477(t *testing.T) {
	eq(t, ResolveRegistryLocalRoute("research", LocalRouteOptions{ProfileRemoteOverride: true}), RegistryLocalRoute{Delegate: true, PoolKey: "research"})
}

func TestRegistryLocalRoutePerProfileOverrideWinsWhenGlobalRemoteIsAlsoActive(t *testing.T) {
	eq(t, ResolveRegistryLocalRoute("research", LocalRouteOptions{GlobalRemote: true, ProfileRemoteOverride: true}), RegistryLocalRoute{Delegate: true, PoolKey: "research"})
}

func TestRegistryLocalRouteAConcreteRemoteOnlyProfileIsRefusedOnTheForcedLocalBranch(t *testing.T) {
	named := ResolveRegistryLocalRoute("inbox", LocalRouteOptions{GlobalRemote: true, LocalProfileExists: boolPtr(false)})
	eq(t, strings.Contains(named.Refuse, `Profile "inbox" no longer exists`), true)
	eq(t, named.Delegate, false)
	eq(t, ResolveRegistryLocalRoute("default", LocalRouteOptions{GlobalRemote: true, LocalProfileExists: boolPtr(false)}), RegistryLocalRoute{PoolKey: "conn:local::default"})
	present := ResolveRegistryLocalRoute("research", LocalRouteOptions{GlobalRemote: true, LocalProfileExists: boolPtr(true)})
	eq(t, present, RegistryLocalRoute{PoolKey: "conn:local::research"})
	eq(t, ResolveRegistryLocalRoute("research", LocalRouteOptions{GlobalRemote: true, LocalProfileExists: boolPtr(false), ProfileRemoteOverride: true}), RegistryLocalRoute{Delegate: true, PoolKey: "research"})
	eq(t, ResolveRegistryLocalRoute("", LocalRouteOptions{GlobalRemote: true, LocalProfileExists: boolPtr(false)}).Refuse, "")
}

func TestRegistryDialAnEmptyConnectionIdIsNotRegistryPrimary(t *testing.T) {
	for _, id := range []any{"", "   ", nil} {
		_, err := RegistryDialConnectionID(id)
		fails(t, err, "No connection with id")
	}
	for _, id := range []string{"local", "homelab", "ssh-other-host"} {
		eq(t, must(RegistryDialConnectionID(id)), id)
	}
}

func TestLocalEnumerationDelegateRouteLocalPrimaryDesktopAlwaysEnumerates(t *testing.T) {
	route := ResolveRegistryLocalRoute("default", LocalRouteOptions{})
	eq(t, ShouldDeferLocalEnumeration(route, nil, ""), false)
	eq(t, ShouldDeferLocalEnumeration(route, []string{"conn:local::default"}, ""), false)
}

func TestLocalEnumerationForcedLocalRouteDefersUntilALocalChildExistsRemotePrimaryDesktop(t *testing.T) {
	route := ResolveRegistryLocalRoute("default", LocalRouteOptions{GlobalRemote: true})
	eq(t, ShouldDeferLocalEnumeration(route, nil, ""), true)
	eq(t, ShouldDeferLocalEnumeration(route, []string{"default", "research"}, ""), true)
	eq(t, ShouldDeferLocalEnumeration(route, []string{"conn:local::default"}, ""), false)
}

var (
	rLocal   = RegistryConnection{ID: "local", Kind: "local", Label: "This device"}
	rHomelab = RegistryConnection{ID: "homelab", Kind: "remote", Label: "Homelab", URL: "http://h:1"}
	rSpark   = RegistryConnection{ID: "spark", Kind: "remote", Label: "Spark", URL: "http://spark:8642"}
	rSparkTS = RegistryConnection{ID: "spark-ts", Kind: "remote", Label: "Spark TS", URL: "http://100.1.2.3:8642"}
)

func rosterKeys(roster []RosterAgent, f func(RosterAgent) string) []string {
	out := make([]string, len(roster))
	for i, a := range roster {
		out[i] = f(a)
	}
	return out
}

func sorted(s []string) []string { sort.Strings(s); return s }

func TestRosterUniqueProfilesKeepBareHandlesDuplicatesGetNameDevice(t *testing.T) {
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rLocal, Profiles: []string{"default", "research"}}, {Connection: rHomelab, Profiles: []string{"research", "coder"}}}, "")
	byKey := map[string]string{}
	for _, a := range roster {
		byKey[a.ConnectionID+"/"+a.Profile] = a.Handle
	}
	eq(t, byKey["local/research"], "research-this-device")
	eq(t, byKey["homelab/research"], "research-homelab")
	eq(t, byKey["local/default"], "default")
	eq(t, byKey["homelab/coder"], "coder")
	eq(t, len(roster), 4)
}

func TestRosterSourceProfileMetadataFollowsTheConnectionQualifiedRow(t *testing.T) {
	vps := RegistryConnection{ID: "vps", Kind: "remote", Label: "VPS", URL: "http://vps:8642"}
	meta := RosterProfileMetadata{DisplayName: "Emma", UIMeta: obj("hermes-bots", obj("title", "Emma", "shape", "blobatar::sun", "color", "#8b5cf6")), HasAvatar: boolPtr(true)}
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rLocal, Profiles: []string{"default"}}, {Connection: vps, Profiles: []string{"default"}, ProfileMetadata: map[string]RosterProfileMetadata{"default": meta}}}, "")
	for _, a := range roster {
		if a.ConnectionID == "vps" {
			eq(t, *a.ProfileMetadata, meta)
		} else {
			eq(t, a.ProfileMetadata, (*RosterProfileMetadata)(nil))
		}
	}
}

func TestRememberSshEnumerationLiveListWinsCacheThenSeedDefault(t *testing.T) {
	eq(t, RememberSSHEnumeration(SSHEnumeration{Profiles: []string{"bob", "kai"}}, []string{"stale"}, "ssh"), SSHEnumeration{Profiles: []string{"bob", "kai"}})
	eq(t, RememberSSHEnumeration(SSHEnumeration{Error: "connect-on-demand"}, []string{"bob", "kai", "rook"}, "ssh"), SSHEnumeration{Profiles: []string{"bob", "kai", "rook"}, Error: "connect-on-demand"})
	eq(t, RememberSSHEnumeration(SSHEnumeration{Error: "connect-on-demand"}, nil, "ssh"), SSHEnumeration{Profiles: []string{"default"}, Error: "connect-on-demand"})
	eq(t, RememberSSHEnumeration(SSHEnumeration{Error: "connect-on-demand"}, nil, "remote"), SSHEnumeration{Error: "connect-on-demand"})
}

func TestRememberSshEnumerationABouncedRemoteSourceKeepsItsLastKnownRoster4BotsShowAs2(t *testing.T) {
	eq(t, RememberSSHEnumeration(SSHEnumeration{Error: "unreachable"}, []string{"default", "ceo", "accounter"}, "remote"), SSHEnumeration{Profiles: []string{"default", "ceo", "accounter"}, Error: "unreachable"})
	eq(t, RememberSSHEnumeration(SSHEnumeration{Error: "unreachable"}, nil, "remote"), SSHEnumeration{Error: "unreachable"})
	eq(t, RememberSSHEnumeration(SSHEnumeration{Error: "boom"}, []string{"default"}, "local"), SSHEnumeration{Error: "boom"})
}

func TestShouldRetrySshInventoryFirstTryCooldownThenRetryCacheNeverRetries(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	eq(t, ShouldRetrySSHInventory(false, nil, 1_000, 0), true)
	eq(t, ShouldRetrySSHInventory(false, ms(1_000), 30_000, 60_000), false)
	eq(t, ShouldRetrySSHInventory(false, ms(1_000), 61_000, 60_000), true)
	eq(t, ShouldRetrySSHInventory(true, ms(1_000), 120_000, 60_000), false)
}

func TestParseRemoteProfileListingMiniSparkDirsBecomeRosterNamesAndDropRollbacks(t *testing.T) {
	eq(t, ParseRemoteProfileListing(strings.Join([]string{"bob", "dixie", "goose", "rambo", "bob.rollback-old", ".hidden", "", "not a name"}, "\n")), []string{"default", "bob", "dixie", "goose", "rambo"})
}

func TestParseRemoteProfileListingEmptyListingIsStillTheDefaultAgent(t *testing.T) {
	eq(t, ParseRemoteProfileListing(""), []string{"default"})
}

func TestRosterUnreachableSourcesContributeNoRowsAndCannotFakeDuplicates(t *testing.T) {
	dead := RegistryConnection{ID: "dead", Kind: "remote", Label: "Dead box", URL: "http://d:1"}
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rLocal, Profiles: []string{"research"}}, {Connection: dead, Error: "unreachable"}}, "")
	eq(t, len(roster), 1)
	eq(t, roster[0].Handle, "research")
}

func TestRosterDuplicateProfilesFromOneConnectionRemainOneRoutableAgent(t *testing.T) {
	roster := BuildAgentRoster([]ConnectionAgents{
		{Connection: rLocal, Profiles: []string{"default", "research", "default"}},
		{Connection: rLocal, Profiles: []string{"research"}},
		{Connection: rHomelab, Profiles: []string{"research", "research"}},
	}, "")
	eq(t, rosterKeys(roster, func(a RosterAgent) string { return a.ConnectionID + "/" + a.Profile }), []string{"local/default", "local/research", "homelab/research"})
	for _, a := range roster {
		switch a.ConnectionID + "/" + a.Profile {
		case "local/research":
			eq(t, a.Handle, "research-this-device")
		case "homelab/research":
			eq(t, a.Handle, "research-homelab")
		}
	}
}

func TestRosterTwoConnectionsWithTheSameInstallIdCollapseToOneRowPerProfile(t *testing.T) {
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rSpark, Profiles: []string{"default", "research"}, InstallID: "aaa"}, {Connection: rSparkTS, Profiles: []string{"default", "research"}, InstallID: "aaa"}}, "")
	eq(t, sorted(rosterKeys(roster, func(a RosterAgent) string { return a.ConnectionID + "/" + a.Profile })), []string{"spark/default", "spark/research"})
	eq(t, sorted(rosterKeys(roster, func(a RosterAgent) string { return a.Handle })), []string{"default", "research"})
}

func TestRosterCollapsePrefersTheActivePrimaryConnection(t *testing.T) {
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rSpark, Profiles: []string{"default"}, InstallID: "aaa"}, {Connection: rSparkTS, Profiles: []string{"default"}, InstallID: "aaa"}}, "spark-ts")
	eq(t, len(roster), 1)
	eq(t, roster[0].ConnectionID, "spark-ts")
}

func TestRosterCollapsePickOrderIsLocalSshRemoteCloudThenRegistrationOrder(t *testing.T) {
	remote := RegistryConnection{ID: "loop", Kind: "remote", Label: "Loopback", URL: "http://127.0.0.1:8642"}
	cloud := RegistryConnection{ID: "cl", Kind: "cloud", Label: "Cloud twin", URL: "http://cl:1"}
	ssh := RegistryConnection{ID: "tun", Kind: "ssh", Label: "Tunnel", Host: "box"}
	one := func(c RegistryConnection) ConnectionAgents {
		return ConnectionAgents{Connection: c, Profiles: []string{"default"}, InstallID: "aaa"}
	}
	roster := BuildAgentRoster([]ConnectionAgents{one(cloud), one(remote), one(ssh), one(rLocal)}, "")
	eq(t, len(roster), 1)
	eq(t, roster[0].ConnectionID, "local")
	eq(t, BuildAgentRoster([]ConnectionAgents{one(cloud), one(remote), one(ssh)}, "")[0].ConnectionID, "tun")
	twin := RegistryConnection{ID: "loop2", Kind: "remote", Label: "Loopback 2", URL: "http://[::1]:8642"}
	eq(t, BuildAgentRoster([]ConnectionAgents{one(remote), one(twin)}, "")[0].ConnectionID, "loop")
}

func TestRosterMissingInstallIdBypassesTheCollapseOlderBackendsKeepCurrentBehavior(t *testing.T) {
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rSpark, Profiles: []string{"default"}}, {Connection: rSparkTS, Profiles: []string{"default"}}}, "")
	eq(t, len(roster), 2)
	eq(t, sorted(rosterKeys(roster, func(a RosterAgent) string { return a.Handle })), []string{"default-spark", "default-spark-ts"})
	mixed := BuildAgentRoster([]ConnectionAgents{{Connection: rSpark, Profiles: []string{"default"}, InstallID: "aaa"}, {Connection: rSparkTS, Profiles: []string{"default"}}}, "")
	eq(t, len(mixed), 2)
}

func TestRosterDifferentInstallIdsStaySeparateRowsWithDisambiguatedHandles(t *testing.T) {
	mini := RegistryConnection{ID: "mini", Kind: "remote", Label: "Mini", URL: "http://mini:8642"}
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rSpark, Profiles: []string{"default"}, InstallID: "aaa"}, {Connection: mini, Profiles: []string{"default"}, InstallID: "bbb"}}, "")
	eq(t, len(roster), 2)
	eq(t, sorted(rosterKeys(roster, func(a RosterAgent) string { return a.Handle })), []string{"default-mini", "default-spark"})
}

func TestRosterCollapseAlsoFoldsAThirdSameBoxConnectionFromAPerProfileV1OverrideImport(t *testing.T) {
	override := RegistryConnection{ID: "spark-lan", Kind: "remote", Label: "Spark LAN", URL: "http://192.168.1.5:8642"}
	profiles := []string{"default", "cron-bot"}
	roster := BuildAgentRoster([]ConnectionAgents{{Connection: rSpark, Profiles: profiles, InstallID: "aaa"}, {Connection: rSparkTS, Profiles: profiles, InstallID: "aaa"}, {Connection: override, Profiles: profiles, InstallID: "aaa"}}, "")
	eq(t, sorted(rosterKeys(roster, func(a RosterAgent) string { return a.Profile + ":" + a.Handle })), []string{"cron-bot:cron-bot", "default:default"})
}

func TestUpdateFanOutCloudIsPlatformManagedEverythingElseEligible(t *testing.T) {
	eq(t, ConnectionUpdateEligibility(RegistryConnection{ID: "c", Kind: "cloud", Label: "Cloud"}), UpdateEligibility{Reason: "cloud-managed"})
	for _, kind := range []string{"local", "remote", "ssh"} {
		eq(t, ConnectionUpdateEligibility(RegistryConnection{ID: "x", Kind: kind, Label: "x"}).Eligible, true)
	}
}

func TestSaveRejectsTheReservedLocalIdOnNonLocalKinds(t *testing.T) {
	_, err := NormalizeConnectionInput(obj("id", "local", "kind", "remote", "label", "Sneaky", "url", "http://x:1"), emptyRegistry())
	fails(t, err, "reserved")
}

func TestTokenOnlyPersistsOnTokenAuthRemotesOauthCloudDropIt(t *testing.T) {
	registry := emptyRegistry()
	tok := obj("enc", "x")
	eq(t, must(NormalizeConnectionInput(obj("kind", "remote", "label", "A", "url", "http://a:1", "authMode", "token", "token", tok), registry)).Token, any(tok))
	eq(t, must(NormalizeConnectionInput(obj("kind", "remote", "label", "B", "url", "http://b:1", "authMode", "oauth", "token", tok), registry)).Token, nil)
	eq(t, must(NormalizeConnectionInput(obj("kind", "cloud", "label", "C", "url", "https://c.hermes.cloud", "authMode", "oauth", "token", tok), registry)).Token, nil)
}

func TestACloudEntryIsSavedAsOauthEvenWhenThePayloadSaysToken89529(t *testing.T) {
	for _, authMode := range []any{nil, "token"} {
		cloud := must(NormalizeConnectionInput(obj("kind", "cloud", "label", "C", "url", "https://c.hermes.cloud", "authMode", authMode, "token", obj("enc", "x")), emptyRegistry()))
		eq(t, cloud.AuthMode, "oauth")
		eq(t, cloud.Token, nil)
	}
	eq(t, must(NormalizeConnectionInput(obj("kind", "remote", "label", "R", "url", "http://r:1"), emptyRegistry())).AuthMode, "token")
}

func TestAStoredCloudEntryLeftOnTokenAuthWithNoTokenReadsBackAsOauth89529(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "local", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "cloud-bare", "kind", "cloud", "label", "Bare cloud", "url", "https://a.hermes.cloud", "authMode", "token"),
		obj("id", "cloud-keyed", "kind", "cloud", "label", "Keyed cloud", "url", "https://b.hermes.cloud", "authMode", "token", "token", obj("v", 1.0)),
		obj("id", "homelab", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119", "authMode", "token"),
	}))
	eq(t, byID(registry, "cloud-bare").AuthMode, "oauth")
	eq(t, byID(registry, "cloud-keyed").AuthMode, "token")
	eq(t, byID(registry, "cloud-keyed").Token, any(obj("v", 1.0)))
	eq(t, byID(registry, "homelab").AuthMode, "token")
}

func TestAnSshEntryKeepsItsSessionTokenThroughALabelRename(t *testing.T) {
	stored := RegistryConnection{Host: "spark1", ID: "spark", Kind: "ssh", Label: "Spark", Port: 2222, Token: obj("enc", "ssh-session-token"), User: "tek"}
	merged := MergeConnectionInput(obj("id", "spark", "kind", "ssh", "label", "Spark (office)", "token", stored.Token), &stored)
	renamed := must(NormalizeConnectionInput(merged, emptyRegistry()))
	eq(t, renamed.Label, "Spark (office)")
	eq(t, renamed.Token, any(obj("enc", "ssh-session-token")))
}

func TestMergePreservesFieldsTheEditorDoesNotCarryOrgSshExtras(t *testing.T) {
	cloud := RegistryConnection{AuthMode: "oauth", ID: "c", Kind: "cloud", Label: "Cloud", Org: "nous", URL: "https://a.cloud"}
	eq(t, MergeConnectionInput(obj("id", "c", "kind", "cloud", "label", "Renamed", "url", "https://a.cloud"), &cloud)["org"], any("nous"))
	ssh := RegistryConnection{Host: "homelab.lan", ID: "s", KeyPath: "/k/id", Kind: "ssh", Label: "Box", Port: 2222, RemoteHermesPath: "/opt/hermes", RemoteProfile: "research", User: "k"}
	labelOnly := MergeConnectionInput(obj("id", "s", "kind", "ssh", "label", "Renamed box"), &ssh)
	eq(t, labelOnly["remoteHermesPath"], any("/opt/hermes"))
	eq(t, labelOnly["remoteProfile"], any("research"))
	eq(t, labelOnly["host"], any("homelab.lan"))
	eq(t, labelOnly["user"], any("k"))
	eq(t, labelOnly["port"], any(2222.0))
}

func TestMergeASuppliedSshHostStringBeatsStoredUserPort(t *testing.T) {
	ssh := RegistryConnection{Host: "spark1", ID: "s", Kind: "ssh", Label: "Spark", Port: 2222, User: "tek"}
	merged := MergeConnectionInput(obj("host", "admin@newbox:2200", "id", "s", "kind", "ssh", "label", "Spark"), &ssh)
	_, hasUser := merged["user"]
	_, hasPort := merged["port"]
	eq(t, hasUser, false)
	eq(t, hasPort, false)
	entry := must(NormalizeConnectionInput(merged, emptyRegistry()))
	eq(t, entry.Host, "newbox")
	eq(t, entry.User, "admin")
	eq(t, entry.Port, 2200)
}

func TestSaveRejectsAMissingLabelWithADeviceNameMessage(t *testing.T) {
	_, err := NormalizeConnectionInput(obj("kind", "remote", "label", "  ", "url", "http://10.0.0.5:9119"), emptyRegistry())
	fails(t, err, "device name")
}

func withRemote(t *testing.T, label, url string) ConnectionRegistry {
	registry := emptyRegistry()
	return UpsertConnection(registry, must(NormalizeConnectionInput(obj("kind", "remote", "label", label, "url", url), registry)))
}

func TestSaveRejectsADuplicateLabelCaseInsensitively(t *testing.T) {
	registry := withRemote(t, "Homelab", "http://10.0.0.5:9119")
	_, err := NormalizeConnectionInput(obj("kind", "remote", "label", " homelab ", "url", "http://10.0.0.9:9119"), registry)
	fails(t, err, "must be unique")
}

func TestEditingAnEntryDoesNotCollideWithItsOwnLabel(t *testing.T) {
	registry := withRemote(t, "Homelab", "http://10.0.0.5:9119")
	entry := firstKind(registry, "remote")
	edited := must(NormalizeConnectionInput(obj("id", entry.ID, "kind", "remote", "label", "Homelab", "url", "http://10.0.0.6:9119"), registry))
	eq(t, edited.ID, entry.ID)
	eq(t, edited.URL, "http://10.0.0.6:9119")
}

func TestDuplicateGatewayURLsAreRejectedAcrossRemoteAndCloudKinds(t *testing.T) {
	registry := withRemote(t, "Homelab", "http://10.0.0.5:9119")
	_, err := NormalizeConnectionInput(obj("kind", "remote", "label", "Twin", "url", "http://10.0.0.5:9119/"), registry)
	fails(t, err, "already exists")
	_, err = NormalizeConnectionInput(obj("kind", "cloud", "label", "Cloud twin", "url", "http://10.0.0.5:9119"), registry)
	fails(t, err, "already exists")
	existing := firstKind(registry, "remote")
	eq(t, must(NormalizeConnectionInput(obj("id", existing.ID, "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119"), registry)).ID, existing.ID)
}

func TestDuplicateSshTargetsAreRejectedOnUserHostPortRemoteProfile(t *testing.T) {
	registry := emptyRegistry()
	registry = UpsertConnection(registry, must(NormalizeConnectionInput(obj("kind", "ssh", "label", "Box", "host", "alice@box:22", "remoteProfile", "work"), registry)))
	_, err := NormalizeConnectionInput(obj("kind", "ssh", "label", "Box twin", "host", "alice@box:22", "remoteProfile", "work"), registry)
	fails(t, err, "already exists")
	eq(t, must(NormalizeConnectionInput(obj("kind", "ssh", "label", "Box other", "host", "alice@box:22", "remoteProfile", "other"), registry)).Kind, "ssh")
}

func TestRemoteInputNormalizesURLAndAuthModeCloudKeepsOrg(t *testing.T) {
	registry := emptyRegistry()
	remote := must(NormalizeConnectionInput(obj("kind", "remote", "label", "LAN box", "url", "10.0.0.5:9119", "authMode", "weird"), registry))
	eq(t, remote.URL, "http://10.0.0.5:9119")
	eq(t, remote.AuthMode, "token")
	cloud := must(NormalizeConnectionInput(obj("kind", "cloud", "label", "Cloud", "url", "https://foo.hermes.cloud", "authMode", "oauth", "org", "nous"), registry))
	eq(t, cloud.Kind, "cloud")
	eq(t, cloud.Org, "nous")
	eq(t, cloud.AuthMode, "oauth")
}

func TestSshInputRequiresAHostLocalInputOnlyCarriesTheLabel(t *testing.T) {
	registry := emptyRegistry()
	_, err := NormalizeConnectionInput(obj("kind", "ssh", "label", "Spark", "host", " "), registry)
	fails(t, err, "host")
	ssh := must(NormalizeConnectionInput(obj("kind", "ssh", "label", "Spark", "host", "tek@spark1:2222"), registry))
	eq(t, []any{ssh.Host, ssh.User, ssh.Port}, []any{"spark1", "tek", 2222})
	local := must(NormalizeConnectionInput(obj("kind", "local", "label", "My MacBook"), registry))
	eq(t, local.ID, LocalConnectionID)
	var keys map[string]any
	b, _ := json.Marshal(local)
	json.Unmarshal(b, &keys)
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	eq(t, sorted(names), []string{"id", "kind", "label"})
}

func TestNormalizeRegistryDegradesJunkToALocalOnlyRegistry(t *testing.T) {
	for _, junk := range []any{nil, 42.0, "nope", obj("connections", "zzz"), obj("version", 99.0)} {
		registry := NormalizeRegistry(junk)
		eq(t, registry.Version, RegistryVersion)
		eq(t, registry.Primary, LocalConnectionID)
		eq(t, registry.LaunchMode, "primary")
		eq(t, registry.LastUsed, LocalConnectionID)
		eq(t, len(registry.Connections), 1)
		eq(t, registry.Connections[0].Kind, "local")
	}
}

func TestNormalizeRegistryGuaranteesLocalDedupesLabelsFixesDanglingPrimary(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "ghost", "connections", []any{
		obj("id", "a", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119"),
		obj("id", "b", "kind", "remote", "label", "homelab", "url", "http://10.0.0.6:9119"),
		obj("id", "c", "kind", "remote", "label", "No URL entry"),
		obj("kind", "nonsense", "label", "x"),
	}))
	eq(t, registry.Primary, LocalConnectionID)
	eq(t, firstKind(registry, "local") != nil, true)
	seen := map[string]bool{}
	for _, c := range registry.Connections {
		if seen[LabelKey(c.Label)] {
			t.Fatalf("duplicate label %s", c.Label)
		}
		seen[LabelKey(c.Label)] = true
	}
	eq(t, countKind(registry, "remote"), 2)
}

func TestNormalizeRegistryRoundTripsAValidRegistryUnchangedInShape(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "homelab", "launchMode", "last-used", "lastUsed", "homelab", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "homelab", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119", "authMode", "token", "token", obj("v", 1.0)),
		obj("id", "cloud-1", "kind", "cloud", "label", "Icebath Cloud", "url", "https://a.hermes.cloud", "authMode", "oauth", "org", "nous"),
		obj("id", "spark", "kind", "ssh", "label", "Spark", "host", "spark1", "user", "tek", "port", 2222.0),
	}))
	eq(t, registry.Primary, "homelab")
	eq(t, registry.LaunchMode, "last-used")
	eq(t, registry.LastUsed, "homelab")
	eq(t, registry.ids(), []string{"local", "homelab", "cloud-1", "spark"})
	eq(t, registry.Connections[1].Token, any(obj("v", 1.0)))
	eq(t, registry.Connections[3].Port, 2222)
}

func TestNormalizeRegistryFallsBackToPrimaryWhenTheLastUsedSourceIsMissing(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "homelab", "launchMode", "last-used", "lastUsed", "retired-host", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "homelab", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119"),
	}))
	eq(t, registry.LaunchMode, "last-used")
	eq(t, registry.LastUsed, "homelab")
}

func roundTrip(r ConnectionRegistry) ConnectionRegistry {
	var decoded any
	b, _ := json.Marshal(r)
	json.Unmarshal(b, &decoded)
	return NormalizeRegistry(decoded)
}

func TestNormalizeRegistryKeepsThePersistedSshSessionTokenAcrossAColdRead(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "spark", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "spark", "kind", "ssh", "label", "Spark", "host", "spark1", "user", "tek", "port", 2222.0, "token", obj("enc", "ssh-session-token")),
	}))
	eq(t, byID(registry, "spark").Token, any(obj("enc", "ssh-session-token")))
	eq(t, byID(registry, "spark").Host, "spark1")
	eq(t, byID(roundTrip(registry), "spark").Token, any(obj("enc", "ssh-session-token")))
}

func TestMigrateV1LocalOnlyConfigLocalOnlyRegistry(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "local", "remote", obj(), "profiles", obj()), nil)
	eq(t, registry.Primary, LocalConnectionID)
	eq(t, len(registry.Connections), 1)
}

func TestMigrateV1GlobalRemoteBecomesALabeledEntryAndThePrimary(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "remote", "remote", obj("url", "http://homelab.lan:9119", "authMode", "token", "token", obj("enc", "x"))), nil)
	remote := firstKind(registry, "remote")
	eq(t, registry.Primary, remote.ID)
	eq(t, remote.Label, "homelab.lan:9119")
	eq(t, remote.Token, any(obj("enc", "x")))
}

func TestMigrateV1CloudKeepsCloudProvenanceOrg(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "cloud", "remote", obj("url", "https://a.hermes.cloud", "authMode", "oauth", "org", "nous")), nil)
	cloud := firstKind(registry, "cloud")
	eq(t, registry.Primary, cloud.ID)
	eq(t, cloud.Org, "nous")
}

func TestMigratePerProfileOverridesBecomeExtraSourcesDedupedByURL(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "remote", "remote", obj("url", "http://homelab.lan:9119", "authMode", "token", "token", obj("enc", "x")), "profiles", obj(
		"research", obj("mode", "remote", "url", "http://homelab.lan:9119", "authMode", "token", "token", obj("enc", "x")),
		"coder", obj("mode", "remote", "url", "http://other.lan:9119", "authMode", "token", "token", obj("enc", "y")),
		"sparky", obj("mode", "ssh", "host", "spark1", "user", "tek"),
		"plain", obj("mode", "local", "savedSsh", obj("mode", "ssh", "host", "spark1", "user", "tek")),
	)), []string{"research", "coder", "sparky", "plain"})
	eq(t, len(registry.Connections), 4)
	eq(t, countKind(registry, "remote"), 2)
	eq(t, countKind(registry, "ssh"), 1)
}

func TestMigrateV1GlobalSshBecomesThePrimary(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "ssh", "remote", obj("mode", "ssh", "host", "spark1", "user", "tek", "port", 2222.0)), nil)
	ssh := firstKind(registry, "ssh")
	eq(t, registry.Primary, ssh.ID)
	eq(t, ssh.Label, "spark1")
}

func TestMigrateDuplicateHostLabelsAreSuffixedNotDropped(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "remote", "remote", obj("url", "http://box.lan:9119", "authMode", "token", "token", obj()), "profiles", obj("a", obj("mode", "ssh", "host", "box.lan"))), []string{"a"})
	seen := map[string]bool{}
	for _, c := range registry.Connections {
		seen[LabelKey(c.Label)] = true
	}
	eq(t, len(seen), len(registry.Connections))
	eq(t, len(registry.Connections), 3)
}

func TestRemoveConnectionLocalRefusesPrimaryAndLastUsedRetargetSafely(t *testing.T) {
	registry := withRemote(t, "Homelab", "http://10.0.0.5:9119")
	entry := firstKind(registry, "remote")
	registry = must(SetPrimaryConnection(registry, entry.ID))
	registry = must(SetLastUsedConnection(registry, entry.ID))
	_, err := RemoveConnection(registry, LocalConnectionID)
	fails(t, err, "cannot be removed")
	after := must(RemoveConnection(registry, entry.ID))
	eq(t, after.Primary, LocalConnectionID)
	eq(t, after.LastUsed, LocalConnectionID)
	eq(t, len(after.Connections), 1)
	eq(t, must(RemoveConnection(after, "ghost")), after)
}

func TestSetPrimaryConnectionValidatesTheTargetId(t *testing.T) {
	_, err := SetPrimaryConnection(emptyRegistry(), "ghost")
	fails(t, err, "No connection")
	eq(t, must(SetPrimaryConnection(emptyRegistry(), LocalConnectionID)).Primary, LocalConnectionID)
}

func TestLastUsedSourceAndLaunchModeValidateTheirPersistedValues(t *testing.T) {
	registry := withRemote(t, "Homelab", "http://10.0.0.5:9119")
	entry := firstKind(registry, "remote")
	_, err := SetLastUsedConnection(registry, "ghost")
	fails(t, err, "No connection")
	eq(t, must(SetLastUsedConnection(registry, entry.ID)).LastUsed, entry.ID)
	eq(t, must(SetConnectionLaunchMode(registry, "last-used")).LaunchMode, "last-used")
	_, err = SetConnectionLaunchMode(registry, "sometimes")
	fails(t, err, "Unknown connection launch mode")
}

func TestUpsertConnectionReplacesByIdAndAppendsNewIds(t *testing.T) {
	registry := emptyRegistry()
	a := must(NormalizeConnectionInput(obj("kind", "remote", "label", "A", "url", "http://a:1"), registry))
	registry = UpsertConnection(registry, a)
	a2 := a
	a2.URL = "http://a:2"
	registry = UpsertConnection(registry, a2)
	n := 0
	for _, c := range registry.Connections {
		if c.ID == a.ID {
			n++
		}
	}
	eq(t, n, 1)
	eq(t, byID(registry, a.ID).URL, "http://a:2")
}

func TestApplyRemoteInsertsIntoAnExistingLocalOnlyRegistryAndBecomesPrimaryCurrent(t *testing.T) {
	registry := apply(t, emptyRegistry(), remoteApply("https://gateway.example.com/"))
	remote := firstKind(registry, "remote")
	eq(t, registry.Primary, remote.ID)
	eq(t, registry.LastUsed, remote.ID)
	eq(t, ResolvedConnectionID(registry, obj("authMode", "oauth", "baseUrl", "https://gateway.example.com", "headers", obj(), "mode", "remote", "remoteKind", "url")), remote.ID)
}

func TestApplyRemotePreservesAnExistingURLIdentityAndLabelWithoutDuplicates(t *testing.T) {
	registry := UpsertConnection(emptyRegistry(), RegistryConnection{ID: "hermes-alex", Kind: "remote", Label: "Existing gateway", URL: "https://gateway.example.com", AuthMode: "token", Token: obj("old", true)})
	applied := apply(t, registry, remoteApply("https://GATEWAY.example.com/"))
	var matches []RegistryConnection
	for _, c := range applied.Connections {
		if c.URL == "https://gateway.example.com" {
			matches = append(matches, c)
		}
	}
	eq(t, len(matches), 1)
	eq(t, []string{matches[0].ID, matches[0].Label, matches[0].AuthMode}, []string{"hermes-alex", "Existing gateway", "oauth"})
	eq(t, applied.Primary, "hermes-alex")
	eq(t, applied.LastUsed, "hermes-alex")
}

func TestApplyLocalMovesPrimaryCurrentToThisDeviceWithoutDeletingRegisteredRemotes(t *testing.T) {
	remoteRegistry := apply(t, emptyRegistry(), remoteApply("https://one.example.com"))
	localRegistry := apply(t, remoteRegistry, obj("mode", "local", "remote", obj()))
	eq(t, localRegistry.Primary, LocalConnectionID)
	eq(t, localRegistry.LastUsed, LocalConnectionID)
	eq(t, countKind(localRegistry, "remote"), 1)
	eq(t, ResolvedConnectionID(localRegistry, obj("mode", "local")), LocalConnectionID)
}

func TestApplyBetweenTwoRemotesKeepsEachRealRegistrationOnceAndActivatesTheLatest(t *testing.T) {
	first := apply(t, emptyRegistry(), remoteApply("https://one.example.com"))
	second := apply(t, first, remoteApply("https://two.example.com/"))
	var urls, ids []string
	primaryURL := ""
	for _, c := range second.Connections {
		if c.Kind == "remote" {
			urls, ids = append(urls, c.URL), append(ids, c.ID)
			if c.URL == "https://two.example.com" {
				primaryURL = c.ID
			}
		}
	}
	eq(t, sorted(urls), []string{"https://one.example.com", "https://two.example.com"})
	eq(t, len(ids), 2)
	eq(t, ids[0] != ids[1], true)
	eq(t, second.Primary, primaryURL)
	eq(t, second.LastUsed, second.Primary)
}

func TestDriftHealRegistersAV1RemoteTheRegistryNeverLearnedAboutAndMakesItPrimary(t *testing.T) {
	registry, changed := drift(t, emptyRegistry(), remoteApply("https://agent.example.com:4443"))
	eq(t, changed, true)
	remote := firstKind(registry, "remote")
	eq(t, registry.Primary, remote.ID)
	eq(t, registry.LastUsed, remote.ID)
	eq(t, ResolvedConnectionID(registry, obj("authMode", "oauth", "baseUrl", "https://agent.example.com:4443", "headers", obj(), "mode", "remote", "remoteKind", "url")), remote.ID)
}

func TestDriftHealLeavesARegistryThatAlreadyKnowsTheV1RouteUntouched(t *testing.T) {
	registered := apply(t, emptyRegistry(), remoteApply("https://agent.example.com"))
	registry, changed := drift(t, registered, remoteApply("https://AGENT.example.com/"))
	eq(t, changed, false)
	eq(t, registry, registered)
}

func TestDriftHealRespectsADeliberatePrimaryPickOnARegisteredRoute(t *testing.T) {
	registry := must(SetPrimaryConnection(apply(t, emptyRegistry(), remoteApply("https://agent.example.com")), LocalConnectionID))
	out, changed := drift(t, registry, remoteApply("https://agent.example.com"))
	eq(t, changed, false)
	eq(t, out.Primary, LocalConnectionID)
}

func TestDriftHealIgnoresLocalAndUnparseableV1Routes(t *testing.T) {
	registry := emptyRegistry()
	for _, v1 := range []any{
		obj("mode", "local", "remote", obj()), obj("mode", "ssh", "remote", obj()), obj("mode", "ssh", "remote", obj("host", "   ")),
		obj("mode", "remote", "remote", obj("url", "not a url")), obj("mode", "remote", "remote", obj()), nil,
	} {
		out, changed := drift(t, registry, v1)
		eq(t, changed, false)
		eq(t, out, registry)
	}
}

func sshDrift(extra ...any) map[string]any {
	return obj("mode", "ssh", "remote", obj(extra...))
}

func TestDriftHealRegistersAV1SSHRouteTheRegistryNeverLearnedAboutAndMakesItPrimary(t *testing.T) {
	registry, changed := drift(t, emptyRegistry(), sshDrift("host", "devbox.example.com", "user", "omar", "port", 2222.0))
	eq(t, changed, true)
	ssh := firstKind(registry, "ssh")
	eq(t, []any{ssh.Host, ssh.User, ssh.Port}, []any{"devbox.example.com", "omar", 2222})
	eq(t, registry.Primary, ssh.ID)
	eq(t, registry.LastUsed, ssh.ID)
	eq(t, ResolvedConnectionID(registry, obj("mode", "remote", "remoteKind", "ssh", "ssh", obj("host", "devbox.example.com", "user", "omar", "port", 2222.0))), ssh.ID)
}

func TestDriftHealLeavesARegistryThatAlreadyKnowsTheV1SSHRouteUntouched(t *testing.T) {
	first, changed := drift(t, emptyRegistry(), sshDrift("host", "devbox.example.com", "user", "omar"))
	eq(t, changed, true)
	out, changed := drift(t, first, sshDrift("host", "DEVBOX.example.com", "user", "Omar"))
	eq(t, changed, false)
	eq(t, out, first)
}

func TestDriftHealAlignsARegisteredSSHRouteWhoseIdentityFieldsDriftedFromTheV1Route(t *testing.T) {
	first, changed := drift(t, emptyRegistry(), sshDrift("host", "devbox.example.com", "user", "omar"))
	eq(t, changed, true)
	drifted, changed := drift(t, first, sshDrift("host", "devbox.example.com", "user", "omar", "keyPath", "~/.ssh/id_rsa"))
	eq(t, changed, true)
	eq(t, countKind(drifted, "ssh"), 1)
	eq(t, firstKind(drifted, "ssh").KeyPath, "~/.ssh/id_rsa")
	eq(t, drifted.Primary, first.Primary)
	eq(t, MatchingConnectionID(drifted, obj("host", "devbox.example.com", "user", "omar", "keyPath", "~/.ssh/id_rsa", "kind", "ssh"), "primary"), firstKind(drifted, "ssh").ID)
	cleared, changed := drift(t, drifted, sshDrift("host", "devbox.example.com", "user", "omar"))
	eq(t, changed, true)
	eq(t, firstKind(cleared, "ssh").KeyPath, "")
}

func TestDriftHealRespectsADeliberatePrimaryPickOnARegisteredSSHRoute(t *testing.T) {
	registry, _ := drift(t, emptyRegistry(), sshDrift("host", "devbox.example.com"))
	registry = must(SetPrimaryConnection(registry, LocalConnectionID))
	out, changed := drift(t, registry, sshDrift("host", "devbox.example.com"))
	eq(t, changed, false)
	eq(t, out.Primary, LocalConnectionID)
}

func homelabRegistry() ConnectionRegistry {
	return UpsertConnection(emptyRegistry(), RegistryConnection{ID: "homelab", Kind: "remote", Label: "Homelab", URL: "https://homelab.example.com", AuthMode: "token", Token: obj("keep", true)})
}

func TestDriftHealAddsTheMissingSSHSourceWithoutDisturbingOtherRegisteredSources(t *testing.T) {
	out, changed := drift(t, homelabRegistry(), sshDrift("host", "devbox.example.com"))
	eq(t, changed, true)
	eq(t, byID(out, "homelab") != nil, true)
	eq(t, countKind(out, "ssh"), 1)
}

func TestDriftHealAddsTheMissingRemoteWithoutDisturbingOtherRegisteredSources(t *testing.T) {
	out, changed := drift(t, homelabRegistry(), remoteApply("https://agent.example.com:4443"))
	eq(t, changed, true)
	eq(t, countKind(out, "remote"), 2)
	eq(t, byID(out, "homelab") != nil, true)
}

func TestConnectionDialFieldsChangedLabelOnlyEditsDoNotRecycle(t *testing.T) {
	before := RegistryConnection{ID: "homelab", Kind: "remote", Label: "Homelab", URL: "http://10.0.0.5:9119", AuthMode: "token", Token: envObj("abc")}
	after := before
	after.Label = "Home lab (renamed)"
	eq(t, ConnectionDialFieldsChanged(before, after), false)
	eq(t, ConnectionDialFieldsChanged(before, before), false)
}

func TestConnectionDialFieldsChangedUrlAuthTokenChangesRecycle(t *testing.T) {
	before := RegistryConnection{ID: "homelab", Kind: "remote", Label: "Homelab", URL: "http://10.0.0.5:9119", AuthMode: "token", Token: envObj("abc")}
	u, oauth, tok := before, before, before
	u.URL = "http://10.0.0.9:9119"
	oauth.AuthMode, oauth.Token = "oauth", nil
	tok.Token = envObj("NEW")
	eq(t, ConnectionDialFieldsChanged(before, u), true)
	eq(t, ConnectionDialFieldsChanged(before, oauth), true)
	eq(t, ConnectionDialFieldsChanged(before, tok), true)
}

func TestConnectionDialFieldsChangedSshRoutingFieldsRecycleKindChangeRecycles(t *testing.T) {
	before := RegistryConnection{ID: "box", Kind: "ssh", Label: "Box", Host: "box.lan", User: "me", Port: 22}
	edit := func(f func(*RegistryConnection)) RegistryConnection { c := before; f(&c); return c }
	eq(t, ConnectionDialFieldsChanged(before, edit(func(c *RegistryConnection) { c.Label = "Box 2" })), false)
	eq(t, ConnectionDialFieldsChanged(before, edit(func(c *RegistryConnection) { c.Host = "other.lan" })), true)
	eq(t, ConnectionDialFieldsChanged(before, edit(func(c *RegistryConnection) { c.Port = 2222 })), true)
	eq(t, ConnectionDialFieldsChanged(before, edit(func(c *RegistryConnection) { c.RemoteProfile = "work" })), true)
	eq(t, ConnectionDialFieldsChanged(before, RegistryConnection{ID: "box", Kind: "remote", Label: "Box", URL: "http://x:1"}), true)
}

func TestNormalizeConnectionInputKeepsFilteredHeadersOnRemoteCloudDropsThemElsewhere(t *testing.T) {
	registry := emptyRegistry()
	remote := must(NormalizeConnectionInput(obj("kind", "remote", "label", "CF box", "url", "https://hermes.example.com", "authMode", "token", "token", obj("enc", "x"),
		"headers", obj("CF-Access-Client-Id", envObj("id"), "Authorization", obj("encoding", "plain", "value", "blocked"))), registry))
	eq(t, remote.Headers, map[string]SecretEnvelope{"CF-Access-Client-Id": env("id")})
	ssh := must(NormalizeConnectionInput(obj("kind", "ssh", "label", "Box", "host", "box.lan", "headers", obj("CF-Access-Client-Id", obj("encoding", "plain", "value", "id"))), registry))
	eq(t, ssh.Headers, map[string]SecretEnvelope(nil))
}

func TestMergeConnectionInputInheritsStoredHeadersWhenTheEditorPayloadOmitsThem(t *testing.T) {
	stored := RegistryConnection{ID: "cf", Kind: "remote", Label: "CF box", URL: "https://hermes.example.com", AuthMode: "token", Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("id")}}
	eq(t, MergeConnectionInput(obj("id", "cf", "kind", "remote", "label", "Renamed"), &stored)["headers"], toJSONValue(stored.Headers))
	eq(t, MergeConnectionInput(obj("id", "cf", "kind", "remote", "label", "CF box", "headers", obj()), &stored)["headers"], any(obj()))
}

func TestConnectionDialFieldsChangedAHeaderChangeRecyclesLiveBackends(t *testing.T) {
	before := RegistryConnection{ID: "cf", Kind: "remote", Label: "CF box", URL: "https://hermes.example.com", AuthMode: "token", Token: obj("enc", "x"), Headers: map[string]SecretEnvelope{"CF-Access-Client-Id": env("id")}}
	other, none := before, before
	other.Headers = map[string]SecretEnvelope{"CF-Access-Client-Id": env("OTHER")}
	none.Headers = nil
	eq(t, ConnectionDialFieldsChanged(before, before), false)
	eq(t, ConnectionDialFieldsChanged(before, other), true)
	eq(t, ConnectionDialFieldsChanged(before, none), true)
}

func TestNormalizeRegistryPreservesStoredHeadersOnRemoteEntriesV2AdditiveField(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "cf", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "cf", "kind", "remote", "label", "CF box", "url", "https://hermes.example.com", "authMode", "token", "token", obj("enc", "x"),
			"headers", obj("CF-Access-Client-Id", envObj("id"), "Cookie", obj("encoding", "plain", "value", "blocked"))),
	}))
	eq(t, byID(registry, "cf").Headers, map[string]SecretEnvelope{"CF-Access-Client-Id": env("id")})
}

func TestMigrateV1ToRegistryCarriesV1RemoteHeadersIntoTheRegistryEntry(t *testing.T) {
	registry := MigrateV1ToRegistry(obj("mode", "remote", "remote", obj("url", "https://hermes.example.com", "authMode", "token", "token", obj("enc", "x"), "headers", obj("CF-Access-Client-Id", envObj("id")))), nil)
	eq(t, firstKind(registry, "remote").Headers, map[string]SecretEnvelope{"CF-Access-Client-Id": env("id")})
}

func TestNormalizeRegistryQuarantinesMalformedEntriesInsteadOfSilentlyDroppingThem(t *testing.T) {
	mystery := obj("kind", "nonsense", "label", "Mystery box", "extra", "still my data")
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "a", "connections", []any{
		obj("id", "local", "kind", "local", "label", "This device"),
		obj("id", "a", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119"),
		obj("id", "c", "kind", "remote", "label", "No URL entry"),
		mystery,
		obj("id", "s", "kind", "ssh", "label", "No host ssh"),
	}))
	eq(t, registry.ids(), []string{"local", "a"})
	eq(t, registry.Primary, "a")
	eq(t, len(registry.Quarantined), 3)
	var reasons []string
	for _, q := range registry.Quarantined {
		reasons = append(reasons, q.Reason)
		if q.Reason == "entry-unrecognized-kind" {
			eq(t, q.Entry, any(mystery))
		}
	}
	eq(t, sorted(reasons), []string{"entry-missing-ssh-host", "entry-missing-url", "entry-unrecognized-kind"})
}

func TestNormalizeRegistryPreservesPreviouslyQuarantinedEntriesAcrossRoundTrips(t *testing.T) {
	first := NormalizeRegistry(obj("version", 2.0, "connections", []any{obj("id", "c", "kind", "remote", "label", "No URL entry")}))
	eq(t, len(first.Quarantined), 1)
	second := roundTrip(first)
	eq(t, len(second.Quarantined), 1)
	eq(t, second.Quarantined[0].Entry, any(obj("id", "c", "kind", "remote", "label", "No URL entry")))
}

type poisonedLabel struct{}

func (poisonedLabel) String() string               { panic(errors.New("poisoned entry")) }
func (poisonedLabel) MarshalJSON() ([]byte, error) { return nil, errors.New("poisoned entry") }

func TestNormalizeRegistryQuarantinesAnEntryThatExplodesDuringNormalizationNoWholeLoadAbort(t *testing.T) {
	poisoned := obj("id", "boom", "kind", "remote", "url", "http://10.0.0.9:9119", "label", poisonedLabel{})
	registry := NormalizeRegistry(obj("version", 2.0, "primary", "a", "connections", []any{poisoned, obj("id", "a", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119")}))
	var remotes []string
	for _, c := range registry.Connections {
		if c.Kind == "remote" {
			remotes = append(remotes, c.ID)
		}
	}
	eq(t, remotes, []string{"a"})
	eq(t, registry.Primary, "a")
	eq(t, len(registry.Quarantined), 1)
	eq(t, registry.Quarantined[0].Reason, "entry-normalization-failed")
}

func TestNormalizeRegistryKeepsACleanRegistryFreeOfTheQuarantinedKeyAndCapsQuarantineGrowth(t *testing.T) {
	clean := NormalizeRegistry(obj("version", 2.0, "connections", []any{obj("id", "a", "kind", "remote", "label", "Homelab", "url", "http://10.0.0.5:9119")}))
	b, _ := json.Marshal(clean)
	eq(t, strings.Contains(string(b), `"quarantined"`), false)
	var items []any
	for i := 0; i < 100; i++ {
		items = append(items, obj("id", fmt.Sprintf("q%d", i), "kind", "remote", "label", fmt.Sprintf("No URL %d", i)))
	}
	eq(t, len(NormalizeRegistry(obj("version", 2.0, "connections", items)).Quarantined) <= 20, true)
}

func TestNormalizeRegistryQuarantinesNonObjectJunkItemsThatCouldStillBeUserData(t *testing.T) {
	registry := NormalizeRegistry(obj("version", 2.0, "connections", []any{"{ mangled json fragment }", nil, false, obj("id", "a", "kind", "remote", "label", "A", "url", "http://x:1")}))
	var kinds []string
	for _, c := range registry.Connections {
		kinds = append(kinds, c.Kind)
	}
	eq(t, kinds, []string{"local", "remote"})
	eq(t, len(registry.Quarantined), 1)
	eq(t, registry.Quarantined[0].Entry, any("{ mangled json fragment }"))
}
