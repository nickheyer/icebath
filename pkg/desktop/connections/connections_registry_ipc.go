package connections

import (
	"sort"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// QuarantineNotice is the reason and label of a malformed registry entry, its raw entry staying on disk
type QuarantineNotice struct {
	Reason string `json:"reason"`
	Label  string `json:"label"`
}

// SanitizedRegistry is the renderer view of connections.json, token and header values never crossing the bridge
type SanitizedRegistry struct {
	Version            int                `json:"version"`
	Primary            string             `json:"primary"`
	LaunchMode         string             `json:"launchMode"`
	LastUsed           string             `json:"lastUsed"`
	SecureTokenStorage bool               `json:"secureTokenStorage"`
	Connections        []map[string]any   `json:"connections"`
	Quarantined        []QuarantineNotice `json:"quarantined"`
}

// RegistryMutationResult is the answer of a registry write, the registry as the renderer sees it
type RegistryMutationResult struct {
	OK       bool              `json:"ok"`
	Registry SanitizedRegistry `json:"registry"`
}

// Whether stored tokens can be sealed, true while the keychain encryption is opted out
func (s *Service) probeSecureTokenStorage() bool {
	if !s.secretStoragePolicy().On {
		return true
	}
	return s.safe.IsEncryptionAvailable()
}

// Renderer view of a registry entry with a token preview, the known install id and header names only
func (s *Service) sanitizeRegistryConnection(entry RegistryConnection) map[string]any {
	out := entry.object()
	delete(out, "token")
	delete(out, "headers")
	decrypted := s.decryptDesktopSecret(entry.Token)
	out["tokenSet"] = decrypted != ""
	out["tokenPreview"] = TokenPreview(decrypted)
	if known, ok := backend.SharedConnectionCaches.InstallID(entry.ID); ok && known.ID != "" {
		out["installId"] = known.ID
	}
	names := []string{}
	if entry.Headers != nil {
		for name := range entry.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	out["headerNames"] = names
	return out
}

// Renderer view of the registry with the keyring signal and quarantine notices, called with s.mu held
func (s *Service) sanitizeConnectionsRegistry(registry ConnectionRegistry) SanitizedRegistry {
	out := SanitizedRegistry{
		Version:            registry.Version,
		Primary:            registry.Primary,
		LaunchMode:         registry.LaunchMode,
		LastUsed:           registry.LastUsed,
		SecureTokenStorage: s.probeSecureTokenStorage(),
		Connections:        make([]map[string]any, 0, len(registry.Connections)),
		Quarantined:        make([]QuarantineNotice, 0, len(registry.Quarantined)),
	}
	for _, entry := range registry.Connections {
		out.Connections = append(out.Connections, s.sanitizeRegistryConnection(entry))
	}
	for _, q := range registry.Quarantined {
		reason := q.Reason
		if reason == "" {
			reason = "unknown"
		}
		label, _ := jsObject(q.Entry)["label"].(string)
		out.Quarantined = append(out.Quarantined, QuarantineNotice{Reason: reason, Label: label})
	}
	return out
}

// ConnectionsList answers the registry as the renderer sees it
func (s *Service) ConnectionsList() (SanitizedRegistry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	registry, err := s.readDesktopConnectionsRegistry()
	if err != nil {
		return SanitizedRegistry{}, err
	}
	return s.sanitizeConnectionsRegistry(registry), nil
}

// ConnectionsSetLastUsed records the source the Sessions workspace last opened
func (s *Service) ConnectionsSetLastUsed(id any) (RegistryMutationResult, error) {
	key := ""
	if jsTruthy(id) {
		key = jsString(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	registry, err := s.readDesktopConnectionsRegistry()
	if err != nil {
		return RegistryMutationResult{}, err
	}
	registry, err = SetLastUsedConnection(registry, key)
	if err != nil {
		return RegistryMutationResult{}, err
	}
	if err := s.writeDesktopConnectionsRegistry(registry); err != nil {
		return RegistryMutationResult{}, err
	}
	return RegistryMutationResult{OK: true, Registry: s.sanitizeConnectionsRegistry(registry)}, nil
}
