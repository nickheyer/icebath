package provider

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CredentialNotSavedError means add wrote the pool but the store does not hold the new row
type CredentialNotSavedError struct{ Message string }

func (e *CredentialNotSavedError) Error() string { return e.Message }

// Copy with every error field cleared and the reset marker stamped for peers
func clearedStatusCopy(entry PooledCredential) PooledCredential {
	u := entry.Clone().ClearStatus()
	u.ModelCooldowns = nil
	u.StatusClearedAt = nowEpoch()
	delete(u.Extra, "failure_reason")
	return u
}

// ResetStatus clears only the target's local error state, preserving sibling cooldowns
func (p *Pool) ResetStatus(credentialID string) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.findByID(credentialID)
	if !ok {
		return PooledCredential{}, false
	}
	cleared := clearedStatusCopy(entry)
	p.replaceEntry(cleared, true)
	p.persist(true, nil, []string{cleared.ID})
	cleared.scope = p.env.Scope
	return cleared, true
}

// ResetStatuses clears exhaustion state on every entry and returns how many were cleared
func (p *Pool) ResetStatuses() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var staleIDs []string
	for i, e := range p.entries {
		if e.LastStatus != "" || e.LastStatusAt != 0 || e.LastErrorCode != 0 || e.FailureReason() != "" || len(e.ModelCooldowns) > 0 {
			staleIDs = append(staleIDs, e.ID)
			p.entries[i] = clearedStatusCopy(e)
		}
	}
	if len(staleIDs) > 0 {
		p.persist(true, nil, staleIDs)
	}
	return len(staleIDs)
}

// RemoveIndex drops the one based entry and renumbers priorities
func (p *Pool) RemoveIndex(index int) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 1 || index > len(p.entries) {
		return PooledCredential{}, false
	}
	removed := p.entries[index-1]
	p.entries = append(p.entries[:index-1], p.entries[index:]...)
	for i := range p.entries {
		p.entries[i].Priority = i
	}
	p.persist(true, []string{removed.ID}, nil)
	if p.currentID == removed.ID {
		p.currentID = ""
	}
	return removed, true
}

// MoveEntry places an entry at a clamped zero based position and persists contiguous priorities
func (p *Pool) MoveEntry(credentialID string, priority int) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.findByID(credentialID)
	if !ok {
		return PooledCredential{}, false
	}
	others := make([]PooledCredential, 0, len(p.entries))
	for _, e := range p.entries {
		if e.ID != credentialID {
			others = append(others, e)
		}
	}
	pos := priority
	if pos < 0 {
		pos = 0
	}
	if pos > len(others) {
		pos = len(others)
	}
	others = append(others[:pos], append([]PooledCredential{entry}, others[pos:]...)...)
	for i := range others {
		others[i].Priority = i
	}
	normalizePoolPriorities(p.provider, others)
	sort.SliceStable(others, func(i, j int) bool { return others[i].Priority < others[j].Priority })
	p.entries = others
	p.persist(true, nil, nil)
	return p.findByID(credentialID)
}

// ResolveTarget finds an entry by id, unique label or one based index, with the user facing error
func (p *Pool) ResolveTarget(target string) (int, PooledCredential, string) {
	raw := strings.TrimSpace(target)
	if raw == "" {
		return 0, PooledCredential{}, "No credential target provided."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.ID == raw {
			return i + 1, e, ""
		}
	}
	var labelMatches []int
	for i, e := range p.entries {
		if strings.ToLower(strings.TrimSpace(e.Label)) == strings.ToLower(raw) {
			labelMatches = append(labelMatches, i)
		}
	}
	if len(labelMatches) == 1 {
		return labelMatches[0] + 1, p.entries[labelMatches[0]], ""
	}
	if len(labelMatches) > 1 {
		return 0, PooledCredential{}, fmt.Sprintf("Ambiguous credential label \"%s\". Use the numeric index or entry id instead.", raw)
	}
	if index, err := strconv.Atoi(raw); err == nil && isDigits(raw) {
		if index >= 1 && index <= len(p.entries) {
			return index, p.entries[index-1], ""
		}
		return 0, PooledCredential{}, fmt.Sprintf("No credential #%d.", index)
	}
	return 0, PooledCredential{}, fmt.Sprintf("No credential matching \"%s\".", raw)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AddEntry appends a credential and persists it to the profile's own store, never to a borrowed root
func (p *Pool) AddEntry(entry PooledCredential) (PooledCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry = entry.Clone()
	entry.Priority = nextPriority(p.entries)
	entry.scope = p.env.Scope
	p.entries = append(p.entries, entry)
	profileBorrows := SingleUseRefreshPoolProviders[p.provider] && !profileOwnsPoolProvider(p.env, p.provider) && borrowedSingleUsePoolRoot(p.env) != ""
	if len(p.borrowedRootIDs) > 0 || profileBorrows {
		p.entries = []PooledCredential{entry}
		written, err := p.env.Store.WriteCredentialPool(p.provider, []map[string]any{entry.ToMap()}, nil, nil, p.persistedTokenPairs)
		if err != nil {
			return PooledCredential{}, err
		}
		p.persistedTokenPairs = TokenPairsByID(written)
		p.borrowedRootIDs = map[string]bool{}
	} else {
		p.persist(true, nil, nil)
	}
	rows, _ := p.env.Store.ReadCredentialPool(p.provider)
	for _, row := range rows {
		if anyString(row["id"]) == entry.ID {
			return entry, nil
		}
	}
	kept := p.entries[:0:0]
	for _, e := range p.entries {
		if e.ID != entry.ID {
			kept = append(kept, e)
		}
	}
	p.entries = kept
	return PooledCredential{}, &CredentialNotSavedError{Message: fmt.Sprintf("The %s credential was not saved: %s does not contain it after the write, so nothing was added. Check that the file is writable, then run the command again.", p.provider, p.env.Store.Path())}
}
