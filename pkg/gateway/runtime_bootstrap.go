package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"sync"
)

// Capabilities each bootstrap purpose grants
var purposeCapabilities = map[string]map[string]bool{
	"interactive": {"session:create": true, "session:read": true, "session:submit": true,
		"session:control": true, "session:approve": true, "session:respond": true},
	"native-http":             {"http:owner": true},
	"native-profile-create":   {"profiles:create": true},
	"native-profile-registry": {"profiles:registry": true},
	"exposure":                {"transport:delegate": true},
	"worker-adoption":         {"worker:adopt": true},
}

// Profile reach a grant may carry, its own home or every served sibling
var ticketScopes = map[string]bool{"profile": true, "host": true}

const (
	ticketTTLSeconds = 30.0
	ticketMaxEntries = 1024
)

// Monotonic seconds the ticket expiries read, replaced by tests the way hermes tests monkeypatch time.monotonic
var ticketMonotonic = monotonicSeconds

// One minted bootstrap grant with its expiry on the monotonic clock
type ticketEntry struct {
	expires float64
	grant   map[string]any
}

// Private local bootstrap grants minted on the control listener and redeemed once by a local client
type TicketStore struct {
	InstanceID string
	mu         sync.Mutex
	profileIDs map[string]bool
	entries    map[string]ticketEntry
}

// Store for one runtime instance serving profileIDs, a ValueError without either
func NewTicketStore(instanceID string, profileIDs map[string]bool) (*TicketStore, error) {
	if instanceID == "" || len(profileIDs) == 0 {
		return nil, &PyError{Kind: "ValueError", Msg: "bootstrap requires instance and served profiles"}
	}
	return &TicketStore{InstanceID: instanceID, profileIDs: maps.Clone(profileIDs), entries: map[string]ticketEntry{}}, nil
}

// Served profile ids a ticket may bind
func (s *TicketStore) ProfileIDs() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.profileIDs)
}

// Replaces the served profile ids after the served set changed
func (s *TicketStore) SetProfileIDs(ids map[string]bool) {
	s.mu.Lock()
	s.profileIDs = maps.Clone(ids)
	s.mu.Unlock()
}

// Sha256 hex key a ticket is stored under
func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// Mints a single use ticket binding a profile, subject and purpose, a PermissionError for a rejected binding
func (s *TicketStore) Mint(profileID, subject, purpose, scope string, refuseRestartWait bool) (string, error) {
	if scope == "" {
		scope = "profile"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	capabilities, known := purposeCapabilities[purpose]
	if !s.profileIDs[profileID] || !known || subject == "" || !ticketScopes[scope] || (scope == "host" && purpose != "interactive") {
		return "", &PyError{Kind: "PermissionError", Msg: "bootstrap binding rejected"}
	}
	now := ticketMonotonic()
	for k, v := range s.entries {
		if v.expires <= now {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= ticketMaxEntries {
		return "", &PyError{Kind: "PermissionError", Msg: "bootstrap capacity exhausted"}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	s.entries[ticketKey(ticket)] = ticketEntry{expires: now + ticketTTLSeconds, grant: map[string]any{
		"instance_id": s.InstanceID, "profile_id": profileID, "subject": subject, "purpose": purpose,
		"capabilities": maps.Clone(capabilities), "scope": scope, "refuse_restart_wait": refuseRestartWait,
	}}
	return ticket, nil
}

// Redeems a ticket once for its purpose, any served profile when profileID is nil, a PermissionError otherwise
func (s *TicketStore) Redeem(ticket any, profileID *string, purpose string) (map[string]any, error) {
	text, ok := ticket.(string)
	if !ok || len([]rune(text)) > 256 {
		return nil, &PyError{Kind: "PermissionError", Msg: "invalid bootstrap ticket"}
	}
	key := ticketKey(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return nil, &PyError{Kind: "PermissionError", Msg: "invalid bootstrap ticket"}
	}
	if entry.expires <= ticketMonotonic() {
		delete(s.entries, key)
		return nil, &PyError{Kind: "PermissionError", Msg: "expired bootstrap ticket"}
	}
	granted, _ := entry.grant["profile_id"].(string)
	served := s.profileIDs[granted]
	if profileID != nil {
		served = granted == *profileID
	}
	if !served || entry.grant["purpose"] != purpose {
		return nil, &PyError{Kind: "PermissionError", Msg: "bootstrap binding rejected"}
	}
	delete(s.entries, key)
	return maps.Clone(entry.grant), nil
}

// Whether an unexpired ticket was minted and not yet redeemed
func (s *TicketStore) Outstanding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := ticketMonotonic()
	for _, v := range s.entries {
		if v.expires > now {
			return true
		}
	}
	return false
}

// Drops every outstanding ticket
func (s *TicketStore) Revoke() {
	s.mu.Lock()
	clear(s.entries)
	s.mu.Unlock()
}
