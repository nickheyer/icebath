package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"weak"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// RoutingIdentity is who a turn is, resolved once at ingress, with explicit transport and runtime profiles
type RoutingIdentity struct {
	TransportProfile  string
	RuntimeProfile    string
	AuthorizationHome string
	RuntimeHome       string
	Multiplexed       bool
	Transport         weak.Pointer[BasePlatformAdapter]
	TransportInferred bool
}

// Agent namespace prefix of this identity's session keys
func (r *RoutingIdentity) Namespace() string {
	return SessionKeyNamespace(r.SessionKeyProfile())
}

// Session database path under the runtime home
func (r *RoutingIdentity) StorePath() string {
	return filepath.Join(r.RuntimeHome, "state.db")
}

// Runtime profile under multiplexing, empty for the legacy namespace of a standalone gateway
func (r *RoutingIdentity) SessionKeyProfile() string {
	if r.Multiplexed {
		return r.RuntimeProfile
	}
	return ""
}

// Live receiving adapter, nil when it is gone or was never known
func (r *RoutingIdentity) Adapter() PlatformAdapter {
	base := r.Transport.Value()
	if base == nil {
		return nil
	}
	return base.Self()
}

// Identity pinned on the source by resolution, nil when none is pinned
func IdentityOf(source *SessionSource) *RoutingIdentity {
	if source == nil {
		return nil
	}
	return source.identity
}

// Drops the pinned identity, the rejection marks and the routed profile so the next event routes afresh
func ClearIdentity(source *SessionSource) {
	source.identity = nil
	source.ProfileRouteRejected = false
	source.authorizationProfileHome = ""
	source.rejectedRouteProfile = nil
	source.Profile = nil
}

// Receiving bot profile to persist with a route, empty outside multiplexing or for an inferred messaging transport
func TransportProfileOf(source *SessionSource) string {
	identity := IdentityOf(source)
	if identity == nil || !identity.Multiplexed {
		return ""
	}
	if identity.TransportInferred && source.Platform != PlatformLocal {
		return ""
	}
	return identity.TransportProfile
}

// Copy of the source with its wire invisible provenance kept and the given edit applied
func ReplaceSource(source *SessionSource, edit func(*SessionSource)) *SessionSource {
	copied := *source
	if edit != nil {
		edit(&copied)
	}
	if source.transportAdapterRef != nil {
		copied.transportAdapterRef = source.transportAdapterRef
	}
	if source.authorizationProfileHome != "" {
		copied.authorizationProfileHome = source.authorizationProfileHome
	}
	if source.identity != nil {
		copied.identity = source.identity
	}
	return &copied
}

// Under multiplexing the event's runtime profile could not be established, the caller drops the event
type IdentityUnresolved struct{ Reason string }

func (e *IdentityUnresolved) Error() string { return e.Reason }

// Runner configuration reader the identity resolver consults for multiplexing
type identityConfigRunner interface {
	RunnerConfig() *GatewayConfig
}

// Runner naming its primary profile, empty when unset
type identityPrimaryRunner interface {
	PrimaryProfileName() string
}

// Runner naming the active profile when no primary is pinned
type identityActiveRunner interface {
	ActiveProfileName(ctx context.Context) string
}

// Runner naming the profile that registered an adapter for a platform
type identityOwningRunner interface {
	OwningProfile(adapter PlatformAdapter, platform Platform) (registered bool, profile string)
}

// Runner resolving the runtime home of a routed source with its missing profile fallback
type identityHomeRunner interface {
	ResolveProfileHomeForSource(ctx context.Context, source *SessionSource) (string, error)
}

// Stripped text of a profile name, empty for blank or absent names
func identityName(value *string) string {
	if value == nil {
		return ""
	}
	return pystr.Strip(*value)
}

// Whether the runner config enables multiplexed profiles
func identityMultiplexed(runner any) bool {
	reader, ok := runner.(identityConfigRunner)
	if !ok {
		return false
	}
	cfg := reader.RunnerConfig()
	return cfg != nil && cfg.MultiplexProfiles != nil && *cfg.MultiplexProfiles
}

// Primary profile name of the runner, else its active profile, else default
func identityPrimaryProfile(ctx context.Context, runner any, activeFallback bool) string {
	if r, ok := runner.(identityPrimaryRunner); ok {
		if name := pystr.Strip(r.PrimaryProfileName()); name != "" {
			return name
		}
	}
	if activeFallback {
		if r, ok := runner.(identityActiveRunner); ok {
			if name := pystr.Strip(r.ActiveProfileName(ctx)); name != "" {
				return name
			}
		}
	}
	return "default"
}

// Pinned identity of the source, else a fresh resolution, nil when the route is unresolved under multiplexing
func canonicalIdentity(ctx context.Context, source *SessionSource, runner TransportOwnerRunner, adapter PlatformAdapter, transportProfile string) (*RoutingIdentity, error) {
	if identity := IdentityOf(source); identity != nil {
		return identity, nil
	}
	identity, err := resolveIdentity(ctx, source, runner, adapter, transportProfile, "")
	var unresolved *IdentityUnresolved
	if errors.As(err, &unresolved) {
		slog.Debug(fmt.Sprintf("identity unresolved: %s", unresolved.Reason))
		return nil, nil
	}
	return identity, err
}

// Pins the identity of a source rebuilt from durable state without a live adapter, nil when nothing is restorable
func restoreIdentity(ctx context.Context, source *SessionSource, runner any, transportProfile *string) (*RoutingIdentity, error) {
	transportName := identityName(transportProfile)
	if transportName == "" || !identityMultiplexed(runner) {
		return nil, nil
	}
	if existing := IdentityOf(source); existing != nil {
		return existing, nil
	}
	primary := identityPrimaryProfile(ctx, runner, false)
	runtimeName := identityName(source.Profile)
	if runtimeName == "" {
		runtimeName = primary
	}
	authorizationHome := home.Process()
	if transportName != primary {
		dir, err := profiles.GetProfileDir(transportName)
		if err != nil {
			return nil, err
		}
		authorizationHome = dir
	}
	runtimeHome := authorizationHome
	if runtimeName != transportName {
		resolved, err := identityRuntimeHome(ctx, runner, source)
		if err != nil {
			return nil, err
		}
		runtimeHome = resolved
	}
	source.authorizationProfileHome = authorizationHome
	identity := &RoutingIdentity{TransportProfile: transportName, RuntimeProfile: runtimeName,
		AuthorizationHome: authorizationHome, RuntimeHome: runtimeHome, Multiplexed: true}
	source.identity = identity
	return identity, nil
}

// Runtime home of a routed source through the runner resolver
func identityRuntimeHome(ctx context.Context, runner any, source *SessionSource) (string, error) {
	resolver, ok := runner.(identityHomeRunner)
	if !ok {
		return "", fmt.Errorf("'%T' object has no attribute '_resolve_profile_home_for_source'", runner)
	}
	return resolver.ResolveProfileHomeForSource(ctx, source)
}

// Resolves and pins the routing identity of an inbound source, IdentityUnresolved when the route is rejected
func resolveIdentity(ctx context.Context, source *SessionSource, runner TransportOwnerRunner, adapter PlatformAdapter, transportProfile, primaryHome string) (*RoutingIdentity, error) {
	multiplexed := identityMultiplexed(runner)
	primary := identityPrimaryProfile(ctx, runner, true)
	platform := source.Platform
	ownerProfile := ""
	if adapter == nil {
		if owner, profile, ok := runner.TransportOwner(source); ok {
			adapter, ownerProfile = owner, pystr.Strip(profile)
		}
	} else {
		if source.transportAdapterRef == nil {
			source.transportAdapterRef = adapter
		}
		owning, ok := runner.(identityOwningRunner)
		if !ok {
			return nil, fmt.Errorf("'%T' object has no attribute '_owning_profile'", runner)
		}
		_, profile := owning.OwningProfile(adapter, platform)
		ownerProfile = pystr.Strip(profile)
	}
	explicit := pystr.Strip(transportProfile)
	transportName := explicit
	if transportName == "" {
		transportName = ownerProfile
	}
	if transportName == "" {
		transportName = primary
	}
	transportInferred := adapter == nil && explicit == "" && ownerProfile == ""
	var transport weak.Pointer[BasePlatformAdapter]
	if adapter != nil {
		transport = weak.Make(adapter.Base())
	}
	if !multiplexed {
		h := home.Dir(ctx)
		identity := &RoutingIdentity{TransportProfile: primary, RuntimeProfile: primary, AuthorizationHome: h,
			RuntimeHome: h, Transport: transport}
		source.identity = identity
		return identity, nil
	}
	authorizationHome := primaryHome
	if transportName == primary {
		if authorizationHome == "" {
			authorizationHome = home.Process()
		}
	} else {
		dir, err := profiles.GetProfileDir(transportName)
		if err != nil {
			return nil, err
		}
		authorizationHome = dir
	}
	source.authorizationProfileHome = authorizationHome
	where := fmt.Sprintf("%s/%s", string(platform), source.ChatID)
	if source.ProfileRouteRejected {
		return nil, &IdentityUnresolved{Reason: where + ": profile route rejected"}
	}
	if identityName(source.Profile) == "" {
		var adapterProfile *string
		if transportName != primary {
			name := transportName
			adapterProfile = &name
		}
		resolver, ok := runner.(SourceProfileResolver)
		if !ok {
			return nil, fmt.Errorf("'%T' object has no attribute '_profile_name_for_source'", runner)
		}
		routed, err := resolver.ProfileNameForSource(ctx, source, adapterProfile)
		var rejected *ProfileRouteRejected
		if errors.As(err, &rejected) {
			source.ProfileRouteRejected = true
			source.rejectedRouteProfile = nil
			if name, ok := rejected.Profile().(string); ok {
				source.rejectedRouteProfile = &name
			}
			return nil, &IdentityUnresolved{Reason: where + ": " + rejected.Error()}
		}
		if err != nil {
			return nil, err
		}
		if routed != nil && *routed != "" {
			source.Profile = routed
		} else {
			source.Profile = adapterProfile
		}
	}
	runtimeName := identityName(source.Profile)
	if runtimeName == "" {
		runtimeName = primary
	}
	runtimeHome := authorizationHome
	if runtimeName != transportName {
		resolved, err := identityRuntimeHome(ctx, runner, source)
		if err != nil {
			return nil, err
		}
		runtimeHome = resolved
	}
	identity := &RoutingIdentity{TransportProfile: transportName, RuntimeProfile: runtimeName,
		AuthorizationHome: authorizationHome, RuntimeHome: runtimeHome, Multiplexed: true, Transport: transport,
		TransportInferred: transportInferred}
	source.identity = identity
	return identity, nil
}

// Login subject prefix of a namespaced dashboard identity
const AuthSubjectPrefix = "auth:v1:"

// Internal server identity of hermes_cli dashboard_auth ws_tickets, never an authenticated login
const (
	wsInternalUserID   = "server-internal"
	wsInternalProvider = "server-internal"
)

// Provider, issuer and user of an auth v1 login subject, ok false for any other form
func ParseAuthenticatedSubject(subject any) (parts [3]string, ok bool) {
	text, isText := subject.(string)
	if !isText || !strings.HasPrefix(text, AuthSubjectPrefix) {
		return parts, false
	}
	decoded, err := pyjson.Loads(text[len(AuthSubjectPrefix):])
	if err != nil {
		return parts, false
	}
	list, isList := decoded.([]any)
	if !isList || len(list) != 3 {
		return parts, false
	}
	for i, item := range list {
		value, isStr := item.(string)
		if !isStr {
			return [3]string{}, false
		}
		parts[i] = value
	}
	return parts, true
}

// Stable subject of a private native identity, else the auth v1 namespaced form of a dashboard login
func AuthenticatedSubject(identity map[string]any) (string, error) {
	subject := "unbound"
	if value := identity["user_id"]; pyTruthy(value) {
		subject = pyStrOf(value)
	}
	_, hasCapabilities := identity["capabilities"]
	if identity["provider"] == "local" && pyTruthy(identity["profile_id"]) && pyTruthy(identity["instance_id"]) && hasCapabilities {
		return subject, nil
	}
	orEmpty := func(key string) any {
		if value := identity[key]; pyTruthy(value) {
			return value
		}
		return ""
	}
	encoded, err := pyjson.Dumps([]any{orEmpty("provider"), orEmpty("issuer"), subject}, pyjson.Separators(",", ":"), pyjson.EnsureASCII(true))
	if err != nil {
		return "", err
	}
	return AuthSubjectPrefix + encoded, nil
}

// Whether the identity is a server minted non internal user id and provider pair
func IsAuthenticatedIdentity(identity any) bool {
	fields, ok := identity.(map[string]any)
	if !ok {
		return false
	}
	userID, okUser := fields["user_id"].(string)
	provider, okProvider := fields["provider"].(string)
	if !okUser || pystr.Strip(userID) == "" || !okProvider || pystr.Strip(provider) == "" {
		return false
	}
	return !(userID == wsInternalUserID && provider == wsInternalProvider)
}

// Server derived dashboard principal id, the first 32 hex digits of the subject digest
func AuthenticatedPrincipalDigest(identity map[string]any) (string, error) {
	raw, err := AuthenticatedSubject(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(raw))
	return "principal:dashboard:" + hex.EncodeToString(sum[:])[:32], nil
}
