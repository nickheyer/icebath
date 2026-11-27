package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest query the word index is handed
const maxQuery = 2048

var (
	quotedPhrase = regexp.MustCompile(`"[^"]*"`)
	ftsSpecial   = regexp.MustCompile("[" + regexp.QuoteMeta(`+{}():"^@/#&|~[]<>,;!?$=\'`) + "]")
	starRun      = regexp.MustCompile(`\*+`)
	leadingStar  = regexp.MustCompile(`(^|\s)\*`)
	leadingOp    = regexp.MustCompile(`(?i)^(AND|OR|NOT)\b\s*`)
	trailingOp   = regexp.MustCompile(`(?i)\s+(AND|OR|NOT)\s*$`)
	compoundTerm = regexp.MustCompile(`\b(\w+(?:[._-]\w+)+)\b`)
	likeToken    = regexp.MustCompile(`"[^"]+"|\S+`)
	operators    = []string{"AND", "OR", "NOT"}
	// Unified ideographs, extensions A and B, CJK symbols, hiragana, katakana and hangul syllables
	cjkRanges = [][2]rune{{0x4E00, 0x9FFF}, {0x3400, 0x4DBF}, {0x20000, 0x2A6DF}, {0x3000, 0x303F}, {0x3040, 0x309F}, {0x30A0, 0x30FF}, {0xAC00, 0xD7AF}}
)

// Query as the word index takes it: quoted phrases kept, characters it refuses dropped, dotted and dashed terms quoted
func sanitize(query string) string {
	if utf8.RuneCountInString(query) > maxQuery {
		query = string([]rune(query)[:maxQuery])
	}
	var phrases []string
	text := quotedPhrase.ReplaceAllStringFunc(query, func(phrase string) string {
		phrases = append(phrases, phrase)
		return fmt.Sprintf("\x00Q%d\x00", len(phrases)-1)
	})
	text = strings.ReplaceAll(text, `"`, " ")
	text = ftsSpecial.ReplaceAllString(text, " ")
	if strings.Contains(text, "%") && !hasCJK(text) {
		text = strings.ReplaceAll(text, "%", " ")
	}
	text = starRun.ReplaceAllString(text, "*")
	text = leadingStar.ReplaceAllString(text, "$1")
	text = leadingOp.ReplaceAllString(strings.TrimSpace(text), "")
	text = trailingOp.ReplaceAllString(strings.TrimSpace(text), "")
	text = compoundTerm.ReplaceAllString(text, `"$1"`)
	for i, phrase := range phrases {
		text = strings.ReplaceAll(text, fmt.Sprintf("\x00Q%d\x00", i), phrase)
	}
	return strings.TrimSpace(text)
}

func isCJK(r rune) bool {
	for _, span := range cjkRanges {
		if span[0] <= r && r <= span[1] {
			return true
		}
	}
	return false
}

func hasCJK(text string) bool {
	return strings.ContainsFunc(text, isCJK)
}

func countCJK(text string) int {
	n := 0
	for _, r := range text {
		if isCJK(r) {
			n++
		}
	}
	return n
}

// Words of a query other than its operators
func terms(query string) []string {
	var out []string
	for _, word := range strings.Fields(query) {
		if !slices.Contains(operators, strings.ToUpper(word)) {
			out = append(out, word)
		}
	}
	return out
}

// Each word but the operators quoted, so the index takes its characters literally
func quoteTerms(query string) string {
	words := strings.Fields(query)
	for i, word := range words {
		if !slices.Contains(operators, strings.ToUpper(word)) {
			words[i] = `"` + strings.ReplaceAll(word, `"`, `""`) + `"`
		}
	}
	return strings.Join(words, " ")
}

// A query of several words that must all match rewritten so any may, false for one word or one with OR or NOT already
func relaxed(query string) (string, bool) {
	var units []string
	for _, token := range likeToken.FindAllString(query, -1) {
		switch strings.ToUpper(token) {
		case "OR", "NOT":
			return "", false
		case "AND":
			continue
		}
		units = append(units, token)
	}
	if len(units) < 2 {
		return "", false
	}
	return strings.Join(units, " OR "), true
}

// Whether every word gives the trigram index three characters to match
func trigramWords(query string) bool {
	words := terms(strings.TrimSpace(strings.Trim(query, `"`)))
	if len(words) == 0 {
		return false
	}
	for _, word := range words {
		if utf8.RuneCountInString(word) < 3 {
			return false
		}
	}
	return true
}

// Shared conditions of a search over messages m and sessions s
func searchWhere(where []string, args []any, r *sessionsv1.SearchRequest) ([]string, []any) {
	where = append(where, "(m.active = 1 OR m.compacted = 1)", "COALESCE(m.display_kind, '') <> 'hidden'", "m.model_only = 0")
	if len(r.GetSources()) > 0 {
		where = append(where, "s.source IN ("+marks(len(r.GetSources()))+")")
		args = append(args, strings2any(r.GetSources())...)
	}
	if len(r.GetExcludeSources()) > 0 {
		where = append(where, "s.source NOT IN ("+marks(len(r.GetExcludeSources()))+")")
		args = append(args, strings2any(r.GetExcludeSources())...)
	}
	if len(r.GetRoles()) > 0 {
		where = append(where, "m.role IN ("+marks(len(r.GetRoles()))+")")
		args = append(args, strings2any(r.GetRoles())...)
	}
	if r.GetAfter() != nil {
		where = append(where, "s.started_at >= ?")
		args = append(args, seconds(r.GetAfter().AsTime()))
	}
	if r.GetBefore() != nil {
		where = append(where, "s.started_at < ?")
		args = append(args, seconds(r.GetBefore().AsTime()))
	}
	if r.GetProfile() != "" {
		where = append(where, "s.profile = ?")
		args = append(args, r.GetProfile())
	}
	return where, args
}

const searchTail = "m.timestamp, COALESCE(m.tool_name, ''), s.source, COALESCE(s.model, ''), s.started_at"

func (s *store) Search(ctx context.Context, r *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, error) {
	started := time.Now()
	hits, path, err := s.search(ctx, r)
	if elapsed := time.Since(started); elapsed >= time.Duration(s.config.GetInt(sqliteconfig.SearchSlowMS))*time.Millisecond {
		count := "err"
		if err == nil {
			count = fmt.Sprint(len(hits))
		}
		slog.Info(fmt.Sprintf("slow session search: path=%s elapsed=%.0fms rows=%s query=%q", path, float64(elapsed)/float64(time.Millisecond), count,
			strs.Head(r.GetQuery(), 200)))
	}
	return hits, err
}

// Matches of the query by the route it takes, and the route
func (s *store) search(ctx context.Context, r *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, string, error) {
	db, err := s.read(ctx)
	if err != nil {
		return nil, "", err
	}
	query := sanitize(r.GetQuery())
	if query == "" {
		return nil, "empty", nil
	}
	limit := int(r.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	order := "ORDER BY rank"
	switch strings.ToLower(strings.TrimSpace(r.GetSort())) {
	case "newest":
		order = "ORDER BY m.timestamp DESC, rank"
	case "oldest":
		order = "ORDER BY m.timestamp ASC, rank"
	}
	if slices.Contains(r.GetRoles(), "tool") || s.isStale(ctx, db) {
		hits, err := s.scanAll(ctx, db, query, limit, r)
		return hits, "like_scan", err
	}
	// The trigram index serves only once its backfill is done; until then CJK words are scanned for
	_, _, filling, err := progressOf(ctx, db, trigramBackfill)
	if err != nil {
		return nil, "", err
	}
	trigram := s.trigram.Load() && !filling
	if hasCJK(query) {
		raw := strings.TrimSpace(strings.Trim(dropStars(query), `"`))
		if trigram && countCJK(raw) >= 3 && !shortCJK(raw) && !slices.ContainsFunc(r.GetSources(), excludedSource) {
			if hits, ok, err := s.match(ctx, db, "messages_fts_trigram", quoteTerms(raw), order, limit, r); err != nil || ok {
				return hits, "trigram", err
			}
		}
		words := terms(raw)
		if len(words) == 0 {
			words = []string{raw}
		}
		clauses := make([]string, len(words))
		var args []any
		for i, word := range words {
			clauses[i] = anyColumn
			args = append(args, likeArgs(word)...)
		}
		hits, err := s.like(ctx, db, words[0], []string{"(" + strings.Join(clauses, " OR ") + ")"}, args, "ORDER BY m.timestamp DESC", limit, r)
		return hits, "like_scan", err
	}
	hits, ok, err := s.match(ctx, db, "messages_fts", query, order, limit, r)
	if err != nil {
		return nil, "fts5", err
	}
	if !ok {
		return nil, "fts5", nil
	}
	// Messages a rebuild of the index has yet to reach are scanned for, so no match goes missing while it fills
	more, err := s.gap(ctx, db, query, limit-len(hits), r)
	if err != nil {
		return nil, "fts5", err
	}
	hits = merged(hits, more, limit)
	if len(hits) == 0 && trigram && trigramWords(query) {
		if more, ok, err := s.match(ctx, db, "messages_fts_trigram", quoteTerms(strings.TrimSpace(strings.Trim(query, `"`))), order, limit, r); err != nil {
			return nil, "trigram", err
		} else if ok && len(more) > 0 {
			return more, "trigram", nil
		}
	}
	if len(hits) == 0 {
		if loose, ok := relaxed(query); ok {
			if more, ok, err := s.match(ctx, db, "messages_fts", loose, order, limit, r); err == nil && ok && len(more) > 0 {
				return more, "fts5_or", nil
			}
		}
	}
	return hits, "fts5", nil
}

func excludedSource(source string) bool {
	return slices.Contains(trigramExcluded, source)
}

// Whether a CJK word of the query is too short for the trigram index, which then can't serve it
func shortCJK(query string) bool {
	for _, word := range terms(query) {
		if hasCJK(word) && countCJK(word) < 3 {
			return true
		}
	}
	return false
}

// Query without the prefix stars no CJK route honours
func dropStars(query string) string {
	words := strings.Fields(query)
	for i, word := range words {
		if !slices.Contains(operators, strings.ToUpper(word)) {
			if trimmed := strings.TrimRight(word, "*"); trimmed != "" {
				words[i] = trimmed
			}
		}
	}
	return strings.Join(words, " ")
}

// Matches of one index, false when the query can't run against it
func (s *store) match(ctx context.Context, db *sql.DB, table, query, order string, limit int, r *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, bool, error) {
	where, args := searchWhere([]string{table + " MATCH ?"}, []any{query}, r)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT m.id, m.session_id, m.role, snippet(%s, -1, '>>>', '<<<', '...', 40), %s
		FROM %s JOIN messages m ON m.id = %s.rowid JOIN sessions s ON s.id = m.session_id%s %s LIMIT ? OFFSET ?`, table, searchTail, table, table,
		whereSQL(where), order), append(args, limit, r.GetOffset())...)
	if err != nil {
		if indexDamage(err) {
			if detach := s.detachIndexes(ctx, db, err); detach == nil {
				hits, err := s.scanAll(ctx, db, query, limit, r)
				return hits, true, err
			}
		}
		slog.Debug("Search query failed", "index", table, "error", err)
		return nil, false, nil
	}
	hits, err := scanHits(rows)
	return hits, err == nil, err
}

// Any of the text columns holding the term
const anyColumn = `(COALESCE(m.content, '') LIKE ? ESCAPE '\' OR COALESCE(m.tool_name, '') LIKE ? ESCAPE '\' OR COALESCE(m.tool_calls, '') LIKE ? ESCAPE '\')`

func likeArgs(term string) []any {
	pattern := "%" + escapeLike(term) + "%"
	return []any{pattern, pattern, pattern}
}

// Matches found by scanning the messages for the query's words, its AND, OR and NOT kept: what search does while the indexes are stale or
// for tool output, which the index holds only the start of
func (s *store) scanAll(ctx context.Context, db *sql.DB, query string, limit int, r *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, error) {
	type term struct {
		text    string
		negated bool
	}
	groups := [][]term{{}}
	negate := false
	for _, token := range likeToken.FindAllString(query, -1) {
		switch strings.ToUpper(token) {
		case "OR":
			if len(groups[len(groups)-1]) > 0 {
				groups = append(groups, []term{})
			}
			negate = false
			continue
		case "AND", "NEAR":
			continue
		case "NOT":
			negate = true
			continue
		}
		if text := strings.TrimSpace(strings.Trim(strings.Trim(token, `"`), "*")); text != "" {
			groups[len(groups)-1] = append(groups[len(groups)-1], term{text, negate})
			negate = false
		}
	}
	var compiled []string
	var args []any
	anchor := ""
	for _, group := range groups {
		if !slices.ContainsFunc(group, func(t term) bool { return !t.negated }) {
			continue
		}
		var clauses []string
		for _, t := range group {
			clause := anyColumn
			if t.negated {
				clause = "NOT " + clause
			} else if anchor == "" {
				anchor = t.text
			}
			clauses = append(clauses, clause)
			args = append(args, likeArgs(t.text)...)
		}
		compiled = append(compiled, "("+strings.Join(clauses, " AND ")+")")
	}
	if len(compiled) == 0 {
		return nil, nil
	}
	direction := "DESC"
	if strings.ToLower(strings.TrimSpace(r.GetSort())) == "oldest" {
		direction = "ASC"
	}
	return s.like(ctx, db, anchor, []string{"(" + strings.Join(compiled, " OR ") + ")"}, args,
		"ORDER BY m.timestamp "+direction+", m.id "+direction, limit, r)
}

func (s *store) like(ctx context.Context, db *sql.DB, anchor string, where []string, args []any, order string, limit int,
	r *sessionsv1.SearchRequest) ([]*sessionsv1.Hit, error) {
	where, args = searchWhere(where, args, r)
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.session_id, m.role, substr(m.content, max(1, instr(m.content, ?) - 40), 120), `+searchTail+`
		FROM messages m JOIN sessions s ON s.id = m.session_id`+whereSQL(where)+` `+order+` LIMIT ? OFFSET ?`,
		append(append([]any{anchor}, args...), limit, r.GetOffset())...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

func scanHits(rows *sql.Rows) ([]*sessionsv1.Hit, error) {
	defer rows.Close()
	var hits []*sessionsv1.Hit
	for rows.Next() {
		hit := &sessionsv1.Hit{}
		var roleText string
		var snippet sql.NullString
		var at, started float64
		if err := rows.Scan(&hit.Id, &hit.SessionId, &roleText, &snippet, &at, &hit.ToolName, &hit.Source, &hit.Model, &started); err != nil {
			return nil, err
		}
		hit.Role, hit.Snippet = role(roleText), snippet.String
		hit.Time, hit.SessionStarted = timestamppb.New(fromSeconds(at)), timestamppb.New(fromSeconds(started))
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}
