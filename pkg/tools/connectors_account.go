package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Wait for the operation's prepare step before reading its first target snapshot
const accountPrepareWaitSeconds = 31.0

var accountStartMu sync.Mutex

// An account operation found open or started on a background goroutine, done closed once prepare ran
type AccountOperationStart struct {
	Operation *ConnectionOperation
	Started   bool
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	failed    bool
}

// Marks the prepare step finished, idempotent
func (s *AccountOperationStart) finish() { s.once.Do(func() { close(s.done) }) }

// Whether the background drive raised before the operation settled
func (s *AccountOperationStart) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// The one open operation every requested connector already belongs to, nil when none is open
func matchingOpenAccountOperation(names []string, alias, repairID *string, profileHome string) (*ConnectionOperation, error) {
	var first *ConnectionOperation
	matches := make([]*ConnectionOperation, 0, len(names))
	for _, name := range names {
		op := LiveFindTarget(name, alias, repairID, profileHome)
		matches = append(matches, op)
		if first == nil && op != nil {
			first = op
		}
	}
	if first == nil {
		return nil, nil
	}
	for _, op := range matches {
		if op != first {
			return nil, &AccountOperationMismatch{Msg: "requested connectors do not share one open operation"}
		}
	}
	return first, nil
}

// Finds the open operation of the connectors or starts one driven on a background goroutine
func FindOrStartAccountOperation(ctx context.Context, env *Env, names []string, action string, profileHome string, alias, repairID *string, initiator string) (*AccountOperationStart, error) {
	accountStartMu.Lock()
	defer accountStartMu.Unlock()
	if op, err := matchingOpenAccountOperation(names, alias, repairID, profileHome); err != nil {
		return nil, err
	} else if op != nil {
		start := &AccountOperationStart{Operation: op, Started: false, done: make(chan struct{})}
		start.finish()
		return start, nil
	}
	targets := make([]*ConnTarget, 0, len(names))
	for _, name := range names {
		t := NewConnTarget(name, "connector", action)
		t.Alias, t.RepairID = alias, repairID
		targets = append(targets, t)
	}
	var b [16]byte
	rand.Read(b[:])
	op := NewConnectionOperation(targets, "account:"+hex.EncodeToString(b[:]), "", nil)
	start := &AccountOperationStart{Operation: op, Started: true, done: make(chan struct{})}
	if err := LiveOpen(ctx, op); err != nil {
		LiveClose(op)
		return nil, err
	}
	go runAccountOperation(context.WithoutCancel(ctx), env, start, action, initiator)
	return start, nil
}

// Drives the operation to its settlement, a raised drive closing it and marking the start failed
func runAccountOperation(ctx context.Context, env *Env, start *AccountOperationStart, action, initiator string) {
	logging.SetSessionContext(start.Operation.SessionKey)
	defer logging.ClearSessionContext()
	defer start.finish()
	defer func() {
		if r := recover(); r != nil {
			LiveClose(start.Operation)
			start.mu.Lock()
			start.failed = true
			start.mu.Unlock()
		}
	}()
	client, err := NewManagedClient(env, true, initiator, nil)
	if err != nil {
		panic(err)
	}
	tick := ManagedWatchTickSeconds
	DriveOperation(ctx, env, start.Operation, ManagedKind(ctx, env, client, action, false),
		func(*pyjson.Object) string { start.finish(); return "" }, &tick, false)
}

// Waits for the prepare step, false when it did not finish within the prepare budget
func WaitForAccountPrepare(start *AccountOperationStart) bool {
	select {
	case <-start.done:
		return true
	case <-time.After(time.Duration(accountPrepareWaitSeconds * float64(time.Second))):
		return false
	}
}
