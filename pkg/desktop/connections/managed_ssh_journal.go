package connections

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

// Writes the journal owner only with the record objects in their key order
func (s *Service) writeManagedSSHRecoveryRecords(records []ManagedSSHRecoveryRecord) error {
	path := s.managedSSHRecoveryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	raw := make([]any, len(records))
	for i, r := range records {
		raw[i] = r.Raw
	}
	text := jsjson.Stringify(jsjson.NewOrderedObject("version", float64(1), "records", raw, "updatedAt", isoNow()), "  ", "")
	return hardening.WriteSecretFileAtomic(path, []byte(text), hardening.SecretFileOptions{})
}

// Journals a prepared managed update with its exact connection snapshot
func (s *Service) persistManagedSSHRecovery(source RegistryConnection, correlationID string, scopes []ManagedSSHScopeInput) error {
	recoveryScopes := ManagedSSHRecoveryScopes(scopes, backend.BackendScopePrefix(source.ID))
	existing, err := s.readManagedSSHRecoveryRecords()
	if err != nil {
		return err
	}
	correlation, err := ValidateCorrelationID(correlationID)
	if err != nil {
		return err
	}
	records := make([]ManagedSSHRecoveryRecord, 0, len(existing)+1)
	for _, r := range existing {
		if r.ConnectionID != source.ID {
			records = append(records, r)
		}
	}
	scopeObjects := make([]any, len(recoveryScopes))
	for i, sc := range recoveryScopes {
		scopeObjects[i] = jsjson.NewOrderedObject("key", sc.Key, "kind", sc.Kind, "profile", sc.Profile)
	}
	sourceObject, err := jsjson.ParseOrdered(jsjson.Stringify(source.object(), "", ""))
	if err != nil {
		return err
	}
	raw := jsjson.NewOrderedObject("connectionId", source.ID, "correlationId", correlation, "createdAt", isoNow(), "phase", "prepared", "scopes", scopeObjects, "source", sourceObject)
	records = append(records, ManagedSSHRecoveryRecord{ConnectionID: source.ID, CorrelationID: correlation, Phase: "prepared", Scopes: recoveryScopes, Source: source.object(), Raw: raw})
	return s.writeManagedSSHRecoveryRecords(records)
}

func (s *Service) updateManagedSSHRecovery(connectionID, correlationID string, required bool, change func(r *ManagedSSHRecoveryRecord)) error {
	records, err := s.readManagedSSHRecoveryRecords()
	if err != nil {
		return err
	}
	for i := range records {
		if records[i].ConnectionID == connectionID && records[i].CorrelationID == correlationID {
			records[i].Raw = records[i].Raw.Clone()
			change(&records[i])
			return s.writeManagedSSHRecoveryRecords(records)
		}
	}
	if required {
		return errors.New("Managed SSH recovery record disappeared before remote update launch.")
	}
	return nil
}

// Marks a journaled update as launching its remote updater
func (s *Service) markManagedSSHRecoveryLaunching(connectionID, correlationID string) error {
	return s.updateManagedSSHRecovery(connectionID, correlationID, true, func(r *ManagedSSHRecoveryRecord) {
		r.Phase = "launching"
		r.Raw.Set("phase", "launching")
	})
}

// Records the failed restore attempts of a journaled update
func (s *Service) recordManagedSSHRecoveryAttempt(connectionID, correlationID string, attempts int) error {
	return s.updateManagedSSHRecovery(connectionID, correlationID, false, func(r *ManagedSSHRecoveryRecord) {
		r.Attempts = &attempts
		r.Raw.Set("attempts", float64(attempts))
	})
}

// Drops a journaled update, the journal itself once it is empty
func (s *Service) clearManagedSSHRecovery(connectionID, correlationID string) error {
	records, err := s.readManagedSSHRecoveryRecords()
	if err != nil {
		return err
	}
	remaining := make([]ManagedSSHRecoveryRecord, 0, len(records))
	for _, r := range records {
		if r.ConnectionID != connectionID || r.CorrelationID != correlationID {
			remaining = append(remaining, r)
		}
	}
	if len(remaining) == len(records) {
		return nil
	}
	if len(remaining) > 0 {
		return s.writeManagedSSHRecoveryRecords(remaining)
	}
	if err := os.Remove(s.managedSSHRecoveryPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Refuses primary routing changes while any managed update, recovery, primary restore or journal entry is pending
func (s *Service) assertCanMutateManagedPrimaryRouting() error {
	records, err := s.readManagedSSHRecoveryRecords()
	if err != nil {
		return err
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	s.ssh.mu.Lock()
	updates := sortedKeys(s.ssh.updates)
	recoveries := sortedKeys(s.ssh.recoveries)
	s.ssh.mu.Unlock()
	for _, id := range updates {
		add(id)
	}
	for _, id := range recoveries {
		add(id)
	}
	s.cloud.mu.Lock()
	restores := make([]string, 0, len(s.cloud.managedPrimaryRestores))
	for id := range s.cloud.managedPrimaryRestores {
		restores = append(restores, id)
	}
	s.cloud.mu.Unlock()
	sort.Strings(restores)
	for _, id := range restores {
		add(id)
	}
	for _, r := range records {
		add(r.ConnectionID)
	}
	if len(ids) > 0 {
		return &CodedError{Message: "Primary connection routing cannot change while managed SSH update recovery is pending for " + strings.Join(ids, ", ") + ".", Code: "managed-update-in-progress"}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
