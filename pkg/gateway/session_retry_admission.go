package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/state"
)

type retryRewriteKey struct{}

// Refusal of a planned retry rewrite that stopped applying, the re-send refused with it and nothing written
type RetryRewriteRefused struct {
	Msg   string
	Cause error
}

func (e *RetryRewriteRefused) Error() string { return e.Msg }

func (e *RetryRewriteRefused) Unwrap() error { return e.Cause }

// One retry transcript rewrite applied at most once inside the admission write of its re-send
type RetryRewrite struct {
	DB        *state.SessionDB
	SessionID string
	Source    *SessionSource
	Write     func(ctx context.Context, conn state.SQLiteConn) error
	OnCommit  func()
	Applied   bool
	taken     bool
}

// Runs the rewrite inside the admission transaction, any failure but a gate refusal or writer contention refusing it
func (r *RetryRewrite) guard(ctx context.Context, conn state.SQLiteConn) error {
	r.Applied = false
	if err := r.Write(ctx, conn); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) || errors.Is(err, context.Canceled) {
			return err
		}
		if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError {
			return err
		}
		return &RetryRewriteRefused{Msg: err.Error(), Cause: err}
	}
	r.Applied = true
	return nil
}

// Context binding a planned rewrite to the re-send admitted under it
func bindRetryRewrite(ctx context.Context, rewrite *RetryRewrite) context.Context {
	return context.WithValue(ctx, retryRewriteKey{}, rewrite)
}

// Bound rewrite of this native admission, once, and only for the same source object on the same store
func takeRetryRewrite(ctx context.Context, event *MessageEvent, db *state.SessionDB) *RetryRewrite {
	rewrite, _ := ctx.Value(retryRewriteKey{}).(*RetryRewrite)
	var source *SessionSource
	if event != nil {
		source = event.Source
	}
	if rewrite == nil || rewrite.taken || source != rewrite.Source || rewrite.DB != db {
		return nil
	}
	rewrite.taken = true
	return rewrite
}

// Write guard running the admission gate first and then the rewrite in one transaction
func composeRetryRewrite(rewrite *RetryRewrite, authorize func(ctx context.Context, conn state.SQLiteConn) error) func(ctx context.Context, conn state.SQLiteConn) error {
	if rewrite == nil {
		return authorize
	}
	return func(ctx context.Context, conn state.SQLiteConn) error {
		if err := authorize(ctx, conn); err != nil {
			return err
		}
		return rewrite.guard(ctx, conn)
	}
}
