package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// OperationAlreadyOpen is the refusal to open a second live operation in one profile session
type OperationAlreadyOpen struct {
	Existing *ConnectionOperation
	msg      string
}

// Refusal naming the session and the operation it already holds open
func newOperationAlreadyOpen(existing *ConnectionOperation) *OperationAlreadyOpen {
	return &OperationAlreadyOpen{Existing: existing,
		msg: fmt.Sprintf("session %s already has operation %s open", strconv.Quote(existing.SessionKey), existing.OpID)}
}

func (e *OperationAlreadyOpen) Error() string { return e.msg }

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
		return newOperationAlreadyOpen(existing)
	}
	liveOpen[key] = op
	liveMu.Unlock()
	parts := make([]string, len(op.Targets))
	for i, t := range op.Targets {
		parts[i] = t.Kind + ":" + t.Name + ":" + t.Action
	}
	pkgLog().Info(fmt.Sprintf("connection op %s opened: %s", op.OpID, strings.Join(parts, ", ")))
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

// Drops the operation from the live table when it is still the registered one
func LiveClose(op *ConnectionOperation) {
	key := liveKey{op.ProfileKey, op.SessionKey}
	liveMu.Lock()
	if liveOpen[key] == op {
		delete(liveOpen, key)
	}
	liveMu.Unlock()
}

// Settles every live operation as interrupted so its driver ends, then empties the live table between tests
func LiveResetForTests() {
	liveMu.Lock()
	open := make([]*ConnectionOperation, 0, len(liveOpen))
	for _, op := range liveOpen {
		open = append(open, op)
	}
	clear(liveOpen)
	liveMu.Unlock()
	for _, op := range open {
		op.Settle(SettleInterrupt)
	}
}
