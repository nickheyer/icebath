package state

import (
	"database/sql"
	"regexp"
	"strings"
	"unicode"
)

// Options of a message search
type SearchOptions struct {
	Sources        []string
	ExcludeSources []string
	Roles          []string
	Limit          int
	Offset         int
	// newest, oldest or empty for rank order
	Sort            string
	IncludeInactive bool
	// Session start bounds in unix seconds, zero for none
	After  float64
	Before float64
}

// One search hit with its surrounding rows
type SearchHit struct {
	RowID          int64
	SessionID      string
	Role           string
	Snippet        string
	Timestamp      float64
	ToolName       string
	Source         string
	Model          string
	SessionStarted float64
	Before         *Message
	After          *Message
}

var quotedPhraseRe = regexp.MustCompile(`"[^"]*"`)
var ftsSpecialRe = regexp.MustCompile("[(){}\\[\\]^:~+\\-/\\\\,;!?@#$&=<>|`'.]")
var starRunRe = regexp.MustCompile(`\*+`)
var leadingStarRe = regexp.MustCompile(`(^|\s)\*`)
var leadingOpRe = regexp.MustCompile(`(?i)^(AND|OR|NOT)\b\s*`)
var trailingOpRe = regexp.MustCompile(`(?i)\s+(AND|OR|NOT)\s*$`)
var compoundTermRe = regexp.MustCompile(`\b(\w+(?:[._-]\w+)+)\b`)

// Query safe for an FTS5 MATCH, quoted phrases kept and stray syntax dropped
func SanitizeFTSQuery(query string) string {
	if len(query) > MaxFTS5QueryChars {
		query = query[:MaxFTS5QueryChars]
	}
	var quoted []string
	sanitized := quotedPhraseRe.ReplaceAllStringFunc(query, func(m string) string {
		quoted = append(quoted, m)
		return "\x00Q" + string(rune('0'+len(quoted)-1)) + "\x00"
	})
	sanitized = strings.ReplaceAll(sanitized, `"`, " ")
	sanitized = compoundTermRe.ReplaceAllString(sanitized, `"$1"`)
	sanitized = ftsSpecialRe.ReplaceAllStringFunc(sanitized, func(s string) string {
		return " "
	})
	sanitized = strings.ReplaceAll(sanitized, "%", " ")
	sanitized = starRunRe.ReplaceAllString(sanitized, "*")
	sanitized = leadingStarRe.ReplaceAllString(sanitized, "$1")
	sanitized = leadingOpRe.ReplaceAllString(strings.TrimSpace(sanitized), "")
	sanitized = trailingOpRe.ReplaceAllString(strings.TrimSpace(sanitized), "")
	for i, q := range quoted {
		sanitized = strings.ReplaceAll(sanitized, "\x00Q"+string(rune('0'+i))+"\x00", q)
	}
	return strings.TrimSpace(sanitized)
}

// Query with every term joined by OR, nil when it already carries operators or one term
func relaxedQuery(query string) string {
	upper := strings.ToUpper(query)
	if strings.Contains(upper, " OR ") || strings.Contains(upper, " NOT ") || strings.Contains(query, `"`) {
		return ""
	}
	terms := strings.Fields(query)
	var kept []string
	for _, t := range terms {
		if strings.ToUpper(t) != "AND" {
			kept = append(kept, t)
		}
	}
	if len(kept) < 2 {
		return ""
	}
	return strings.Join(kept, " OR ")
}

// Reports whether text carries CJK characters the word tokenizer splits poorly
func containsCJK(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// Shared visibility, source, role and time predicates of every search route
func searchFilters(o SearchOptions) (string, []any) {
	where := []string{}
	var params []any
	if !o.IncludeInactive {
		where = append(where, "(m.active = 1 OR m.compacted = 1)")
	}
	where = append(where, "COALESCE(m.display_kind, '') <> 'hidden'")
	if len(o.Sources) > 0 {
		where = append(where, "s.source IN ("+placeholders(len(o.Sources))+")")
		params = append(params, args(o.Sources)...)
	}
	if len(o.ExcludeSources) > 0 {
		where = append(where, "s.source NOT IN ("+placeholders(len(o.ExcludeSources))+")")
		params = append(params, args(o.ExcludeSources)...)
	}
	if len(o.Roles) > 0 {
		where = append(where, "m.role IN ("+placeholders(len(o.Roles))+")")
		params = append(params, args(o.Roles)...)
	}
	if o.After > 0 {
		where = append(where, "s.started_at >= ?")
		params = append(params, o.After)
	}
	if o.Before > 0 {
		where = append(where, "s.started_at < ?")
		params = append(params, o.Before)
	}
	return strings.Join(where, " AND "), params
}

// Order clause of a search sort
func searchOrder(sort string) string {
	switch strings.ToLower(strings.TrimSpace(sort)) {
	case "newest":
		return "ORDER BY m.timestamp DESC, rank"
	case "oldest":
		return "ORDER BY m.timestamp ASC, rank"
	}
	return "ORDER BY rank"
}

// Full text search over every session, hits carrying a snippet and one row of context each side
func (d *DB) SearchMessages(query string, o SearchOptions) ([]SearchHit, error) {
	if o.Limit <= 0 {
		o.Limit = 20
	}
	q := SanitizeFTSQuery(query)
	if q == "" {
		return nil, nil
	}
	toolOnly := false
	for _, r := range o.Roles {
		if r == "tool" {
			toolOnly = true
		}
	}
	var hits []SearchHit
	var err error
	if toolOnly || containsCJK(q) {
		hits, err = d.likeSearch(q, o)
	} else {
		hits, err = d.ftsSearch(q, o)
		if err == nil && len(hits) == 0 {
			if relaxed := relaxedQuery(q); relaxed != "" {
				hits, err = d.ftsSearch(relaxed, o)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	for i := range hits {
		around, err := d.MessagesAround(hits[i].SessionID, hits[i].RowID, 1)
		if err != nil {
			return nil, err
		}
		for _, m := range around {
			switch {
			case m.RowID < hits[i].RowID:
				hits[i].Before = m
			case m.RowID > hits[i].RowID && hits[i].After == nil:
				hits[i].After = m
			}
		}
	}
	return hits, nil
}

// Hits of the word index for a sanitized query
func (d *DB) ftsSearch(q string, o SearchOptions) ([]SearchHit, error) {
	where, params := searchFilters(o)
	sqlText := `SELECT m.id, m.session_id, m.role, snippet(messages_fts, 0, '>>>', '<<<', '...', 32), m.timestamp,
		m.tool_name, s.source, s.model, s.started_at
		FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH ? AND ` + where + " " + searchOrder(o.Sort) + " LIMIT ? OFFSET ?"
	all := append([]any{q}, params...)
	all = append(all, o.Limit, o.Offset)
	return d.scanHits(sqlText, all)
}

// Hits of a LIKE scan over the stored rows for terms the index cannot serve
func (d *DB) likeSearch(q string, o SearchOptions) ([]SearchHit, error) {
	where, params := searchFilters(o)
	terms := strings.Fields(strings.ReplaceAll(q, `"`, ""))
	if len(terms) == 0 {
		return nil, nil
	}
	var clauses []string
	for _, t := range terms {
		if strings.EqualFold(t, "AND") || strings.EqualFold(t, "OR") {
			continue
		}
		clauses = append(clauses, "m.content LIKE ? ESCAPE '\\'")
		params = append(params, "%"+EscapeLike(strings.TrimSuffix(t, "*"))+"%")
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	order := "ORDER BY m.timestamp DESC"
	if strings.EqualFold(o.Sort, "oldest") {
		order = "ORDER BY m.timestamp ASC"
	}
	sqlText := `SELECT m.id, m.session_id, m.role, substr(COALESCE(m.content, ''), 1, 200), m.timestamp, m.tool_name, s.source, s.model, s.started_at
		FROM messages m JOIN sessions s ON s.id = m.session_id WHERE ` + where + " AND " + strings.Join(clauses, " AND ") + " " + order + " LIMIT ? OFFSET ?"
	params = append(params, o.Limit, o.Offset)
	return d.scanHits(sqlText, params)
}

// Hits decoded from a search statement
func (d *DB) scanHits(sqlText string, params []any) ([]SearchHit, error) {
	var hits []SearchHit
	err := d.read(func(q querier) error {
		rows, err := q.Query(sqlText, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		hits = nil
		for rows.Next() {
			var h SearchHit
			var snippet, toolName, model sql.NullString
			if err := rows.Scan(&h.RowID, &h.SessionID, &h.Role, &snippet, &h.Timestamp, &toolName, &h.Source, &model, &h.SessionStarted); err != nil {
				return err
			}
			h.Snippet, h.ToolName, h.Model = snippet.String, toolName.String, model.String
			if strings.HasPrefix(h.Snippet, contentJSONPrefix) {
				h.Snippet = ContentText(decodeContent(snippet))
			}
			hits = append(hits, h)
		}
		return rows.Err()
	})
	return hits, err
}

// Rebuilds the word index from the stored rows
func (d *DB) RebuildFTS() error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')")
		return err
	})
}

// Merges the word index segments
func (d *DB) OptimizeFTS() error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO messages_fts(messages_fts) VALUES ('optimize')")
		return err
	})
}
