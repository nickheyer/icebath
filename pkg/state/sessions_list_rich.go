package state

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Listing filters of a rich session page, a nil Limit meaning 20 and a nil ProjectCompressionTips meaning true
type ListSessionsRichOptions struct {
	Source                 string
	Sources                []string
	ExcludeSources         []string
	CwdPrefix              string
	Limit                  *int
	Offset                 int
	IncludeChildren        bool
	MinMessageCount        int
	ProjectCompressionTips *bool
	OrderByLastActive      bool
	IncludeArchived        bool
	ArchivedOnly           bool
	IDQuery                string
	SearchQuery            string
	CompactRows            bool
	IncludePinned          bool
	SessionKey             string
	IncludeHidden          bool
	IncludeSubagents       bool
	LocalCreatedSources    []string
}

// Shared sessions WHERE filters so counts line up with listed rows
type sessionFilter struct {
	excludeChildren     bool
	source              string
	sources             []string
	sessionKey          string
	excludeSources      []string
	cwdPrefix           string
	minMessageCount     int
	archivedOnly        bool
	includeArchived     bool
	includeSubagents    bool
	localCreatedSources []string
}

// WHERE joining the clauses after lead, empty when there are none
func whereSQL(clauses []string, lead string) string {
	if len(clauses) == 0 {
		return ""
	}
	return lead + "WHERE " + strings.Join(clauses, " AND ")
}

// Delegate marker extraction over a model_config column
func delegateFromJSON(col string) string { return sqlJSONExtract(col, "$._delegate_from") }

// Cwd at or under the prefix, LIKE wildcards in the path escaped
func cwdPrefixClause(cwdPrefix string) (string, []any) {
	prefix := strings.TrimRight(cwdPrefix, `/\`)
	if prefix == "" {
		prefix = cwdPrefix
	}
	esc := EscapeLike(prefix)
	return `(s.cwd = ? OR s.cwd LIKE ? ESCAPE '\' OR s.cwd LIKE ? ESCAPE '\')`, []any{prefix, esc + "/%", esc + `\\%`}
}

// Clauses and params of the sessions filter in the pinned clause order
func sessionFilterWhere(f sessionFilter) ([]string, []any) {
	where := []string{}
	params := []any{}
	if f.excludeChildren && f.includeSubagents {
		where = append(where, "("+listableChildSQL+" OR "+delegateFromJSON("s.model_config")+" IS NOT NULL)")
	} else if f.excludeChildren {
		where = append(where, listableChildSQL, delegateFromJSON("s.model_config")+" IS NULL")
	}
	includeSources := f.sources
	if f.source != "" {
		includeSources = []string{f.source}
	}
	sourceClause, sourceParams := sourceMatchClause(includeSources, f.localCreatedSources)
	type pair struct {
		clause string
		values []any
	}
	pairs := []pair{{sourceClause, sourceParams}}
	if f.sessionKey != "" {
		pairs = append(pairs, pair{"s.session_key = ?", []any{f.sessionKey}})
	}
	pairs = append(pairs, pair{"s.source NOT IN (" + placeholders(len(f.excludeSources)) + ")", stringsToAny(f.excludeSources)})
	if f.cwdPrefix != "" {
		c, v := cwdPrefixClause(f.cwdPrefix)
		pairs = append(pairs, pair{c, v})
	}
	if f.minMessageCount > 0 {
		pairs = append(pairs, pair{"s.message_count >= ?", []any{f.minMessageCount}})
	}
	for _, p := range pairs {
		if len(p.values) > 0 {
			where = append(where, p.clause)
			params = append(params, p.values...)
		}
	}
	if f.archivedOnly {
		where = append(where, "s.archived = 1")
	} else if !f.includeArchived {
		where = append(where, "s.archived = 0")
	}
	return where, params
}

var compactNeedleRe = mustPyRe(`[\W_]+`)

// Extends the WHERE with id and title needles matched across each row's forward compression chain
func chainSearchWhere(where, idNeedle, searchNeedle string) (string, []any, error) {
	params := []any{}
	var clauses []string
	like := func(needle string) string { return "%" + EscapeLike(needle) + "%" }
	if idNeedle != "" {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM chain cq        WHERE cq.root_id = s.id"+
			"          AND LOWER(cq.cur_id) LIKE ? ESCAPE '\\')")
		params = append(params, like(idNeedle))
	}
	if searchNeedle != "" {
		compactNeedle, err := compactNeedleRe.Replace(searchNeedle, "", -1, -1)
		if err != nil {
			return "", nil, err
		}
		searchClause := "EXISTS (SELECT 1 FROM chain cq JOIN sessions cs ON cs.id = cq.cur_id" +
			" WHERE cq.root_id = s.id AND (LOWER(COALESCE(cs.title, '')) LIKE ? ESCAPE '\\'" +
			" OR LOWER(cq.cur_id) LIKE ? ESCAPE '\\'"
		params = append(params, like(searchNeedle), like(searchNeedle))
		if compactNeedle != "" {
			searchClause += " OR REPLACE(REPLACE(REPLACE(REPLACE(LOWER(COALESCE(cs.title, ''))," +
				" '-', ''), '_', ''), '.', ''), ' ', '') LIKE ? ESCAPE '\\'"
			params = append(params, like(compactNeedle))
		}
		clauses = append(clauses, searchClause+"))")
	}
	if len(clauses) == 0 {
		return where, params, nil
	}
	combined := strings.Join(clauses, " AND ")
	if where != "" {
		return where + " AND " + combined, params, nil
	}
	return "WHERE " + combined, params, nil
}

// Unread when activity postdates the last read watermark, a missing watermark reading as read
func sessionUnread(row *pyjson.Object) (bool, error) {
	lastRead := rowValue(row, "last_read_at")
	if lastRead == nil {
		return false, nil
	}
	lastActive := rowValue(row, "last_active")
	if !pyTruthy(lastActive) {
		lastActive = rowValue(row, "started_at")
	}
	if !pyTruthy(lastActive) {
		lastActive = int64(0)
	}
	active, err := pyint.Float(lastActive)
	if err != nil {
		return false, err
	}
	read, err := pyint.Float(lastRead)
	if err != nil {
		return false, err
	}
	return active > read, nil
}

// List row with its preview shaped and the internal ordering column dropped
func listRow(row *pyjson.Object) *pyjson.Object {
	s := richRow(row)
	s.Delete("_effective_last_active")
	return s
}

// Sessions with preview and last_active in one query, chain tip ordered when asked, pins back filled and tips projected
func (d *SessionDB) ListSessionsRich(ctx context.Context, o ListSessionsRichOptions) ([]*pyjson.Object, error) {
	limit := 20
	if o.Limit != nil {
		limit = *o.Limit
	}
	projectTips := o.ProjectCompressionTips == nil || *o.ProjectCompressionTips
	d.FlushForRead(ctx)
	filter := sessionFilter{excludeChildren: !o.IncludeChildren, source: o.Source, sources: o.Sources, sessionKey: o.SessionKey,
		excludeSources: o.ExcludeSources, cwdPrefix: o.CwdPrefix, minMessageCount: o.MinMessageCount, archivedOnly: o.ArchivedOnly,
		includeArchived: o.IncludeArchived, includeSubagents: o.IncludeSubagents, localCreatedSources: o.LocalCreatedSources}
	whereClauses, params := sessionFilterWhere(filter)
	if !o.IncludeHidden && !o.ArchivedOnly {
		whereClauses = append(whereClauses, "s.hidden = 0")
	}
	var heads map[string]localLineageHead
	if !o.IncludeChildren {
		var err error
		if heads, err = d.excludeSupersededSegments(ctx, &whereClauses, &params); err != nil {
			return nil, err
		}
	}
	where := whereSQL(whereClauses, "")
	cols := "s.*"
	if o.CompactRows {
		var err error
		if cols, err = compactSessionCols(ctx); err != nil {
			return nil, err
		}
	}
	selectHead := "SELECT " + cols
	if !o.CompactRows {
		selectHead += ", COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved"
	}
	selectHead += ",\n                    " + sqlPreviewRaw("s.id") + ",\n                    "
	promptJoin := "LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash"
	if o.CompactRows {
		promptJoin = ""
	}
	fromSessions := "FROM sessions s\n                " + promptJoin
	var query string
	if o.OrderByLastActive {
		idNeedle := pystr.Lower(pystr.Strip(o.IDQuery))
		searchNeedle := pystr.Lower(pystr.Strip(o.SearchQuery))
		seedWhere := where
		if idNeedle == "" && searchNeedle == "" {
			seedWhere = whereSQL(append(append([]string{}, whereClauses...),
				"EXISTS (SELECT 1 FROM sessions _seed_c WHERE _seed_c.parent_session_id = s.id)"), "")
		}
		outerWhere, idParams, err := chainSearchWhere(where, idNeedle, searchNeedle)
		if err != nil {
			return nil, err
		}
		query = `
                WITH RECURSIVE chain(root_id, cur_id) AS (
                    SELECT s.id, s.id FROM sessions s ` + seedWhere + `
                    UNION ALL
                    SELECT c.root_id, child.id
                    FROM chain c
                    JOIN sessions parent ON parent.id = c.cur_id
                    JOIN sessions child ON child.parent_session_id = c.cur_id
                    WHERE parent.end_reason = 'compression'
                      AND ` + sqlJSONExtract("child.model_config", "$._branched_from") + ` IS NULL
                      AND ` + sqlJSONExtract("child.model_config", "$._delegate_from") + ` IS NULL
                      AND NOT (` + resetChildSQL("child") + `)
                      AND COALESCE(child.source, '') != 'tool'
                ),
                chain_max AS (
                    SELECT
                        root_id,
                        MAX(` + sqlSessionLastActiveByID("cur_id") + `) AS effective_last_active
                    FROM chain
                    GROUP BY root_id
                ),
                page AS (
                    SELECT s.id AS page_id,
                        COALESCE(cm.effective_last_active, ` + sqlSessionLastActive("s") + `,
                                 ` + sqlInWindow("s.started_at") + `) AS effective_last_active
                    FROM sessions s
                    LEFT JOIN chain_max cm ON cm.root_id = s.id
                    ` + outerWhere + `
                    ORDER BY effective_last_active DESC, s.started_at DESC, s.id DESC
                    LIMIT ? OFFSET ?
                )
                ` + selectHead + sqlSessionLastActive("s") + ` AS last_active,
                    page.effective_last_active AS _effective_last_active
                FROM page
                JOIN sessions s ON s.id = page.page_id
                ` + promptJoin + `
                ORDER BY _effective_last_active DESC, s.started_at DESC, s.id DESC
            `
		bound := append(append(append([]any{}, params...), params...), idParams...)
		params = append(bound, limit, o.Offset)
	} else {
		query = `
                ` + selectHead + sqlSessionLastActive("s") + ` AS last_active
                ` + fromSessions + `
                ` + where + `
                ORDER BY s.started_at DESC
                LIMIT ? OFFSET ?
            `
		params = append(params, limit, o.Offset)
	}
	rows, err := d.readAll(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	sessions := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, listRow(row))
	}
	if o.IncludePinned {
		seen := map[any]bool{}
		for _, s := range sessions {
			seen[rowValue(s, "id")] = true
		}
		pinnedFilter := filter
		pinnedFilter.archivedOnly, pinnedFilter.includeArchived = false, true
		pinnedClauses, pinnedParams := sessionFilterWhere(pinnedFilter)
		if !o.IncludeHidden && !o.ArchivedOnly {
			pinnedClauses = append(pinnedClauses, "s.hidden = 0")
		}
		if len(heads) > 0 {
			if _, err := d.excludeSupersededSegments(ctx, &pinnedClauses, &pinnedParams); err != nil {
				return nil, err
			}
		}
		pinnedClauses = append(pinnedClauses, "s.pinned = 1")
		pinnedQuery := `
                ` + selectHead + sqlSessionLastActive("s") + ` AS last_active
                ` + fromSessions + `
                ` + whereSQL(pinnedClauses, "") + `
                ORDER BY s.started_at DESC
            `
		pinnedRows, err := d.readAll(ctx, pinnedQuery, pinnedParams...)
		if err != nil {
			return nil, err
		}
		for _, row := range pinnedRows {
			s := listRow(row)
			if id := rowValue(s, "id"); !seen[id] {
				seen[id] = true
				sessions = append(sessions, s)
			}
		}
	}
	if projectTips && !o.IncludeChildren {
		if sessions, err = d.projectCompressionTips(ctx, sessions, o.CompactRows); err != nil {
			return nil, err
		}
	}
	if len(heads) > 0 {
		sessions = annotateLocalLineages(sessions, heads)
	}
	for _, s := range sessions {
		unread, err := sessionUnread(s)
		if err != nil {
			return nil, err
		}
		s.Set("unread", unread)
	}
	return sessions, nil
}
