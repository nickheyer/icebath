package gateway

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Serializes every load, mutate and save of the restart failure counters file
var restartFailureCountsLock sync.Mutex

func stuckLoopLog() *slog.Logger { return slog.Default().With("logger", "gateway.stuck_loop_counts") }

// Parsed counters, empty when the file is missing, unreadable or not a JSON object
func readRestartFailureCounts(path string) *pyjson.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pyjson.NewObject()
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return pyjson.NewObject()
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return pyjson.NewObject()
	}
	counts, ok := data.(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	return counts
}

// Counter plus one of every session active at shutdown, every other counter dropped
func IncrementRestartFailureCounts(ctx context.Context, path string, activeSessionKeys []string) {
	restartFailureCountsLock.Lock()
	defer restartFailureCountsLock.Unlock()
	counts := readRestartFailureCounts(path)
	next := pyjson.NewObject()
	for _, key := range activeSessionKeys {
		prev, _ := counts.Get(key)
		n, err := pyAddOne(prev)
		if err != nil {
			stuckLoopLog().Debug("Could not write restart-failure counters to "+path, "exc_info", err)
			return
		}
		next.Set(key, n)
	}
	if err := config.AtomicJSONWriteWith(ctx, path, next); err != nil {
		stuckLoopLog().Debug("Could not write restart-failure counters to "+path, "exc_info", err)
	}
}

// Python counts.get(key, 0) + 1 over a decoded JSON value
func pyAddOne(v any) (any, error) {
	switch n := v.(type) {
	case nil:
		return int64(1), nil
	case int64:
		return n + 1, nil
	case *big.Int:
		return new(big.Int).Add(n, big.NewInt(1)), nil
	case float64:
		return n + 1, nil
	case bool:
		if n {
			return int64(2), nil
		}
		return int64(1), nil
	}
	return nil, errors.New("TypeError: can only add numbers to the restart failure count")
}

// Drops a completed session's counter, removing the file once no counter is left
func ClearRestartFailureCount(ctx context.Context, path, sessionKey string) {
	restartFailureCountsLock.Lock()
	defer restartFailureCountsLock.Unlock()
	counts := readRestartFailureCounts(path)
	if _, ok := counts.Get(sessionKey); !ok {
		return
	}
	counts.Delete(sessionKey)
	var err error
	if len(counts.Keys) > 0 {
		err = config.AtomicJSONWriteWith(ctx, path, counts)
	} else if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		err = rmErr
	}
	if err != nil {
		stuckLoopLog().Debug("Could not clear restart-failure counter in "+path, "exc_info", err)
	}
}
