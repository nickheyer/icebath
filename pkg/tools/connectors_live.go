package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// OperationAlreadyOpen is the refusal to open a second live operation in one profile session
type OperationAlreadyOpen struct{ Existing *ConnectionOperation }

func (e *OperationAlreadyOpen) Error() string {
	return fmt.Sprintf("session %s already has operation %s open", pyrepr.StrRepr(e.Existing.SessionKey), e.Existing.OpID)
}

type liveKey struct{ profile, session string }

var (
	liveMu   sync.Mutex
	liveOpen = map[liveKey]*ConnectionOperation{}
)

// Profile key of a session record home, the process home when the record names none
func liveProfileKey(profileHome string) string {
	if profileHome == "" {
		profileHome = home.Process()
	}
	return home.Key(profileHome)
}

// Registers the operation as its profile session's open one, refusing while another is unsettled
func LiveOpen(ctx context.Context, op *ConnectionOperation) error {
	op.ProfileKey = home.Key(home.Dir(ctx))
	key := liveKey{op.ProfileKey, op.SessionKey}
	liveMu.Lock()
	if existing := liveOpen[key]; existing != nil && !existing.Settled() {
		liveMu.Unlock()
		return &OperationAlreadyOpen{Existing: existing}
	}
	liveOpen[key] = op
	liveMu.Unlock()
	parts := make([]string, len(op.Targets))
	for i, t := range op.Targets {
		parts[i] = t.Kind + ":" + t.Name + ":" + t.Action
	}
	slog.Default().Info(fmt.Sprintf("connection op %s opened: %s", op.OpID, strings.Join(parts, ", ")))
	return nil
}

// The session's unsettled open operation or nil
func LiveCurrent(sessionKey, profileHome string) *ConnectionOperation {
	liveMu.Lock()
	op := liveOpen[liveKey{liveProfileKey(profileHome), sessionKey}]
	liveMu.Unlock()
	if op != nil && !op.Settled() {
		return op
	}
	return nil
}

// The session's open operation when its id matches, settled or not
func LiveGet(sessionKey, opID, profileHome string) *ConnectionOperation {
	liveMu.Lock()
	op := liveOpen[liveKey{liveProfileKey(profileHome), sessionKey}]
	liveMu.Unlock()
	if op != nil && op.OpID == opID {
		return op
	}
	return nil
}

// The profile's unsettled open operation with this id in any session
func LiveGetByOpID(opID, profileHome string) *ConnectionOperation {
	pk := liveProfileKey(profileHome)
	liveMu.Lock()
	defer liveMu.Unlock()
	for key, op := range liveOpen {
		if key.profile == pk && op.OpID == opID && !op.Settled() {
			return op
		}
	}
	return nil
}

// The unsettled operation connecting this connector account, matched on name, alias and repaired account id
func LiveFindTarget(name string, alias, repairID *string, profileHome string) *ConnectionOperation {
	pk := liveProfileKey(profileHome)
	liveMu.Lock()
	defer liveMu.Unlock()
	for key, op := range liveOpen {
		if key.profile != pk || op.Settled() {
			continue
		}
		t := op.Target(name)
		if t != nil && t.Kind == "connector" && sameOptional(t.Alias, alias) && sameOptional(t.RepairID, repairID) {
			return op
		}
	}
	return nil
}

func sameOptional(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Drops the operation from the live table when it is still the registered one
func LiveClose(op *ConnectionOperation) {
	key := liveKey{op.ProfileKey, op.SessionKey}
	liveMu.Lock()
	if liveOpen[key] == op {
		delete(liveOpen, key)
	}
	liveMu.Unlock()
}

// Empties the live table between tests
func LiveResetForTests() {
	liveMu.Lock()
	clear(liveOpen)
	liveMu.Unlock()
}
