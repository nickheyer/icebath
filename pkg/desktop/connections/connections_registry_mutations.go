package connections

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// ConnectionsChanged is the registry lifecycle push a removed, edited or saved connection sends every window
type ConnectionsChanged struct {
	ConnectionID string `json:"connectionId"`
	Reason       string `json:"reason"`
}

func (s *Service) broadcastConnectionsChanged(connectionID, reason string) {
	s.broadcast("connections:changed", ConnectionsChanged{ConnectionID: connectionID, Reason: reason})
}

// Plaintext header values sealed like tokens, a null value keeping the stored one and an empty one dropping it
func (s *Service) encryptIncomingRemoteHeaders(raw any, existing map[string]SecretEnvelope, allowPlainText bool) (map[string]any, error) {
	out := map[string]any{}
	stored := map[string]any{}
	for k, v := range existing {
		stored[k] = toJSONValue(v)
	}
	for _, entry := range jsEntriesOf(raw) {
		key := jsTrim(jsOrString(entry[0]))
		if key == "" {
			continue
		}
		switch value := entry[1].(type) {
		case string:
			if trimmed := jsTrim(value); trimmed != "" {
				sealed, err := s.encryptDesktopSecret(trimmed, allowPlainText)
				if err != nil {
					return nil, err
				}
				out[key] = sealed
			}
		case nil:
			if jsTruthy(stored[key]) {
				out[key] = stored[key]
			}
		default:
			if jsIsObject(value) {
				out[key] = value
			}
		}
	}
	return out, nil
}

// Creates or edits a registry connection, recycling its backends when dial material changed
func (s *Service) saveRegistryConnection(input map[string]any) (map[string]any, error) {
	s.mu.Lock()
	registry, err := s.readDesktopConnectionsRegistry()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var existing *RegistryConnection
	if jsTruthy(input["id"]) {
		existing = registry.Find(jsString(input["id"]))
	}
	incomingToken := ""
	if t, ok := input["token"].(string); ok {
		incomingToken = jsTrim(t)
	}
	allowPlainText := input["allowPlainTextToken"] == true
	var token any
	if incomingToken == "" {
		if existing != nil {
			token = existing.Token
		}
	} else if token, err = s.encryptDesktopSecret(incomingToken, allowPlainText); err != nil {
		return nil, err
	}
	headers := input["headers"]
	if jsTruthy(headers) && jsIsObject(headers) {
		var stored map[string]SecretEnvelope
		if existing != nil {
			stored = existing.Headers
		}
		if headers, err = s.encryptIncomingRemoteHeaders(headers, stored, allowPlainText); err != nil {
			return nil, err
		}
	}
	next := jsSpreadOf(input)
	next["token"] = token
	next["headers"] = headers
	merged := MergeConnectionInput(next, existing)
	s.mu.Lock()
	entry, err := NormalizeConnectionInput(merged, registry)
	if err == nil && entry.Kind == "remote" && entry.AuthMode != "oauth" && s.decryptDesktopSecret(entry.Token) == "" {
		err = errors.New("Remote gateway session token is required.")
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	changed := existing != nil && ConnectionDialFieldsChanged(*existing, entry)
	if changed {
		if err := s.ssh.gate.AssertCanMutate(entry.ID); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	err = s.writeDesktopConnectionsRegistry(UpsertConnection(registry, entry))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if changed {
		if err := s.stopRegistryConnectionBackends(entry.ID); err != nil {
			return nil, err
		}
		backend.SharedConnectionCaches.Evict(entry.ID)
		s.broadcastConnectionsChanged(entry.ID, "updated")
	} else {
		s.broadcastConnectionsChanged(entry.ID, "saved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sanitizeRegistryConnection(entry), nil
}

// ConnectionSaveResult is the answer of connections.save
type ConnectionSaveResult struct {
	OK         bool              `json:"ok"`
	Connection map[string]any    `json:"connection"`
	Registry   SanitizedRegistry `json:"registry"`
}

// ConnectionsSave creates or edits a registry connection
func (s *Service) ConnectionsSave(payload any) (ConnectionSaveResult, error) {
	input := jsObject(payload)
	if input == nil {
		input = map[string]any{}
	}
	saved, err := s.saveRegistryConnection(input)
	if err != nil {
		return ConnectionSaveResult{}, err
	}
	list, err := s.ConnectionsList()
	if err != nil {
		return ConnectionSaveResult{}, err
	}
	return ConnectionSaveResult{OK: true, Connection: saved, Registry: list}, nil
}

// ConnectionsRemove deletes a registry connection and everything it still runs or caches
func (s *Service) ConnectionsRemove(id any) (RegistryMutationResult, error) {
	key := ""
	if jsTruthy(id) {
		key = jsString(id)
	}
	if err := s.ssh.gate.AssertCanMutate(key); err != nil {
		return RegistryMutationResult{}, err
	}
	s.mu.Lock()
	registry, err := s.readDesktopConnectionsRegistry()
	if err == nil {
		registry, err = RemoveConnection(registry, key)
	}
	if err == nil {
		err = s.writeDesktopConnectionsRegistry(registry)
	}
	s.mu.Unlock()
	if err != nil {
		return RegistryMutationResult{}, err
	}
	if err := s.stopRegistryConnectionBackends(key); err != nil {
		return RegistryMutationResult{}, err
	}
	backend.SharedConnectionCaches.Evict(key)
	s.broadcastConnectionsChanged(key, "removed")
	s.profiles.mu.Lock()
	err = s.profiles.connectionRemoved(key)
	s.profiles.mu.Unlock()
	if err != nil {
		return RegistryMutationResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return RegistryMutationResult{OK: true, Registry: s.sanitizeConnectionsRegistry(registry)}, nil
}

// Writes a registry change to the primary routing once no managed SSH update owns it
func (s *Service) mutatePrimaryRouting(change func(ConnectionRegistry) (ConnectionRegistry, error)) (RegistryMutationResult, error) {
	if err := s.assertCanMutateManagedPrimaryRouting(); err != nil {
		return RegistryMutationResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	registry, err := s.readDesktopConnectionsRegistry()
	if err != nil {
		return RegistryMutationResult{}, err
	}
	if registry, err = change(registry); err != nil {
		return RegistryMutationResult{}, err
	}
	if err := s.writeDesktopConnectionsRegistry(registry); err != nil {
		return RegistryMutationResult{}, err
	}
	return RegistryMutationResult{OK: true, Registry: s.sanitizeConnectionsRegistry(registry)}, nil
}

// ConnectionsSetPrimary names the source the primary window dials
func (s *Service) ConnectionsSetPrimary(id any) (RegistryMutationResult, error) {
	key := ""
	if jsTruthy(id) {
		key = jsString(id)
	}
	return s.mutatePrimaryRouting(func(r ConnectionRegistry) (ConnectionRegistry, error) { return SetPrimaryConnection(r, key) })
}

// ConnectionsSetLaunchMode picks whether launch opens the primary or the last used source
func (s *Service) ConnectionsSetLaunchMode(mode any) (RegistryMutationResult, error) {
	value := ""
	if jsTruthy(mode) {
		value = jsString(mode)
	}
	return s.mutatePrimaryRouting(func(r ConnectionRegistry) (ConnectionRegistry, error) { return SetConnectionLaunchMode(r, value) })
}
