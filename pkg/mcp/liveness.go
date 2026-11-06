package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Liveness is one portable plugin liveness declaration, kind static, server_json or interactive_session
type Liveness struct {
	Kind       string
	Path       string
	URLField   string
	TokenField string
	PIDField   string
}

// Declaration with the default runtime file field names
func newLiveness(kind string) Liveness {
	return Liveness{Kind: kind, URLField: "http", TokenField: "token", PIDField: "pid"}
}

// AppDefinition is a platform app definition plus the liveness keys its resolver reads
type AppDefinition struct {
	Platform         any
	LivenessKind     string
	LivenessPath     string
	LivenessPIDKey   string
	LivenessURLKey   string
	LivenessTokenKey string
}

// Definition with the runtime file liveness keys applied, unchanged for other kinds
func (l Liveness) AppDefinition(def AppDefinition) AppDefinition {
	if l.Kind != "server_json" {
		return def
	}
	def.LivenessKind = "server_json"
	def.LivenessPath = l.Path
	def.LivenessPIDKey = l.PIDField
	def.LivenessURLKey = l.URLField
	def.LivenessTokenKey = l.TokenField
	return def
}

// AppDeclaration is the registered platform declaration of an application backed server
type AppDeclaration struct {
	Name        string
	RequiredGPU string
	AppFor      func(osFamily string) (AppDefinition, bool)
	Platform    any
}

// AppAvailability is the install time availability verdict of a declaration
type AppAvailability struct {
	State      string
	MinVersion string
	Version    string
	Path       string
}

// AppProbe is a local resolver probe, Running nil when unknown
type AppProbe struct {
	Running         *bool
	EndpointPresent bool
}

// appPlatform is the declaration registry and app resolver surface liveness reads
type appPlatform interface {
	Declaration(serverName string) *AppDeclaration
	Availability(decl *AppDeclaration) AppAvailability
	GPULabel(gpu string) string
	ProbeLocal(def AppDefinition) AppProbe
	Endpoint(def AppDefinition) (url, token string, ok bool)
}

// LiveEndpointUnavailableError is a declared runtime file without a usable live endpoint, a connection error
type LiveEndpointUnavailableError struct{ Msg string }

func (e *LiveEndpointUnavailableError) Error() string { return e.Msg }

// LivenessStatus is the current unavailable state of a declared server with its one user action
type LivenessStatus struct {
	State        string
	Availability AppAvailability
	Liveness     Liveness
	UserAction   string
	Retry        string
}

// Errors when a liveness declaration does not follow the portable plugin contract
func ParseLiveness(raw any) (Liveness, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Liveness{}, errors.New("liveness must be an object")
	}
	onlyKind := len(m) == 1
	switch kind := m["kind"]; kind {
	case "static":
		if !onlyKind {
			return Liveness{}, errors.New("static liveness only accepts 'kind'")
		}
		return newLiveness("static"), nil
	case "interactive_session":
		if !onlyKind {
			return Liveness{}, errors.New("interactive_session liveness only accepts 'kind'")
		}
		return newLiveness("interactive_session"), nil
	case "server_json":
	default:
		return Liveness{}, fmt.Errorf("unknown liveness kind: %s", pyRepr(kind))
	}
	for key := range m {
		if key != "kind" && key != "path" && key != "fields" {
			return Liveness{}, errors.New("server_json liveness has unknown fields")
		}
	}
	fields := map[string]any{"url": "http", "token": "token", "pid": "pid"}
	if raw := m["fields"]; pyTruthy(raw) {
		overrides, ok := raw.(map[string]any)
		if !ok {
			return Liveness{}, fmt.Errorf("'%s' object is not a mapping", pyTypeName(raw))
		}
		for k, v := range overrides {
			fields[k] = v
		}
	}
	path, ok := m["path"].(string)
	if !ok || pyStrip(path) == "" {
		return Liveness{}, errors.New("server_json liveness requires a non-empty path")
	}
	if len(fields) != 3 {
		return Liveness{}, errors.New("server_json liveness fields may only override url, token, and pid")
	}
	keys := make([]string, 0, 3)
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := fields[k].(string); !ok || pyStrip(s) == "" {
			return Liveness{}, errors.New("server_json liveness field names must be non-empty strings")
		}
	}
	return Liveness{Kind: "server_json", Path: pyStrip(path), URLField: pyStrip(fields["url"].(string)),
		TokenField: pyStrip(fields["token"].(string)), PIDField: pyStrip(fields["pid"].(string))}, nil
}

// Registered liveness of a server, static when none is registered or the declaration is invalid
func livenessFor(ctx context.Context, serverName string) Liveness {
	raw, ok := agentPluginLiveness(ctx, serverName)
	if !ok {
		return newLiveness("static")
	}
	live, err := ParseLiveness(raw)
	if err != nil {
		mcpLogger().Warn(fmt.Sprintf("MCP server '%s' has an invalid liveness declaration (%s); treating it as static", serverName, err.Error()))
		return newLiveness("static")
	}
	return live
}

// Kind of the registered liveness of a server
func livenessKind(ctx context.Context, serverName string) string {
	return livenessFor(ctx, serverName).Kind
}

// User action sentence and retry policy for an unavailable state
func livenessAction(state, appName, gpuLabel string) (string, string) {
	switch state {
	case "app_not_running":
		return fmt.Sprintf("Start %s, then try again.", appName), "after_user_action"
	case "icebath_not_connected":
		return fmt.Sprintf("Reconnect %s in Icebath, then try again.", appName), "after_user_action"
	case "endpoint_unavailable":
		return fmt.Sprintf("Open %s and enable its local connection, then try again.", appName), "after_user_action"
	case "no_interactive_session":
		return fmt.Sprintf("Open an interactive desktop session and start %s, then try again.", appName), "never_here"
	case "version_too_old":
		return fmt.Sprintf("Update %s, then try again.", appName), "after_user_action"
	case "missing_app":
		return fmt.Sprintf("Install %s, then try again.", appName), "after_user_action"
	case "unsupported_gpu":
		return fmt.Sprintf("Use %s on a machine with %s.", appName, gpuLabel), "never_here"
	}
	panic(fmt.Sprintf("KeyError: %s", pyReprStr(state)))
}

// Availability states that make a declared app unavailable and the state each maps to
var availabilityProblems = map[string]string{
	"missing_app":     "missing_app",
	"unsupported_os":  "missing_app",
	"unsupported_gpu": "unsupported_gpu",
	"version_too_old": "version_too_old",
}

// Unavailable state an install time availability verdict maps to, empty when it is not a problem
func availabilityProblem(available AppAvailability) string {
	return availabilityProblems[available.State]
}

// Current unavailable state of a registered declaration, nil when the server declares no app
func livenessStatus(ctx context.Context, serverName string) *LivenessStatus {
	platform := platformApps()
	decl := platform.Declaration(serverName)
	if decl == nil {
		return nil
	}
	available := platform.Availability(decl)
	live := livenessFor(ctx, serverName)
	state := availabilityProblem(available)
	switch {
	case state != "":
	case live.Kind == "interactive_session" && !hostinfo.InteractiveSession():
		state = "no_interactive_session"
	case live.Kind == "server_json":
		definition, found := decl.AppFor(hostinfo.OSFamily())
		if !found {
			state = "missing_app"
			break
		}
		probe := platform.ProbeLocal(live.AppDefinition(definition))
		switch {
		case probe.Running == nil || !*probe.Running:
			state = "app_not_running"
		case !probe.EndpointPresent:
			state = "endpoint_unavailable"
		default:
			state = "icebath_not_connected"
		}
	default:
		state = "icebath_not_connected"
	}
	action, retry := livenessAction(state, decl.Name, platform.GPULabel(decl.RequiredGPU))
	return &LivenessStatus{State: state, Availability: available, Liveness: live, UserAction: action, Retry: retry}
}

// One unavailable state sentence with one user action, displayName overriding the declaration slug
func describeUnavailable(decl *AppDeclaration, available AppAvailability, state, displayName string) string {
	appName := displayName
	if appName == "" {
		appName = decl.Name
	}
	gpu := platformApps().GPULabel(decl.RequiredGPU)
	action, _ := livenessAction(state, appName, gpu)
	var reason string
	switch {
	case state == "missing_app" && available.State == "unsupported_os":
		reason = appName + " is not supported on this operating system."
		action = "Use a supported operating system."
	case state == "missing_app":
		reason = appName + " is not installed."
		if available.MinVersion != "" {
			action = fmt.Sprintf("Install %s %s or newer, then try again.", appName, available.MinVersion)
		}
	case state == "unsupported_gpu":
		reason = fmt.Sprintf("%s needs %s; none was found on this machine.", appName, gpu)
	case state == "version_too_old":
		found, minimum := "", ""
		if available.Version != "" {
			found = " version " + available.Version
		}
		if available.MinVersion != "" {
			minimum = fmt.Sprintf("; version %s or newer is required", available.MinVersion)
		}
		reason = fmt.Sprintf("%s%s is too old%s.", appName, found, minimum)
	case state == "no_interactive_session":
		reason = appName + " needs an interactive desktop session."
	case state == "endpoint_unavailable":
		reason = appName + "'s local endpoint is unavailable."
	case state == "icebath_not_connected":
		reason = appName + "'s MCP connection is missing."
	default:
		reason = appName + " is not running."
	}
	return reason + " " + action
}

// Declaration, structured state and composed sentence of an unavailable declared server, ok false when undeclared
func unavailableDetails(ctx context.Context, serverName string) (*AppDeclaration, *LivenessStatus, string, bool) {
	decl := platformApps().Declaration(serverName)
	current := livenessStatus(ctx, serverName)
	if decl == nil || current == nil {
		return nil, nil, "", false
	}
	return decl, current, describeUnavailable(decl, current.Availability, current.State, ""), true
}

// Live URL and bearer header of a runtime file server, ok false for other kinds, an unavailable error when unusable
func liveEndpoint(ctx context.Context, serverName string) (url string, headers map[string]string, ok bool, err error) {
	live := livenessFor(ctx, serverName)
	if live.Kind != "server_json" {
		return "", nil, false, nil
	}
	platform := platformApps()
	token, found := "", false
	if decl := platform.Declaration(serverName); decl != nil {
		if definition, has := decl.AppFor(hostinfo.OSFamily()); has {
			url, token, found = platform.Endpoint(live.AppDefinition(definition))
		}
	}
	if !found {
		return "", nil, false, &LiveEndpointUnavailableError{Msg: fmt.Sprintf("MCP server '%s' has no usable live endpoint", serverName)}
	}
	headers = map[string]string{}
	if token != "" {
		tools.RegisterVaultRedactionValueIn(ctx, token)
		headers["Authorization"] = "Bearer " + token
	}
	return url, headers, true, nil
}

// toolsPlatform is the hermes_platform declaration registry and app resolver of pkg/tools
type toolsPlatform struct{}

// Declaration registry and app resolver liveness reads
func platformApps() appPlatform { return toolsPlatform{} }

// Installs the single hook run after every declaration registry mutation
func onAppDeclarationChange(fn func()) { tools.SetPlatformDeclarationOnChange(fn) }

// Registered declaration of a server, nil when none
func (toolsPlatform) Declaration(serverName string) *AppDeclaration {
	decl := tools.LookupPlatformDeclaration(serverName)
	if decl == nil {
		return nil
	}
	gpu := ""
	if g := decl.RequiredGPU(); g != nil {
		gpu = *g
	}
	return &AppDeclaration{Name: decl.Name, RequiredGPU: gpu, Platform: decl, AppFor: func(osFamily string) (AppDefinition, bool) {
		def, ok := decl.AppFor(osFamily)
		if !ok {
			return AppDefinition{}, false
		}
		return AppDefinition{Platform: def, LivenessKind: def.LivenessKind, LivenessPath: def.LivenessPath,
			LivenessPIDKey: def.LivenessPIDKey, LivenessURLKey: def.LivenessURLKey, LivenessTokenKey: def.LivenessTokenKey}, true
	}}
}

// Install time availability of a declaration, a bare one carrying only its GPU requirement
func (toolsPlatform) Availability(decl *AppDeclaration) AppAvailability {
	pd, ok := decl.Platform.(*tools.PlatformDeclaration)
	if !ok {
		pd = &tools.PlatformDeclaration{Name: decl.Name}
		if decl.RequiredGPU != "" {
			gpu := decl.RequiredGPU
			pd.Requires.GPU = &gpu
		}
	}
	a := tools.PlatformAvailabilityOf(pd, "")
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return AppAvailability{State: a.State, MinVersion: deref(a.MinVersion), Version: deref(a.Version), Path: deref(a.Path)}
}

func (toolsPlatform) GPULabel(gpu string) string { return tools.PlatformGPULabel(gpu) }

// Platform definition with the liveness keys the declared runtime file applies
func platformDefinition(def AppDefinition) tools.PlatformAppDef {
	pd, _ := def.Platform.(tools.PlatformAppDef)
	pd.LivenessKind, pd.LivenessPath = def.LivenessKind, def.LivenessPath
	pd.LivenessPIDKey, pd.LivenessURLKey, pd.LivenessTokenKey = def.LivenessPIDKey, def.LivenessURLKey, def.LivenessTokenKey
	return pd
}

// Local probe of the runtime file, Running set only when the pid is alive, an OverflowError propagating
func (toolsPlatform) ProbeLocal(def AppDefinition) AppProbe {
	r := tools.PlatformAppResolver{Definition: platformDefinition(def)}
	p, err := r.Probe(r.Locate(nil), tools.EffortLocal, 3.0)
	if err != nil {
		panic(err)
	}
	var probe AppProbe
	if p.Running.Value != nil && *p.Running.Value {
		running := true
		probe.Running = &running
	}
	probe.EndpointPresent = p.Endpoint.State == "present"
	return probe
}

// Current validated endpoint and token of the runtime file, an OverflowError propagating
func (toolsPlatform) Endpoint(def AppDefinition) (url, token string, ok bool) {
	ep, ok, err := tools.PlatformAppResolver{Definition: platformDefinition(def)}.Endpoint()
	if err != nil {
		panic(err)
	}
	return ep.URL, ep.Token, ok
}
