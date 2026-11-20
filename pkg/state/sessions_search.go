package state

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// WHERE for a workspace key, git_repo_root equal to it or an older row whose cwd lies at or under it
func workspaceKeyClause(key string) (string, []any) {
	prefix := strings.TrimRight(key, "/\\")
	if prefix == "" {
		prefix = key
	}
	cwdClause, cwdParams := cwdPrefixClause(prefix)
	return "(s.git_repo_root = ? OR (COALESCE(s.git_repo_root, '') = '' AND " + cwdClause + "))", append([]any{prefix}, cwdParams...)
}

// Arguments of search_sessions
type SearchSessionsOptions struct {
	Sources             []string
	Limit               int
	Offset              int
	WorkspaceKey        string
	LocalCreatedSources []string
	SkipLaunchStubs     bool
}

// Sessions most recently active first with a computed last_active, scoped by source and workspace
func (d *SessionDB) SearchSessions(ctx context.Context, o SearchSessionsOptions) ([]*pyjson.Object, error) {
	var where []string
	var params []any
	if o.SkipLaunchStubs {
		clause, err := d.launchStubFilterSQL()
		if err != nil {
			return nil, err
		}
		where = append(where, clause)
	}
	if len(o.Sources) > 0 {
		clause, bound := sourceMatchClause(o.Sources, o.LocalCreatedSources)
		where = append(where, clause)
		params = append(params, bound...)
	}
	if o.WorkspaceKey != "" {
		clause, bound := workspaceKeyClause(o.WorkspaceKey)
		where = append(where, clause)
		params = append(params, bound...)
	}
	params = append(params, int64(o.Limit), int64(o.Offset))
	rows, err := d.readAll(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		sqlSessionLastActive("s")+" AS last_active "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
		whereSQL(where, " ")+" "+
		"ORDER BY last_active DESC, s.started_at DESC, s.id DESC LIMIT ? OFFSET ?", params...)
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionRowDict(row))
	}
	return out, nil
}

// Filters of an id search, a nil Limit meaning 20 and a nil IncludeArchived meaning archived rows included
type SearchSessionsByIDOptions struct {
	Limit           *int
	IncludeArchived *bool
	Source          string
	Sources         []string
	ExcludeSources  []string
}

// Surfaced sessions ranked exact, prefix then substring by id, an old compression root id resolving to its live tip
func (d *SessionDB) SearchSessionsByID(ctx context.Context, query string, o SearchSessionsByIDOptions) ([]*pyjson.Object, error) {
	limit := 20
	if o.Limit != nil {
		limit = *o.Limit
	}
	needle := pystr.Lower(pystr.Strip(query))
	if needle == "" || limit <= 0 {
		return []*pyjson.Object{}, nil
	}
	fetch := max(limit*4, limit)
	candidates, err := d.ListSessionsRich(ctx, ListSessionsRichOptions{
		Source: o.Source, Sources: o.Sources, ExcludeSources: o.ExcludeSources, Limit: &fetch, Offset: 0,
		IncludeArchived: o.IncludeArchived == nil || *o.IncludeArchived, OrderByLastActive: true, IDQuery: needle})
	if err != nil {
		return nil, err
	}
	score := func(row *pyjson.Object) int {
		normalized := []string{}
		for _, key := range []string{"id", "_lineage_root_id"} {
			if v := rowValue(row, key); pyTruthy(v) {
				normalized = append(normalized, pystr.Lower(pyStr(v)))
			}
		}
		if slices.Contains(normalized, needle) {
			return 0
		}
		if slices.ContainsFunc(normalized, func(v string) bool { return strings.HasPrefix(v, needle) }) {
			return 1
		}
		return 2
	}
	ranked := slices.Clone(candidates)
	slices.SortStableFunc(ranked, func(a, b *pyjson.Object) int { return score(a) - score(b) })
	return ranked[:min(limit, len(ranked))], nil
}
