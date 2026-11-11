package kanban

import (
	"context"
	"fmt"
	"math"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	DefaultLogRotateBytes = 2 * 1024 * 1024
	DefaultLogBackupCount = 1
)

// Python int of a config value when at or above minimum, the default for anything else
func positiveInt(value any, def, minimum int64) int64 {
	n, err := pyint.Of(value)
	if err != nil {
		return def
	}
	parsed := int64(math.MaxInt64)
	if n.IsInt64() {
		parsed = n.Int64()
	} else if n.Sign() < 0 {
		return def
	}
	if parsed >= minimum {
		return parsed
	}
	return def
}

// Rotation size and backup count for worker logs from the kanban config, defaults when unset
func WorkerLogRotationConfig(ctx context.Context, kanbanCfg any) (int64, int64, error) {
	if kanbanCfg == nil {
		kanbanCfg = map[string]any{}
		if cfg, err := loadKanbanConfigTree(ctx, false); err == nil && pyrepr.Truthy(cfg["kanban"]) {
			kanbanCfg = cfg["kanban"]
		}
	}
	if !pyrepr.Truthy(kanbanCfg) {
		kanbanCfg = map[string]any{}
	}
	cfg, ok := kanbanCfg.(map[string]any)
	if !ok {
		return 0, 0, &RuntimeError{fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(kanbanCfg))}
	}
	maxBytes := positiveInt(cfg["worker_log_rotate_bytes"], DefaultLogRotateBytes, 1)
	backupCount := positiveInt(cfg["worker_log_backup_count"], DefaultLogBackupCount, 0)
	return maxBytes, backupCount, nil
}

// Log path with the rotation generation appended to its suffix
func rotatedLogPath(logPath string, generation int64) string {
	return fmt.Sprintf("%s.%d", logPath, generation)
}

// Rotates a worker log past max bytes into numbered generations up to the backup count, ignoring OS errors
func RotateWorkerLog(logPath string, maxBytes, backupCount int64) {
	st, err := os.Stat(logPath)
	if err != nil || st.Size() <= maxBytes {
		return
	}
	backupCount = positiveInt(backupCount, DefaultLogBackupCount, 0)
	if backupCount == 0 {
		_ = os.Remove(logPath)
		return
	}
	oldest := rotatedLogPath(logPath, backupCount)
	if _, err := os.Stat(oldest); err == nil {
		_ = os.Remove(oldest)
	}
	for generation := backupCount - 1; generation > 0; generation-- {
		src := rotatedLogPath(logPath, generation)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		_ = os.Rename(src, rotatedLogPath(logPath, generation+1))
	}
	_ = os.Rename(logPath, rotatedLogPath(logPath, 1))
}
