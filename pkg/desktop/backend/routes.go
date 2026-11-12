package backend

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// WindowConnectionRoute is the connection and profile a renderer window last published
type WindowConnectionRoute struct {
	ConnectionID   *string `json:"connectionId"`
	Profile        *string `json:"profile,omitempty"`
	RegistryScoped bool    `json:"registryScoped"`
}

func (r *WindowConnectionRoute) connectionID() string {
	if r == nil || r.ConnectionID == nil {
		return ""
	}
	return *r.ConnectionID
}

func (r *WindowConnectionRoute) profile() string {
	if r == nil || r.Profile == nil {
		return ""
	}
	return *r.Profile
}

// RegistrySource is one connection of the desktop connections registry
type RegistrySource struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// ConnectionRegistry is the registry view the backend handlers read
type ConnectionRegistry struct {
	Primary     string           `json:"primary"`
	Connections []RegistrySource `json:"connections"`
}

// Route a window published, nil when the value is not an object
func NormalizeWindowConnectionRoute(value any) *WindowConnectionRoute {
	input, ok := value.(map[string]any)
	if !ok || input == nil {
		return nil
	}
	route := &WindowConnectionRoute{RegistryScoped: input["registryScoped"] == true}
	if id, ok := input["connectionId"].(string); ok && strings.TrimSpace(id) != "" {
		trimmed := strings.TrimSpace(id)
		route.ConnectionID = &trimmed
	}
	if p, ok := input["profile"].(string); ok && strings.TrimSpace(p) != "" {
		trimmed := strings.TrimSpace(p)
		route.Profile = &trimmed
	}
	return route
}

// SSH scope of a registry scoped window route on an SSH source, empty otherwise
func RegistrySSHScopeForWindowRoute(route *WindowConnectionRoute, registry ConnectionRegistry) (string, bool) {
	if route == nil || !route.RegistryScoped || route.connectionID() == "" {
		return "", false
	}
	for _, source := range registry.Connections {
		if source.ID == route.connectionID() {
			if source.Kind != "ssh" {
				return "", false
			}
			return BackendScopeKey(route.connectionID(), route.profile()), true
		}
	}
	return "", false
}

// Window route a primary apply re-points to, registry scoped only for a non local primary
func AppliedPrimaryWindowRoute(registry ConnectionRegistry, profile string) WindowConnectionRoute {
	key := strings.TrimSpace(profile)
	if key == "" {
		key = "default"
	}
	primary := strings.TrimSpace(registry.Primary)
	if primary != "" && primary != LocalConnectionID {
		return WindowConnectionRoute{ConnectionID: &primary, Profile: &key, RegistryScoped: true}
	}
	return WindowConnectionRoute{Profile: &key}
}

// RegistrySSHPoolEntry is the part of a pool entry the SSH scope lookup reads
type RegistrySSHPoolEntry struct {
	Scope                string
	RegistryConnectionID string
	HasSSH               bool
}

// Pool key a registry SSH tunnel was published under, false when no live tunnel names the connection
func RegistrySSHPoolScopeByConnectionID(entries []RegistrySSHPoolEntry, connectionID string) (string, bool) {
	for _, e := range entries {
		if e.HasSSH && e.RegistryConnectionID == connectionID {
			return e.Scope, true
		}
	}
	return "", false
}

// VersionScope is the connection and profile the about panel version reads
type VersionScope struct {
	ConnectionID string `json:"connectionId,omitempty"`
	Profile      string `json:"profile,omitempty"`
}

// Version scope of a window route, the connection only when registry scoped
func WindowRouteVersionScope(route *WindowConnectionRoute) VersionScope {
	if route == nil {
		return VersionScope{}
	}
	var scope VersionScope
	if route.RegistryScoped && route.connectionID() != "" {
		scope.ConnectionID = route.connectionID()
	}
	scope.Profile = route.profile()
	return scope
}

// WindowConnectionRoutes holds the active route each renderer window published
type WindowConnectionRoutes struct {
	mu     sync.Mutex
	routes map[string]*WindowConnectionRoute
}

// Empty window route table
func NewWindowConnectionRoutes() *WindowConnectionRoutes {
	return &WindowConnectionRoutes{routes: map[string]*WindowConnectionRoute{}}
}

// Records the route a window published, an invalid value clearing only that window
func (w *WindowConnectionRoutes) Set(windowID string, value any) *WindowConnectionRoute {
	route := NormalizeWindowConnectionRoute(value)
	w.mu.Lock()
	defer w.mu.Unlock()
	if route == nil {
		delete(w.routes, windowID)
		return nil
	}
	w.routes[windowID] = route
	return route
}

// Route a window published, nil when none
func (w *WindowConnectionRoutes) Get(windowID string) *WindowConnectionRoute {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.routes[windowID]
}

// Forgets the route of a closed window
func (w *WindowConnectionRoutes) Delete(windowID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.routes, windowID)
}

// Version scope of a live window route, empty for a destroyed or unknown window
func (w *WindowConnectionRoutes) VersionScopeFor(windowID string, live bool) VersionScope {
	if !live {
		return VersionScope{}
	}
	return WindowRouteVersionScope(w.Get(windowID))
}

// Window state the calling window reports, the fallback window when the caller has none
func LiveWindowState(caller, fallback func() (map[string]any, bool)) map[string]any {
	if caller != nil {
		if state, ok := caller(); ok {
			return state
		}
	}
	if fallback != nil {
		if state, ok := fallback(); ok {
			return state
		}
	}
	return nil
}

// Descriptor copy with the live window state laid over it
func OverlayWindowState(connection Connection, windowState map[string]any) Connection {
	out := connection.Clone()
	for k, v := range windowState {
		out[k] = v
	}
	return out
}

// PrimaryBootRequest is the profile key a primary boot runs and whether it follows the primary
type PrimaryBootRequest struct {
	Key             string `json:"key"`
	InheritsPrimary bool   `json:"inheritsPrimary"`
}

// Boot request for a renderer profile, an unnamed one inheriting the primary key
func NewPrimaryBootRequest(profile *string, primaryKey string, inheritsPrimary bool) PrimaryBootRequest {
	named := ""
	if profile != nil {
		named = strings.TrimSpace(*profile)
	}
	if named == "" {
		return PrimaryBootRequest{Key: primaryKey, InheritsPrimary: true}
	}
	return PrimaryBootRequest{Key: named, InheritsPrimary: inheritsPrimary}
}

// ProfileBackendRoute names which backend serves a profile and the profile its descriptor carries
type ProfileBackendRoute struct {
	Backend           string `json:"backend"`
	DescriptorProfile string `json:"descriptorProfile,omitempty"`
	ScopePath         bool   `json:"scopePath"`
}

// JoinedBoot is the primary connection a request joined with the key and route it settled on
type JoinedBoot struct {
	Connection Connection
	Descriptor Connection
	Key        string
	Route      ProfileBackendRoute
}

// Starts or joins the primary boot and labels the descriptor with the profile the boot settled on
func JoinPrimaryBoot(request PrimaryBootRequest, route ProfileBackendRoute, primaryKey func() string, resolveRoute func(string) ProfileBackendRoute, start func(PrimaryBootRequest) (Connection, error)) (JoinedBoot, error) {
	connection, err := start(request)
	if err != nil {
		return JoinedBoot{}, err
	}
	key := request.Key
	if request.InheritsPrimary {
		key = primaryKey()
	}
	settled := route
	if key != request.Key {
		settled = resolveRoute(key)
	}
	descriptor := connection.Clone()
	if settled.DescriptorProfile != "" {
		descriptor["profile"] = settled.DescriptorProfile
		descriptor["sharedPrimary"] = true
	} else {
		descriptor["profile"] = key
	}
	return JoinedBoot{Connection: connection, Descriptor: descriptor, Key: key, Route: settled}, nil
}

// PrimaryProfilePin keeps a live primary answering for the profile it booted
type PrimaryProfilePin struct {
	mu     sync.Mutex
	booted string
}

// Pins the booted profile, default when blank
func (p *PrimaryProfilePin) Pin(profile string) string {
	value := strings.TrimSpace(profile)
	if value == "" {
		value = "default"
	}
	p.mu.Lock()
	p.booted = value
	p.mu.Unlock()
	return value
}

// Releases the pin so the next start follows the preference
func (p *PrimaryProfilePin) Clear() {
	p.mu.Lock()
	p.booted = ""
	p.mu.Unlock()
}

// Booted profile, empty when nothing is pinned
func (p *PrimaryProfilePin) Booted() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.booted
}

// Pinned profile, else the remembered preference, else default
func (p *PrimaryProfilePin) Resolve(readPreference func() string) string {
	if booted := p.Booted(); booted != "" {
		return booted
	}
	if v := strings.TrimSpace(readPreference()); v != "" {
		return v
	}
	return "default"
}

// LaunchProfile is the one launch decision routing, argv and env derive from
type LaunchProfile struct {
	ArgvProfile    string
	RoutingProfile string
}

// Launch decision from one preference read, the argv profile empty when none is remembered
func ResolveLaunchProfile(readPreference func() string) LaunchProfile {
	argv := strings.TrimSpace(readPreference())
	routing := argv
	if routing == "" {
		routing = "default"
	}
	return LaunchProfile{ArgvProfile: argv, RoutingProfile: routing}
}

// FirstRunSetupResetError rejects a startup whose first run choice was reset before it completed
type FirstRunSetupResetError struct{}

func (FirstRunSetupResetError) Error() string {
	return "First-run setup was reset before a choice completed."
}

// FirstRunSetupReset marks the error for the boot failure handling
func (FirstRunSetupResetError) FirstRunSetupReset() bool { return true }

// PrimaryStartupKind names the outcome of a primary backend startup
type PrimaryStartupKind string

const (
	StartupRemote   PrimaryStartupKind = "remote"
	StartupLocal    PrimaryStartupKind = "local"
	StartupAttached PrimaryStartupKind = "attached"
)

// PrimaryStartupResult is a remote connection, a prepared local runtime or an attached host backend
type PrimaryStartupResult[B, C any] struct {
	Kind       PrimaryStartupKind
	Backend    B
	Connection C
	Attached   C
}

// PrimaryStartupOptions are the steps a primary startup runs in order
type PrimaryStartupOptions[B, R, C any] struct {
	Context               context.Context
	AssertCurrentAttempt  func() error
	AttachHostBackend     func() (*C, error)
	ConnectRemote         func(R) (C, error)
	EnsureLocalRuntime    func(B) (B, error)
	PrepareLocalBackend   func() (B, error)
	ResolveRemote         func() (*R, error)
	SelectRegistryPrimary func() (*R, error)
	WaitForDecision       func(B) (string, error)
	WaitForLocalStart     func() error
}

// Resolves a saved remote, else waits out an update, selects a registry primary, attaches or prepares local
func RunPrimaryBackendStartup[B, R, C any](o PrimaryStartupOptions[B, R, C]) (PrimaryStartupResult[B, C], error) {
	var none PrimaryStartupResult[B, C]
	step := func(run func() (any, error)) (any, error) {
		out, err := RunBackendStartStep(o.Context, run)
		if err != nil {
			return nil, err
		}
		if err := o.AssertCurrentAttempt(); err != nil {
			return nil, err
		}
		return out, nil
	}
	connect := func(remote R) (PrimaryStartupResult[B, C], error) {
		out, err := step(func() (any, error) { return o.ConnectRemote(remote) })
		if err != nil {
			return none, err
		}
		return PrimaryStartupResult[B, C]{Kind: StartupRemote, Connection: out.(C)}, nil
	}
	resolve := func(f func() (*R, error)) (*R, error) {
		out, err := step(func() (any, error) { return f() })
		if err != nil {
			return nil, err
		}
		return out.(*R), nil
	}
	saved, err := resolve(o.ResolveRemote)
	if err != nil {
		return none, err
	}
	if saved != nil {
		return connect(*saved)
	}
	if _, err := step(func() (any, error) { return nil, o.WaitForLocalStart() }); err != nil {
		return none, err
	}
	if o.SelectRegistryPrimary != nil {
		primary, err := resolve(o.SelectRegistryPrimary)
		if err != nil {
			return none, err
		}
		if primary != nil {
			return connect(*primary)
		}
	}
	if o.AttachHostBackend != nil {
		out, err := step(func() (any, error) { return o.AttachHostBackend() })
		if err != nil {
			return none, err
		}
		if attached := out.(*C); attached != nil {
			return PrimaryStartupResult[B, C]{Kind: StartupAttached, Attached: *attached}, nil
		}
	}
	prepared, err := step(func() (any, error) { return o.PrepareLocalBackend() })
	if err != nil {
		return none, err
	}
	backend := prepared.(B)
	decision, err := step(func() (any, error) { return o.WaitForDecision(backend) })
	if err != nil {
		return none, err
	}
	switch decision.(string) {
	case "remote-applied":
		applied, err := resolve(o.ResolveRemote)
		if err != nil {
			return none, err
		}
		if applied == nil {
			return none, errors.New("First-run remote setup completed without a saved remote backend.")
		}
		return connect(*applied)
	case "reset":
		return none, FirstRunSetupResetError{}
	}
	ensured, err := step(func() (any, error) { return o.EnsureLocalRuntime(backend) })
	if err != nil {
		return none, err
	}
	return PrimaryStartupResult[B, C]{Kind: StartupLocal, Backend: ensured.(B)}, nil
}
