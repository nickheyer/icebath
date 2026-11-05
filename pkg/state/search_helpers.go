package state

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python floor division of two ints
func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// Messages a backfill pass indexed out of those at or below its high water, counted in rows
type backfillProgress struct {
	phase   string
	total   int
	indexed int
}

// Progress callback payload of a backfill pass
func (p *backfillProgress) info() *pyjson.Object {
	done, total := p.indexed, p.total
	percent := 100
	if total != 0 {
		percent = min(100, floorDiv(100*done, total))
	}
	out := pyjson.NewObject()
	out.Set("phase", p.phase)
	out.Set("percent", percent)
	out.Set("indexed", min(done, total))
	out.Set("total", total)
	return out
}

// Demoted shadow table optimize storage is draining and how far, counted false skipping the per table COUNT
type teardownProgress struct {
	table      string
	tableIndex int
	tableCount int
	total      int
	drained    int
	counted    bool
}

// Teardown progress with every count zero and per table counting on
func newTeardownProgress() *teardownProgress {
	return &teardownProgress{counted: true}
}

// Progress callback payload of the teardown phase
func (p *teardownProgress) info() *pyjson.Object {
	done, total := p.drained, p.total
	percent := 100
	if total != 0 {
		percent = min(100, floorDiv(100*done, total))
	}
	out := pyjson.NewObject()
	out.Set("phase", "teardown")
	out.Set("percent", percent)
	out.Set("indexed", done)
	out.Set("total", total)
	out.Set("table", p.table)
	out.Set("table_index", p.tableIndex)
	out.Set("table_count", p.tableCount)
	return out
}

// VACUUM liveness as elapsed time against the size before and the estimated size after
type vacuumProgress struct {
	sizeBytes      int64
	estimatedBytes int64
	started        float64
}

// Vacuum progress started now on the monotonic clock
func newVacuumProgress(sizeBytes, estimatedBytes int64) *vacuumProgress {
	return &vacuumProgress{sizeBytes: sizeBytes, estimatedBytes: estimatedBytes, started: monotonicSeconds()}
}

// Progress callback payload of the vacuum phase
func (p *vacuumProgress) info() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("phase", "vacuum")
	out.Set("percent", 0)
	out.Set("indexed", 0)
	out.Set("total", 0)
	out.Set("elapsed", monotonicSeconds()-p.started)
	out.Set("size_bytes", p.sizeBytes)
	out.Set("estimated_bytes", p.estimatedBytes)
	return out
}

// Foreign holder scan rerun every interval on a side goroutine while optimize storage runs
type holderWatch struct {
	scan     func() (bool, error)
	interval time.Duration
	isHeld   atomic.Bool
	stop     chan struct{}
	done     chan struct{}
}

// Watch over scan, which reports whether a foreign process holds the store
func newHolderWatch(scan func() (bool, error), interval float64) *holderWatch {
	return &holderWatch{scan: scan, interval: time.Duration(interval * float64(time.Second)),
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Reports the latest scan verdict
func (w *holderWatch) held() bool { return w.isHeld.Load() }

func (w *holderWatch) refresh() error {
	held, err := w.scan()
	if err != nil {
		return err
	}
	w.isHeld.Store(held)
	return nil
}

// Rescans every interval until the watch exits, a failed scan ending the goroutine as it ends the Python thread
func (w *holderWatch) run() {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			return
		case <-time.After(w.interval):
			if err := w.refresh(); err != nil {
				reportThreadException("optimize-holder-watch", err)
				return
			}
		}
	}
}

// Scans once so a present holder paces the first chunk, then starts the side goroutine
func (w *holderWatch) enter() (*holderWatch, error) {
	if err := w.refresh(); err != nil {
		return nil, err
	}
	go w.run()
	return w, nil
}

// Writes the report threading.excepthook prints for an exception escaping a thread
var reportThreadException = func(name string, err error) {
	fmt.Fprintf(os.Stderr, "Exception in thread %s:\n%s\n", name, errText(err))
}

// Stops the side goroutine and waits for it
func (w *holderWatch) exit() {
	close(w.stop)
	<-w.done
}

// sessions.search_slow_ms for the served profile, 1000 when unset, blank or not a number
func searchSlowMS(ctx context.Context) (float64, error) {
	value := RoutedSessionsSetting(ctx, "search_slow_ms", "ICEBATH_SEARCH_SLOW_MS")
	if value == nil {
		return 1000.0, nil
	}
	text, err := pyrepr.Format(value, "")
	if err != nil {
		text = pyrepr.Repr(value)
	}
	if pystr.Strip(text) == "" {
		return 1000.0, nil
	}
	f, err := pyint.Float(value)
	if err != nil {
		if pe, ok := err.(*pyint.Error); ok && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
			return 1000.0, nil
		}
		return 0, err
	}
	return f, nil
}

// Characters FTS5 rejects outside a quoted phrase, percent kept literal for the CJK LIKE fallback
const fts5SpecialChars = `+{}():"^@/#&|~[]<>,;!?$=\'`

// Python unicode whitespace complement, the \S of a str pattern
const pyNonSpaceClass = `[^\t\n\v\f\r \x1c-\x1f\x85\xa0\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	fts5SpecialRE  = regexp.MustCompile("[" + regexp.QuoteMeta(fts5SpecialChars) + "]")
	ftsOperators   = []string{"AND", "OR", "NOT"}
	likeSkipTokens = []string{"AND", "OR", "NOT", "NEAR"}
	likeTokenRE    = regexp.MustCompile(`"[^"]+"|` + pyNonSpaceClass + `+`)
)

// Result columns every search route shares, snippet and metadata but never content
const (
	searchSelectTail       = "m.timestamp, m.tool_name, s.source, s.model, s.started_at AS session_started"
	likeSnippetSQL         = "substr(m.content, max(1, instr(m.content, ?) - 40), 120) AS snippet"
	likeAnyColumnSQL       = `(m.content LIKE ? ESCAPE '\' OR m.tool_name LIKE ? ESCAPE '\' OR m.tool_calls LIKE ? ESCAPE '\')`
	likeCoalescedColumnSQL = `(COALESCE(m.content, '') LIKE ? ESCAPE '\' OR ` +
		`COALESCE(m.tool_name, '') LIKE ? ESCAPE '\' OR ` +
		`COALESCE(m.tool_calls, '') LIKE ? ESCAPE '\')`
)

// ORDER BY of the FTS routes per sort, unknown values ranking only
var ftsOrderBy = map[string]string{"newest": "ORDER BY m.timestamp DESC, rank", "oldest": "ORDER BY m.timestamp ASC, rank"}

// Neighbor seeks around each hit, the ids slot filled with the hit id list
const contextWindowSQL = `WITH target AS (
    SELECT session_id, timestamp, id FROM messages WHERE id IN ({ids})
)
SELECT t.id AS match_id, m.role, m.content
FROM target t JOIN messages m ON m.id IN (
    t.id,
    (SELECT p.id FROM messages p
     WHERE p.session_id = t.session_id AND (p.timestamp, p.id) < (t.timestamp, t.id)
     ORDER BY p.timestamp DESC, p.id DESC LIMIT 1),
    (SELECT n.id FROM messages n
     WHERE n.session_id = t.session_id AND (n.timestamp, n.id) > (t.timestamp, t.id)
     ORDER BY n.timestamp, n.id LIMIT 1)
)
ORDER BY t.id, m.timestamp, m.id`

// Unified ideographs, extensions A and B, CJK symbols, hiragana, katakana and hangul syllables
var searchCJKRanges = [][2]rune{
	{0x4E00, 0x9FFF}, {0x3400, 0x4DBF}, {0x20000, 0x2A6DF}, {0x3000, 0x303F}, {0x3040, 0x309F},
	{0x30A0, 0x30FF}, {0xAC00, 0xD7AF},
}

// One state_meta row, nil when absent
func metaRow(ctx context.Context, conn SQLiteConn, key string) ([]any, error) {
	return fetchOne(ctx, conn, "SELECT value FROM state_meta WHERE key = ?", key)
}

// Deletes state_meta rows by key
func deleteMeta(ctx context.Context, conn SQLiteConn, keys ...string) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("DELETE FROM state_meta WHERE key IN (%s)", sqlBindMarks(len(keys))), stringArgs(keys)...)
	return err
}

// Reports a codepoint inside the search CJK ranges
func searchIsCJK(cp rune) bool {
	for _, r := range searchCJKRanges {
		if r[0] <= cp && cp <= r[1] {
			return true
		}
	}
	return false
}

// Whitespace tokens of a query other than AND, OR and NOT
func nonOperatorTokens(rawQuery string) []string {
	out := []string{}
	for _, tok := range pystr.SplitWhitespace(rawQuery, -1) {
		if !slices.Contains(ftsOperators, pystr.Upper(tok)) {
			out = append(out, tok)
		}
	}
	return out
}

// Quotes each non operator token so FTS5 special characters are literal, keeping AND, OR and NOT
func quoteFTSTokens(rawQuery string) string {
	tokens := pystr.SplitWhitespace(rawQuery, -1)
	out := make([]string, len(tokens))
	for i, tok := range tokens {
		if slices.Contains(ftsOperators, pystr.Upper(tok)) {
			out[i] = tok
		} else {
			out[i] = `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
		}
	}
	return strings.Join(out, " ")
}

// One escaped substring bind per column of the any column LIKE clause
func likeParams(term string) []string {
	p := "%" + EscapeLike(term) + "%"
	return []string{p, p, p}
}

// Drops trailing prefix stars no CJK route can honour, an all star token keeping its form
func stripCJKWildcards(rawQuery string) string {
	if !strings.Contains(rawQuery, "*") {
		return rawQuery
	}
	var stripped []string
	for _, tok := range pystr.SplitWhitespace(rawQuery, -1) {
		if slices.Contains(ftsOperators, pystr.Upper(tok)) {
			stripped = append(stripped, tok)
			continue
		}
		if trimmed := strings.TrimRight(tok, "*"); trimmed != "" {
			stripped = append(stripped, trimmed)
		} else {
			stripped = append(stripped, tok)
		}
	}
	if joined := strings.Join(stripped, " "); joined != "" {
		return joined
	}
	return rawQuery
}

// Joined text parts of a multimodal list or the multimodal content marker, a string as is, anything else empty
func flattenText(decoded any) (string, error) {
	list, ok := decoded.([]any)
	if !ok {
		s, _ := decoded.(string)
		return s, nil
	}
	var parts []any
	for _, p := range list {
		obj, ok := p.(*pyjson.Object)
		if !ok {
			continue
		}
		if kind, _ := obj.Get("type"); kind != "text" {
			continue
		}
		text, found := obj.Get("text")
		if !found {
			text = ""
		}
		parts = append(parts, text)
	}
	var texts []string
	for _, t := range parts {
		if !pyTruthy(t) {
			continue
		}
		s, ok := t.(string)
		if !ok {
			return "", &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("sequence item %d: expected str instance, %s found", len(texts), pyTypeName(t))}
		}
		texts = append(texts, s)
	}
	if joined := pystr.Strip(strings.Join(texts, " ")); joined != "" {
		return joined, nil
	}
	return "[multimodal content]", nil
}

// TypeError unless value is an int other than bool, ValueError unless it is positive
func positiveInt(name string, value any) error {
	var n *big.Int
	switch v := value.(type) {
	case int:
		n = big.NewInt(int64(v))
	case int64:
		n = big.NewInt(v)
	case int32:
		n = big.NewInt(int64(v))
	case *big.Int:
		n = v
	}
	if n == nil {
		return &pyint.Error{Kind: "TypeError", Msg: name + " must be an integer"}
	}
	if n.Sign() <= 0 {
		return &pyint.Error{Kind: "ValueError", Msg: name + " must be greater than zero"}
	}
	return nil
}

// Result row SELECT the FTS and LIKE routes share, its text pinned
func searchSelectSQL(snippetSQL, fromSQL string, where []string, orderBy, limitSQL string) string {
	return fmt.Sprintf(`
            SELECT m.id, m.session_id, m.role,
                   %s,
                   %s
            FROM %s
            JOIN sessions s ON s.id = m.session_id
            WHERE %s
            %s
            %s
        `, snippetSQL, searchSelectTail, fromSQL, strings.Join(where, " AND "), orderBy, limitSQL)
}

// Visibility, source, role and session start predicates of a search, nil source lists meaning no filter
type searchFilter struct {
	includeInactive bool
	sourceFilter    []string
	excludeSources  []string
	roleFilter      []string
	afterTS         *int64
	beforeTS        *int64
}

// Appends the predicates every search route shares with their binds
func searchFilterClauses(where []string, params []any, f searchFilter) ([]string, []any) {
	if !f.includeInactive {
		where = append(where, "(m.active = 1 OR m.compacted = 1)")
	}
	where = append(where, "COALESCE(m.display_kind, '') <> 'hidden'")
	if f.sourceFilter != nil {
		where = append(where, fmt.Sprintf("s.source IN (%s)", sqlBindMarks(len(f.sourceFilter))))
		params = append(params, stringArgs(f.sourceFilter)...)
	}
	if f.excludeSources != nil {
		where = append(where, fmt.Sprintf("s.source NOT IN (%s)", sqlBindMarks(len(f.excludeSources))))
		params = append(params, stringArgs(f.excludeSources)...)
	}
	if len(f.roleFilter) > 0 {
		where = append(where, fmt.Sprintf("m.role IN (%s)", sqlBindMarks(len(f.roleFilter))))
		params = append(params, stringArgs(f.roleFilter)...)
	}
	if f.afterTS != nil {
		where = append(where, "s.started_at >= ?")
		params = append(params, *f.afterTS)
	}
	if f.beforeTS != nil {
		where = append(where, "s.started_at < ?")
		params = append(params, *f.beforeTS)
	}
	return where, params
}
