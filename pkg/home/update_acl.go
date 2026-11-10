package home

import (
	"context"
	"fmt"
	"log/slog"
)

// Grants the user back the home tree an elevated update left Administrators only, an error when nothing took
func RepairWindowsHomeACL(ctx context.Context, elevated *bool) error {
	isElevated := ProcessIsElevated()
	if elevated != nil {
		isElevated = *elevated
	}
	if !isElevated {
		return nil
	}
	h := Dir(ctx)
	granted, skipped, failed, err := GrantTreeUserAccess(h, true, true)
	if err != nil {
		return err
	}
	if len(failed) > 0 && granted == 0 {
		return fmt.Errorf("could not restore user access to %s: %s: %s", h, failed[0].Path, failed[0].Reason)
	}
	if granted == 0 {
		why := "nothing was walkable"
		if len(skipped) > 0 {
			why = skipped[0] + " is a junction, symlink or hard link"
		}
		return fmt.Errorf("could not restore user access to %s: nothing was repaired (%s)", h, why)
	}
	if len(failed) > 0 {
		entries := "ies"
		if len(failed) == 1 {
			entries = "y"
		}
		slog.Warn(fmt.Sprintf("Restoring user access under %s failed for %d entr%s, e.g. %s: %s",
			h, len(failed), entries, failed[0].Path, failed[0].Reason))
	}
	return nil
}
