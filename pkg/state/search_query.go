package state

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python re patterns of the FTS5 sanitizer compiled with Python semantics
var (
	sanitizeStarRunRE      = regexp.MustCompile(`\*+`)
	sanitizeLeadingStarRE  = mustPyRe(`(^|\s)\*`)
	sanitizeLeadingOpRE    = mustPyRe(`(?i)^(AND|OR|NOT)\b\s*`)
	sanitizeTrailingOpRE   = mustPyRe(`(?i)\s+(AND|OR|NOT)\s*$`)
	sanitizeCompoundTermRE = mustPyRe(`\b(\w+(?:[._-]\w+)+)\b`)
)

// Compiles a Python re pattern, panicking on a pattern re.compile rejects
func mustPyRe(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// re.sub over a regexp2 pattern with a replacement in regexp2 group syntax
func pySub(re *regexp2.Regexp, text, replacement string) string {
	out, err := re.Replace(text, replacement, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Sanitizes user input for FTS5 MATCH keeping paired quoted phrases and quoting dotted or hyphenated terms
func sanitizeFTS5Query(query string) string {
	if runes := []rune(query); len(runes) > MaxFTS5QueryChars {
		query = string(runes[:MaxFTS5QueryChars])
	}
	var quotedParts []string
	sanitized := quotedPhraseRe.ReplaceAllStringFunc(query, func(m string) string {
		quotedParts = append(quotedParts, m)
		return fmt.Sprintf("\x00Q%d\x00", len(quotedParts)-1)
	})
	sanitized = strings.ReplaceAll(sanitized, `"`, " ")
	sanitized = fts5SpecialRE.ReplaceAllString(sanitized, " ")
	if strings.Contains(sanitized, "%") && !queryContainsCJK(sanitized) {
		sanitized = strings.ReplaceAll(sanitized, "%", " ")
	}
	sanitized = sanitizeStarRunRE.ReplaceAllString(sanitized, "*")
	sanitized = pySub(sanitizeLeadingStarRE, sanitized, "$1")
	sanitized = pySub(sanitizeLeadingOpRE, pystr.Strip(sanitized), "")
	sanitized = pySub(sanitizeTrailingOpRE, pystr.Strip(sanitized), "")
	sanitized = pySub(sanitizeCompoundTermRE, sanitized, `"$1"`)
	for i, quoted := range quotedParts {
		sanitized = strings.ReplaceAll(sanitized, fmt.Sprintf("\x00Q%d\x00", i), quoted)
	}
	return pystr.Strip(sanitized)
}

// Reports any codepoint of text inside the search CJK ranges
func queryContainsCJK(text string) bool {
	for _, ch := range text {
		if searchIsCJK(ch) {
			return true
		}
	}
	return false
}

// Codepoints of text inside the search CJK ranges
func countCJK(text string) int {
	n := 0
	for _, ch := range text {
		if searchIsCJK(ch) {
			n++
		}
	}
	return n
}

// Reports a maximal CJK run of exactly one character, which the bigram index cannot match inside longer runs
func hasLoneCJKRun(query string) bool {
	run := 0
	for _, ch := range query {
		if searchIsCJK(ch) {
			run++
		} else {
			if run == 1 {
				return true
			}
			run = 0
		}
	}
	return run == 1
}

// Implicit AND query rewritten as an any term OR query, false for one unit or an explicit OR or NOT
func orRelaxedQuery(query string) (string, bool) {
	var units []string
	for _, rawToken := range likeTokenRE.FindAllString(query, -1) {
		upper := pystr.Upper(rawToken)
		if upper == "OR" || upper == "NOT" {
			return "", false
		}
		if upper != "AND" {
			units = append(units, rawToken)
		}
	}
	if len(units) < 2 {
		return "", false
	}
	return strings.Join(units, " OR "), true
}

// Reports every non operator token has at least three characters, so each yields trigrams
func trigramEligibleTokens(query string) bool {
	tokens := nonOperatorTokens(pystr.Strip(strings.Trim(query, `"`)))
	if len(tokens) == 0 {
		return false
	}
	for _, t := range tokens {
		if utf8.RuneCountInString(t) < 3 {
			return false
		}
	}
	return true
}

// Reports a non operator CJK token with fewer than three CJK characters, which must take the LIKE route
func hasShortCJKToken(rawQuery string) bool {
	for _, t := range nonOperatorTokens(rawQuery) {
		if queryContainsCJK(t) && countCJK(t) < 3 {
			return true
		}
	}
	return false
}

// Reports the trigram index can serve a CJK query, every CJK token at least three characters
func (s *sessionSearch) trigramRouteOK(rawQuery string) bool {
	return countCJK(rawQuery) >= 3 && !hasShortCJKToken(rawQuery) && s.fts.trigramAvailable.Load()
}

// Name of the routing path a query takes for the slow search log, unknown when classification fails
func (s *sessionSearch) describeSearchPath(query string) (path string) {
	defer func() {
		if recover() != nil {
			path = "unknown"
		}
	}()
	if s.fts.ftsStale.Load() {
		return "like_scan_fts_stale"
	}
	sanitized := sanitizeFTS5Query(query)
	if sanitized == "" {
		return "empty"
	}
	if !queryContainsCJK(sanitized) {
		return "fts5"
	}
	raw := pystr.Strip(strings.Trim(sanitized, `"`))
	if s.fts.ftsCJKAvailable.Load() && !hasLoneCJKRun(raw) {
		return "fts_cjk"
	}
	if s.trigramRouteOK(raw) {
		return "trigram"
	}
	return "like_scan"
}

// MATCH query and params against one FTS5 index joined to messages and sessions
func ftsMatchSQL(table, matchQuery, orderBySQL string, limit, offset int, f searchFilter) (string, []any) {
	where, params := searchFilterClauses([]string{table + " MATCH ?"}, []any{matchQuery}, f)
	params = append(params, limit, offset)
	query := searchSelectSQL(
		fmt.Sprintf("snippet(%s, -1, '>>>', '<<<', '...', 40) AS snippet", table),
		fmt.Sprintf("%s\n            JOIN messages m ON m.id = %s.rowid", table, table), where, orderBySQL, "LIMIT ? OFFSET ?")
	return query, params
}

// Rows of a query as dicts in column order, a repeated name keeping its first column as sqlite3 Row does
func queryObjects(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]*pyjson.Object, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []*pyjson.Object{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		obj := pyjson.NewObject()
		for i, c := range cols {
			if _, dup := obj.Get(c); !dup {
				obj.Set(c, vals[i])
			}
		}
		out = append(out, obj)
	}
	return out, rows.Err()
}

// Runs one MATCH, false when the query cannot execute or a fail open index detached after corruption
func (s *sessionSearch) matchRows(ctx context.Context, table, matchQuery, orderBySQL, failOpen, operationalDebug string,
	limit, offset int, f searchFilter) ([]*pyjson.Object, bool, error) {
	query, params := ftsMatchSQL(table, matchQuery, orderBySQL, limit, offset, f)
	rows, err := s.host.readAll(ctx, query, params...)
	if err == nil {
		return rows, true, nil
	}
	if isOperationalError(err) {
		if operationalDebug != "" {
			slog.Debug(operationalDebug + "\n" + errText(err))
		}
		return nil, false, nil
	}
	if !isDatabaseError(err) || failOpen == "" {
		return nil, false, err
	}
	detached, ferr := s.fts.enterFTSFailOpen(ctx, err, nil, nil)
	if ferr != nil {
		return nil, false, ferr
	}
	if !detached {
		return nil, false, err
	}
	slog.Warn(fmt.Sprintf("%s FTS search hit a corruption error (%s); detached FTS and falling back to canonical LIKE.",
		failOpen, errText(err)))
	return nil, false, nil
}

// Canonical table LIKE scan whose first param is the snippet anchor term
func (s *sessionSearch) likeRows(ctx context.Context, where []string, params []any, orderBy, limitSQL string) ([]*pyjson.Object, error) {
	return s.host.readAll(ctx, searchSelectSQL(likeSnippetSQL, "messages m", where, orderBy, limitSQL), params...)
}

// LIKE term of a boolean query and whether NOT negates it
type likeTerm struct {
	term    string
	negated bool
}

// Compiles the FTS boolean subset into LIKE predicates with params and the snippet term, false when none
func compileLikeBooleanQuery(query string) (string, []any, string, bool) {
	groups := [][]likeTerm{{}}
	negateNext := false
	for _, rawToken := range likeTokenRE.FindAllString(query, -1) {
		switch pystr.Upper(rawToken) {
		case "OR":
			if len(groups[len(groups)-1]) > 0 {
				groups = append(groups, []likeTerm{})
			}
			negateNext = false
			continue
		case "AND", "NEAR":
			continue
		case "NOT":
			negateNext = true
			continue
		}
		term := pystr.Strip(strings.Trim(strings.Trim(rawToken, `"`), "*"))
		if term != "" {
			groups[len(groups)-1] = append(groups[len(groups)-1], likeTerm{term, negateNext})
			negateNext = false
		}
	}
	var compiled []string
	params := []any{}
	snippet, hasSnippet := "", false
	for _, group := range groups {
		positive := false
		for _, t := range group {
			positive = positive || !t.negated
		}
		if !positive {
			continue
		}
		var clauses []string
		for _, t := range group {
			if t.negated {
				clauses = append(clauses, "NOT "+likeCoalescedColumnSQL)
			} else {
				clauses = append(clauses, likeCoalescedColumnSQL)
			}
			params = append(params, stringArgs(likeParams(t.term))...)
			if !hasSnippet && !t.negated {
				snippet, hasSnippet = t.term, true
			}
		}
		compiled = append(compiled, "("+strings.Join(clauses, " AND ")+")")
	}
	return strings.Join(compiled, " OR "), params, snippet, hasSnippet
}

// Searches canonical messages with the compiled boolean LIKE predicate while derived FTS state is stale
func (s *sessionSearch) searchMessagesLikeFallback(ctx context.Context, query string, limit, offset int, sort string,
	f searchFilter) ([]*pyjson.Object, error) {
	predicate, params, snippet, ok := compileLikeBooleanQuery(query)
	if predicate == "" || !ok {
		return []*pyjson.Object{}, nil
	}
	where, params := searchFilterClauses([]string{"(" + predicate + ")"}, params, f)
	order := "DESC"
	if pystr.Lower(pystr.Strip(sort)) == "oldest" {
		order = "ASC"
	}
	args := append(append([]any{snippet}, params...), limit, offset)
	return s.likeRows(ctx, where, args, fmt.Sprintf("ORDER BY m.timestamp %s, m.id %s", order, order), "LIMIT ? OFFSET ?")
}

// Marks FTS stale and disables every index once another process recorded fail open in state_meta
func (s *sessionSearch) refreshFTSStaleState(ctx context.Context) error {
	if s.fts.ftsStale.Load() || !s.fts.ftsEnabled.Load() {
		return nil
	}
	stale, err := s.host.readOne(ctx, "SELECT 1 FROM state_meta WHERE key = ? LIMIT 1", FTSStaleKey)
	if err != nil {
		if _, ok := sqliteClassOf(err); ok {
			return nil
		}
		return err
	}
	if stale != nil {
		s.fts.ftsStale.Store(true)
		s.fts.ftsEnabled.Store(false)
		s.fts.trigramAvailable.Store(false)
		s.fts.ftsCJKAvailable.Store(false)
	}
	return nil
}

// Attaches up to 200 characters of each neighbouring message when context is requested, then projects the fields
func (s *sessionSearch) finalizeSearchMatches(ctx context.Context, matches []*pyjson.Object, resultFields []string) []*pyjson.Object {
	if resultFields == nil || containsString(resultFields, "context") {
		for start := 0; start < len(matches); start += 500 {
			batch := matches[start:min(start+500, len(matches))]
			contexts := s.searchContexts(ctx, batch)
			for _, match := range batch {
				id, _ := match.Get("id")
				match.Set("context", s.searchContextEntries(contexts[contextKey(id)]))
			}
		}
	}
	for _, match := range matches {
		match.Delete("content")
	}
	if resultFields == nil {
		return matches
	}
	out := make([]*pyjson.Object, 0, len(matches))
	for _, match := range matches {
		projected := pyjson.NewObject()
		for _, field := range resultFields {
			if v, ok := match.Get(field); ok {
				projected.Set(field, v)
			}
		}
		out = append(out, projected)
	}
	return out
}

// Reports whether values holds s
func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// Dict key of a match id, ids of equal Python value sharing one key
func contextKey(id any) string {
	return fmt.Sprintf("%T:%v", id, id)
}

// Neighbour rows of each match id in the batch, empty when the context query fails
func (s *sessionSearch) searchContexts(ctx context.Context, batch []*pyjson.Object) map[string][]*pyjson.Object {
	contexts := map[string][]*pyjson.Object{}
	var ids []any
	for _, match := range batch {
		id, _ := match.Get("id")
		if _, seen := contexts[contextKey(id)]; !seen {
			contexts[contextKey(id)] = []*pyjson.Object{}
			ids = append(ids, id)
		}
	}
	query := strings.ReplaceAll(contextWindowSQL, "{ids}", sqlBindMarks(len(ids)))
	var rows []*pyjson.Object
	err := s.host.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		rows, err = queryObjects(ctx, conn, query, ids...)
		return err
	})
	if err != nil {
		return map[string][]*pyjson.Object{}
	}
	for _, row := range rows {
		matchID, _ := row.Get("match_id")
		key := contextKey(matchID)
		if _, ok := contexts[key]; !ok {
			return map[string][]*pyjson.Object{}
		}
		contexts[key] = append(contexts[key], row)
	}
	return contexts
}

// Role and the first 200 characters of flattened content per neighbour, empty when one cannot flatten
func (s *sessionSearch) searchContextEntries(rows []*pyjson.Object) []any {
	entries := []any{}
	for _, row := range rows {
		role, _ := row.Get("role")
		content, _ := row.Get("content")
		text, err := flattenText(s.host.decodeContent(content))
		if err != nil {
			return []any{}
		}
		if runes := []rune(text); len(runes) > 200 {
			text = string(runes[:200])
		}
		entry := pyjson.NewObject()
		entry.Set("role", role)
		entry.Set("content", text)
		entries = append(entries, entry)
	}
	return entries
}

// Merges each present FTS5 index into one segment, a failing index logged, refused on a quarantined handle
func (s *sessionSearch) OptimizeFTS(ctx context.Context) (int, error) {
	if err := s.host.raiseIfDBCorrupt(false); err != nil {
		return 0, err
	}
	lock := s.host.writerLock()
	lock.Lock()
	defer lock.Unlock()
	if err := s.host.raiseIfDBReplaced(); err != nil {
		return 0, err
	}
	if s.host.writerConn() == nil {
		if err := s.host.reopenAfterCloseLocked("write"); err != nil {
			return 0, err
		}
	}
	present, err := s.presentFTSTables(ctx)
	if err != nil {
		return 0, err
	}
	optimized := 0
	for _, tbl := range present {
		if _, err := s.host.writerConn().ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES('optimize')", tbl, tbl)); err != nil {
			if !isDatabaseError(err) {
				return optimized, err
			}
			slog.Warn(fmt.Sprintf("FTS optimize failed for %s: %s", tbl, errText(err)))
			continue
		}
		optimized++
	}
	return optimized, nil
}

// Rebuilds each present FTS5 index from messages under the cross process admission, zero when deferred
func (s *sessionSearch) RebuildFTS(ctx context.Context) (int, error) {
	if err := s.host.raiseIfDBCorrupt(false); err != nil {
		return 0, err
	}
	rebuilt := 0
	err := FTSRebuildAdmission(s.host.dbPath(), nil, func(admitted bool) error {
		if !admitted {
			slog.Warn("Deferred in-place FTS rebuild: another process holds the rebuild authority for this state.db.")
			return nil
		}
		lock := s.host.writerLock()
		lock.Lock()
		defer lock.Unlock()
		if err := s.host.raiseIfDBReplaced(); err != nil {
			return err
		}
		if s.host.writerConn() == nil {
			if err := s.host.reopenAfterCloseLocked("write"); err != nil {
				return err
			}
		}
		present, err := s.presentFTSTables(ctx)
		if err != nil {
			return err
		}
		for _, tbl := range present {
			conn := s.host.writerConn()
			_, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES('rebuild')", tbl, tbl))
			if err == nil {
				err = endTransaction(ctx, conn, "COMMIT")
			}
			if err == nil {
				rebuilt++
				continue
			}
			if !isDatabaseError(err) {
				return err
			}
			if rerr := endTransaction(ctx, conn, "ROLLBACK"); rerr != nil {
				return rerr
			}
			if IsMalformedDBError(err) {
				slog.Error(fmt.Sprintf("FTS rebuild failed with a corruption-class error for %s: %s; "+
					"run 'icebath sessions repair' (or 'icebath doctor --fix') to rebuild the index offline", tbl, errText(err)))
			} else {
				slog.Warn(fmt.Sprintf("FTS rebuild failed for %s: %s", tbl, errText(err)))
			}
		}
		return nil
	})
	return rebuilt, err
}
