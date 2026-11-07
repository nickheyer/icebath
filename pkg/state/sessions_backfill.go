package state

import "context"

// Stamps the given owner onto rows whose profile is null or blank, never overwriting an owner, returning rows stamped
func (d *SessionDB) BackfillNullSessionProfiles(ctx context.Context, profileName string) (int, error) {
	stamp := pyStrip(profileName)
	if stamp == "" {
		return 0, nil
	}
	n, err := d.writeRowcount(ctx, `UPDATE sessions
               SET profile_name = ?
             WHERE profile_name IS NULL OR TRIM(profile_name) = ''`, []any{stamp}, nil)
	return int(n), err
}

// Promotes the model config cwd into the empty cwd column of acp rows, returning rows changed
func (d *SessionDB) BackfillACPSessionCwd(ctx context.Context) (int, error) {
	n, err := d.writeRowcount(ctx, `UPDATE sessions
                  SET cwd = json_extract(model_config, '$.cwd')
                WHERE source = 'acp'
                  AND COALESCE(cwd, '') = ''
                  AND json_valid(model_config)
                  AND COALESCE(json_extract(model_config, '$.cwd'), '') != ''`, []any{}, nil)
	return int(n), err
}
