package state

import (
	"context"
	"math/big"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Longest sanitized title a session may carry, counted in code points
const MaxTitleLength = 100

// Exact title of the hidden canonical Bot Chat whose name is its identity
const CanonicalBotChatTitle = "Bot Chat"

// Ended, empty, unarchived session predicate shared by the empty session badge, sweep and title conflicts
const emptySessionWhere = "message_count = 0 AND ended_at IS NOT NULL AND archived = 0 AND NOT EXISTS (" +
	"SELECT 1 FROM messages WHERE messages.session_id = sessions.id)"

// Title provenance authority from lowest to highest
var titleSourceRank = map[string]int{TitleSourceDerived: 0, TitleSourceLLM: 1, TitleSourceUser: 2}

var (
	titleControlRe    = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	titleInvisibleRe  = regexp.MustCompile(`[\x{200b}-\x{200f}\x{2028}-\x{202e}\x{2060}-\x{2069}\x{feff}\x{fffc}\x{fff9}-\x{fffb}]`)
	numberedTitleRe   = mustPyRe(`^(.*?) #(\d+)$`)
	titleWhitespaceRe = mustPyRe(`\s+`)
)

// SessionDB members the title rules call, implemented by the hermes_state SessionDB
type titlesHost interface {
	executeWrite(ctx context.Context, fn func(conn SQLiteConn) error, patienceS *float64) error
	readOne(ctx context.Context, query string, args ...any) (*pyjson.Object, error)
	readAll(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error)
	writeRowcount(ctx context.Context, query string, args []any, patienceS *float64) (int64, error)
	readRetryingIOErr(ctx context.Context, fn func(conn SQLiteConn) error) error
	sessionRowDict(row *pyjson.Object) *pyjson.Object
}

// Title sanitizing, provenance ranking and lineage lookups, embedded in SessionDB so its methods are SessionDB methods
type sessionTitles struct {
	host titlesHost
}

func newSessionTitles(host titlesHost) *sessionTitles {
	return &sessionTitles{host: host}
}

// Matches the numbered title pattern, raising the TypeError re.match gives a non string title
func matchNumberedTitle(title any) (*big.Int, string, bool, error) {
	var text string
	switch v := title.(type) {
	case string:
		text = v
	case []byte:
		return nil, "", false, &pyint.Error{Kind: "TypeError", Msg: "cannot use a string pattern on a bytes-like object"}
	default:
		return nil, "", false, &pyint.Error{Kind: "TypeError", Msg: "expected string or bytes-like object, got '" + pyrepr.TypeName(title) + "'"}
	}
	m, err := numberedTitleRe.FindStringMatch(text)
	if err != nil || m == nil {
		return nil, "", false, err
	}
	n, _ := pyint.Parse(m.GroupByNumber(2).String())
	return n, m.GroupByNumber(1).String(), true, nil
}

// Next lineage title seen by conn, stripping a number suffix then incrementing the highest existing number
func nextTitleInLineage(ctx context.Context, conn SQLiteConn, baseTitle string) (string, error) {
	base := baseTitle
	if _, head, ok, err := matchNumberedTitle(baseTitle); err != nil {
		return "", err
	} else if ok {
		base = head
	}
	rows, err := queryObjects(ctx, conn, "SELECT title FROM sessions WHERE title = ? OR title LIKE ? ESCAPE '\\'", base, EscapeLike(base)+" #%")
	if err != nil || len(rows) == 0 {
		return base, err
	}
	highest := big.NewInt(1)
	for _, row := range rows {
		title, _ := row.Get("title")
		n, _, ok, err := matchNumberedTitle(title)
		if err != nil {
			return "", err
		}
		if ok && n.Cmp(highest) > 0 {
			highest = n
		}
	}
	return base + " #" + new(big.Int).Add(highest, big.NewInt(1)).String(), nil
}

// Next lineage title on an authority owned transaction connection, counting siblings committed inside it
func LineageTitleOnConn(ctx context.Context, conn SQLiteConn, baseTitle string) (string, error) {
	return nextTitleInLineage(ctx, conn, baseTitle)
}

// Authority of a stored title source, NULL ranking as user and unknown sources ranking zero
func titleRankOf(source any) int {
	if source == nil {
		return titleSourceRank[TitleSourceUser]
	}
	return titleSourceRank[pyStr(source)]
}

// Strips controls, invisibles and lone surrogates and collapses whitespace, empty meaning None, ValueError past the length cap
func SanitizeTitle(title string) (string, error) {
	if title == "" {
		return "", nil
	}
	cleaned := titleInvisibleRe.ReplaceAllString(titleControlRe.ReplaceAllString(SanitizeSurrogates(title), ""), "")
	cleaned = pystr.Strip(pySub(titleWhitespaceRe, cleaned, " "))
	if cleaned == "" {
		return "", nil
	}
	if n := pystr.Len(cleaned); n > MaxTitleLength {
		return "", &pyint.Error{Kind: "ValueError", Msg: "Title too long (" + big.NewInt(int64(n)).String() + " chars, max " + big.NewInt(MaxTitleLength).String() + ")"}
	}
	return cleaned, nil
}

// Reports whether ancestorID is a compression predecessor of descendantID along the canonical continuation edge
func (t *sessionTitles) isCompressionAncestor(ctx context.Context, conn SQLiteConn, ancestorID, descendantID string) (bool, error) {
	if ancestorID == "" || descendantID == "" || ancestorID == descendantID {
		return false, nil
	}
	edge := compressionChildSQL("child") + nonContinuationChildSQL("child.", "parent.id")
	rows, err := queryObjects(ctx, conn, `
            WITH RECURSIVE ancestors(id) AS (
                SELECT ?
                UNION
                SELECT parent.id
                FROM ancestors a
                JOIN sessions child ON child.id = a.id
                JOIN sessions parent ON parent.id = child.parent_session_id
                WHERE `+edge+`
            )
            SELECT 1 FROM ancestors WHERE id = ? AND id != ? LIMIT 1
            `, descendantID, ancestorID, descendantID)
	return len(rows) > 0, err
}

// Frees title for sessionID inside the caller transaction, returning the cleared holder or a ValueError when it is legitimately held
func (t *sessionTitles) resolveTitleConflict(ctx context.Context, conn SQLiteConn, sessionID, title string) (string, bool, error) {
	rows, err := queryObjects(ctx, conn, "SELECT id, archived, hidden, "+emptySessionWhere+" AS ghost "+
		"FROM sessions WHERE title = ? AND id != ?", title, sessionID)
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	conflict := rows[0]
	rawID, _ := conflict.Get("id")
	conflictID := pyStr(rawID)
	ancestor, err := t.isCompressionAncestor(ctx, conn, conflictID, sessionID)
	if err != nil {
		return "", false, err
	}
	if ancestor {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET title = NULL WHERE id = ?", rawID); err != nil {
			return "", false, err
		}
		return conflictID, true, nil
	}
	archived, _ := conflict.Get("archived")
	hidden, _ := conflict.Get("hidden")
	ghost, _ := conflict.Get("ghost")
	if (title == CanonicalBotChatTitle && pyTruthy(archived) && pyTruthy(hidden)) || (pyTruthy(ghost) && !pyTruthy(hidden)) {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET title = NULL, title_source = NULL WHERE id = ?", rawID); err != nil {
			return "", false, err
		}
		return conflictID, true, nil
	}
	return "", false, &pyint.Error{Kind: "ValueError", Msg: "Title '" + title + "' is already in use by session " + conflictID}
}

// Applies the title rules inside an authority owned transaction without committing, returning the affected session ids
func (t *sessionTitles) setSessionTitleInTransaction(ctx context.Context, conn SQLiteConn, sessionID, title, source string) ([]string, error) {
	title, err := SanitizeTitle(title)
	if err != nil {
		return nil, err
	}
	isUser := source == TitleSourceUser
	affected := []string{}
	rows, err := queryObjects(ctx, conn, "SELECT title, title_source, hidden FROM sessions WHERE id = ?", sessionID)
	if err != nil || len(rows) == 0 {
		return affected, err
	}
	current := rows[0]
	currentTitle, _ := current.Get("title")
	currentSource, _ := current.Get("title_source")
	hidden, _ := current.Get("hidden")
	if held, _ := currentTitle.(string); held == CanonicalBotChatTitle && pyTruthy(hidden) && title != CanonicalBotChatTitle {
		if isUser {
			return nil, &pyint.Error{Kind: "ValueError", Msg: "This is the bot's canonical Bot Chat — its name is its " +
				"identity, and renaming it would orphan the conversation. " +
				"To start fresh, create a new bot instead."}
		}
		return affected, nil
	}
	if !isUser && currentTitle != nil && titleRankOf(currentSource) >= titleRankOf(source) {
		return affected, nil
	}
	var newTitle, newSource any
	if title != "" {
		cleared, ok, err := t.resolveTitleConflict(ctx, conn, sessionID, title)
		if err != nil {
			return nil, err
		}
		if ok {
			affected = append(affected, cleared)
		}
		newTitle, newSource = title, source
	}
	result, err := conn.ExecContext(ctx,
		"UPDATE sessions SET title = ?, title_source = ? WHERE id = ? AND title IS ? AND title_source IS ?",
		newTitle, newSource, sessionID, currentTitle, currentSource)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 0 {
		affected = append(affected, sessionID)
	}
	return affected, nil
}

// Writes a title under provenance precedence as one compare and swap transaction, reporting whether any row changed
func (t *sessionTitles) setSessionTitle(ctx context.Context, sessionID, title, source string) (bool, error) {
	var affected []string
	err := t.host.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		affected, err = t.setSessionTitleInTransaction(ctx, conn, sessionID, title, source)
		return err
	}, nil)
	return len(affected) > 0, err
}

// Sets a user provenance title, empty clearing it, ValueError on conflict or validation failure
func (t *sessionTitles) SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error) {
	return t.setSessionTitle(ctx, sessionID, title, TitleSourceUser)
}

// Sets a derived or llm title, false when a higher authority title holds the row
func (t *sessionTitles) SetAutoTitle(ctx context.Context, sessionID, title, source string) (bool, error) {
	if source != TitleSourceDerived && source != TitleSourceLLM {
		return false, &pyint.Error{Kind: "ValueError", Msg: "invalid automatic title source: " + pyrepr.StrRepr(source)}
	}
	return t.setSessionTitle(ctx, sessionID, title, source)
}

// Provenance checked automatic title write of the titles port, reachable although the DB level setter shadows it
func (t *sessionTitles) SetAutoTitleContext(ctx context.Context, sessionID, title, source string) (bool, error) {
	return t.SetAutoTitle(ctx, sessionID, title, source)
}

// Text of a nullable column value, false for NULL
func optionalText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	default:
		return pyStr(x), true
	}
}

// Title of a session, false when the session is missing or untitled
func (t *sessionTitles) GetSessionTitle(ctx context.Context, sessionID string) (string, bool, error) {
	row, err := t.host.readOne(ctx, "SELECT title FROM sessions WHERE id = ?", sessionID)
	if err != nil || row == nil {
		return "", false, err
	}
	title, _ := row.Get("title")
	text, ok := optionalText(title)
	return text, ok, nil
}

// Provenance of a session title, false when the session is missing or untitled
func (t *sessionTitles) GetSessionTitleSource(ctx context.Context, sessionID string) (string, bool, error) {
	row, err := t.host.readOne(ctx, "SELECT title, title_source FROM sessions WHERE id = ?", sessionID)
	if err != nil || row == nil {
		return "", false, err
	}
	if title, _ := row.Get("title"); title == nil {
		return "", false, nil
	}
	source, _ := row.Get("title_source")
	text, ok := optionalText(source)
	return text, ok, nil
}

// Overwrites the provenance of a titled session without touching its text
func (t *sessionTitles) SetSessionTitleSource(ctx context.Context, sessionID, source string) (bool, error) {
	if _, ok := titleSourceRank[source]; !ok {
		return false, &pyint.Error{Kind: "ValueError", Msg: "invalid title source: " + pyrepr.StrRepr(source)}
	}
	n, err := t.host.writeRowcount(ctx, "UPDATE sessions SET title_source = ? WHERE id = ? AND title IS NOT NULL", []any{source, sessionID}, nil)
	return n > 0, err
}

// Session dict of the exact title holder with its resolved system prompt, nil when none holds it
func (t *sessionTitles) GetSessionByTitle(ctx context.Context, title string) (*pyjson.Object, error) {
	row, err := t.host.readOne(ctx,
		"SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved "+
			"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
			"WHERE s.title = ?", title)
	if err != nil || row == nil {
		return nil, err
	}
	return t.host.sessionRowDict(row), nil
}

// Session id a title names, preferring the latest numbered continuation except for the canonical Bot Chat
func (t *sessionTitles) ResolveSessionByTitle(ctx context.Context, title string) (string, bool, error) {
	exact, err := t.GetSessionByTitle(ctx, title)
	if err != nil {
		return "", false, err
	}
	exactID := func() (string, bool, error) {
		if exact == nil {
			return "", false, nil
		}
		id, _ := exact.Get("id")
		text, ok := optionalText(id)
		return text, ok, nil
	}
	if exact != nil && title == CanonicalBotChatTitle {
		return exactID()
	}
	numbered, err := t.host.readAll(ctx,
		"SELECT id, title, started_at FROM sessions "+
			"WHERE title LIKE ? ESCAPE '\\' ORDER BY started_at DESC", EscapeLike(title)+" #%")
	if err != nil {
		return "", false, err
	}
	if len(numbered) > 0 {
		id, _ := numbered[0].Get("id")
		text, ok := optionalText(id)
		return text, ok, nil
	}
	return exactID()
}

// Next title in a lineage read through the IOERR retrying reader
func (t *sessionTitles) GetNextTitleInLineage(ctx context.Context, baseTitle string) (string, error) {
	var next string
	err := t.host.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		var err error
		next, err = nextTitleInLineage(ctx, conn, baseTitle)
		return err
	})
	return next, err
}
