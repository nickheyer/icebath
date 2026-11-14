package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Runner double serving default, the ops satellite and team_b with its own Telegram bot
type identityTestRunner struct {
	cfg     *GatewayConfig
	home    string
	primary *turnAdapter
	teamB   *turnAdapter
	served  map[string]bool
}

func (r *identityTestRunner) RunnerConfig() *GatewayConfig { return r.cfg }
func (r *identityTestRunner) PrimaryProfileName() string   { return "default" }

func (r *identityTestRunner) TransportOwner(source *SessionSource) (PlatformAdapter, string, bool) {
	adapter := source.TransportAdapterRef()
	if adapter == nil {
		return nil, "", false
	}
	_, profile := r.OwningProfile(adapter, source.Platform)
	return adapter, profile, true
}

func (r *identityTestRunner) OwningProfile(adapter PlatformAdapter, platform Platform) (bool, string) {
	if adapter == PlatformAdapter(r.teamB) {
		return true, "team_b"
	}
	return adapter == PlatformAdapter(r.primary), ""
}

func (r *identityTestRunner) ProfileNameForSource(ctx context.Context, source *SessionSource, adapterProfile *string) (*string, error) {
	chatID := source.ChatID
	route, err := MatchProfileRoute(ctx, r.cfg.ProfileRoutes, RouteQuery{Platform: string(source.Platform), ChatID: &chatID, AdapterProfile: adapterProfile, UserID: source.UserID})
	if err != nil || route == nil {
		return nil, err
	}
	if !r.served[route.Profile] {
		return nil, &ProfileRouteRejected{Args: []any{"route targets unserved profile " + route.Profile, route.Profile}}
	}
	profile := route.Profile
	return &profile, nil
}

func (r *identityTestRunner) ResolveProfileHomeForSource(ctx context.Context, source *SessionSource) (string, error) {
	if source.Profile == nil || *source.Profile == "default" {
		return r.home, nil
	}
	return filepath.Join(r.home, "profiles", *source.Profile), nil
}

// Multiplexed or standalone rig with the admin-dm route to ops and the ghost route to an unserved profile
func newIdentityRig(t *testing.T, multiplex bool) *identityTestRunner {
	t.Helper()
	ctx := context.Background()
	h := filepath.Join(t.TempDir(), "hh")
	for _, name := range []string{"ops", "team_b"} {
		if err := os.MkdirAll(filepath.Join(h, "profiles", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	cfg := DefaultGatewayConfig(ctx)
	cfg.MultiplexProfiles = &multiplex
	if multiplex {
		routes, err := ParseProfileRoutes(ctx, []any{
			map[string]any{"name": "admin-dm", "platform": "telegram", "profile": "ops", "chat_id": "72719239"},
			map[string]any{"name": "ghost", "platform": "telegram", "profile": "ghost", "chat_id": "4040"},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg.ProfileRoutes = routes
	}
	r := &identityTestRunner{cfg: cfg, home: h, primary: newTurnAdapter(PlatformTelegram), teamB: newTurnAdapter(PlatformTelegram),
		served: map[string]bool{"default": true, "ops": true, "team_b": true}}
	r.teamB.SetOwnerProfile("team_b")
	r.primary.GatewayRunner, r.teamB.GatewayRunner = r, r
	return r
}

// Dm source built by the adapter the way an inbound event is
func identityDM(a *turnAdapter, chatID string) *SessionSource {
	user := chatID
	return a.BuildSource(context.Background(), chatID, SourceFields{ChatType: "dm", UserID: &user})
}

func TestIdentityIsOneFrozenValueThatEveryReaderAgreesOn(t *testing.T) {
	rig := newIdentityRig(t, true)
	ctx := context.Background()
	routed := identityDM(rig.primary, "72719239")
	identity, err := resolveIdentity(ctx, routed, rig, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := RoutingIdentity{TransportProfile: "default", RuntimeProfile: "ops", AuthorizationHome: rig.home,
		RuntimeHome: filepath.Join(rig.home, "profiles", "ops"), Multiplexed: true, Transport: identity.Transport}
	if *identity != want || identity.TransportInferred {
		t.Fatalf("identity %+v", identity)
	}
	if identity.Namespace() != "agent:ops" || identity.StorePath() != filepath.Join(rig.home, "profiles", "ops", "state.db") {
		t.Fatalf("namespace %q store %q", identity.Namespace(), identity.StorePath())
	}
	if identity.Adapter() != PlatformAdapter(rig.primary) || IdentityOf(routed) != identity {
		t.Fatal("the pinned identity must name the receiving adapter and be the source's identity")
	}
	if routed.authorizationProfileHome != rig.home {
		t.Fatalf("authorization home %q", routed.authorizationProfileHome)
	}
	own := identityDM(rig.teamB, "72719239")
	ownIdentity, err := resolveIdentity(ctx, own, rig, nil, "team_b", "")
	if err != nil {
		t.Fatal(err)
	}
	teamHome := filepath.Join(rig.home, "profiles", "team_b")
	if ownIdentity.TransportProfile != "team_b" || ownIdentity.RuntimeProfile != "team_b" ||
		ownIdentity.AuthorizationHome != teamHome || ownIdentity.RuntimeHome != teamHome || *ownIdentity == *identity {
		t.Fatalf("own identity %+v", ownIdentity)
	}
	again := identityDM(rig.primary, "72719239")
	againIdentity, err := resolveIdentity(ctx, again, rig, nil, "", "")
	if err != nil || *againIdentity != *identity {
		t.Fatalf("a second event from the same bot must have the same identity: %+v %v", againIdentity, err)
	}
	if pinned, err := canonicalIdentity(ctx, again, rig, rig.primary, ""); err != nil || pinned != againIdentity {
		t.Fatalf("canonical identity must return the pinned value: %v %v", pinned, err)
	}
}

func TestUnresolvedUnderMultiplexRaisesAndNeverMeansDefault(t *testing.T) {
	rig := newIdentityRig(t, true)
	ctx := context.Background()
	rejected := identityDM(rig.primary, "4040")
	_, err := resolveIdentity(ctx, rejected, rig, nil, "", "")
	var unresolved *IdentityUnresolved
	if !errors.As(err, &unresolved) || unresolved.Reason != "telegram/4040: profile route rejected" || IdentityOf(rejected) != nil {
		t.Fatalf("rejected route %v", err)
	}
	fresh := &SessionSource{Platform: PlatformTelegram, ChatID: "4040", ChatType: "dm"}
	fresh.SetTransportAdapterRef(rig.primary)
	if _, err := resolveIdentity(ctx, fresh, rig, nil, "", ""); !errors.As(err, &unresolved) || !fresh.ProfileRouteRejected ||
		fresh.rejectedRouteProfile == nil || *fresh.rejectedRouteProfile != "ghost" {
		t.Fatalf("routing rejection must mark the source: %v %+v", err, fresh)
	}
	if identity, err := canonicalIdentity(ctx, identityDM(rig.primary, "4040"), rig, rig.primary, ""); identity != nil || err != nil {
		t.Fatalf("canonical identity of an unresolved route must be nil: %v %v", identity, err)
	}
	solo := newIdentityRig(t, false)
	source := identityDM(solo.primary, "4040")
	identity, err := resolveIdentity(ctx, source, solo, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if identity.TransportProfile != "default" || identity.RuntimeProfile != "default" || identity.Multiplexed ||
		identity.RuntimeHome != solo.home || identity.Namespace() != "agent:main" || source.Profile != nil {
		t.Fatalf("standalone identity %+v profile %v", identity, source.Profile)
	}
	if key, err := solo.primary.SourceSessionKey(ctx, source); err != nil || key != "agent:main:telegram:dm:4040" {
		t.Fatalf("standalone key %q %v", key, err)
	}
}

func TestReplaceSourceKeepsIdentityAndTransportWhereAPlainCopyDropsThem(t *testing.T) {
	rig := newIdentityRig(t, true)
	ctx := context.Background()
	routed := identityDM(rig.primary, "72719239")
	identity, err := resolveIdentity(ctx, routed, rig, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	bare := &SessionSource{Platform: routed.Platform, ChatID: routed.ChatID, ChatType: routed.ChatType, Profile: routed.Profile}
	if IdentityOf(bare) != nil || bare.TransportAdapterRef() != nil {
		t.Fatal("a plain copy carries no provenance")
	}
	thread := "7"
	copied := ReplaceSource(routed, func(s *SessionSource) { s.ThreadID = &thread })
	if copied == routed || *copied.ThreadID != "7" || IdentityOf(copied) != identity ||
		copied.TransportAdapterRef() != PlatformAdapter(rig.primary) || copied.authorizationProfileHome != rig.home {
		t.Fatalf("copied %+v", copied)
	}
	if key, err := rig.primary.SourceSessionKey(ctx, copied); err != nil || key != "agent:ops:telegram:dm:72719239:7" {
		t.Fatalf("copied key %q %v", key, err)
	}
}

// A restored row pins its persisted transport under multiplexing, nothing for standalone or a pre column row
func TestRestoreIdentityPinsThePersistedTransportOnlyUnderMultiplexing(t *testing.T) {
	rig := newIdentityRig(t, true)
	ctx := context.Background()
	ops := "ops"
	teamB := "team_b"
	source := &SessionSource{Platform: PlatformTelegram, ChatID: "555", ChatType: "dm", Profile: &ops}
	identity, err := restoreIdentity(ctx, source, rig, &teamB)
	if err != nil {
		t.Fatal(err)
	}
	if identity.TransportProfile != "team_b" || identity.RuntimeProfile != "ops" || !identity.Multiplexed ||
		identity.AuthorizationHome != filepath.Join(rig.home, "profiles", "team_b") ||
		identity.RuntimeHome != filepath.Join(rig.home, "profiles", "ops") || identity.Adapter() != nil || IdentityOf(source) != identity {
		t.Fatalf("restored %+v", identity)
	}
	if again, _ := restoreIdentity(ctx, source, rig, &teamB); again != identity {
		t.Fatal("an existing identity is kept")
	}
	primary := "default"
	own := &SessionSource{Platform: PlatformTelegram, ChatID: "1", ChatType: "dm"}
	if identity, _ := restoreIdentity(ctx, own, rig, &primary); identity.AuthorizationHome != rig.home || identity.RuntimeHome != rig.home {
		t.Fatalf("primary restore %+v", identity)
	}
	blank := "  "
	for _, transport := range []*string{nil, &blank} {
		if identity, err := restoreIdentity(ctx, &SessionSource{Platform: PlatformTelegram, ChatID: "2"}, rig, transport); identity != nil || err != nil {
			t.Fatalf("a pre column row restores nothing: %v %v", identity, err)
		}
	}
	solo := newIdentityRig(t, false)
	if identity, err := restoreIdentity(ctx, &SessionSource{Platform: PlatformTelegram, ChatID: "3"}, solo, &teamB); identity != nil || err != nil {
		t.Fatalf("standalone restores nothing: %v %v", identity, err)
	}
}

// Auth v1 subjects parse back, native identities keep their bare subject and the digest is stable
func TestAuthenticatedSubjectFormsAndPrincipalDigest(t *testing.T) {
	login := map[string]any{"user_id": "u1", "provider": "github", "issuer": "https://github.com"}
	subject, err := AuthenticatedSubject(login)
	if err != nil || subject != `auth:v1:["github","https://github.com","u1"]` {
		t.Fatalf("subject %q %v", subject, err)
	}
	if parts, ok := ParseAuthenticatedSubject(subject); !ok || parts != [3]string{"github", "https://github.com", "u1"} {
		t.Fatalf("parsed %v %v", parts, ok)
	}
	for _, bad := range []any{"u1", `auth:v1:["a","b"]`, `auth:v1:["a","b",3]`, "auth:v1:{", 7} {
		if _, ok := ParseAuthenticatedSubject(bad); ok {
			t.Fatalf("%v must not parse", bad)
		}
	}
	native := map[string]any{"user_id": "1000", "provider": "local", "profile_id": "p", "instance_id": "i", "capabilities": nil}
	if subject, _ := AuthenticatedSubject(native); subject != "1000" {
		t.Fatalf("native subject %q", subject)
	}
	delete(native, "capabilities")
	if subject, _ := AuthenticatedSubject(native); subject != `auth:v1:["local","","1000"]` {
		t.Fatalf("local without capabilities %q", subject)
	}
	if subject, _ := AuthenticatedSubject(map[string]any{"user_id": "caf\u00e9"}); subject != `auth:v1:["","","caf\u00e9"]` {
		t.Fatalf("ascii escaped subject %q", subject)
	}
	if subject, _ := AuthenticatedSubject(map[string]any{}); subject != `auth:v1:["","","unbound"]` {
		t.Fatalf("unbound subject %q", subject)
	}
	digest, err := AuthenticatedPrincipalDigest(login)
	if err != nil || len(digest) != len("principal:dashboard:")+32 || digest[:20] != "principal:dashboard:" {
		t.Fatalf("digest %q %v", digest, err)
	}
	if !IsAuthenticatedIdentity(login) || IsAuthenticatedIdentity(map[string]any{"user_id": "server-internal", "provider": "server-internal"}) ||
		IsAuthenticatedIdentity(map[string]any{"user_id": " ", "provider": "github"}) || IsAuthenticatedIdentity("u1") {
		t.Fatal("authenticated identity check")
	}
}
