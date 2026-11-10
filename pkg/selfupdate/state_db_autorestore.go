package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/backup"
)

// Restores state.db from a snapshot through the backup reservation and epoch floor, false when refused or invalid
func restoreStateDBFromSnapshot(ctx context.Context, w io.Writer, statePath, snapState string) (bool, error) {
	if _, err := os.Stat(snapState); err != nil {
		return false, err
	}
	if !backup.VerifySQLiteIntegrity(snapState, backup.IntegrityOptions{}).Valid {
		return false, nil
	}
	ok, err := backup.SafeRestoreDB(ctx, snapState, statePath)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(w, "  ✗ Auto-restore refused or failed; state.db was not restored.")
		fmt.Fprintln(w, "    Stop the owning runtime and close database handles before retrying.")
		fmt.Fprintln(w, "    If the canonical runtime epoch is unreadable, do not restore in place:")
		fmt.Fprintln(w, "    use `icebath sessions recover --source <snapshot> --output <separate-path>`")
		fmt.Fprintln(w, "    to salvage transcripts into a separate output.")
		return false, nil
	}
	return backup.VerifySQLiteIntegrity(statePath, backup.IntegrityOptions{}).Valid, nil
}
