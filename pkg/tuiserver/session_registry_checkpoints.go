package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Runs a body against the session agent's checkpoint manager and the session working directory
func withCheckpoints[T any](ctx context.Context, session *tuigateway.Session, fn func(mgr *tools.CheckpointManager, cwd string) T) T {
	a := sessionAgent(session)
	if a == nil {
		panic(fmt.Errorf("'NoneType' object has no attribute '_checkpoint_mgr'"))
	}
	return fn(a.CheckpointManager(), sessionCwd(ctx, session))
}

// Hash a rollback number names, a reference that is not a number passing through unchanged
func resolveCheckpointHash(mgr *tools.CheckpointManager, cwd string, ref any) (string, error) {
	checkpoints := mgr.ListCheckpoints(cwd)
	number, err := pyint.Of(ref)
	if err != nil {
		var pyErr *pyint.Error
		if errors.As(err, &pyErr) && pyErr.Kind == "ValueError" {
			return pyrepr.Str(ref), nil
		}
		return "", err
	}
	idx := new(big.Int).Sub(number, big.NewInt(1))
	if idx.Sign() >= 0 && idx.Cmp(big.NewInt(int64(len(checkpoints)))) < 0 {
		return checkpoints[idx.Int64()].Hash, nil
	}
	return "", fmt.Errorf("Invalid checkpoint number. Use 1-%d.", len(checkpoints))
}
