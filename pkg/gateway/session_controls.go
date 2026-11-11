package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Refusal carrying the revision and execution generation a mutation conflict reports
type revisionedRefusal interface {
	RefusalRevision() (revision int64, generation int64, ok bool)
}

// Refusal with the legacy wire code and message a connector or onboarding handler answers with
type wireCodedRefusal interface {
	WireCode() int
	WireMessage() string
}

// Refusal telling a bot relay sender whether anything was admitted
type admittedRefusal interface {
	Admitted() bool
}

// Error data of one runtime refusal, the reason plus the field, detail, revision and admitted flag it names
func refusalData(err error) map[string]any {
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		return map[string]any{}
	}
	data := map[string]any{"reason": refusal.Reason}
	if refusal.Field != nil && *refusal.Field != "" {
		data["field"] = *refusal.Field
	}
	if refusal.Detail != nil && *refusal.Detail != "" {
		data["detail"] = *refusal.Detail
	}
	var revisioned revisionedRefusal
	if errors.As(err, &revisioned) {
		if revision, generation, ok := revisioned.RefusalRevision(); ok {
			data["revision"], data["execution_generation"] = revision, generation
		}
	}
	var admitted admittedRefusal
	if errors.As(err, &admitted) && !admitted.Admitted() {
		data["admitted"] = false
	}
	return data
}

// Profile as a log line carries it, a named profile by name and a home path by its profile name
func profileLabel(profileID string) string {
	if profileID == "" {
		return "default"
	}
	if !strings.ContainsAny(profileID, "/\\") {
		return profileID
	}
	if name, ok := ProfileNameForHome(profileID); ok && name != "" {
		return name
	}
	return "default"
}

// Logs a refusal at its audience's level, routine bare codes at debug and a retried admission at info
func logRefusal(profile, method string, err error) {
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		return
	}
	field, detail := "", ""
	if refusal.Field != nil {
		field = *refusal.Field
	}
	if refusal.Detail != nil {
		detail = *refusal.Detail
	}
	line := fmt.Sprintf("%s refused for profile=%s: %s", method, profileLabel(profile), refusal.Reason)
	if field != "" {
		line += " field=" + field
	}
	if detail != "" {
		line += " detail=" + detail
	}
	switch {
	case refusal.Reason == "session_busy" || (field == "" && detail == ""):
		slog.Debug(line)
	case field == "request_id" && refusal.Reason == "admission_conflict":
		slog.Info(line)
	default:
		slog.Warn(line)
	}
}

// Method and error class pairs whose storage failure detail this process already logged
var (
	storageTracebacks   = map[[2]string]bool{}
	storageTracebacksMu sync.Mutex
)

// Logs a storage failure once with its detail per method and class, every failure keeping one capped line
func logStorageFailure(profile, method string, sessionID any, err error, class string) {
	storageTracebacksMu.Lock()
	key := [2]string{method, class}
	first := !storageTracebacks[key]
	storageTracebacks[key] = true
	storageTracebacksMu.Unlock()
	shown := pyrepr.Repr(sessionID)
	if length := pystr.Len(shown); length > 64 {
		shown = fmt.Sprintf("%s… (%d chars)", pystr.Slice(shown, 0, 48), length)
	}
	line := fmt.Sprintf("%s storage failure for profile=%s session_id=%s: %s", method, profileLabel(profile), shown, err)
	if first {
		slog.Warn(line, "exc_info", fmt.Sprintf("%s: %+v", class, err))
		return
	}
	slog.Warn(line)
}

// Short snake case label a detach may store as the session's end reason
var endReasonPattern = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

// Verbs whose closed param sets never carried a profile key
var profileImplicitMethods = map[string]bool{"session.create": true, "session.list": true, "session.info": true,
	"session.resume": true, "session.detach": true, "prompt.submit": true, "prompt.receipt": true, "prompt.cancel": true,
	"prompt.resolve_unknown": true, "prompt.steer_queued": true, "prompt.replace_queued": true, "session.interrupt": true,
	"session.events.since": true, "session.mutate": true, "session.stats": true, "approval.respond": true, "clarify.respond": true}

// Authenticated projection of one client socket onto the authority, its principal, subscriptions and sibling routes
type AuthorityConnection struct {
	authority     *SessionAuthority
	transport     tuigateway.Transport
	actor         Principal
	nativeOwner   bool
	mu            sync.Mutex
	subscriptions map[string]any
	replayCursors map[string]ReplayCursor
	handedBack    map[string]bool
	identity      map[string]any
	operator      bool
	siblings      map[string]*AuthorityConnection
}

// Connection of a verified identity, its capabilities emptied for a missing user or a foreign instance
func NewAuthorityConnection(a *SessionAuthority, transport tuigateway.Transport, identity map[string]any, operator bool) (*AuthorityConnection, error) {
	capabilities := map[string]bool{"session:read": true, "session:submit": true, "session:control": true,
		"session:approve": true, "session:respond": true}
	if len(identity) > 0 {
		capabilities["session:create"] = true
	}
	if granted, ok := identity["capabilities"]; ok {
		capabilities = map[string]bool{}
		items, err := pyIterable(granted)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			name, isText := item.(string)
			if !isText {
				return nil, &PyError{Kind: "TypeError", Msg: "unhashable capability " + pyrepr.Repr(item)}
			}
			capabilities[name] = true
		}
	}
	delete(capabilities, "session:operator")
	if operator {
		capabilities["session:operator"] = true
	}
	instance, hasInstance := identity["instance_id"]
	if !pyTruthy(identity["user_id"]) || (hasInstance && !pyrepr.Equal(instance, a.InstanceID)) {
		capabilities = map[string]bool{}
	}
	subject, err := PrincipalSubject(identity)
	if err != nil {
		return nil, err
	}
	profileID := a.ProfileID
	if value, ok := identity["profile_id"]; ok {
		profileID = pyStrOf(value)
	}
	c := &AuthorityConnection{authority: a, transport: transport,
		actor:         Principal{Subject: subject, ProfileID: profileID, Capabilities: capabilities, TransportID: uuidHex()},
		subscriptions: map[string]any{}, replayCursors: map[string]ReplayCursor{}, handedBack: map[string]bool{},
		identity: identity, operator: operator, siblings: map[string]*AuthorityConnection{}}
	bindNativeTransport(a, c.actor, identity)
	c.nativeOwner = identity["native_bootstrap"] == true
	a.mu.Lock()
	a.Events[c.actor.TransportID] = transport
	a.mu.Unlock()
	return c, nil
}

// Items of a list, tuple or set value, the TypeError of anything else
func pyIterable(value any) ([]any, error) {
	switch typed := value.(type) {
	case []any:
		return typed, nil
	case []string:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = item
		}
		return out, nil
	case map[string]bool:
		out := make([]any, 0, len(typed))
		for item := range typed {
			out = append(out, item)
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range typed {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(value) + "' object is not iterable"}
}

// Authority the connection acts on
func (c *AuthorityConnection) SessionAuthority() *SessionAuthority { return c.authority }

// Authority the connection acts on, for the finite turn checks
func (c *AuthorityConnection) ConnectionAuthority() *SessionAuthority { return c.authority }

// Authority the connection acts on, for the cron RPCs
func (c *AuthorityConnection) CronAuthority() *SessionAuthority { return c.authority }

// Principal the connection acts as
func (c *AuthorityConnection) Actor() Principal { return c.actor }

// Principal the connection acts as, for the cron RPCs
func (c *AuthorityConnection) CronActor() *Principal { return &c.actor }

// Whether the connection is the native desktop bootstrap owner
func (c *AuthorityConnection) NativeOwner() bool { return c.nativeOwner }

// Whether a verified transport gate granted the operator capability
func (c *AuthorityConnection) Operator() bool { return c.operator }

// Whether the grant acts for every profile this process serves, the desktop shared socket or the dashboard gate
func (c *AuthorityConnection) HostScoped() bool {
	return c.identity["profile_scope"] == "host"
}

// Served profile registry of the authority's runner, nil when the runner keeps none
func runnerRegistry(a *SessionAuthority) *SessionAuthorities {
	if runner, ok := a.Runner.(authorityRegistryRunner); ok {
		return runner.SessionAuthorities()
	}
	return nil
}

// Slot of a profile whose authority hot serve is still building
func registryStarting(ctx context.Context, registry *SessionAuthorities, profile string) (string, <-chan struct{}, bool, error) {
	key, started, found := registry.Starting(ctx, profile)
	return key, started, found, nil
}

// Connection bound to the sibling authority a profile param names, nil for this connection's own home
func (c *AuthorityConnection) siblingFor(ctx context.Context, profile string) (*AuthorityConnection, error) {
	if profiles.ProfileMatchesHome(ctx, profile, c.authority.ProfileID) {
		return nil, nil
	}
	if !c.HostScoped() {
		return nil, runtimeRefusal("profile_mismatch")
	}
	registry := runnerRegistry(c.authority)
	if registry != nil {
		for _, sibling := range registry.Authorities() {
			if sibling == c.authority || !profiles.ProfileMatchesHome(ctx, profile, filepath.Clean(sibling.ProfileID)) {
				continue
			}
			c.mu.Lock()
			connection := c.siblings[sibling.ProfileID]
			c.mu.Unlock()
			if connection == nil || connection.authority != sibling {
				identity := map[string]any{}
				for key, value := range c.identity {
					identity[key] = value
				}
				identity["profile_id"], identity["instance_id"] = sibling.ProfileID, sibling.InstanceID
				built, err := NewAuthorityConnection(sibling, c.transport, identity, c.operator)
				if err != nil {
					return nil, err
				}
				connection = built
				c.mu.Lock()
				c.siblings[sibling.ProfileID] = connection
				c.mu.Unlock()
			}
			return connection, nil
		}
		_, _, found, err := registryStarting(ctx, registry, profile)
		if err != nil {
			return nil, err
		}
		if found {
			field, detail := "profile", "the profile is still starting"
			return nil, state.NewRuntimeStoreError("runtime_draining", &field, &detail)
		}
	}
	return nil, runtimeRefusal("profile_mismatch")
}

// Hold of a host scoped frame naming a starting or held profile, its started signal nil for a held key
func (c *AuthorityConnection) AwaitsProfileStart(ctx context.Context, request map[string]any, held []string) (string, <-chan struct{}, bool, error) {
	params, _ := request["params"].(map[string]any)
	profile, _ := params["profile"].(string)
	registry := runnerRegistry(c.authority)
	if !c.HostScoped() || profile == "" || registry == nil {
		return "", nil, false, nil
	}
	for _, key := range held {
		if profiles.ProfileMatchesHome(ctx, profile, key) {
			return key, nil, true, nil
		}
	}
	return registryStarting(ctx, registry, profile)
}

// Liveness answer for a connection that holds any capability and passes no params
func (c *AuthorityConnection) ping(params map[string]any) (map[string]any, error) {
	if len(c.actor.Capabilities) == 0 {
		return nil, runtimeRefusal("permission_denied")
	}
	if len(params) > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	return map[string]any{"pong": true}, nil
}

// Binds a native desktop or host owner connection that may create sessions as a legacy transport of the authority
func bindNativeTransport(a *SessionAuthority, actor Principal, identity map[string]any) {
	native := identity["provider"] == "local" && pyrepr.Equal(identity["profile_id"], a.ProfileID) &&
		pyrepr.Equal(identity["instance_id"], a.InstanceID)
	if (native || actor.Subject == HostOwnerSubject()) && actor.ProfileID == a.ProfileID && actor.Capabilities["session:create"] {
		a.mu.Lock()
		if a.nativeLegacyTransports == nil {
			a.nativeLegacyTransports = map[string]Principal{}
		}
		a.nativeLegacyTransports[actor.TransportID] = actor
		a.mu.Unlock()
	}
}

// Forgets a closed connection's legacy transport binding
func unbindNativeTransport(a *SessionAuthority, actor Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.nativeLegacyTransports, actor.TransportID)
}
