package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Kanban card operations of the session host, bound by the extras tier that carries kanban and nil in a build without it
var (
	kanbanRunTask      func(ctx context.Context, c *AuthorityConnection, params *jsonutil.Object) (any, error)
	kanbanBindWorker   func(ctx context.Context, frame *jsonutil.Object) error
	kanbanStartFailure func(ctx context.Context, frame *jsonutil.Object, cause error) error
	kanbanWorkerLoss   func(ctx context.Context, policy *LocalSessionPolicy, reason string, pid any, bound bool)
	kanbanSpawnEnv     func(ctx context.Context, policy *LocalSessionPolicy) (map[string]string, error)
)

// Store refusal of a verb whose tier this binary leaves out, its wire message the line naming the build that has it
type tierAbsentRefusal struct {
	*state.RuntimeStoreError
	line string
}

func (r *tierAbsentRefusal) WireCode() int       { return 4001 }
func (r *tierAbsentRefusal) WireMessage() string { return r.line }
func (r *tierAbsentRefusal) Unwrap() error       { return r.RuntimeStoreError }

// Refusal of a verb of an unlinked tier, reason tier_not_linked with the build line as detail and message
func tierRefusal(display, tier string) error {
	line := linktier.Missing(display, tier)
	return &tierAbsentRefusal{RuntimeStoreError: state.NewRuntimeStoreError("tier_not_linked", nil, &line), line: line}
}

// Kanban run verb, refused by a build without kanban
func (c *AuthorityConnection) kanbanRun(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
	if kanbanRunTask == nil {
		return nil, tierRefusal("kanban", linktier.Extras)
	}
	return kanbanRunTask(ctx, c, params)
}

// Binds the adopted worker to its Kanban claim before tool discovery, a card attempt refused by a build without kanban
func BindWorkerContext(ctx context.Context, frame *jsonutil.Object) error {
	if kanbanBindWorker != nil {
		return kanbanBindWorker(ctx, frame)
	}
	kctx, err := WorkerKanbanContext(frame)
	if err != nil || kctx == nil {
		return err
	}
	return errors.New(linktier.Missing("a kanban attempt", linktier.Extras))
}

// Books the one shot credential exit of a worker that died before any turn ran, nothing without a card or kanban
func RecordStartFailure(ctx context.Context, frame *jsonutil.Object, cause error) error {
	if kanbanStartFailure == nil {
		return nil
	}
	return kanbanStartFailure(ctx, frame, cause)
}
