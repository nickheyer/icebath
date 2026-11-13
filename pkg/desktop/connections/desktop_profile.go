package connections

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DesktopProfileRoute is a connection and profile pair, a nil connection meaning the primary route
type DesktopProfileRoute struct {
	ConnectionID *string `json:"connectionId"`
	Profile      string  `json:"profile"`
}

// MissingLocalProfileCheck holds the facts deciding whether a remembered local profile is gone
type MissingLocalProfileCheck struct {
	GlobalRemote      bool
	HasRemoteOverride func(profile string) bool
	LaunchProfile     string
	Log               func(line string)
	ProfileExists     func(profile string) bool
}

// ProfileResult is the answer of the profile get and remember calls, a nil profile for none
type ProfileResult struct {
	Profile *string `json:"profile"`
}

// RequireDesktopProfileRoute validates a renderer route object
func RequireDesktopProfileRoute(value any) (DesktopProfileRoute, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return DesktopProfileRoute{}, errors.New("A connection and profile route is required.")
	}
	profile, ok := obj["profile"].(string)
	if !ok || !desktopProfileNameRE.MatchString(profile) {
		return DesktopProfileRoute{}, errors.New("Invalid profile name.")
	}
	raw, present := obj["connectionId"]
	if !present {
		return DesktopProfileRoute{}, errors.New("Invalid connection id.")
	}
	if raw == nil {
		return DesktopProfileRoute{Profile: profile}, nil
	}
	id, ok := raw.(string)
	if !ok || id == "" || strings.TrimSpace(id) != id {
		return DesktopProfileRoute{}, errors.New("Invalid connection id.")
	}
	return DesktopProfileRoute{ConnectionID: &id, Profile: profile}, nil
}

// Remembered profile, default route and per connection profiles of active-profile.json
type desktopProfilePreferences struct {
	mu               sync.Mutex
	path             string
	onDefaultChanged func(route *DesktopProfileRoute)
	validateRoute    func(route DesktopProfileRoute) error
}

// The preference object, strict refusing an unreadable or malformed file other than a missing one
func (p *desktopProfilePreferences) read(strict bool) (map[string]any, error) {
	data, err := os.ReadFile(p.path)
	var value any
	if err == nil {
		err = json.Unmarshal(data, &value)
	}
	if err != nil {
		if strict && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return map[string]any{}, nil
	}
	if obj, ok := value.(map[string]any); ok {
		return obj, nil
	}
	return map[string]any{}, nil
}

// Writes the preference object through a sibling temp file
func (p *desktopProfilePreferences) write(value map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o777); err != nil {
		return err
	}
	data, err := jsonStringifyIndent(value)
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// Strict read with key overrides applied, the shape every write starts from
func (p *desktopProfilePreferences) update(set map[string]any) error {
	current, err := p.read(true)
	if err != nil {
		return err
	}
	return p.write(jsSpread(current, set))
}

func (p *desktopProfilePreferences) getDefault() *DesktopProfileRoute {
	current, _ := p.read(false)
	route, err := RequireDesktopProfileRoute(current["defaultRoute"])
	if err != nil {
		return nil
	}
	return &route
}

func (p *desktopProfilePreferences) readActive() *string {
	current, _ := p.read(false)
	value, _ := current["profile"].(string)
	profile := strings.TrimSpace(value)
	if !desktopProfileNameRE.MatchString(profile) {
		return nil
	}
	return &profile
}

func (p *desktopProfilePreferences) remember(name any) (*string, error) {
	value, _ := name.(string)
	profile := strings.TrimSpace(value)
	if profile != "" && !desktopProfileNameRE.MatchString(profile) {
		return nil, fmt.Errorf("Invalid profile name: %s", profile)
	}
	var stored any
	if profile != "" {
		stored = profile
	}
	if err := p.update(map[string]any{"profile": stored}); err != nil {
		return nil, err
	}
	if profile == "" {
		return nil, nil
	}
	return &profile, nil
}

// Route of a JSON object for the preference file
func routeValue(route DesktopProfileRoute) map[string]any {
	var id any
	if route.ConnectionID != nil {
		id = *route.ConnectionID
	}
	return map[string]any{"connectionId": id, "profile": route.Profile}
}

func (p *desktopProfilePreferences) setDefault(value any) (DesktopProfileRoute, error) {
	route, err := RequireDesktopProfileRoute(value)
	if err != nil {
		return route, err
	}
	if p.validateRoute != nil {
		if err := p.validateRoute(route); err != nil {
			return route, err
		}
	}
	if err := p.update(map[string]any{"defaultRoute": routeValue(route)}); err != nil {
		return route, err
	}
	if p.onDefaultChanged != nil {
		p.onDefaultChanged(&route)
	}
	return route, nil
}

func (p *desktopProfilePreferences) clearDefault() error {
	if err := p.update(map[string]any{"defaultRoute": nil}); err != nil {
		return err
	}
	if p.onDefaultChanged != nil {
		p.onDefaultChanged(nil)
	}
	return nil
}

// Follows a renamed or deleted profile in the remembered profile and the default route
func (p *desktopProfilePreferences) profileChanged(connectionID *string, oldName string, newName string, backendMode string) error {
	if active := p.readActive(); backendMode == "local" && active != nil && *active == oldName {
		next := newName
		if next == "" {
			next = "default"
		}
		if _, err := p.remember(next); err != nil {
			return err
		}
	}
	route := p.getDefault()
	if route == nil || !sameConnectionID(route.ConnectionID, connectionID) || route.Profile != oldName {
		return nil
	}
	if newName != "" {
		_, err := p.setDefault(routeValue(DesktopProfileRoute{ConnectionID: route.ConnectionID, Profile: newName}))
		return err
	}
	return p.clearDefault()
}

func sameConnectionID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (p *desktopProfilePreferences) readConnectionProfiles() map[string]string {
	current, _ := p.read(false)
	out := map[string]string{}
	if obj, ok := current["connectionProfiles"].(map[string]any); ok {
		for id, v := range obj {
			if profile, ok := v.(string); ok && desktopProfileNameRE.MatchString(profile) {
				out[id] = profile
			}
		}
	}
	return out
}

func (p *desktopProfilePreferences) connectionProfile(connectionID string) *string {
	if connectionID == "" {
		return nil
	}
	if profile, ok := p.readConnectionProfiles()[connectionID]; ok {
		return &profile
	}
	return nil
}

func stringMapValue(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (p *desktopProfilePreferences) rememberConnectionProfile(connectionID, profile any, isKnownConnection func(string) (bool, error)) error {
	id, ok := connectionID.(string)
	name, isString := profile.(string)
	if !ok || strings.TrimSpace(id) == "" || !isString {
		return nil
	}
	existing := p.readConnectionProfiles()
	if current, ok := existing[id]; !desktopProfileNameRE.MatchString(name) || (ok && current == name) {
		return nil
	}
	if known, err := isKnownConnection(id); err != nil || !known {
		return err
	}
	existing[id] = name
	return p.update(map[string]any{"connectionProfiles": stringMapValue(existing)})
}

func (p *desktopProfilePreferences) importConnectionProfiles(value any, isKnownConnection func(string) (bool, error)) error {
	current, err := p.read(true)
	if err != nil {
		return err
	}
	if current["connectionProfilesImported"] == true {
		return nil
	}
	existing := p.readConnectionProfiles()
	merged := map[string]string{}
	for k, v := range existing {
		merged[k] = v
	}
	if record, ok := value.(map[string]any); ok {
		for id, v := range record {
			if _, taken := existing[id]; strings.TrimSpace(id) == "" || taken {
				continue
			}
			known, err := isKnownConnection(id)
			if err != nil {
				return err
			}
			if !known {
				continue
			}
			if profile, ok := v.(string); ok && desktopProfileNameRE.MatchString(profile) {
				merged[id] = profile
			}
		}
	}
	return p.write(jsSpread(current, map[string]any{"connectionProfiles": stringMapValue(merged), "connectionProfilesImported": true}))
}

// Forgets a removed connection in the default route and the per connection profiles
func (p *desktopProfilePreferences) connectionRemoved(connectionID string) error {
	if route := p.getDefault(); route != nil && route.ConnectionID != nil && *route.ConnectionID == connectionID {
		if err := p.clearDefault(); err != nil {
			return err
		}
	}
	rest := p.readConnectionProfiles()
	if _, ok := rest[connectionID]; ok && rest[connectionID] != "" {
		delete(rest, connectionID)
		return p.update(map[string]any{"connectionProfiles": stringMapValue(rest)})
	}
	return nil
}

// Follows a successful profile rename or delete the renderer sent through the api call
func (p *desktopProfilePreferences) afterProfileRequest(connectionID *string, request *ProfileRequest, response any, backendMode string) error {
	if result, ok := response.(map[string]any); ok {
		if result["ok"] == false || result["success"] == false || jsTruthy(result["error"]) {
			return nil
		}
	}
	if renamed := ProfileRenameFromRequest(request); renamed != nil {
		return p.profileChanged(connectionID, renamed.OldName, renamed.NewName, backendMode)
	}
	if deleted := ProfileNameFromDeleteRequest(request); deleted != "" {
		return p.profileChanged(connectionID, deleted, "", backendMode)
	}
	return nil
}

// Resets a remembered profile or default route whose local profile no longer exists, the names forgotten
func (p *desktopProfilePreferences) forgetMissingLocalProfiles(check MissingLocalProfileCheck) []string {
	isGone := func(route DesktopProfileRoute) bool {
		scoped := !check.GlobalRemote
		if route.ConnectionID != nil {
			scoped = *route.ConnectionID == LocalConnectionID
		}
		return route.Profile != "default" && route.Profile != check.LaunchProfile && scoped &&
			!check.HasRemoteOverride(route.Profile) && !check.ProfileExists(route.Profile)
	}
	forgotten := []string{}
	run := func() error {
		if active := p.readActive(); active != nil && isGone(DesktopProfileRoute{Profile: *active}) {
			if _, err := p.remember("default"); err != nil {
				return err
			}
			forgotten = append(forgotten, *active)
			check.Log(fmt.Sprintf(`[profile] last-used profile "%s" no longer exists on this machine; starting as default`, *active))
		}
		if route := p.getDefault(); route != nil && isGone(*route) {
			if err := p.clearDefault(); err != nil {
				return err
			}
			forgotten = append(forgotten, route.Profile)
			check.Log(fmt.Sprintf(`[profile] cleared default profile "%s": it no longer exists on this machine`, route.Profile))
		}
		return nil
	}
	if err := run(); err != nil {
		check.Log("[profile] could not forget missing local profiles: " + err.Error())
	}
	return forgotten
}

// Whether the registry holds a connection with this id
func (s *Service) isKnownConnection(connectionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	registry, err := s.readDesktopConnectionsRegistry()
	if err != nil {
		return false, err
	}
	return registry.has(connectionID), nil
}

// Refuses a default route naming a connection the registry does not hold
func (s *Service) validateDesktopProfileRoute(route DesktopProfileRoute) error {
	if route.ConnectionID != nil && *route.ConnectionID != "" {
		s.mu.Lock()
		registry, err := s.readDesktopConnectionsRegistry()
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if !registry.has(*route.ConnectionID) {
			return fmt.Errorf("No connection with id \"%s\".", *route.ConnectionID)
		}
	}
	return nil
}

// ActiveProfile is the remembered profile of the primary, empty for none
func (s *Service) ActiveProfile() string {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	if active := s.profiles.readActive(); active != nil {
		return *active
	}
	return ""
}

// ProfileGetDefault answers the app wide default route, nil when none is set
func (s *Service) ProfileGetDefault() *DesktopProfileRoute {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	return s.profiles.getDefault()
}

// ProfileSetDefault stores the app wide default route and tells every window
func (s *Service) ProfileSetDefault(route any) (DesktopProfileRoute, error) {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	return s.profiles.setDefault(route)
}

// ProfileGet answers the remembered profile
func (s *Service) ProfileGet() ProfileResult {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	return ProfileResult{Profile: s.profiles.readActive()}
}

// ProfileRemember records the last used profile without touching the backend
func (s *Service) ProfileRemember(name any) (ProfileResult, error) {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	profile, err := s.profiles.remember(name)
	return ProfileResult{Profile: profile}, err
}

// ProfileImportConnections imports the renderer's per connection profiles once
func (s *Service) ProfileImportConnections(record any) error {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	return s.profiles.importConnectionProfiles(record, s.isKnownConnection)
}

// ProfileRememberConnection records the last profile used on a known connection
func (s *Service) ProfileRememberConnection(connectionID, profile any) error {
	s.profiles.mu.Lock()
	defer s.profiles.mu.Unlock()
	return s.profiles.rememberConnectionProfile(connectionID, profile, s.isKnownConnection)
}
