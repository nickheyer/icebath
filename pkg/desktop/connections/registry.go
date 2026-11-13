package connections

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// RegistryVersion is the connections.json schema version
const RegistryVersion = 2

// LocalConnectionID is the reserved id of this machine's own runtime
const LocalConnectionID = backend.LocalConnectionID

// RegistryQuarantineCap bounds the malformed entries kept from one load
const RegistryQuarantineCap = 20

const labelMax = 64

// RegistryConnection is one named agent source of the registry
type RegistryConnection struct {
	ID               string                    `json:"id"`
	Kind             string                    `json:"kind"`
	Label            string                    `json:"label"`
	URL              string                    `json:"url,omitempty"`
	AuthMode         string                    `json:"authMode,omitempty"`
	Token            any                       `json:"token,omitempty"`
	Headers          map[string]SecretEnvelope `json:"headers,omitempty"`
	Org              string                    `json:"org,omitempty"`
	Name             string                    `json:"name,omitempty"`
	Host             string                    `json:"host,omitempty"`
	User             string                    `json:"user,omitempty"`
	Port             int                       `json:"port,omitempty"`
	KeyPath          string                    `json:"keyPath,omitempty"`
	RemoteHermesPath string                    `json:"remoteHermesPath,omitempty"`
	RemoteProfile    string                    `json:"remoteProfile,omitempty"`
}

// QuarantinedRegistryEntry is a malformed entry kept verbatim instead of dropped
type QuarantinedRegistryEntry struct {
	Reason string `json:"reason"`
	Entry  any    `json:"entry"`
}

// ConnectionRegistry is the persisted connections.json document
type ConnectionRegistry struct {
	Version     int                        `json:"version"`
	Primary     string                     `json:"primary"`
	LaunchMode  string                     `json:"launchMode"`
	LastUsed    string                     `json:"lastUsed"`
	Connections []RegistryConnection       `json:"connections"`
	Quarantined []QuarantinedRegistryEntry `json:"quarantined,omitempty"`
}

// Find returns the connection with the id, nil when absent
func (r ConnectionRegistry) Find(id string) *RegistryConnection {
	for i := range r.Connections {
		if r.Connections[i].ID == id {
			return &r.Connections[i]
		}
	}
	return nil
}

func (r ConnectionRegistry) has(id string) bool { return r.Find(id) != nil }

func (r ConnectionRegistry) ids() []string {
	out := make([]string, len(r.Connections))
	for i, c := range r.Connections {
		out[i] = c.ID
	}
	return out
}

func (r ConnectionRegistry) labels() []string {
	out := make([]string, len(r.Connections))
	for i, c := range r.Connections {
		out[i] = c.Label
	}
	return out
}

// Copy with its own connections slice so edits never alias the source
func (r ConnectionRegistry) clone() ConnectionRegistry {
	out := r
	out.Connections = append([]RegistryConnection(nil), r.Connections...)
	out.Quarantined = append([]QuarantinedRegistryEntry(nil), r.Quarantined...)
	if len(out.Quarantined) == 0 {
		out.Quarantined = nil
	}
	return out
}

// LabelKey is the case insensitive identity of a connection label
func LabelKey(label string) string { return strings.ToLower(strings.TrimSpace(label)) }

// UniqueLabel suffixes a label with the first free number among the taken labels
func UniqueLabel(candidate string, taken []string) string {
	used := map[string]bool{}
	for _, l := range taken {
		used[LabelKey(l)] = true
	}
	base := []rune(strings.TrimSpace(candidate))
	if len(base) > labelMax-4 {
		base = base[:labelMax-4]
	}
	if !used[LabelKey(string(base))] {
		return string(base)
	}
	for n := 2; ; n++ {
		suffixed := string(base) + " " + strconv.Itoa(n)
		if !used[LabelKey(suffixed)] {
			return suffixed
		}
	}
}

var (
	nonSlug     = regexp.MustCompile(`[^a-z0-9]+`)
	edgeHyphens = regexp.MustCompile(`^-+|-+$`)
)

// LabelSlug is the URL safe form of a label, connection when nothing survives
func LabelSlug(label string) string {
	slug := edgeHyphens.ReplaceAllString(nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(label)), "-"), "")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	if slug == "" {
		return "connection"
	}
	return slug
}

// AgentHandle is the profile name, suffixed with the connection slug when another source shares it
func AgentHandle(profile, connectionLabel string, duplicated bool) string {
	name := strings.TrimSpace(profile)
	if name == "" {
		name = "default"
	}
	if duplicated {
		return name + "-" + LabelSlug(connectionLabel)
	}
	return name
}

// RegistryLocalRoute is how a registry local dial resolves
type RegistryLocalRoute struct {
	Delegate bool   `json:"delegate"`
	PoolKey  string `json:"poolKey"`
	Refuse   string `json:"refuse,omitempty"`
}

// ResolvedConnectionSSHDescriptor is the SSH part of a resolved backend descriptor
type ResolvedConnectionSSHDescriptor struct {
	EffectiveConfigFingerprint string `json:"effectiveConfigFingerprint,omitempty"`
	Host                       string `json:"host,omitempty"`
	KeyPath                    string `json:"keyPath,omitempty"`
	Port                       int    `json:"port,omitempty"`
	RemoteHermesPath           string `json:"remoteHermesPath,omitempty"`
	RemoteProfile              string `json:"remoteProfile,omitempty"`
	User                       string `json:"user,omitempty"`
}

// ResolvedConnectionID is the registry id a backend descriptor came from, empty when none or ambiguous
func ResolvedConnectionID(registry ConnectionRegistry, descriptor map[string]any) string {
	if explicit, ok := descriptor["connectionId"]; ok {
		id, isString := explicit.(string)
		if !isString || strings.TrimSpace(id) == "" || !registry.has(id) {
			return ""
		}
		return id
	}
	switch descriptor["mode"] {
	case "local":
		var locals []string
		for _, c := range registry.Connections {
			if c.Kind == "local" {
				locals = append(locals, c.ID)
			}
		}
		if len(locals) == 1 {
			return locals[0]
		}
		return ""
	case "remote":
	default:
		return ""
	}
	if descriptor["remoteKind"] == "ssh" {
		if raw, ok := descriptor["ssh"]; ok {
			sshObj := jsObject(raw)
			if sshObj == nil {
				return ""
			}
			return MatchingConnectionID(registry, jsSpread(sshObj, map[string]any{"kind": "ssh"}), "unique")
		}
		sshCfg := NormalizeSSHConfig(map[string]any{"mode": "ssh", "host": descriptor["remoteHost"]})
		if sshCfg == nil {
			return ""
		}
		target := normalizedSSHTarget(sshCfg.object())
		matches := 0
		for _, c := range registry.Connections {
			if c.Kind == "ssh" && normalizedSSHTarget(c.object()) == target {
				matches++
			}
		}
		if target == "" || matches != 1 {
			return ""
		}
		return MatchingConnectionID(registry, jsSpread(sshCfg.object(), map[string]any{"kind": "ssh"}), "unique")
	}
	kind := ""
	switch descriptor["remoteKind"] {
	case "cloud":
		kind = "cloud"
	case "url":
		kind = "remote"
	default:
		return ""
	}
	u, err := NormalizeRemoteBaseURL(descriptor["baseUrl"])
	if err != nil {
		return ""
	}
	authMode := NormAuthMode(descriptor["authMode"])
	route := map[string]any{"authMode": authMode, "headers": descriptor["headers"], "kind": kind, "org": descriptor["org"], "token": descriptor["token"], "url": u}
	_, hasAuth := descriptor["authMode"]
	_, hasHeaders := descriptor["headers"]
	_, hasToken := descriptor["token"]
	_, hasOrg := descriptor["org"]
	exact := hasAuth && hasHeaders && (authMode == "oauth" || hasToken) && (kind == "remote" || hasOrg)
	if !exact {
		matches := 0
		for _, c := range registry.Connections {
			if c.Kind != kind {
				continue
			}
			if cu, err := NormalizeRemoteBaseURL(c.URL); err == nil && cu == u {
				matches++
			}
		}
		if matches != 1 {
			return ""
		}
	}
	return MatchingConnectionID(registry, route, "unique")
}

// Object form of a registry connection as the route identity reads it
func (c RegistryConnection) object() map[string]any {
	out := map[string]any{"id": c.ID, "kind": c.Kind, "label": c.Label}
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("url", c.URL)
	set("authMode", c.AuthMode)
	set("org", c.Org)
	set("name", c.Name)
	set("host", c.Host)
	set("user", c.User)
	set("keyPath", c.KeyPath)
	set("remoteHermesPath", c.RemoteHermesPath)
	set("remoteProfile", c.RemoteProfile)
	if c.Port != 0 {
		out["port"] = float64(c.Port)
	}
	if c.Token != nil {
		out["token"] = c.Token
	}
	if c.Headers != nil {
		out["headers"] = toJSONValue(c.Headers)
	}
	return out
}

// Stable JSON text of a value with sorted object keys
func stableValue(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			key, _ := jsonMarshal(k)
			parts[i] = key + ":" + stableValue(t[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = stableValue(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	b, _ := jsonMarshal(v)
	return b
}

func canonicalHeaders(headers any) map[string]any {
	out := map[string]any{}
	normalized := NormalizeRemoteHeaders(headers)
	names := make([]string, 0, len(normalized))
	for name := range normalized {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := normalized[name]
		out[strings.ToLower(name)] = map[string]any{"encoding": value.Encoding, "value": value.Value}
	}
	return out
}

// Identity of a stored route, empty when the route cannot be normalized
func routeIdentity(route map[string]any) string {
	if route["kind"] == "ssh" {
		s := NormalizeSSHConfig(jsSpread(route, map[string]any{"mode": "ssh"}))
		if s == nil {
			return ""
		}
		port := s.Port
		if port == 0 {
			port = 22
		}
		return stableValue(map[string]any{
			"host": strings.ToLower(strings.TrimSpace(s.Host)), "keyPath": s.KeyPath, "kind": "ssh", "port": float64(port),
			"remoteHermesPath": s.RemoteHermesPath, "remoteProfile": s.RemoteProfile, "user": strings.ToLower(strings.TrimSpace(s.User)),
		})
	}
	u, err := NormalizeRemoteBaseURL(route["url"])
	if err != nil {
		return ""
	}
	authMode := NormAuthMode(route["authMode"])
	org := ""
	if route["kind"] == "cloud" {
		org = strings.TrimSpace(jsOrString(route["org"]))
	}
	var token any
	if authMode == "token" {
		token = route["token"]
	}
	return stableValue(map[string]any{
		"authMode": authMode, "headers": canonicalHeaders(route["headers"]), "kind": route["kind"], "org": org, "token": token, "url": u,
	})
}

// MatchingConnectionID finds the registry entry a stored route names, by primary or unique match
func MatchingConnectionID(registry ConnectionRegistry, route map[string]any, strategy string) string {
	identity := routeIdentity(route)
	if identity == "" {
		return ""
	}
	if strategy == "primary" {
		primary := registry.Find(registry.Primary)
		if primary != nil && primary.Kind != "local" && routeIdentity(primary.object()) == identity {
			return primary.ID
		}
		return ""
	}
	var matches []string
	for _, c := range registry.Connections {
		if c.Kind != "local" && routeIdentity(c.object()) == identity {
			matches = append(matches, c.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// ReuseMatchingPrimarySSHBackendOptions are the inputs of ReuseMatchingPrimarySSHBackend
type ReuseMatchingPrimarySSHBackendOptions struct {
	ConnectionID         string
	EffectiveFingerprint func(RegistryConnection) (string, error)
	EnsurePrimary        func() (map[string]any, error)
	Profile              string
	Registry             ConnectionRegistry
	Source               RegistryConnection
}

// ReuseMatchingPrimarySSHBackend returns the primary descriptor when the registry SSH source resolves to the same effective host
func ReuseMatchingPrimarySSHBackend(o ReuseMatchingPrimarySSHBackendOptions) (map[string]any, error) {
	id := strings.TrimSpace(o.ConnectionID)
	profileKey := strings.TrimSpace(o.Profile)
	if profileKey == "" {
		profileKey = "default"
	}
	if profileKey != "default" || id == "" || id != o.Registry.Primary || o.Source.ID != id || o.Source.Kind != "ssh" {
		return nil, nil
	}
	fingerprint, err := o.EffectiveFingerprint(o.Source)
	if err != nil {
		return nil, fmt.Errorf("Could not resolve effective SSH config for connection \"%s\" (%s) via ssh -G: %w", o.Source.Label, o.Source.ID, err)
	}
	fingerprint = strings.TrimSpace(fingerprint)
	descriptor, err := o.EnsurePrimary()
	if err != nil {
		return nil, err
	}
	var active map[string]any
	if descriptor["mode"] == "remote" && descriptor["remoteKind"] == "ssh" {
		active = jsObject(descriptor["ssh"])
	}
	rootProfile := func(v any) string {
		if s := strings.TrimSpace(jsOrString(v)); s != "" {
			return s
		}
		return "default"
	}
	if fingerprint == "" || active == nil ||
		fingerprint != strings.TrimSpace(jsOrString(active["effectiveConfigFingerprint"])) ||
		strings.TrimSpace(o.Source.RemoteHermesPath) != strings.TrimSpace(jsOrString(active["remoteHermesPath"])) ||
		rootProfile(o.Source.RemoteProfile) != rootProfile(active["remoteProfile"]) {
		return nil, nil
	}
	return descriptor, nil
}

// RegistrySourceOwnsPrimaryBackend reports the primary backend descriptor belonging to the registry primary
func RegistrySourceOwnsPrimaryBackend(registry ConnectionRegistry, connectionID string, descriptor map[string]any) bool {
	id := strings.TrimSpace(connectionID)
	return id != "" && id == registry.Primary && ResolvedConnectionID(registry, descriptor) == id
}

// ReuseMatchingPrimaryRemoteBackend shares the primary remote backend with a registry dial of the primary source
func ReuseMatchingPrimaryRemoteBackend(connectionID string, ensurePrimary func(profile string) (map[string]any, error), profile string, registry ConnectionRegistry, source RegistryConnection) (map[string]any, error) {
	if connectionID != registry.Primary || source.Kind == "local" || source.Kind == "ssh" {
		return nil, nil
	}
	descriptor, err := ensurePrimary(profile)
	if err != nil {
		return nil, err
	}
	if !RegistrySourceOwnsPrimaryBackend(registry, connectionID, descriptor) {
		return nil, nil
	}
	key := strings.TrimSpace(profile)
	if key == "" {
		key = "default"
	}
	return jsSpread(descriptor, map[string]any{"profile": key, "connectionId": connectionID, "sharedRemote": true}), nil
}

func normalizedSSHTarget(route map[string]any) string {
	s := NormalizeSSHConfig(jsSpread(route, map[string]any{"mode": "ssh"}))
	if s == nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(s.Host))
	if user := strings.ToLower(strings.TrimSpace(s.User)); user != "" {
		return user + "@" + host
	}
	return host
}

// LocalRouteOptions are the facts a registry local dial decides on
type LocalRouteOptions struct {
	GlobalRemote          bool
	LocalProfileExists    *bool
	ProfileRemoteOverride bool
}

// ResolveRegistryLocalRoute decides whether a registry local dial delegates to the primary path or pools
func ResolveRegistryLocalRoute(profile string, o LocalRouteOptions) RegistryLocalRoute {
	raw := strings.TrimSpace(profile)
	key := raw
	if key == "" {
		key = "default"
	}
	if o.ProfileRemoteOverride {
		return RegistryLocalRoute{Delegate: true, PoolKey: key}
	}
	if o.GlobalRemote {
		poolKey := backend.BackendScopePrefix(LocalConnectionID) + key
		if raw != "" && key != "default" && o.LocalProfileExists != nil && !*o.LocalProfileExists {
			return RegistryLocalRoute{PoolKey: poolKey, Refuse: fmt.Sprintf("Profile \"%s\" no longer exists.", key)}
		}
		return RegistryLocalRoute{PoolKey: poolKey}
	}
	return RegistryLocalRoute{Delegate: true, PoolKey: key}
}

// RegistryDialConnectionID is the trimmed id a registry dial names, refusing an empty id
func RegistryDialConnectionID(connectionID any) (string, error) {
	id := strings.TrimSpace(jsNullishString(connectionID))
	if id == "" {
		return "", errors.New(`No connection with id "".`)
	}
	return id, nil
}

// ShouldDeferLocalEnumeration reports a pooled local route with no live pool backend yet
func ShouldDeferLocalEnumeration(route RegistryLocalRoute, poolKeys []string, connectionID string) bool {
	if route.Delegate {
		return false
	}
	if connectionID == "" {
		connectionID = LocalConnectionID
	}
	prefix := backend.BackendScopePrefix(connectionID)
	for _, k := range poolKeys {
		if strings.HasPrefix(k, prefix) {
			return false
		}
	}
	return true
}

// RosterProfileMetadata is the display metadata a source reports for one profile
type RosterProfileMetadata struct {
	DisplayName string         `json:"display_name,omitempty"`
	Title       *string        `json:"title,omitempty"`
	UIMeta      map[string]any `json:"ui_meta,omitempty"`
	HasAvatar   *bool          `json:"has_avatar,omitempty"`
}

// ConnectionAgents is one source's profile enumeration, Profiles nil when it could not be listed
type ConnectionAgents struct {
	Connection      RegistryConnection
	Profiles        []string
	ProfileMetadata map[string]RosterProfileMetadata
	Error           string
	InstallID       string
}

// RosterAgent is one row of the union agent roster
type RosterAgent struct {
	ConnectionID    string                 `json:"connectionId"`
	ConnectionKind  string                 `json:"connectionKind"`
	ConnectionLabel string                 `json:"connectionLabel"`
	Profile         string                 `json:"profile"`
	TargetProfile   string                 `json:"targetProfile,omitempty"`
	Handle          string                 `json:"handle"`
	ProfileMetadata *RosterProfileMetadata `json:"profileMetadata,omitempty"`
}

// SSHEnumeration is the profiles and error part of an enumeration
type SSHEnumeration struct {
	Profiles []string `json:"profiles"`
	Error    string   `json:"error,omitempty"`
}

// RememberSSHEnumeration keeps the last good remote profile list when a fresh listing comes back empty
func RememberSSHEnumeration(e SSHEnumeration, cached []string, kind string) SSHEnumeration {
	if len(e.Profiles) > 0 || kind == "local" {
		return e
	}
	if len(cached) > 0 {
		return SSHEnumeration{Profiles: cached, Error: e.Error}
	}
	if kind == "ssh" && e.Error == "connect-on-demand" {
		return SSHEnumeration{Profiles: []string{"default"}, Error: "connect-on-demand"}
	}
	return e
}

// ShouldRetrySSHInventory reports an uncached SSH source whose last listing attempt is old enough to retry
func ShouldRetrySSHInventory(hasCache bool, lastAttemptMs *int64, nowMs int64, retryAfterMs int64) bool {
	if hasCache {
		return false
	}
	if lastAttemptMs == nil {
		return true
	}
	if retryAfterMs == 0 {
		retryAfterMs = 60_000
	}
	return nowMs-*lastAttemptMs >= retryAfterMs
}

var profileNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

var newlineSplit = regexp.MustCompile(`\r?\n`)

// ParseRemoteProfileListing reads an ls of the remote profiles directory, default first then sorted names
func ParseRemoteProfileListing(text string) []string {
	names := map[string]bool{}
	for _, raw := range newlineSplit.Split(text, -1) {
		name := strings.TrimSpace(raw)
		if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".rollback-old") || !profileNameRe.MatchString(name) {
			continue
		}
		names[name] = true
	}
	out := []string{}
	for n := range names {
		if n != "default" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return append([]string{"default"}, out...)
}

var canonicalKindPriority = map[string]int{"cloud": 3, "local": 0, "remote": 2, "ssh": 1}

type rosterIdentity struct {
	connection RegistryConnection
	installID  string
	order      int
	profile    string
	metadata   *RosterProfileMetadata
}

// BuildAgentRoster unions every source's profiles, collapsing one backend reached twice into its canonical source
func BuildAgentRoster(enumerations []ConnectionAgents, primaryConnectionID string) []RosterAgent {
	var identities []rosterIdentity
	seen := map[string]bool{}
	for order, e := range enumerations {
		for _, p := range e.Profiles {
			name := strings.TrimSpace(p)
			if name == "" {
				name = "default"
			}
			key := e.Connection.ID + "\x00" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			id := rosterIdentity{connection: e.Connection, installID: e.InstallID, order: order, profile: name}
			if md, ok := e.ProfileMetadata[name]; ok {
				id.metadata = &md
			}
			identities = append(identities, id)
		}
	}
	var keys []string
	groups := map[string][]rosterIdentity{}
	for _, id := range identities {
		key := "conn:" + id.connection.ID + "\x00" + id.profile
		if id.installID != "" {
			key = "id:" + id.installID + "\x00" + id.profile
		}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], id)
	}
	rows := make([]rosterIdentity, len(keys))
	counts := map[string]int{}
	for i, key := range keys {
		rows[i] = pickCanonicalConnection(groups[key], primaryConnectionID)
		counts[rows[i].profile]++
	}
	roster := make([]RosterAgent, 0, len(rows))
	for _, r := range rows {
		target := r.connection.RemoteProfile
		if target == "" {
			target = r.profile
		}
		roster = append(roster, RosterAgent{
			ConnectionID: r.connection.ID, ConnectionKind: r.connection.Kind, ConnectionLabel: r.connection.Label,
			Profile: r.profile, TargetProfile: target, Handle: AgentHandle(r.profile, r.connection.Label, counts[r.profile] > 1),
			ProfileMetadata: r.metadata,
		})
	}
	return roster
}

func pickCanonicalConnection(candidates []rosterIdentity, primaryConnectionID string) rosterIdentity {
	if primaryConnectionID != "" {
		for _, c := range candidates {
			if c.connection.ID == primaryConnectionID {
				return c
			}
		}
	}
	sorted := append([]rosterIdentity(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		pi, pj := canonicalKindPriority[sorted[i].connection.Kind], canonicalKindPriority[sorted[j].connection.Kind]
		if pi != pj {
			return pi < pj
		}
		return sorted[i].order < sorted[j].order
	})
	return sorted[0]
}

// UpdateEligibility says whether a connection takes a fanned out update
type UpdateEligibility struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

// ConnectionUpdateEligibility skips platform managed cloud instances
func ConnectionUpdateEligibility(c RegistryConnection) UpdateEligibility {
	if c.Kind == "cloud" {
		return UpdateEligibility{Reason: "cloud-managed"}
	}
	return UpdateEligibility{Eligible: true}
}

// ConnectionIDForLabel is the label slug, suffixed until it is free and never the local id
func ConnectionIDForLabel(label string, taken []string) string {
	used := map[string]bool{}
	for _, t := range taken {
		used[t] = true
	}
	base := LabelSlug(label)
	if !used[base] && base != LocalConnectionID {
		return base
	}
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !used[candidate] && candidate != LocalConnectionID {
			return candidate
		}
	}
}

// ConnectionIDForPendingLogin settles the id a draft connection will be saved under
func ConnectionIDForPendingLogin(connectionID, label any, registry ConnectionRegistry) string {
	if id, ok := connectionID.(string); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	l, _ := label.(string)
	return ConnectionIDForLabel(l, registry.ids())
}

func storedAuthMode(kind string, authMode any, token any) string {
	if kind == "cloud" && !jsTruthy(token) {
		return "oauth"
	}
	return NormAuthMode(authMode)
}

// NormalizeConnectionInput validates a connection edit against the registry into a stored entry
func NormalizeConnectionInput(input map[string]any, registry ConnectionRegistry) (RegistryConnection, error) {
	label := strings.TrimSpace(jsOrString(input["label"]))
	if label == "" {
		return RegistryConnection{}, errors.New(`Every connection needs a name. Give this instance a device name (e.g. "Homelab", "Work laptop").`)
	}
	if len([]rune(label)) > labelMax {
		return RegistryConnection{}, fmt.Errorf("Connection name is too long (max %d characters).", labelMax)
	}
	inputID, _ := input["id"].(string)
	key := LabelKey(label)
	for _, c := range registry.Connections {
		if LabelKey(c.Label) == key && c.ID != inputID {
			return RegistryConnection{}, fmt.Errorf("A connection named \"%s\" already exists. Connection names must be unique.", c.Label)
		}
	}
	kind := input["kind"]
	if kind == "local" {
		return RegistryConnection{ID: LocalConnectionID, Kind: "local", Label: label}, nil
	}
	if inputID == LocalConnectionID {
		return RegistryConnection{}, errors.New(`The id "local" is reserved for the local connection.`)
	}
	id := jsOrString(input["id"])
	if id == "" {
		id = ConnectionIDForLabel(label, registry.ids())
	}
	switch kind {
	case "ssh":
		s := NormalizeSSHConfig(map[string]any{
			"mode": "ssh", "host": input["host"], "user": input["user"], "port": input["port"], "keyPath": input["keyPath"],
			"remoteHermesPath": input["remoteHermesPath"], "remoteProfile": input["remoteProfile"],
		})
		if s == nil {
			return RegistryConnection{}, errors.New("SSH connections need a host.")
		}
		sshKey := func(user, host string, port int, remoteProfile string) string {
			if port == 0 {
				port = 22
			}
			return strings.ToLower(user) + "@" + strings.ToLower(host) + ":" + strconv.Itoa(port) + "::" + strings.TrimSpace(remoteProfile)
		}
		mine := sshKey(s.User, s.Host, s.Port, s.RemoteProfile)
		for _, c := range registry.Connections {
			if c.Kind == "ssh" && c.ID != id && sshKey(c.User, c.Host, c.Port, c.RemoteProfile) == mine {
				return RegistryConnection{}, fmt.Errorf("A connection to this SSH host already exists (\"%s\").", c.Label)
			}
		}
		entry := RegistryConnection{ID: id, Kind: "ssh", Label: label, Host: s.Host, User: s.User, Port: s.Port,
			KeyPath: s.KeyPath, RemoteHermesPath: s.RemoteHermesPath, RemoteProfile: s.RemoteProfile}
		if token, ok := input["token"]; ok {
			entry.Token = token
		}
		return entry, nil
	case "remote", "cloud":
		k := kind.(string)
		u, err := NormalizeRemoteBaseURL(input["url"])
		if err != nil {
			return RegistryConnection{}, err
		}
		urlKey := func(v string) string {
			return strings.ToLower(trailingSlashes.ReplaceAllString(strings.TrimSpace(v), ""))
		}
		for _, c := range registry.Connections {
			if (c.Kind == "remote" || c.Kind == "cloud") && c.ID != id && urlKey(c.URL) == urlKey(u) {
				return RegistryConnection{}, fmt.Errorf("A connection to this gateway URL already exists (\"%s\").", c.Label)
			}
		}
		authMode := storedAuthMode(k, input["authMode"], nil)
		entry := RegistryConnection{ID: id, Kind: k, Label: label, URL: u, AuthMode: authMode}
		if token, ok := input["token"]; ok && k == "remote" && authMode == "token" {
			entry.Token = token
		}
		if raw, ok := input["headers"]; ok {
			if headers := NormalizeRemoteHeaders(raw); len(headers) > 0 {
				entry.Headers = headers
			}
		}
		if name := strings.TrimSpace(jsOrString(input["name"])); k == "cloud" && name != "" {
			entry.Name = name
		}
		if org := strings.TrimSpace(jsOrString(input["org"])); k == "cloud" && org != "" {
			entry.Org = org
		}
		return entry, nil
	}
	return RegistryConnection{}, fmt.Errorf("Unknown connection kind: %s", jsNullishOrUndefined(kind))
}

// JavaScript String of a value that may be missing, undefined when absent
func jsNullishOrUndefined(v any) string {
	if v == nil {
		return "undefined"
	}
	return jsString(v)
}

// MergeConnectionInput fills fields an edit left out from the stored entry of the same kind
func MergeConnectionInput(input map[string]any, existing *RegistryConnection) map[string]any {
	if existing == nil || existing.Kind != input["kind"] {
		return input
	}
	merged := jsSpread(input, nil)
	stored := existing.object()
	inherit := func(field string) {
		if _, ok := merged[field]; !ok && stored[field] != nil {
			merged[field] = stored[field]
		}
	}
	inherit("url")
	inherit("authMode")
	inherit("org")
	if input["kind"] == "cloud" {
		_, hasURL := input["url"]
		sameURL := !hasURL
		if !sameURL {
			a, errA := NormalizeRemoteBaseURL(input["url"])
			b, errB := NormalizeRemoteBaseURL(existing.URL)
			sameURL = errA == nil && errB == nil && a == b
		}
		if sameURL {
			inherit("name")
		}
	}
	inherit("host")
	inherit("keyPath")
	inherit("remoteHermesPath")
	inherit("remoteProfile")
	inherit("headers")
	if host, ok := input["host"]; !ok || strings.TrimSpace(jsString(host)) == "" {
		inherit("user")
		inherit("port")
	}
	return merged
}

// ConnectionDialFieldsChanged reports an edit that changes where or how the connection dials
func ConnectionDialFieldsChanged(before, after RegistryConnection) bool {
	if before.Kind != after.Kind || before.URL != after.URL || before.AuthMode != after.AuthMode || before.Org != after.Org ||
		before.Host != after.Host || before.User != after.User || before.Port != after.Port || before.KeyPath != after.KeyPath ||
		before.RemoteHermesPath != after.RemoteHermesPath || before.RemoteProfile != after.RemoteProfile {
		return true
	}
	if !jsonEqual(before.Token, after.Token) {
		return true
	}
	var bh, ah any
	if before.Headers != nil {
		bh = before.Headers
	}
	if after.Headers != nil {
		ah = after.Headers
	}
	return !jsonEqual(bh, ah)
}

func localEntry() RegistryConnection {
	return RegistryConnection{ID: LocalConnectionID, Kind: "local", Label: "This device"}
}

// NormalizeRegistry loads a decoded connections.json, quarantining malformed entries and ensuring one local source
func NormalizeRegistry(raw any) ConnectionRegistry {
	parsed := jsObject(raw)
	rawConnections, _ := parsed["connections"].([]any)
	seenLabels := []string{}
	seenIDs := map[string]bool{}
	var connections []RegistryConnection
	var quarantined []QuarantinedRegistryEntry
	quarantine := func(reason string, entry any) {
		if len(quarantined) < RegistryQuarantineCap {
			quarantined = append(quarantined, QuarantinedRegistryEntry{Reason: reason, Entry: entry})
		}
	}
	if items, ok := parsed["quarantined"].([]any); ok {
		for _, item := range items {
			if obj := jsObject(item); obj != nil {
				if entry, has := obj["entry"]; has {
					reason := jsOrString(obj["reason"])
					if reason == "" {
						reason = "unknown"
					}
					quarantine(reason, entry)
				}
			}
		}
	}
	for _, item := range rawConnections {
		if !jsTruthy(item) {
			continue
		}
		if clean, ok := normalizeRegistryEntry(item, &seenLabels, seenIDs, quarantine); ok {
			connections = append(connections, clean)
		}
	}
	return finishRegistry(parsed, connections, quarantined)
}

// One stored entry normalized, quarantined as entry-normalization-failed when reading it panics
func normalizeRegistryEntry(item any, seenLabels *[]string, seenIDs map[string]bool, quarantine func(string, any)) (clean RegistryConnection, ok bool) {
	defer func() {
		if recover() != nil {
			quarantine("entry-normalization-failed", safeEntryCopy(item))
			clean, ok = RegistryConnection{}, false
		}
	}()
	{
		entry := jsObject(item)
		if entry == nil {
			if _, isArray := item.([]any); !isArray {
				quarantine("entry-malformed", item)
				return RegistryConnection{}, false
			}
			entry = map[string]any{}
		}
		kind, _ := entry["kind"].(string)
		if kind != "local" && kind != "remote" && kind != "cloud" && kind != "ssh" {
			quarantine("entry-unrecognized-kind", item)
			return RegistryConnection{}, false
		}
		label := strings.TrimSpace(jsOrString(entry["label"]))
		if label == "" {
			switch {
			case kind == "ssh":
				label = jsString(entry["host"])
				if !jsTruthy(entry["host"]) {
					label = "ssh"
				}
			default:
				label = HostLabelFromBaseURL(jsOrString(entry["url"]))
				if label == "" {
					label = kind
				}
			}
		}
		label = UniqueLabel(label, *seenLabels)
		id := LocalConnectionID
		if kind != "local" {
			id = strings.TrimSpace(jsOrString(entry["id"]))
		}
		if id == "" || (seenIDs[id] && kind != "local") {
			ids := make([]string, 0, len(seenIDs))
			for k := range seenIDs {
				ids = append(ids, k)
			}
			id = ConnectionIDForLabel(label, ids)
		}
		if seenIDs[id] {
			return RegistryConnection{}, false
		}
		*seenLabels = append(*seenLabels, label)
		seenIDs[id] = true
		clean = RegistryConnection{ID: id, Kind: kind, Label: label}
		switch kind {
		case "remote", "cloud":
			u := strings.TrimSpace(jsOrString(entry["url"]))
			if u == "" {
				quarantine("entry-missing-url", item)
				return RegistryConnection{}, false
			}
			clean.URL = u
			clean.AuthMode = storedAuthMode(kind, entry["authMode"], entry["token"])
			if token, ok := entry["token"]; ok {
				clean.Token = token
			}
			if headers := NormalizeRemoteHeaders(entry["headers"]); len(headers) > 0 {
				clean.Headers = headers
			}
			if name := strings.TrimSpace(jsOrString(entry["name"])); kind == "cloud" && name != "" {
				clean.Name = name
			}
			if org := strings.TrimSpace(jsOrString(entry["org"])); kind == "cloud" && org != "" {
				clean.Org = org
			}
		case "ssh":
			s := NormalizeSSHConfig(jsSpread(entry, map[string]any{"mode": "ssh"}))
			if s == nil {
				quarantine("entry-missing-ssh-host", item)
				return RegistryConnection{}, false
			}
			clean.Host, clean.User, clean.Port, clean.KeyPath = s.Host, s.User, s.Port, s.KeyPath
			clean.RemoteHermesPath, clean.RemoteProfile = s.RemoteHermesPath, s.RemoteProfile
			if token, ok := entry["token"]; ok {
				clean.Token = token
			}
		}
		return clean, true
	}
}

// Registry document around the normalized entries, local guaranteed and dangling ids retargeted
func finishRegistry(parsed map[string]any, connections []RegistryConnection, quarantined []QuarantinedRegistryEntry) ConnectionRegistry {
	hasLocal := false
	for _, c := range connections {
		if c.Kind == "local" {
			hasLocal = true
		}
	}
	if !hasLocal {
		connections = append([]RegistryConnection{localEntry()}, connections...)
	}
	out := ConnectionRegistry{Version: RegistryVersion, Connections: connections, LaunchMode: "primary"}
	storedPrimary := strings.TrimSpace(jsOrString(parsed["primary"]))
	out.Primary = LocalConnectionID
	if out.has(storedPrimary) {
		out.Primary = storedPrimary
	}
	if parsed["launchMode"] == "last-used" {
		out.LaunchMode = "last-used"
	}
	storedLastUsed := strings.TrimSpace(jsOrString(parsed["lastUsed"]))
	out.LastUsed = out.Primary
	if out.has(storedLastUsed) {
		out.LastUsed = storedLastUsed
	}
	out.Quarantined = quarantined
	return out
}

// JSON round trip of a quarantined entry, a marker object when it cannot be serialized
func safeEntryCopy(item any) any {
	text, err := jsonMarshal(item)
	if err != nil {
		return map[string]any{"unserializable": true}
	}
	var out any
	if json.Unmarshal([]byte(text), &out) != nil {
		return map[string]any{"unserializable": true}
	}
	return out
}

// MigrateV1ToRegistry builds the first registry from a v1 connection.json in its profile key order, the global remote as primary
func MigrateV1ToRegistry(v1 any, profileOrder []string) ConnectionRegistry {
	config := jsObject(v1)
	reg := ConnectionRegistry{Version: RegistryVersion, LaunchMode: "primary", Connections: []RegistryConnection{localEntry()}}
	byFingerprint := map[string]string{}
	addRemoteLike := func(block map[string]any, kind string) string {
		u := strings.TrimSpace(jsOrString(block["url"]))
		if u == "" {
			return ""
		}
		fingerprint := kind + ":" + u
		if id, ok := byFingerprint[fingerprint]; ok {
			return id
		}
		fallback := "Remote gateway"
		if kind == "cloud" {
			fallback = "Icebath Cloud"
		}
		host := HostLabelFromBaseURL(u)
		if host == "" {
			host = fallback
		}
		label := UniqueLabel(host, reg.labels())
		entry := RegistryConnection{ID: ConnectionIDForLabel(label, reg.ids()), Kind: kind, Label: label, URL: u, AuthMode: NormAuthMode(block["authMode"])}
		if token, ok := block["token"]; ok {
			entry.Token = token
		}
		if headers := NormalizeRemoteHeaders(block["headers"]); len(headers) > 0 {
			entry.Headers = headers
		}
		if name := strings.TrimSpace(jsOrString(block["name"])); kind == "cloud" && name != "" {
			entry.Name = name
		}
		if org := strings.TrimSpace(jsOrString(block["org"])); kind == "cloud" && org != "" {
			entry.Org = org
		}
		reg.Connections = append(reg.Connections, entry)
		byFingerprint[fingerprint] = entry.ID
		return entry.ID
	}
	addSSH := func(block map[string]any) string {
		s := NormalizeSSHConfig(jsSpread(block, map[string]any{"mode": "ssh"}))
		if s == nil {
			return ""
		}
		port := s.Port
		if port == 0 {
			port = 22
		}
		fingerprint := "ssh:" + s.User + "@" + s.Host + ":" + strconv.Itoa(port)
		if id, ok := byFingerprint[fingerprint]; ok {
			return id
		}
		label := UniqueLabel(s.Host, reg.labels())
		entry := RegistryConnection{ID: ConnectionIDForLabel(label, reg.ids()), Kind: "ssh", Label: label, Host: s.Host, User: s.User,
			Port: s.Port, KeyPath: s.KeyPath, RemoteHermesPath: s.RemoteHermesPath, RemoteProfile: s.RemoteProfile}
		reg.Connections = append(reg.Connections, entry)
		byFingerprint[fingerprint] = entry.ID
		return entry.ID
	}
	primary := LocalConnectionID
	remote := jsObject(config["remote"])
	if remote == nil {
		remote = map[string]any{}
	}
	switch mode := config["mode"]; {
	case ModeIsRemoteLike(mode):
		kind := "remote"
		if mode == "cloud" {
			kind = "cloud"
		}
		if id := addRemoteLike(remote, kind); id != "" {
			primary = id
		}
	case mode == "ssh":
		if id := addSSH(remote); id != "" {
			primary = id
		}
	}
	profiles := jsObject(config["profiles"])
	for _, name := range orderedKeys(profiles, profileOrder) {
		block := jsObject(profiles[name])
		if block == nil {
			continue
		}
		switch {
		case ModeIsRemoteLike(block["mode"]):
			kind := "remote"
			if block["mode"] == "cloud" {
				kind = "cloud"
			}
			addRemoteLike(block, kind)
		case block["mode"] == "ssh":
			addSSH(block)
		case block["mode"] == "local" && jsTruthy(block["savedSsh"]):
			if saved := jsObject(block["savedSsh"]); saved != nil {
				addSSH(saved)
			} else {
				addSSH(map[string]any{})
			}
		}
	}
	reg.Primary, reg.LastUsed = primary, primary
	return reg
}

// Keys of an object in JavaScript enumeration order given the insertion order it was decoded in
func orderedKeys(obj map[string]any, insertion []string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, k := range insertion {
		if _, ok := obj[k]; ok && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range obj {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	sortJSKeys(keys)
	return keys
}

// Orders object keys the way JavaScript enumerates them, integer keys ascending first
func sortJSKeys(keys []string) {
	isIndex := func(k string) (uint64, bool) {
		if k == "" || (len(k) > 1 && k[0] == '0') {
			return 0, false
		}
		n, err := strconv.ParseUint(k, 10, 32)
		return n, err == nil && n < 1<<32-1
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, aok := isIndex(keys[i])
		b, bok := isIndex(keys[j])
		switch {
		case aok && bok:
			return a < b
		case aok != bok:
			return aok
		}
		return false
	})
}

// UpsertConnection replaces the entry with the same id or appends it
func UpsertConnection(registry ConnectionRegistry, entry RegistryConnection) ConnectionRegistry {
	out := registry.clone()
	for i := range out.Connections {
		if out.Connections[i].ID == entry.ID {
			out.Connections[i] = entry
			return out
		}
	}
	out.Connections = append(out.Connections, entry)
	return out
}

// RemoveConnection drops a non local entry, retargeting primary and last used to local
func RemoveConnection(registry ConnectionRegistry, id string) (ConnectionRegistry, error) {
	target := registry.Find(id)
	if target == nil {
		return registry, nil
	}
	if target.Kind == "local" {
		return registry, errors.New("The local connection cannot be removed.")
	}
	out := registry.clone()
	if out.Primary == id {
		out.Primary = LocalConnectionID
	}
	if out.LastUsed == id {
		out.LastUsed = out.Primary
	}
	kept := out.Connections[:0]
	for _, c := range out.Connections {
		if c.ID != id {
			kept = append(kept, c)
		}
	}
	out.Connections = kept
	return out, nil
}

// SetPrimaryConnection designates the connection that owns the window backend
func SetPrimaryConnection(registry ConnectionRegistry, id string) (ConnectionRegistry, error) {
	if !registry.has(id) {
		return registry, fmt.Errorf("No connection with id \"%s\".", id)
	}
	out := registry.clone()
	out.Primary = id
	return out, nil
}

// SetLastUsedConnection records the source the Sessions workspace last opened
func SetLastUsedConnection(registry ConnectionRegistry, id string) (ConnectionRegistry, error) {
	if !registry.has(id) {
		return registry, fmt.Errorf("No connection with id \"%s\".", id)
	}
	out := registry.clone()
	out.LastUsed = id
	return out, nil
}

// ReconcileAppliedGlobalConnection mirrors an applied global connection into the registry as primary
func ReconcileAppliedGlobalConnection(registry ConnectionRegistry, config map[string]any) (ConnectionRegistry, error) {
	mode := config["mode"]
	if !ModeIsRemoteLike(mode) {
		if mode == "local" {
			out := registry.clone()
			out.Primary, out.LastUsed = LocalConnectionID, LocalConnectionID
			return out, nil
		}
		return registry, nil
	}
	block := jsObject(config["remote"])
	if block == nil {
		block = map[string]any{}
	}
	u, err := NormalizeRemoteBaseURL(block["url"])
	if err != nil {
		return registry, err
	}
	var existing *RegistryConnection
	for i, c := range registry.Connections {
		if c.Kind != "remote" && c.Kind != "cloud" {
			continue
		}
		if cu, err := NormalizeRemoteBaseURL(c.URL); err == nil && cu == u {
			existing = &registry.Connections[i]
			break
		}
	}
	kind := "remote"
	fallback := "Remote gateway"
	if mode == "cloud" {
		kind, fallback = "cloud", "Icebath Cloud"
	}
	hostLabel := HostLabelFromBaseURL(u)
	if hostLabel == "" {
		hostLabel = fallback
	}
	name := ""
	if kind == "cloud" {
		raw, ok := block["name"]
		if (!ok || raw == nil) && existing != nil {
			raw = existing.Name
		}
		name = strings.TrimSpace(jsNullishString(raw))
	}
	var label string
	if existing != nil && (name == "" || existing.Label != hostLabel) {
		label = existing.Label
	} else {
		var taken []string
		for _, c := range registry.Connections {
			if existing == nil || c.ID != existing.ID {
				taken = append(taken, c.Label)
			}
		}
		candidate := name
		if candidate == "" {
			candidate = hostLabel
		}
		label = UniqueLabel(candidate, taken)
	}
	input := map[string]any{"kind": kind, "label": label, "url": u, "authMode": block["authMode"], "token": block["token"],
		"headers": block["headers"], "org": block["org"], "name": name}
	if existing != nil {
		input["id"] = existing.ID
	}
	if _, ok := block["token"]; !ok {
		delete(input, "token")
	}
	if _, ok := block["headers"]; !ok {
		delete(input, "headers")
	}
	entry, err := NormalizeConnectionInput(input, registry)
	if err != nil {
		return registry, err
	}
	out := UpsertConnection(registry, entry)
	out.Primary, out.LastUsed = entry.ID, entry.ID
	return out, nil
}

// ReconcileRegistryDrift registers a global connection applied by an older build that never wrote the registry
func ReconcileRegistryDrift(registry ConnectionRegistry, v1 any) (ConnectionRegistry, bool, error) {
	config := jsObject(v1)
	if config == nil {
		config = map[string]any{}
	}
	remote := jsObject(config["remote"])
	if remote == nil {
		remote = map[string]any{}
	}
	if config["mode"] == "ssh" {
		s := NormalizeSSHConfig(jsSpread(remote, map[string]any{"mode": "ssh"}))
		if s == nil {
			return registry, false, nil
		}
		target := normalizedSSHTarget(s.object())
		port := func(p int) int {
			if p == 0 {
				return 22
			}
			return p
		}
		var registered *RegistryConnection
		for i, c := range registry.Connections {
			if c.Kind == "ssh" && normalizedSSHTarget(c.object()) == target && port(c.Port) == port(s.Port) {
				registered = &registry.Connections[i]
				break
			}
		}
		if registered != nil {
			if MatchingConnectionID(registry, jsSpread(s.object(), map[string]any{"kind": "ssh"}), "unique") != "" {
				return registry, false, nil
			}
			aligned := *registered
			aligned.KeyPath, aligned.RemoteHermesPath, aligned.RemoteProfile = s.KeyPath, s.RemoteHermesPath, s.RemoteProfile
			return UpsertConnection(registry, aligned), true, nil
		}
		input := s.object()
		delete(input, "mode")
		input["kind"] = "ssh"
		input["label"] = UniqueLabel(s.Host, registry.labels())
		entry, err := NormalizeConnectionInput(input, registry)
		if err != nil {
			return registry, false, nil
		}
		out := UpsertConnection(registry, entry)
		out.Primary, out.LastUsed = entry.ID, entry.ID
		return out, true, nil
	}
	if !ModeIsRemoteLike(config["mode"]) {
		return registry, false, nil
	}
	u, err := NormalizeRemoteBaseURL(remote["url"])
	if err != nil || u == "" {
		return registry, false, nil
	}
	for _, c := range registry.Connections {
		if c.Kind != "remote" && c.Kind != "cloud" {
			continue
		}
		if cu, err := NormalizeRemoteBaseURL(c.URL); err == nil && cu == u {
			return registry, false, nil
		}
	}
	out, err := ReconcileAppliedGlobalConnection(registry, config)
	if err != nil {
		return registry, false, err
	}
	return out, true, nil
}

// SetConnectionLaunchMode sets which source Sessions restores at launch
func SetConnectionLaunchMode(registry ConnectionRegistry, launchMode string) (ConnectionRegistry, error) {
	if launchMode != "last-used" && launchMode != "primary" {
		return registry, fmt.Errorf("Unknown connection launch mode \"%s\".", launchMode)
	}
	out := registry.clone()
	out.LaunchMode = launchMode
	return out, nil
}
