package state

import (
	"context"
	"strings"
)

// Stamps the given owner onto rows whose profile is null or blank, never overwriting an owner, returning rows stamped
func (d *SessionDB) BackfillNullSessionProfiles(ctx context.Context, profileName string) (int, error) {
	stamp := strings.TrimSpace(profileName)
	if stamp == "" {
		return 0, nil
	}
	n, err := d.writeRowcount(ctx, `UPDATE sessions
               SET profile_name = ?
             WHERE profile_name IS NULL OR TRIM(profile_name) = ''`, []any{stamp}, nil)
	return int(n), err
}
