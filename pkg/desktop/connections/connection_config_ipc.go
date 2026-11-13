package connections

import (
	"context"
	"errors"
	"os"
)

// ConnectionConfigView is the renderer view of the v1 connection settings of one scope
type ConnectionConfigView struct {
	Mode                 string  `json:"mode"`
	Profile              *string `json:"profile"`
	RemoteAuthMode       string  `json:"remoteAuthMode"`
	RemoteOauthConnected bool    `json:"remoteOauthConnected"`
	RemoteURL            string  `json:"remoteUrl"`
	CloudOrg             string  `json:"cloudOrg"`
	RemoteTokenPreview   *string `json:"remoteTokenPreview"`
	RemoteTokenSet       bool    `json:"remoteTokenSet"`
	SecureTokenStorage   bool    `json:"secureTokenStorage"`
	RemoteTokenPlainText bool    `json:"remoteTokenPlainText"`
	SSHHost              string  `json:"sshHost"`
	SSHUser              string  `json:"sshUser"`
	SSHPort              *int    `json:"sshPort"`
	SSHKeyPath           string  `json:"sshKeyPath"`
	SSHRemoteHermesPath  string  `json:"sshRemoteHermesPath"`
	SSHRemoteProfile     string  `json:"sshRemoteProfile"`
	EnvOverride          bool    `json:"envOverride"`
}

// Whether the renderer warns that a saved token sits in plain text on a machine that cannot secure it
func ResolveRemoteTokenPlainText(envOverride bool, token any, secureTokenStorage bool) bool {
	if envOverride {
		return false
	}
	encoding, _, ok := secretFields(token)
	return ok && encoding == "plain" && !secureTokenStorage
}

func stringPointer(s string) *string { return &s }

// Renderer view of a scope's settings, caller holds s.mu, the OAuth liveness filled in after the lock
func (s *Service) connectionConfigView(config connectionConfig, profile any) (ConnectionConfigView, string) {
	key := ConnectionScopeKey(profile)
	var scoped, block map[string]any
	if key != "" {
		scoped = jsObject(config.Profiles[key])
		block = scoped
	} else {
		block = jsObject(config.Remote)
	}
	envURL := os.Getenv("ICEBATH_DESKTOP_REMOTE_URL")
	envOverride := key == "" && envURL != ""
	var savedMode any = config.Mode
	if key != "" {
		savedMode = scoped["mode"]
	}
	var ssh, savedSSH *SSHConfig
	if savedMode == "ssh" {
		ssh = NormalizeSSHConfig(block)
	}
	if savedMode == "local" {
		if key != "" {
			savedSSH = SavedProfileSSH(config.object(), key)
		} else {
			savedSSH = NormalizeSSHConfig(block)
		}
	}
	remoteToken := s.decryptDesktopSecret(block["token"])
	authMode := NormAuthMode(block["authMode"])
	remoteURL := jsOrString(block["url"])
	if envOverride {
		remoteURL = envURL
	}
	mode := "local"
	switch {
	case envOverride:
		mode = "remote"
	case savedMode == "ssh":
		mode = "ssh"
	case ModeIsRemoteLike(savedMode):
		mode = savedMode.(string)
	}
	secureTokenStorage := s.probeSecureTokenStorage()
	view := ConnectionConfigView{
		Mode:                 mode,
		RemoteAuthMode:       authMode,
		RemoteURL:            remoteURL,
		RemoteTokenSet:       remoteToken != "",
		SecureTokenStorage:   secureTokenStorage,
		RemoteTokenPlainText: ResolveRemoteTokenPlainText(envOverride, block["token"], secureTokenStorage),
		EnvOverride:          envOverride,
	}
	if key != "" {
		view.Profile = stringPointer(key)
	}
	if mode == "cloud" {
		view.CloudOrg = jsOrString(block["org"])
	}
	if preview := TokenPreview(remoteToken); preview != "" {
		view.RemoteTokenPreview = stringPointer(preview)
	}
	if shown := ssh; shown != nil || savedSSH != nil {
		if shown == nil {
			shown = savedSSH
		}
		view.SSHHost, view.SSHUser, view.SSHKeyPath = shown.Host, shown.User, shown.KeyPath
		view.SSHRemoteHermesPath, view.SSHRemoteProfile = shown.RemoteHermesPath, shown.RemoteProfile
		if shown.Port != 0 {
			port := shown.Port
			view.SSHPort = &port
		}
	}
	return view, remoteURL
}

// Renderer view of a scope's settings with the OAuth session liveness of an OAuth remote
func (s *Service) sanitizeDesktopConnectionConfig(config connectionConfig, profile any) ConnectionConfigView {
	s.mu.Lock()
	view, remoteURL := s.connectionConfigView(config, profile)
	s.mu.Unlock()
	if view.RemoteAuthMode == "oauth" && remoteURL != "" {
		hasNative := s.hasNativeSession(remoteURL)
		view.RemoteOauthConnected = OauthSessionIsLive(hasNative, s.hasLiveOauthSession(remoteURL))
	}
	return view
}

// GetConnectionConfig is the renderer view of the saved settings of a profile scope or the global scope
func (s *Service) GetConnectionConfig(profile any) ConnectionConfigView {
	s.mu.Lock()
	config := s.readDesktopConnectionConfig()
	s.mu.Unlock()
	return s.sanitizeDesktopConnectionConfig(config, profile)
}

// Token a save stores, the existing one when none was typed, plain when not persisted, else sealed
func ResolvePersistedRemoteToken(incomingToken string, persistToken bool, existingToken any, allowPlainText any, encrypt func(value string, allowPlainText bool) (any, error)) (any, error) {
	if incomingToken == "" {
		return existingToken, nil
	}
	if !persistToken {
		return map[string]any{"encoding": "plain", "value": incomingToken}, nil
	}
	return encrypt(incomingToken, allowPlainText == true)
}

// Policy aware secret envelope for a caller holding s.mu, plain while keychain encryption is off
func (s *Service) encryptDesktopSecretLocked(value string, allowPlainText bool) (any, error) {
	if !s.secretStoragePolicy().On {
		if value == "" {
			return nil, nil
		}
		return SecretEnvelope{Encoding: "plain", Value: value}, nil
	}
	return EncryptDesktopSecretStrict(value, s.safe, allowPlainText)
}

// Remote block of a URL gateway, a token required unless it signs in with OAuth, caller holds s.mu
func (s *Service) buildRemoteBlock(remoteURL, authMode string, token any, tokenSet bool, org, headers, name, remoteProfile any) (map[string]any, error) {
	if authMode != "oauth" && s.decryptDesktopSecret(token) == "" {
		return nil, errors.New("Remote gateway session token is required.")
	}
	url, err := NormalizeRemoteBaseURL(remoteURL)
	if err != nil {
		return nil, err
	}
	block := map[string]any{"url": url, "authMode": authMode}
	if tokenSet {
		block["token"] = token
	}
	if remoteHeaders := NormalizeRemoteHeaders(headers); len(remoteHeaders) > 0 {
		block["headers"] = remoteHeaders
	}
	if value, ok := name.(string); ok && jsTrim(value) != "" {
		block["name"] = jsTrim(value)
	}
	if value, ok := org.(string); ok && jsTrim(value) != "" {
		block["org"] = jsTrim(value)
	}
	if rp := NormalizeRemoteProfileName(remoteProfile); rp != "" {
		block["remoteProfile"] = rp
	}
	return block, nil
}

func jsCoalesce(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// SSH block of a save, explicit empty fields winning, the adopted dashboard token kept while the host is unchanged
func buildSSHBlock(input, existingBlock map[string]any) (map[string]any, error) {
	merged := NormalizeSSHConfig(map[string]any{
		"mode":             "ssh",
		"host":             jsCoalesce(input["sshHost"], existingBlock["host"]),
		"user":             jsCoalesce(input["sshUser"], existingBlock["user"]),
		"port":             jsCoalesce(input["sshPort"], existingBlock["port"]),
		"keyPath":          jsCoalesce(input["sshKeyPath"], existingBlock["keyPath"]),
		"remoteHermesPath": jsCoalesce(input["sshRemoteHermesPath"], existingBlock["remoteHermesPath"]),
		"remoteProfile":    jsCoalesce(input["sshRemoteProfile"], existingBlock["remoteProfile"]),
	})
	if merged == nil {
		return nil, errors.New("SSH host is required.")
	}
	block := merged.object()
	if jsTruthy(existingBlock["token"]) && existingBlock["host"] == merged.Host {
		block["token"] = existingBlock["token"]
	}
	return block, nil
}

// Mode the profile scoped saves keep at the top level, local unless it already names ssh or a remote
func keptTopLevelMode(existing connectionConfig) string {
	if existing.Mode == "ssh" || ModeIsRemoteLike(existing.Mode) {
		return existing.Mode
	}
	return "local"
}

func objectOrEmpty(v any) map[string]any {
	if obj := jsObject(v); obj != nil {
		return obj
	}
	return map[string]any{}
}

// Profiles of a config with one entry set, a new name appended to the file order
func withProfile(existing connectionConfig, key string, entry any) connectionConfig {
	next := existing.clone()
	if _, ok := next.Profiles[key]; !ok {
		next.order = append(next.profileOrder(), key)
	}
	next.Profiles[key] = entry
	return next
}

// Next v1 settings of a payload over existing, a cloud or SSH block never leaking into another mode, caller holds s.mu
func (s *Service) coerceDesktopConnectionConfig(input map[string]any, existing connectionConfig, persistToken bool) (connectionConfig, error) {
	key := ConnectionScopeKey(input["profile"])
	mode := "local"
	if input["mode"] == "ssh" {
		mode = "ssh"
	} else if ModeIsRemoteLike(input["mode"]) {
		mode = input["mode"].(string)
	}
	remoteLike := ModeIsRemoteLike(mode)
	var rawExistingBlock map[string]any
	var existingMode any
	if key != "" {
		rawExistingBlock = objectOrEmpty(existing.Profiles[key])
		existingMode = jsObject(existing.Profiles[key])["mode"]
	} else {
		rawExistingBlock = objectOrEmpty(existing.Remote)
		existingMode = existing.Mode
	}
	leavingCloud := existingMode == "cloud" && mode != "cloud"
	leavingSSH := rawExistingBlock["mode"] == "ssh" && mode != "ssh" && mode != "local"
	existingBlock := rawExistingBlock
	if leavingCloud || leavingSSH {
		existingBlock = map[string]any{}
	}
	remoteURL := jsTrim(jsString(jsCoalesce(input["remoteUrl"], existingBlock["url"], "")))
	authMode := ResolveAuthMode(input["remoteAuthMode"], existingBlock["authMode"])
	cloudOrg := ""
	if mode == "cloud" {
		cloudOrg = jsTrim(jsString(jsCoalesce(input["cloudOrg"], existingBlock["org"], "")))
	}
	cloudName := ""
	if mode == "cloud" {
		name := input["cloudName"]
		if name == nil {
			name = ""
			if jsTruthy(existingBlock["url"]) {
				next, err := NormalizeRemoteBaseURL(remoteURL)
				if err != nil {
					return connectionConfig{}, err
				}
				saved, err := NormalizeRemoteBaseURL(existingBlock["url"])
				if err != nil {
					return connectionConfig{}, err
				}
				if next == saved {
					name = jsCoalesce(existingBlock["name"], "")
				}
			}
		}
		cloudName = jsTrim(jsString(name))
	}
	incomingToken := ""
	if typed, ok := input["remoteToken"].(string); ok {
		incomingToken = jsTrim(typed)
	}
	remoteHeaders := existingBlock["headers"]
	if headers := input["remoteHeaders"]; headers != nil {
		switch headers.(type) {
		case map[string]any, []any:
			remoteHeaders = headers
		}
	}
	existingToken, existingTokenSet := existingBlock["token"]
	nextToken, err := ResolvePersistedRemoteToken(incomingToken, persistToken, existingToken, input["allowPlainTextToken"], s.encryptDesktopSecretLocked)
	if err != nil {
		return connectionConfig{}, err
	}
	nextTokenSet := incomingToken != "" || existingTokenSet
	if mode == "ssh" {
		sshSource := rawExistingBlock
		if saved := SavedProfileSSH(existing.object(), key); saved != nil {
			sshSource = saved.object()
		}
		sshBlock, err := buildSSHBlock(input, sshSource)
		if err != nil {
			return connectionConfig{}, err
		}
		if key != "" {
			next := withProfile(existing, key, sshBlock)
			next.Mode = keptTopLevelMode(existing)
			next.Remote = objectOrEmpty(existing.Remote)
			return next, nil
		}
		next := existing.clone()
		next.Mode, next.Remote = "ssh", sshBlock
		return next, nil
	}
	if key != "" {
		var next connectionConfig
		if remoteLike {
			block, err := s.buildRemoteBlock(remoteURL, authMode, nextToken, nextTokenSet, cloudOrg, remoteHeaders, cloudName, input["remoteProfile"])
			if err != nil {
				return connectionConfig{}, err
			}
			next = withProfile(existing, key, jsSpread(map[string]any{"mode": mode}, block))
		} else if localEntry := LocalProfileEntry(rawExistingBlock); localEntry != nil {
			next = withProfile(existing, key, localEntry)
		} else {
			next = existing.clone()
			delete(next.Profiles, key)
		}
		next.Mode = keptTopLevelMode(existing)
		next.Remote = objectOrEmpty(existing.Remote)
		return next, nil
	}
	var nextRemote any
	switch {
	case remoteLike:
		block, err := s.buildRemoteBlock(remoteURL, authMode, nextToken, nextTokenSet, cloudOrg, remoteHeaders, cloudName, nil)
		if err != nil {
			return connectionConfig{}, err
		}
		nextRemote = block
	case existingMode == "ssh":
		nextRemote = rawExistingBlock
	default:
		url := remoteURL
		if remoteURL != "" {
			if url, err = NormalizeRemoteBaseURL(remoteURL); err != nil {
				return connectionConfig{}, err
			}
		}
		block := map[string]any{"url": url, "authMode": authMode}
		if nextTokenSet {
			block["token"] = nextToken
		}
		nextRemote = block
	}
	next := existing.clone()
	next.Mode, next.Remote = mode, nextRemote
	return next, nil
}

// AuthProvider is one sign in provider a gated gateway lists
type AuthProvider struct {
	Name             string `json:"name"`
	DisplayName      string `json:"displayName"`
	SupportsPassword bool   `json:"supportsPassword"`
}

// AuthModeProbe is how a gateway expects callers to authenticate, read without sending credentials
type AuthModeProbe struct {
	BaseURL   string         `json:"baseUrl"`
	Reachable bool           `json:"reachable"`
	AuthMode  string         `json:"authMode"`
	Providers []AuthProvider `json:"providers"`
	Version   any            `json:"version"`
	Error     *string        `json:"error"`
}

// ProbeConnectionConfig reads the public status and providers of a gateway, an unreachable one answering reachable false
func (s *Service) ProbeConnectionConfig(rawURL any) (AuthModeProbe, error) {
	baseURL, err := NormalizeRemoteBaseURL(rawURL)
	if err != nil {
		return AuthModeProbe{}, err
	}
	ctx := context.Background()
	status, err := s.fetchPublicJSON(ctx, baseURL+"/api/status", JSONOptions{TimeoutMs: 8_000})
	if err != nil {
		return AuthModeProbe{BaseURL: baseURL, AuthMode: "unknown", Providers: []AuthProvider{}, Error: stringPointer(err.Error())}, nil
	}
	authRequired := AuthModeFromStatus(status) == "oauth"
	providers := []AuthProvider{}
	if authRequired {
		if body, err := s.fetchPublicJSON(ctx, baseURL+"/api/auth/providers", JSONOptions{TimeoutMs: 8_000}); err == nil {
			if listed, ok := jsObject(body)["providers"].([]any); ok {
				for _, item := range listed {
					p, isObject := item.(map[string]any)
					if !isObject {
						continue
					}
					provider := AuthProvider{Name: jsOrString(p["name"]), SupportsPassword: jsTruthy(p["supports_password"])}
					if jsTruthy(p["display_name"]) {
						provider.DisplayName = jsString(p["display_name"])
					} else {
						provider.DisplayName = provider.Name
					}
					if provider.Name != "" {
						providers = append(providers, provider)
					}
				}
			}
		}
	}
	out := AuthModeProbe{BaseURL: baseURL, Reachable: true, AuthMode: "token", Providers: providers}
	if authRequired {
		out.AuthMode = "oauth"
	}
	if version := jsObject(status)["version"]; jsTruthy(version) {
		out.Version = version
	}
	return out, nil
}
