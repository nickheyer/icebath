package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Owner refusal reasons of a child reservation as the delegate_task tool error reads them
var childRefusals = map[string]string{
	"delegation_capacity": "the gateway owner refused another subagent: this execution already runs " +
		"delegation.max_concurrent_children of them",
	"delegation_depth_exceeded": "the gateway owner refused a subagent this deep (delegation.max_spawn_depth)",
}

// Delegated child's store inside a worker, owner reserved and persisting under its own claim
type ChildSessionStore struct {
	*RuntimeSessionStore
}

// Session id the owner reserved for this child
func (c *ChildSessionStore) ReservedSessionID() string { return c.scopeSessionID() }

// Adds only the child's constructor facts to the row the owner created at reservation
func (c *ChildSessionStore) CreateSession(n state.NewSession) error {
	config := map[string]any{}
	for k, v := range n.ModelConfig {
		if state.IsWorkerConstructorConfigKey(k) {
			config[k] = v
		}
	}
	child := state.NewSession{ID: n.ID, Source: "subagent", Model: n.Model, SystemPrompt: n.SystemPrompt}
	if len(config) > 0 {
		child.ModelConfig = config
	}
	return c.RuntimeSessionStore.CreateSession(child)
}

// Settles this execution after the child's last write, then releases the outbox
func (c *ChildSessionStore) Close() error {
	var finishErr error
	if !c.OutboxClosed() {
		if failure := c.Failure(); failure == nil || *failure == "" {
			_, finishErr = c.Finish(context.Background())
		}
	}
	closeErr := c.RuntimeSessionStore.Close()
	if finishErr != nil {
		return finishErr
	}
	return closeErr
}

// secrets.token_urlsafe of n random bytes
func tokenURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Child store the owner reserved under the parent's verified execution, a refusal surfacing as ValueError
func ReserveChildStore(ctx context.Context, parent *RuntimeSessionStore) (*ChildSessionStore, error) {
	secret, err := tokenURLSafe(32)
	if err != nil {
		return nil, err
	}
	var result any
	if err := CallOffEventLoop(ctx, func(ctx context.Context) error {
		var err error
		result, err = parent.apply(ctx, "child.reserve", pyjson.ObjectOf("secret", secret))
		return err
	}); err != nil {
		return nil, err
	}
	obj, ok := result.(*pyjson.Object)
	if !ok {
		return nil, &workerStoreError{"TypeError", "argument of type '" + pyrepr.TypeName(result) + "' is not iterable"}
	}
	if refused, has := obj.Get("refused"); has {
		reason := pyrepr.Str(refused)
		message, known := childRefusals[reason]
		if !known {
			message = "the gateway owner refused a subagent: " + reason
		}
		return nil, &workerStoreError{"ValueError", message}
	}
	childRaw, err := receiptItem(result, "child")
	if err != nil {
		return nil, err
	}
	child, ok := childRaw.(*pyjson.Object)
	if !ok {
		return nil, &workerStoreError{"TypeError", "'" + pyrepr.TypeName(childRaw) + "' object is not a mapping"}
	}
	scope := parent.Scope()
	for _, k := range child.Keys {
		scope.Set(k, child.Vals[k])
	}
	scope.Set("secret", secret)
	executionID, err := receiptText(child.GetOr("execution_id", nil))
	if err != nil {
		return nil, err
	}
	tail := executionID
	if i := strings.LastIndex(executionID, ":"); i >= 0 {
		tail = executionID[i+1:]
	}
	outbox := filepath.Join(parent.OutboxDir(), "child-"+tail)
	offLoop := func(ctx context.Context, method string, params *pyjson.Object) (any, error) {
		var out any
		err := CallOffEventLoop(ctx, func(ctx context.Context) error {
			var err error
			out, err = parent.rpc(ctx, method, params)
			return err
		})
		return out, err
	}
	store, err := openRuntimeSessionStore(ctx, offLoop, scope, outbox, nil, 0, "ChildSessionStore", false)
	if err != nil {
		return nil, err
	}
	return &ChildSessionStore{store}, nil
}
