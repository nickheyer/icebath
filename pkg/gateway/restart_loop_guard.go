package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	RestartLoopDefaultMaxRestarts   = 3
	RestartLoopDefaultWindowSeconds = 60
	RestartLoopDefaultMaxGapSeconds = 300
	restartLoopMaxStoredBoots       = 50
)

// Persisted restart interrupted boot log of the active home
func restartLoopStatePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "gateway", "restart_loop.json")
}

// Recorded boot times, empty when the log is missing, unreadable or malformed
func restartLoopLoadBoots(ctx context.Context) ([]float64, error) {
	raw, err := os.ReadFile(restartLoopStatePath(ctx))
	if err != nil {
		return []float64{}, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return []float64{}, nil
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return []float64{}, nil
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: %T object has no attribute 'get'", data)
	}
	boots := []float64{}
	value, found := obj.Get("boots")
	if !found {
		return boots, nil
	}
	items, ok := value.([]any)
	if !ok {
		return boots, nil
	}
	for _, t := range items {
		switch v := t.(type) {
		case bool:
			if v {
				boots = append(boots, 1)
			} else {
				boots = append(boots, 0)
			}
		case int64:
			boots = append(boots, float64(v))
		case *big.Int:
			f, _ := new(big.Float).SetInt(v).Float64()
			boots = append(boots, f)
		case float64:
			boots = append(boots, v)
		}
	}
	return boots, nil
}

// Writes the boot log, an OS failure swallowed
func restartLoopSaveBoots(ctx context.Context, boots []float64) {
	path := restartLoopStatePath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return
	}
	items := make([]any, len(boots))
	for i, b := range boots {
		items[i] = b
	}
	obj := pyjson.NewObject()
	obj.Set("boots", items)
	text, err := pyjson.Dumps(obj)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(text), 0o666)
}

// Inter boot gap that still links two boots, floored by the window
func restartLoopChainGap(windowSeconds, maxGapSeconds int) float64 {
	return float64(max(1, windowSeconds, maxGapSeconds))
}

// Unbroken chain of boots leading up to ts oldest first, a future entry counted as adjacent
func restartLoopChainEndingAt(boots []float64, ts, gap float64) []float64 {
	sorted := append([]float64(nil), boots...)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
	chain := []float64{}
	prev := ts
	for _, t := range sorted {
		if t > ts {
			chain = append(chain, t)
			continue
		}
		if prev-t > gap {
			break
		}
		chain = append(chain, t)
		prev = t
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// Records a restart interrupted boot and returns the linked chain ending with it
func RecordRestartInterruptedBoot(ctx context.Context, windowSeconds int, now *float64, maxGapSeconds int) ([]float64, error) {
	ts := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		ts = *now
	}
	loaded, err := restartLoopLoadBoots(ctx)
	if err != nil {
		return nil, err
	}
	boots := append(restartLoopChainEndingAt(loaded, ts, restartLoopChainGap(windowSeconds, maxGapSeconds)), ts)
	stored := boots
	if len(stored) > restartLoopMaxStoredBoots {
		stored = stored[len(stored)-restartLoopMaxStoredBoots:]
	}
	restartLoopSaveBoots(ctx, stored)
	return boots, nil
}

// Removes the persisted boot log
func ClearRestartLoopGuard(ctx context.Context) {
	_ = os.Remove(restartLoopStatePath(ctx))
}

// Records this boot and reports whether the chain reached the restart limit so auto resume is skipped
func RestartLoopCheckAndRecord(ctx context.Context, maxRestarts, windowSeconds int, now *float64, maxGapSeconds int) (bool, error) {
	boots, err := RecordRestartInterruptedBoot(ctx, windowSeconds, now, maxGapSeconds)
	if err != nil {
		return false, err
	}
	tripped := maxRestarts > 0 && len(boots) >= maxRestarts
	if tripped {
		slog.Default().With("logger", "gateway.run").Warn(fmt.Sprintf("Restart-loop breaker TRIPPED: %d chained restart-interrupted gateway boots (no gap wider than %ds; "+
			"threshold %d). Skipping auto-resume to break a suspected SIGTERM-respawn loop (#30719, #81642). "+
			"Restart-interrupted sessions stay resume-pending and will continue on the next real user message. "+
			"If this is a false positive, delete %s.",
			len(boots), int(restartLoopChainGap(windowSeconds, maxGapSeconds)), maxRestarts, restartLoopStatePath(ctx)))
	}
	return tripped, nil
}
