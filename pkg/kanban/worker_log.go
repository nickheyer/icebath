package kanban

import (
	"context"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Rotation size and backup count of worker logs from kanban.worker_log_rotate_bytes and kanban.worker_log_backup_count
func workerLogRotation(ctx context.Context) (maxBytes, backupCount int64) {
	return int64(settings.KanbanWorkerLogRotateBytes.Get(ctx)), int64(settings.KanbanWorkerLogBackupCount.Get(ctx))
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
	if backupCount <= 0 {
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
