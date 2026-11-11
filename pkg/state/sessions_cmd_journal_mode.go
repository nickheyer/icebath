package state

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// Journal mode the SQLite header bytes of a file report, read with no connection open
func headerMode(dbPath string) (string, error) {
	fh, err := os.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	head := make([]byte, 20)
	n, err := io.ReadFull(fh, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	if n < 20 || string(head[:16]) != "SQLite format 3\x00" {
		return "not-a-database", nil
	}
	switch {
	case head[18] == 1 && head[19] == 1:
		return "delete", nil
	case head[18] == 2 && head[19] == 2:
		return "wal", nil
	}
	return fmt.Sprintf("unknown(%d/%d)", head[18], head[19]), nil
}

// Admission refusal that does not depend on holder discovery, empty when the switch may proceed
func journalModeRefusal(target, current string, onCrossVMFS bool) string {
	if current == "not-a-database" || strings.HasPrefix(current, "unknown(") {
		return "its file header reads " + current + ", so it is not an Icebath SQLite store this command can convert " +
			"(pass --db PATH to point at one)."
	}
	if target == "wal" && onCrossVMFS {
		return "WAL shared memory silently corrupts on cross-VM filesystems (virtiofs/9p — Docker Desktop, " +
			"OrbStack, Lima bind mounts). Keep this store on journal_mode=delete or move it to a native " +
			"filesystem."
	}
	return ""
}

// Switches the on disk journal mode of a store nobody else holds and verifies the file header
func CmdSetJournalMode(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	printf := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	target, _ := ns["mode"].(string)
	dbPath, _ := ns["db"].(string)
	if dbPath == "" {
		dbPath = defaultDBPath(ctx)
	}
	dbPath = pathlibStr(dbPath)
	if _, err := os.Stat(dbPath); err != nil {
		printf("No database at %s (nothing to convert).", dbPath)
		return 1, nil
	}
	current, err := headerMode(dbPath)
	if err != nil {
		printf("✗ Cannot read %s: %s", dbPath, cmdExcText(err))
		return 1, nil
	}
	if current == target {
		printf("✓ %s is already journal_mode=%s.", dbPath, target)
		return 0, nil
	}
	onCrossVMFS := false
	if target == "wal" {
		if onCrossVMFS, err = pathOnCrossVMFS(dbPath); err != nil {
			return 1, err
		}
	}
	if refusal := journalModeRefusal(target, current, onCrossVMFS); refusal != "" {
		printf("✗ Refusing to change the journal mode of %s: %s", dbPath, refusal)
		return 1, nil
	}
	if target == "wal" && WALResetVulnerable(nil) {
		printf("✗ Refusing to enable WAL: the linked SQLite %s has the WAL-reset bug "+
			"(https://sqlite.org/wal.html#walresetbug). Upgrade to 3.51.3+ first.", LinkedSQLiteVersion())
		return 1, nil
	}
	holders, err := foreignStateDBHoldersOf(dbPath)
	if err != nil {
		return 1, err
	}
	if pyTruthy(ns["force"]) {
		found := holders[:0:0]
		for _, holder := range holders {
			if holder.PID > 0 {
				found = append(found, holder)
			}
		}
		holders = found
	}
	if len(holders) > 0 {
		printf("✗ Refusing to change the journal mode of %s: other processes hold it open "+
			"(a live switch would destroy their uncheckpointed commits). Stop them and re-run:", dbPath)
		for _, holder := range holders {
			label := "scan"
			if holder.PID > 0 {
				label = DescribeHolderPID(holder.PID)
			}
			printf("    pid %d — %s: %s", holder.PID, label, holder.Target)
		}
		return 1, nil
	}
	conn, err := ConnectSQLite(dbPath, SQLiteConfig{Timeout: 0.0})
	if err != nil {
		if _, sqliteErr := sqliteClassOf(err); !sqliteErr && !isOSError(err) {
			return 1, err
		}
		printf("✗ Cannot open %s: %s", dbPath, cmdExcText(err))
		return 1, nil
	}
	reported, err := setJournalModeNoWait(ctx, conn, strings.ToUpper(target))
	closeErr := conn.Close()
	if err != nil {
		if !isOperationalError(err) {
			return 1, err
		}
		printf("✗ journal_mode=%s refused: %s (another opener appeared; nothing was changed)", strings.ToUpper(target), cmdExcText(err))
		return 1, nil
	}
	if closeErr != nil {
		return 1, closeErr
	}
	after, err := headerMode(dbPath)
	if err != nil {
		printf("✗ Cannot re-read %s after the switch: %s", dbPath, cmdExcText(err))
		return 1, nil
	}
	if after != target {
		if reported == "" {
			reported = "?"
		}
		printf("✗ SQLite reported journal_mode=%s but the file header reads %s; not converted.", reported, after)
		return 1, nil
	}
	printf("✓ %s: journal_mode %s → %s (header verified).", dbPath, current, after)
	if configured := resolveJournalMode(ctx); configured != target {
		printf("  Note: config.yaml has database.journal_mode: %s — the next open would switch the file "+
			"back. Set `database.journal_mode: %s` in config.yaml to keep it.", configured, target)
	}
	return 0, nil
}
