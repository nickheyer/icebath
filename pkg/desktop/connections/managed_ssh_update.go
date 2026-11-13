package connections

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

const (
	// Update exit code meaning an independently supervised rollout owns the terminal marker
	UpdateExitIndependentHandoff = 75
	// Journal retries of a scope that failed to restore before it stops fencing the connection
	MaxManagedSSHRecoveryAttempts = 3
	managedSSHRecoveryFile        = "managed-ssh-update-recovery.json"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// CodedError is an error the renderer and callers branch on by its code
type CodedError struct {
	Message string
	Code    string
	Cause   error
}

func (e *CodedError) Error() string { return e.Message }
func (e *CodedError) Unwrap() error { return e.Cause }

// ErrorCode is the code of the error
func (e *CodedError) ErrorCode() string { return e.Code }

// Lowercased trimmed correlation id, refusing anything but a UUID
func ValidateCorrelationID(correlationID string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(correlationID))
	if !uuidRE.MatchString(value) {
		return "", errors.New("Managed SSH update correlation ID is invalid.")
	}
	return value, nil
}

// ManagedSSHTokenPersistence names the stores an adopted per serve token is mirrored to
type ManagedSSHTokenPersistence struct {
	LegacySource         string
	RegistryConnectionID string
}

// Registry id and legacy store a served token persists to, registry sources writing no legacy store
func ManagedSSHTokenPersistencePlan(source, registryConnectionID string) ManagedSSHTokenPersistence {
	sourceRegistryID := ""
	if strings.HasPrefix(source, "registry:") {
		sourceRegistryID = source[len("registry:"):]
	}
	legacy := "global"
	if sourceRegistryID != "" {
		legacy = ""
	} else if source == "profile" {
		legacy = "profile"
	}
	id := registryConnectionID
	if id == "" {
		id = sourceRegistryID
	}
	return ManagedSSHTokenPersistence{LegacySource: legacy, RegistryConnectionID: strings.TrimSpace(id)}
}

// Role a cached SSH scope plays for a registry connection, pool, primary or none
func ManagedSSHScopeRole(connectionID, key, prefix, routeConnectionID string, state *SSHConnectionState) string {
	if state != nil && state.RegistryConnectionID == connectionID {
		if state.PrimaryRegistryScope {
			return "primary"
		}
		return "pool"
	}
	if strings.HasPrefix(key, prefix) || (routeConnectionID != "" && routeConnectionID == connectionID) {
		return "pool"
	}
	return ""
}

// ManagedSSHRecoveryScope is one drained scope a crash recovery restores
type ManagedSSHRecoveryScope struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Profile string `json:"profile"`
}

// ManagedSSHScopeInput is a captured scope with whether it is the primary
type ManagedSSHScopeInput struct {
	Key     string
	Primary bool
	Profile string
}

// Unique recovery scopes by key, a primary entry winning over a pool one
func ManagedSSHRecoveryScopes(scopes []ManagedSSHScopeInput, registryPrefix string) []ManagedSSHRecoveryScope {
	var order []string
	unique := map[string]ManagedSSHRecoveryScope{}
	for _, scope := range scopes {
		kind := "legacy"
		if scope.Primary {
			kind = "primary"
		} else if strings.HasPrefix(scope.Key, registryPrefix) {
			kind = "registry"
		}
		profile := scope.Profile
		if profile == "" {
			profile = "default"
		}
		next := ManagedSSHRecoveryScope{Key: scope.Key, Kind: kind, Profile: profile}
		_, ok := unique[scope.Key]
		if !ok {
			order = append(order, scope.Key)
		}
		if !ok || next.Kind == "primary" {
			unique[scope.Key] = next
		}
	}
	out := make([]ManagedSSHRecoveryScope, 0, len(order))
	for _, key := range order {
		out = append(out, unique[key])
	}
	return out
}

// Publishes a bootstrap result only while the gate allows it, rolling it back otherwise
func FenceManagedSSHBootstrapPublication(assertCanPublish func() error, publish func(), rollback func(cause error) error) error {
	if err := assertCanPublish(); err != nil {
		if rollbackErr := rollback(err); rollbackErr != nil {
			return rollbackErr
		}
		return err
	}
	publish()
	return nil
}

// Waits for the bootstraps of a connection and surfaces one whose publication could not be fenced
func WaitForManagedSSHBootstrapFence(entries []*BootstrapEntry, connectionID string) error {
	var errs []error
	for _, e := range entries {
		if e.Metadata.RegistryConnectionID == connectionID {
			if _, err := e.Future.Wait(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, err := range errs {
		if _, unsafe := err.(*managedFenceError); unsafe {
			return err
		}
	}
	return nil
}

// Error of a bootstrap that crossed the managed update gate with an unfenced serve
type managedFenceError struct {
	Message string
	Cause   error
}

func (e *managedFenceError) Error() string { return e.Message }
func (e *managedFenceError) Unwrap() error { return e.Cause }

// Code of the fence failure
func (e *managedFenceError) ErrorCode() string { return "managed-update-bootstrap-fence-failed" }

// ManagedConnectionUpdateGate pauses dials and edits of a registry connection while its managed update owns it
type ManagedConnectionUpdateGate struct {
	mu           sync.Mutex
	claims       map[string]string
	durableOwner func(connectionID string) (string, error)
}

// NewManagedConnectionUpdateGate reads durable owners through durableOwner
func NewManagedConnectionUpdateGate(durableOwner func(connectionID string) (string, error)) *ManagedConnectionUpdateGate {
	return &ManagedConnectionUpdateGate{claims: map[string]string{}, durableOwner: durableOwner}
}

// Claims a connection for an update, false when another claim or a different durable owner holds it
func (g *ManagedConnectionUpdateGate) Claim(connectionID, correlationID string) (bool, error) {
	id := strings.TrimSpace(connectionID)
	correlation, err := ValidateCorrelationID(correlationID)
	if err != nil {
		return false, err
	}
	durable, err := g.durableOwner(id)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, held := g.claims[id]; id == "" || held || (durable != "" && durable != correlation) {
		return false, nil
	}
	g.claims[id] = correlation
	return true, nil
}

// Drops a claim this correlation holds
func (g *ManagedConnectionUpdateGate) Release(connectionID, correlationID string) {
	id := strings.TrimSpace(connectionID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.claims[id] == correlationID {
		delete(g.claims, id)
	}
}

// Owner of a connection, the live claim ahead of the durable journal, empty when none
func (g *ManagedConnectionUpdateGate) Owner(connectionID string) (string, error) {
	id := strings.TrimSpace(connectionID)
	g.mu.Lock()
	claim := g.claims[id]
	g.mu.Unlock()
	if claim != "" {
		return claim, nil
	}
	return g.durableOwner(id)
}

// Refuses a dial of a connection another update owns
func (g *ManagedConnectionUpdateGate) AssertCanDial(connectionID, correlationID string) error {
	id := strings.TrimSpace(connectionID)
	owner, err := g.Owner(id)
	if err != nil {
		return err
	}
	if owner != "" && owner != correlationID {
		return &CodedError{Message: fmt.Sprintf("SSH connection \"%s\" is paused while its managed update is in progress.", id), Code: "managed-update-in-progress"}
	}
	return nil
}

// Refuses an edit or removal of a connection any update owns
func (g *ManagedConnectionUpdateGate) AssertCanMutate(connectionID string) error {
	id := strings.TrimSpace(connectionID)
	owner, err := g.Owner(id)
	if err != nil {
		return err
	}
	if owner != "" {
		return &CodedError{Message: fmt.Sprintf("SSH connection \"%s\" cannot be edited or removed while its managed update recovery is pending.", id), Code: "managed-update-in-progress"}
	}
	return nil
}

// ManagedSSHRecoveryRecord is one journaled managed update with its drained scopes and connection snapshot
type ManagedSSHRecoveryRecord struct {
	ConnectionID  string
	CorrelationID string
	Phase         string
	Attempts      *int
	Scopes        []ManagedSSHRecoveryScope
	Source        map[string]any
	Raw           *jsjson.OrderedObject
}

func (s *Service) managedSSHRecoveryPath() string {
	return filepath.Join(s.userData, managedSSHRecoveryFile)
}

func recoveryUnavailable(cause error) error {
	return &CodedError{Message: "Managed SSH recovery state is unreadable or malformed; refusing connection startup and edits.", Code: "managed-update-recovery-unavailable", Cause: cause}
}

func validRecoveryRecord(record any) (ManagedSSHRecoveryRecord, bool) {
	o, ok := record.(*jsjson.OrderedObject)
	if !ok {
		return ManagedSSHRecoveryRecord{}, false
	}
	connectionID, ok := o.Get("connectionId").(string)
	if !ok {
		return ManagedSSHRecoveryRecord{}, false
	}
	source := ordered(o.Get("source"))
	if source == nil || source.Get("kind") != "ssh" || source.Get("id") != connectionID {
		return ManagedSSHRecoveryRecord{}, false
	}
	phase, _ := o.Get("phase").(string)
	if phase != "prepared" && phase != "launching" {
		return ManagedSSHRecoveryRecord{}, false
	}
	var attempts *int
	if o.Has("attempts") && o.Get("attempts") != nil {
		n, ok := jsInteger(o.Get("attempts"))
		if !ok || n < 0 || n > 9007199254740991 {
			return ManagedSSHRecoveryRecord{}, false
		}
		v := int(n)
		attempts = &v
	} else if o.Has("attempts") {
		return ManagedSSHRecoveryRecord{}, false
	}
	rawScopes, ok := o.Get("scopes").([]any)
	if !ok || len(rawScopes) > 256 {
		return ManagedSSHRecoveryRecord{}, false
	}
	correlation, _ := o.Get("correlationId").(string)
	if _, err := ValidateCorrelationID(correlation); err != nil {
		return ManagedSSHRecoveryRecord{}, false
	}
	scopes := make([]ManagedSSHRecoveryScope, 0, len(rawScopes))
	seen := map[string]bool{}
	primaries := 0
	for _, raw := range rawScopes {
		sc := ordered(raw)
		if sc == nil {
			return ManagedSSHRecoveryRecord{}, false
		}
		key, keyOK := sc.Get("key").(string)
		profile, profileOK := sc.Get("profile").(string)
		kind, _ := sc.Get("kind").(string)
		if !keyOK || utf16Len(key) > 256 || !profileOK || profile == "" || utf16Len(profile) > 128 ||
			(kind != "legacy" && kind != "primary" && kind != "registry") || (kind != "primary" && key == "") {
			return ManagedSSHRecoveryRecord{}, false
		}
		identity := kind + "\x00" + key + "\x00" + profile
		if seen[identity] {
			return ManagedSSHRecoveryRecord{}, false
		}
		seen[identity] = true
		if kind == "primary" {
			primaries++
		}
		scopes = append(scopes, ManagedSSHRecoveryScope{Key: key, Kind: kind, Profile: profile})
	}
	if primaries > 1 {
		return ManagedSSHRecoveryRecord{}, false
	}
	plain, _ := jsjson.Parse(jsjson.Stringify(source, "", ""))
	return ManagedSSHRecoveryRecord{ConnectionID: connectionID, CorrelationID: correlation, Phase: phase, Attempts: attempts, Scopes: scopes, Source: jsObject(plain), Raw: o}, true
}

// Journaled managed updates, empty when the journal is missing, refusing an unsafe or malformed one
func (s *Service) readManagedSSHRecoveryRecords() ([]ManagedSSHRecoveryRecord, error) {
	path := s.managedSSHRecoveryPath()
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, recoveryUnavailable(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !hardening.TightenSecretFileMode(path, hardening.SecretFileOptions{}) {
		return nil, recoveryUnavailable(errors.New("Managed SSH recovery journal is not a safe owner-only file."))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, recoveryUnavailable(err)
	}
	payload, err := jsjson.ParseOrdered(string(data))
	if err != nil {
		return nil, recoveryUnavailable(err)
	}
	p := ordered(payload)
	var records []any
	isArray := false
	if p != nil {
		records, isArray = p.Get("records").([]any)
	}
	if p == nil || p.Get("version") != float64(1) || !isArray {
		return nil, recoveryUnavailable(errors.New("Managed SSH recovery journal has an unsupported shape."))
	}
	out := make([]ManagedSSHRecoveryRecord, 0, len(records))
	for _, raw := range records {
		record, ok := validRecoveryRecord(raw)
		if !ok {
			return nil, recoveryUnavailable(errors.New("Managed SSH recovery journal contains an invalid record."))
		}
		out = append(out, record)
	}
	return out, nil
}

// Durable owner correlation of a connection, empty when the journal names none
func (s *Service) managedSSHDurableOwner(connectionID string) (string, error) {
	records, err := s.readManagedSSHRecoveryRecords()
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.ConnectionID == connectionID {
			return r.CorrelationID, nil
		}
	}
	return "", nil
}
