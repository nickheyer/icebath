package state

import (
	"context"
	"fmt"
	"io"
	"os"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Text of an error a store command reports to the user, SQLite failures with their engine message
func cmdExcText(err error) string {
	if _, ok := sqliteClassOf(err); ok {
		return errText(err)
	}
	if errorutil.IsSystem(err) {
		return err.Error()
	}
	return err.Error()
}

// Repairs a state.db that does not open cleanly or lags the declared schema, a backup made first
func CmdRepair(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	printf := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	dbPath := defaultDBPath(ctx)
	if _, err := os.Stat(dbPath); err != nil {
		printf("No session database at %s (nothing to repair).", dbPath)
		return 0, nil
	}
	reason, unhealthy, err := repairProbeFn(ctx, dbPath)
	if err != nil {
		return 1, err
	}
	driftOnly := !unhealthy
	if driftOnly {
		reason, unhealthy, err = SchemaDriftReason(ctx, dbPath)
		if err != nil {
			if _, sqliteErr := sqliteClassOf(err); !sqliteErr && !errorutil.IsSystem(err) {
				return 1, err
			}
			printf("✗ Could not check session schema: %s", cmdExcText(err))
			return 1, nil
		}
	}
	if !unhealthy {
		printf("✓ %s opens cleanly — no repair needed.", dbPath)
		return 0, nil
	}
	printf("✗ %s does not open cleanly: %s", dbPath, reason)
	if anyutil.IsTrue(ns["check_only"]) {
		return 1, nil
	}
	printf("%s", "Repairing (a backup copy is made first)…")
	backup := !anyutil.IsTrue(ns["no_backup"])
	var repaired bool
	var strategy, backupPath, failure string
	if driftOnly {
		report := RepairDeclarativeSchema(ctx, dbPath, backup)
		repaired, strategy, backupPath, failure = report.Repaired, report.Strategy, report.BackupPath, report.Error
	} else {
		report, err := openRepairFn(ctx, dbPath, backup)
		if err != nil {
			return 1, err
		}
		repaired, strategy, backupPath, failure = report.Repaired, report.Strategy, report.BackupPath, report.Error
	}
	if repaired {
		if backupPath != "" {
			printf("  backup: %s", backupPath)
		}
		if strategy != "" {
			printf("  strategy: %s", strategy)
		}
		if n, err := repairedSessionCount(ctx, dbPath, driftOnly); err == nil {
			printf("✓ Repaired — %d sessions recovered.", n)
		} else {
			printf("%s", "✓ Repaired.")
		}
		return 0, nil
	}
	if failure != "" {
		printf("✗ Repair failed: %s", failure)
	} else {
		printf("%s", "✗ Repair failed.")
	}
	if backupPath != "" {
		printf("  A backup is preserved at: %s", backupPath)
	}
	if driftOnly {
		return 1, nil
	}
	sourceHint := backupPath
	if sourceHint == "" {
		sourceHint = dbPath
	}
	printf("%s", "  Keep state.db and the backup; do not delete them.\n"+
		"\n  Next step — offline recovery (never modifies the source):\n"+
		"    icebath sessions recover --source "+sourceHint+" \\\n"+
		"        --inspect-only\n"+
		"  If that reports the data is recoverable, rebuild it into\n"+
		"  a NEW database (the active one is left untouched):\n"+
		"    icebath sessions recover --source "+sourceHint+" \\\n"+
		"        --output recovered-state.db")
	return 1, nil
}

// Session row count of a repaired store opened once more, read only after a drift repair
func repairedSessionCount(ctx context.Context, dbPath string, readOnly bool) (int64, error) {
	db, err := NewSessionDB(ctx, dbPath, readOnly)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	row, err := db.readOne(ctx, "SELECT COUNT(*) AS n FROM sessions")
	if err != nil {
		return 0, err
	}
	n, _ := rowInt(row, "n")
	return n, nil
}
