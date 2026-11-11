package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One owner call a worker store makes, a WorkerRPC Call or an in process stand in with the same contract
type WorkerCall func(ctx context.Context, method string, params *pyjson.Object) (any, error)

// Worker persistence facade with synchronous durable results and a private byte bounded retry journal
type RuntimeSessionStore struct {
	rpc                   WorkerCall
	scope                 *pyjson.Object
	maxBytes              int
	mu                    sync.Mutex
	failure               *string
	path                  string
	outboxOwner           *os.File
	ownerClosed           bool
	journal               *pyjson.Object
	kind                  string
	bindsDelegationLedger bool
}

// Default journal byte bound of a worker outbox
const runtimeStoreMaxBytes = 4 * 1024 * 1024

// Opens the worker's outbox under its exclusive lock, replaying or reconciling a retained journal onto the adopted scope
func NewRuntimeSessionStore(ctx context.Context, rpc WorkerCall, scope *pyjson.Object, outboxDir string, adopted any, maxBytes int) (*RuntimeSessionStore, error) {
	return openRuntimeSessionStore(ctx, rpc, scope, outboxDir, adopted, maxBytes, "RuntimeSessionStore", true)
}

func openRuntimeSessionStore(ctx context.Context, rpc WorkerCall, scope *pyjson.Object, outboxDir string, adopted any, maxBytes int, kind string, bindsLedger bool) (*RuntimeSessionStore, error) {
	if maxBytes <= 0 {
		maxBytes = runtimeStoreMaxBytes
	}
	s := &RuntimeSessionStore{rpc: rpc, scope: scope.Copy(), maxBytes: maxBytes, path: filepath.Join(outboxDir, "pending.json"),
		kind: kind, bindsDelegationLedger: bindsLedger}
	parent := filepath.Dir(s.path)
	if _, err := home.MakeUserDir(ctx, parent, 0o700, true, true); err != nil {
		return nil, err
	}
	if isSymlink(parent) || isSymlink(s.path) {
		return nil, tools.NewWorkerPersistenceError("unsafe_outbox")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(parent, "owner.lock")
	if isSymlink(lockPath) {
		return nil, tools.NewWorkerPersistenceError("unsafe_outbox")
	}
	owner, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	s.outboxOwner = owner
	if err := os.Chmod(lockPath, 0o600); err != nil {
		owner.Close()
		return nil, err
	}
	if !gateway.TryAcquireFileLock(owner) {
		owner.Close()
		return nil, tools.NewWorkerPersistenceError("outbox_in_use")
	}
	if err := s.load(ctx, scope, adopted); err != nil {
		s.closeOwner()
		return nil, err
	}
	return s, nil
}

// Reads or starts the journal, replays what is pending and binds the delegation ledger of a worker
func (s *RuntimeSessionStore) load(ctx context.Context, requested *pyjson.Object, adopted any) error {
	reconciled := false
	if info, err := os.Stat(s.path); err == nil {
		if info.Size() > int64(s.maxBytes) {
			return tools.NewWorkerPersistenceError("outbox_full")
		}
		raw, err := os.ReadFile(s.path)
		if err != nil {
			return err
		}
		decoded, err := pyjson.LoadsBytes(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
		if err != nil {
			return err
		}
		journal, ok := decoded.(*pyjson.Object)
		if !ok {
			return &workerStoreError{"TypeError", fmt.Sprintf("%s indices must be integers or slices, not str", pyrepr.TypeName(decoded))}
		}
		s.journal = journal
		oldScope, err := journalItem(journal, "scope")
		if err != nil {
			return err
		}
		reconciled = !pyjson.Equal(oldScope, s.scope)
		if reconciled {
			if err := s.reconcileAdopted(adopted); err != nil {
				return err
			}
		}
	} else {
		s.journal = pyjson.ObjectOf("scope", s.scope, "next_sequence", int64(1), "pending", []any{})
		if err := s.save(ctx, s.journal); err != nil {
			return err
		}
	}
	pending, err := journalPending(s.journal)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		if _, err := s.retryPendingLocked(ctx); err != nil {
			return err
		}
	}
	if reconciled && !pyjson.Equal(s.scope, requested) {
		return tools.NewWorkerPersistenceError("outbox_scope_mismatch")
	}
	if IsWorkerProcess() && s.bindsDelegationLedger {
		if err := tools.BindWorkerDelegationStore(s); err != nil {
			return err
		}
	}
	return nil
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// Item of the journal dict, KeyError when absent
func journalItem(journal *pyjson.Object, key string) (any, error) {
	v, ok := journal.Get(key)
	if !ok {
		return nil, &workerStoreError{"KeyError", pyrepr.Repr(key)}
	}
	return v, nil
}

// Pending entries of the journal
func journalPending(journal *pyjson.Object) ([]any, error) {
	raw, err := journalItem(journal, "pending")
	if err != nil {
		return nil, err
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("'%s' object is not a list", pyrepr.TypeName(raw))}
	}
	return list, nil
}

// Carries a retained outbox across verified owner replacement onto the adopted epoch, refusing any other drift
func (s *RuntimeSessionStore) reconcileAdopted(adopted any) error {
	oldRaw, _ := s.journal.Get("scope")
	old, okOld := oldRaw.(*pyjson.Object)
	next := s.scope
	adoptedObj, okAdopted := adopted.(*pyjson.Object)
	mismatch := !okAdopted || !okOld || !sameKeys(old, next)
	if !mismatch {
		for _, key := range next.Keys {
			if key != "epoch" && key != "session_id" && !pyjson.Equal(old.GetOr(key, nil), next.GetOr(key, nil)) {
				mismatch = true
			}
		}
	}
	if !mismatch {
		for _, pair := range [][2]string{{"execution_id", "execution_id"}, {"generation", "generation"}, {"session_id", "session_id"}, {"owner_epoch", "epoch"}} {
			if !pyjson.Equal(adoptedObj.GetOr(pair[0], nil), next.GetOr(pair[1], nil)) {
				mismatch = true
			}
		}
		if !pyjson.Equal(adoptedObj.GetOr("status", nil), "running") {
			mismatch = true
		}
	}
	if !mismatch {
		oldEpoch, oldInt := old.GetOr("epoch", nil).(int64)
		newEpoch, newInt := next.GetOr("epoch", nil).(int64)
		if !oldInt || !newInt || oldEpoch > newEpoch {
			mismatch = true
		} else if !pyjson.Equal(old.GetOr("session_id", nil), next.GetOr("session_id", nil)) {
			pending, err := journalPending(s.journal)
			if err != nil {
				return err
			}
			publishing := false
			for _, entry := range pending {
				op, err := entryItem(entry, "operation")
				if err != nil {
					return err
				}
				if pyjson.Equal(op, "compression.publish") {
					publishing = true
					break
				}
			}
			mismatch = !publishing
		}
	}
	if mismatch {
		return tools.NewWorkerPersistenceError("outbox_scope_mismatch")
	}
	scope := old.Copy()
	scope.Set("epoch", next.GetOr("epoch", nil))
	candidate := s.journal.Copy()
	candidate.Set("scope", scope)
	if err := s.save(context.Background(), candidate); err != nil {
		return err
	}
	s.scope = scope
	s.journal = candidate
	return nil
}

func sameKeys(a, b *pyjson.Object) bool {
	if a.Len() != b.Len() {
		return false
	}
	for _, k := range a.Keys {
		if _, ok := b.Get(k); !ok {
			return false
		}
	}
	return true
}

// Field of a pending entry, the subscript errors of a non dict entry included
func entryItem(entry any, key string) (any, error) {
	obj, ok := entry.(*pyjson.Object)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("%s indices must be integers or slices, not str", pyrepr.TypeName(entry))}
	}
	return journalItem(obj, key)
}

// Writes the journal atomically as an owner only file, outbox_full and sticky once it outgrows the bound
func (s *RuntimeSessionStore) save(ctx context.Context, journal *pyjson.Object) error {
	if s.ownerClosed {
		return tools.NewWorkerPersistenceError("outbox_closed")
	}
	encoded, err := pyjson.Dumps(journal, pyjson.EnsureASCII(true), pyjson.AllowNaN(false), pyjson.Separators(",", ":"))
	if err != nil {
		return err
	}
	if len(encoded) > s.maxBytes {
		failure := "outbox_full"
		s.failure = &failure
		return tools.NewWorkerPersistenceError(failure)
	}
	mode := os.FileMode(0o600)
	return pyfs.AtomicWriteBytes(ctx, s.path, []byte(encoded), ".pending-", &mode, true)
}

// Refuses any session but the assigned one
func (s *RuntimeSessionStore) session(sessionID string) error {
	if !pyjson.Equal(sessionID, s.scope.GetOr("session_id", nil)) {
		return tools.NewWorkerPersistenceError("permission_denied")
	}
	return nil
}

// Assigned session id of the scope
func (s *RuntimeSessionStore) scopeSessionID() string {
	sid, _ := s.scope.GetOr("session_id", nil).(string)
	return sid
}

// Journals one operation, then replays the journal and answers this operation's durable receipt
func (s *RuntimeSessionStore) apply(ctx context.Context, operation string, payload *pyjson.Object) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil && *s.failure != "" {
		return nil, tools.NewWorkerPersistenceError(*s.failure)
	}
	pending, err := journalPending(s.journal)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, tools.NewWorkerPersistenceError("pending_receipt")
	}
	next, err := journalItem(s.journal, "next_sequence")
	if err != nil {
		return nil, err
	}
	journalText, err := pyjson.Dumps(s.journal)
	if err != nil {
		return nil, err
	}
	copied, err := pyjson.LoadsDict(journalText)
	if err != nil {
		return nil, err
	}
	entryText, err := pyjson.Dumps(pyjson.ObjectOf("sequence", next, "operation", operation, "payload", payload), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	entry, err := pyjson.Loads(entryText)
	if err != nil {
		return nil, err
	}
	copiedPending, err := journalPending(copied)
	if err != nil {
		return nil, err
	}
	copied.Set("pending", append(copiedPending, entry))
	sequence, ok := copied.GetOr("next_sequence", nil).(int64)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("unsupported operand type(s) for +=: '%s' and 'int'", pyrepr.TypeName(copied.GetOr("next_sequence", nil)))}
	}
	copied.Set("next_sequence", sequence+1)
	if err := s.save(ctx, copied); err != nil {
		failure := err.Error()
		s.failure = &failure
		return nil, err
	}
	s.journal = copied
	results, err := s.retryPendingLocked(ctx)
	if err != nil {
		return nil, err
	}
	return results[0], nil
}

// Replays every pending entry in order, each receipt advancing the journal, a failure sticky until the next success
func (s *RuntimeSessionStore) RetryPending(ctx context.Context) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retryPendingLocked(ctx)
}

func (s *RuntimeSessionStore) retryPendingLocked(ctx context.Context) ([]any, error) {
	var results []any
	fail := func(err error) ([]any, error) {
		failure := err.Error()
		s.failure = &failure
		return nil, err
	}
	for {
		pending, err := journalPending(s.journal)
		if err != nil {
			return fail(err)
		}
		if len(pending) == 0 {
			break
		}
		entry, ok := pending[0].(*pyjson.Object)
		if !ok {
			return fail(&workerStoreError{"TypeError", fmt.Sprintf("argument after ** must be a mapping, not %s", pyrepr.TypeName(pending[0]))})
		}
		params := s.scope.Copy()
		for _, k := range entry.Keys {
			if _, dup := params.Get(k); dup {
				return fail(&workerStoreError{"TypeError", fmt.Sprintf("keyword argument repeated: %s", k)})
			}
			params.Set(k, entry.Vals[k])
		}
		result, err := s.rpc(ctx, "worker.persist", params)
		if err != nil {
			return fail(err)
		}
		candidate := s.journal.Copy()
		candidate.Set("pending", append([]any(nil), pending[1:]...))
		candidate, err = s.compressionReceiptJournal(candidate, result)
		if err != nil {
			return fail(err)
		}
		if err := s.save(ctx, candidate); err != nil {
			return fail(err)
		}
		s.scope = candidate.GetOr("scope", nil).(*pyjson.Object)
		s.journal = candidate
		results = append(results, result)
	}
	s.failure = nil
	return results, nil
}

// Moves the journal scope onto the physical child a publication receipt assigned, refusing a foreign parent
func (s *RuntimeSessionStore) compressionReceiptJournal(candidate *pyjson.Object, result any) (*pyjson.Object, error) {
	obj, ok := result.(*pyjson.Object)
	if !ok {
		return nil, &workerStoreError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(result))}
	}
	assignment := obj.GetOr("worker_assignment", nil)
	if !pyrepr.Truthy(assignment) {
		return candidate, nil
	}
	parent, err := receiptItem(assignment, "parent")
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(parent, s.scope.GetOr("session_id", nil)) {
		return nil, tools.NewWorkerPersistenceError("outbox_scope_mismatch")
	}
	child, err := receiptItem(assignment, "session_id")
	if err != nil {
		return nil, err
	}
	scope := s.scope.Copy()
	scope.Set("session_id", child)
	out := candidate.Copy()
	out.Set("scope", scope)
	return out, nil
}

// Installs a separately verified worker.adopt epoch, never adopting itself
func (s *RuntimeSessionStore) Adopt(ctx context.Context, epoch int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := s.scope.Copy()
	scope.Set("epoch", epoch)
	candidate := s.journal.Copy()
	candidate.Set("scope", scope)
	if err := s.save(ctx, candidate); err != nil {
		return err
	}
	s.scope = scope
	s.journal = candidate
	s.failure = nil
	return nil
}

// Item of a receipt dict, KeyError or TypeError as subscripting raises
func receiptItem(result any, key string) (any, error) {
	obj, ok := result.(*pyjson.Object)
	if !ok {
		if result == nil {
			return nil, &workerStoreError{"TypeError", "'NoneType' object is not subscriptable"}
		}
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("%s indices must be integers or slices, not str", pyrepr.TypeName(result))}
	}
	return journalItem(obj, key)
}

// Value field of this operation's receipt
func (s *RuntimeSessionStore) applyValue(ctx context.Context, operation string, payload *pyjson.Object) (any, error) {
	result, err := s.apply(ctx, operation, payload)
	if err != nil {
		return nil, err
	}
	return receiptItem(result, "value")
}

// Settles the execution as terminal
func (s *RuntimeSessionStore) Finish(ctx context.Context) (any, error) {
	return s.apply(ctx, "execution.finish", pyjson.NewObject())
}

// Refuses a failed or pending journal, every usage call already owning its receipt
func (s *RuntimeSessionStore) FlushTokenCountsChecked(timeoutS float64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *RuntimeSessionStore) flushLocked() (bool, error) {
	if s.failure != nil && *s.failure != "" {
		return false, tools.NewWorkerPersistenceError(*s.failure)
	}
	pending, err := journalPending(s.journal)
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		return false, tools.NewWorkerPersistenceError("pending_receipt")
	}
	return true, nil
}

// Flushes then releases the outbox lock even when the flush refuses
func (s *RuntimeSessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.flushLocked()
	s.closeOwner()
	return err
}

func (s *RuntimeSessionStore) closeOwner() {
	if !s.ownerClosed {
		s.ownerClosed = true
		s.outboxOwner.Close()
	}
}

// Scope pid the delegation ledger binds to
func (s *RuntimeSessionStore) ScopePID() int {
	pid, _ := s.scope.GetOr("pid", nil).(int64)
	return int(pid)
}

// Assigned session id the delegation ledger writes under
func (s *RuntimeSessionStore) ScopeSessionID() string { return s.scopeSessionID() }

// One ledger operation through the outbox, its receipt as a plain dict
func (s *RuntimeSessionStore) Apply(op string, payload map[string]any) (map[string]any, error) {
	obj, _ := pyjson.AsObject(payload)
	result, err := s.apply(context.Background(), op, obj)
	if err != nil {
		return nil, err
	}
	plain, ok := pyjson.Plain(result).(map[string]any)
	if !ok {
		return nil, &workerStoreError{"TypeError", fmt.Sprintf("receipt is a %s, not a dict", pyrepr.TypeName(result))}
	}
	return plain, nil
}

// Scope the store writes under, a copy
func (s *RuntimeSessionStore) Scope() *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope.Copy()
}

// Directory of the private outbox journal
func (s *RuntimeSessionStore) OutboxDir() string { return filepath.Dir(s.path) }

// Sticky failure of the journal, nil once a replay succeeded
func (s *RuntimeSessionStore) Failure() *string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// Whether the outbox lock was released
func (s *RuntimeSessionStore) OutboxClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownerClosed
}
