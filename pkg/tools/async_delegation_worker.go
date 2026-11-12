package tools

import (
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// WorkerDelegationStore is the worker's runtime session store whose durable outbox carries ledger writes to the owner
type WorkerDelegationStore interface {
	ScopePID() int
	ScopeSessionID() string
	Apply(op string, payload map[string]any) (map[string]any, error)
}

var (
	workerDelegationStoreMu sync.Mutex
	workerDelegationStore   WorkerDelegationStore
)

// Binds the worker process's runtime store as its delegation ledger, refusing a foreign process or a second store
func BindWorkerDelegationStore(store WorkerDelegationStore) error {
	if !isWorkerProcess() || store.ScopePID() != os.Getpid() {
		return NewWorkerPersistenceError("permission_denied")
	}
	workerDelegationStoreMu.Lock()
	defer workerDelegationStoreMu.Unlock()
	if workerDelegationStore != nil && workerDelegationStore != store {
		return NewWorkerPersistenceError("worker_delegation_already_bound")
	}
	workerDelegationStore = store
	return nil
}

// Ledger every delegation write resolves, the bound store ledger in production
var WorkerLedger = boundWorkerLedger

// Ledger routed through the bound store in a worker, nil in the owner, an error for an unbound worker
func boundWorkerLedger() (*WorkerDelegationLedger, error) {
	if !isWorkerProcess() {
		return nil, nil
	}
	workerDelegationStoreMu.Lock()
	defer workerDelegationStoreMu.Unlock()
	if workerDelegationStore == nil || workerDelegationStore.ScopePID() != os.Getpid() {
		return nil, NewWorkerPersistenceError("worker_delegation_ledger_unbound")
	}
	return &WorkerDelegationLedger{Store: workerDelegationStore}, nil
}

// Refuses an owner only ledger operation inside a worker process
func requireLedgerOwner() error {
	if isWorkerProcess() {
		return NewWorkerPersistenceError("worker_delegation_owner_only")
	}
	return nil
}

// WorkerDelegationLedger forwards each ledger operation to the owner through the worker's store
type WorkerDelegationLedger struct{ Store WorkerDelegationStore }

// Value field of one store operation's reply
func (l *WorkerDelegationLedger) apply(op string, payload map[string]any) (any, error) {
	reply, err := l.Store.Apply(op, payload)
	if err != nil {
		return nil, err
	}
	return reply["value"], nil
}

// Publishes a dispatch owned by the worker's own session, refusing foreign parents and routed producers
func (l *WorkerDelegationLedger) Dispatch(record map[string]any) (any, error) {
	if parent, _ := record["parent_session_id"].(string); record["parent_session_id"] == nil || parent != l.Store.ScopeSessionID() {
		return nil, NewWorkerPersistenceError("permission_denied")
	}
	for _, key := range asyncRoutingKeys {
		if pyrepr.Truthy(record[key]) {
			return nil, NewWorkerPersistenceError("unsupported_producer")
		}
	}
	payload := map[string]any{}
	for _, key := range []string{"delegation_id", "parent_session_id", "session_key", "origin_ui_session_id", "origin_session_id", "dispatched_at"} {
		if v, ok := record[key]; ok {
			payload[key] = v
		} else {
			payload[key] = ""
		}
	}
	task := map[string]any{}
	for _, key := range []string{"goal", "goals", "context", "toolsets", "role", "model", "is_batch", "task_indexes"} {
		if v, ok := record[key]; ok {
			task[key] = v
		}
	}
	payload["task"] = task
	return l.apply("delegation.dispatch", payload)
}

// Persists a terminal event and result for the owner
func (l *WorkerDelegationLedger) Complete(event, result map[string]any) (any, error) {
	return l.apply("delegation.complete", map[string]any{"event": event, "result": result})
}

// Records one finished child of a running unit
func (l *WorkerDelegationLedger) Child(delegationID string, entry map[string]any) (any, error) {
	return l.apply("delegation.child", map[string]any{"delegation_id": delegationID, "entry": entry})
}

// Reads a durable row the worker's session owns
func (l *WorkerDelegationLedger) Read(delegationID string) (any, error) {
	return l.apply("delegation.read", map[string]any{"delegation_id": delegationID})
}

// Claims a pending completion delivery
func (l *WorkerDelegationLedger) Claim(delegationID, claimID string) (any, error) {
	return l.apply("delegation.claim", map[string]any{"delegation_id": delegationID, "claim_id": claimID})
}

// Acknowledges a claimed delivery
func (l *WorkerDelegationLedger) Ack(delegationID, claimID string) (any, error) {
	return l.apply("delegation.ack", map[string]any{"delegation_id": delegationID, "claim_id": claimID})
}

// Returns an unadmitted claim without spending an attempt
func (l *WorkerDelegationLedger) Defer(delegationID, claimID string) (any, error) {
	return l.apply("delegation.defer", map[string]any{"delegation_id": delegationID, "claim_id": claimID})
}

// Releases a failed claim for retry
func (l *WorkerDelegationLedger) Release(delegationID, claimID string) (any, error) {
	return l.apply("delegation.release", map[string]any{"delegation_id": delegationID, "claim_id": claimID})
}

// Terminally drops a claimed delivery
func (l *WorkerDelegationLedger) Drop(delegationID, claimID string) (any, error) {
	return l.apply("delegation.drop", map[string]any{"delegation_id": delegationID, "claim_id": claimID})
}

// Removes a dispatch whose worker could not be scheduled
func (l *WorkerDelegationLedger) Unscheduled(delegationID string) (any, error) {
	return l.apply("delegation.unscheduled", map[string]any{"delegation_id": delegationID})
}
