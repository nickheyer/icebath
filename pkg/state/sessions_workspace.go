package state

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// Persists the cwd and claims a new git metadata generation, nil when the session is missing
func (d *SessionDB) UpdateSessionCwdClaim(ctx context.Context, sessionID, cwd, gitBranch, gitRepoRoot string, replaceGitMeta bool) (*int64, error) {
	if sessionID == "" || cwd == "" {
		return nil, nil
	}
	branch, repoRoot := strings.TrimSpace(gitBranch), strings.TrimSpace(gitRepoRoot)
	var generation *int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		generation = nil
		var current sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT cwd FROM sessions WHERE id = ?", sessionID).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		sets := []string{"cwd = ?", "git_metadata_generation = COALESCE(git_metadata_generation, 0) + 1"}
		params := []any{cwd}
		if !current.Valid || current.String != cwd || replaceGitMeta {
			sets = append(sets, "git_branch = ?", "git_repo_root = ?")
			params = append(params, nullStr(branch), nullStr(repoRoot))
		} else {
			for _, cv := range [][2]string{{"git_branch", branch}, {"git_repo_root", repoRoot}} {
				if cv[1] != "" {
					sets = append(sets, cv[0]+" = ?")
					params = append(params, cv[1])
				}
			}
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(params, sessionID)...); err != nil {
			return err
		}
		var gen int64
		if err := conn.QueryRowContext(ctx, "SELECT git_metadata_generation FROM sessions WHERE id = ?", sessionID).Scan(&gen); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		generation = &gen
		return nil
	}, nil)
	return generation, err
}

// Publishes async git enrichment only while the cwd claim and its generation are still current
func (d *SessionDB) PublishSessionGitMetadata(ctx context.Context, sessionID, cwd string, generation int64, gitBranch, gitRepoRoot string) (bool, error) {
	if sessionID == "" || cwd == "" || generation < 1 {
		return false, nil
	}
	var sets []string
	var params []any
	for _, cv := range [][2]string{{"git_branch", strings.TrimSpace(gitBranch)}, {"git_repo_root", strings.TrimSpace(gitRepoRoot)}} {
		if cv[1] != "" {
			sets = append(sets, cv[0]+" = ?")
			params = append(params, cv[1])
		}
	}
	if len(sets) == 0 {
		return false, nil
	}
	n, err := d.writeRowcount(ctx, "UPDATE sessions SET "+strings.Join(sets, ", ")+" WHERE id = ? AND cwd = ? AND git_metadata_generation = ?",
		append(params, sessionID, cwd, generation), nil)
	return n == 1, err
}

// Fills the git repo root of every listed cwd that has none, never clobbering a recorded root
func (d *SessionDB) BackfillRepoRoots(ctx context.Context, cwdToRoot map[string]string) error {
	cwds := make([]string, 0, len(cwdToRoot))
	for cwd, root := range cwdToRoot {
		if root != "" && cwd != "" {
			cwds = append(cwds, cwd)
		}
	}
	if len(cwds) == 0 {
		return nil
	}
	sort.Strings(cwds)
	pairs := make([]any, len(cwds))
	for i, cwd := range cwds {
		pairs[i] = []any{cwdToRoot[cwd], cwd}
	}
	return d.writeSQL(ctx, "UPDATE sessions SET git_repo_root = ? WHERE cwd = ? AND COALESCE(git_repo_root, '') = ''", pairs, true, nil)
}
