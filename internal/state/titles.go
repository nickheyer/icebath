package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Characters a session title may hold
const MaxTitleLength = 100

// Title text rejected for its length
type TitleTooLongError struct{ Length int }

// Length and the maximum
func (e *TitleTooLongError) Error() string {
	return fmt.Sprintf("Title too long (%d chars, max %d)", e.Length, MaxTitleLength)
}

// Title another session legitimately holds
type TitleConflictError struct {
	Title    string
	HolderID string
}

// Title and its holder
func (e *TitleConflictError) Error() string {
	return fmt.Sprintf("Title '%s' is already in use by session %s", e.Title, e.HolderID)
}

// Refusal of a user rename of the hidden canonical Bot Chat, whose name is its identity
var ErrCanonicalBotChatRename = errors.New("This is the bot's canonical Bot Chat — its name is its identity, and renaming it " +
	"would orphan the conversation. To start fresh, create a new bot instead.")

// Error of a title source outside the provenance set
var ErrInvalidTitleSource = errors.New("invalid title source")

// Control, zero width, bidi override, object replacement and interlinear annotation characters a title drops
var (
	titleControl   = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	titleInvisible = regexp.MustCompile(`[\x{200b}-\x{200f}\x{2028}-\x{202e}\x{2060}-\x{2069}\x{feff}\x{fffc}\x{fff9}-\x{fffb}]`)
)

// Text with every lone surrogate encoding and invalid byte replaced by the replacement character
func replaceSurrogates(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size <= 1 {
			if i+2 < len(text) && text[i] == 0xED && text[i+1] >= 0xA0 && text[i+1] <= 0xBF && text[i+2] >= 0x80 && text[i+2] <= 0xBF {
				size = 3
			} else {
				size = 1
			}
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Title with invisible characters stripped and whitespace collapsed, false when nothing remains
func SanitizeTitle(title string) (string, bool, error) {
	if title == "" {
		return "", false, nil
	}
	cleaned := titleInvisible.ReplaceAllString(titleControl.ReplaceAllString(replaceSurrogates(title), ""), "")
	cleaned = strings.Join(previewFields(cleaned), " ")
	if cleaned == "" {
		return "", false, nil
	}
	if n := utf8.RuneCountInString(cleaned); n > MaxTitleLength {
		return "", false, &TitleTooLongError{Length: n}
	}
	return cleaned, true, nil
}

// Rank of a provenance being written, user above llm above derived
func sourceRank(source string) int { return titleRank(sql.NullString{String: source, Valid: true}) }

// Writes a title under provenance precedence in one compare and swap transaction, true when the row changed
func (s *Store) setSessionTitle(ctx context.Context, sessionID, title, source string) (bool, error) {
	var affected []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		affected, err = s.SetSessionTitleIn(ctx, c, sessionID, title, source)
		return err
	})
	return len(affected) > 0, err
}

// Title rules inside an authority owned transaction, returning the ids whose title changed
func (s *Store) SetSessionTitleIn(ctx context.Context, c Conn, sessionID, title, source string) ([]string, error) {
	clean, has, err := SanitizeTitle(title)
	if err != nil {
		return nil, err
	}
	isUser := source == TitleSourceUser
	var curTitle, curSource sql.NullString
	var hidden sql.NullInt64
	err = c.QueryRowContext(ctx, "SELECT title, title_source, hidden FROM sessions WHERE id = ?", sessionID).Scan(&curTitle, &curSource, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if curTitle.String == CanonicalBotChatTitle && hidden.Int64 != 0 && clean != CanonicalBotChatTitle {
		if isUser {
			return nil, ErrCanonicalBotChatRename
		}
		return nil, nil
	}
	if !isUser && curTitle.Valid && titleRank(curSource) >= sourceRank(source) {
		return nil, nil
	}
	var affected []string
	if has {
		cleared, err := resolveTitleConflict(ctx, c, sessionID, clean)
		if err != nil {
			return nil, err
		}
		if cleared != "" {
			affected = append(affected, cleared)
		}
	}
	newTitle, newSource := any(nil), any(nil)
	if has {
		newTitle, newSource = clean, source
	}
	res, err := c.ExecContext(ctx, "UPDATE sessions SET title = ?, title_source = ? WHERE id = ? AND title IS ? AND title_source IS ?",
		newTitle, newSource, sessionID, curTitle, curSource)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n > 0 {
		affected = append(affected, sessionID)
	}
	return affected, nil
}

// Rows the empty sweep and its count treat as ended abandoned shells
const emptySessionWhere = "message_count = 0 AND ended_at IS NOT NULL AND archived = 0 AND NOT EXISTS (" +
	"SELECT 1 FROM messages WHERE messages.session_id = sessions.id)"

// Frees title for the session in the caller's transaction, returning the id it cleared or a conflict error
func resolveTitleConflict(ctx context.Context, c Conn, sessionID, title string) (string, error) {
	var id string
	var archived, hidden, ghost sql.NullInt64
	err := c.QueryRowContext(ctx, "SELECT id, archived, hidden, "+emptySessionWhere+" AS ghost "+
		"FROM sessions WHERE title = ? AND id != ?", title, sessionID).Scan(&id, &archived, &hidden, &ghost)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	ancestor, err := isCompressionAncestor(ctx, c, id, sessionID)
	if err != nil {
		return "", err
	}
	if ancestor {
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET title = NULL WHERE id = ?", id); err != nil {
			return "", err
		}
		return id, nil
	}
	if (title == CanonicalBotChatTitle && archived.Int64 != 0 && hidden.Int64 != 0) || (ghost.Int64 != 0 && hidden.Int64 == 0) {
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET title = NULL, title_source = NULL WHERE id = ?", id); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", &TitleConflictError{Title: title, HolderID: id}
}

// Exported conflict resolution for a create that names its title inside an authority transaction
func ResolveTitleConflictIn(ctx context.Context, c Conn, sessionID, title string) (string, error) {
	return resolveTitleConflict(ctx, c, sessionID, title)
}

// Reports whether ancestorID is a compression predecessor of descendantID over the canonical continuation edge
func isCompressionAncestor(ctx context.Context, c Conn, ancestorID, descendantID string) (bool, error) {
	if ancestorID == "" || descendantID == "" || ancestorID == descendantID {
		return false, nil
	}
	edge := compressionChildSQL("child") + nonContinuationChildSQL("child.", "parent.id")
	var one int
	err := c.QueryRowContext(ctx, `
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
            `, descendantID, ancestorID, descendantID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Sets a title on the user's behalf, an empty title clearing it
func (s *Store) SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error) {
	return s.setSessionTitle(ctx, sessionID, title, TitleSourceUser)
}

// Sets an automatic title, false when a higher authority title holds the row
func (s *Store) SetAutoTitle(ctx context.Context, sessionID, title, source string) (bool, error) {
	if source != TitleSourceDerived && source != TitleSourceLLM {
		return false, fmt.Errorf("%w: invalid automatic title source: %q", ErrInvalidTitleSource, source)
	}
	return s.setSessionTitle(ctx, sessionID, title, source)
}

// Title of a session, false when untitled or absent
func (s *Store) GetSessionTitle(ctx context.Context, sessionID string) (string, bool, error) {
	var title sql.NullString
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT title FROM sessions WHERE id = ?", sessionID).Scan(&title)
		if errors.Is(err, sql.ErrNoRows) {
			title = sql.NullString{}
			return nil
		}
		return err
	})
	return title.String, title.Valid, err
}

// Provenance of a session's title, false when untitled
func (s *Store) GetSessionTitleSource(ctx context.Context, sessionID string) (string, bool, error) {
	var title, source sql.NullString
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT title, title_source FROM sessions WHERE id = ?", sessionID).Scan(&title, &source)
		if errors.Is(err, sql.ErrNoRows) {
			title, source = sql.NullString{}, sql.NullString{}
			return nil
		}
		return err
	})
	if !title.Valid {
		return "", false, err
	}
	return source.String, source.Valid, err
}

// Overwrites a title's provenance without touching its text, true when a titled row changed
func (s *Store) SetSessionTitleSource(ctx context.Context, sessionID, source string) (bool, error) {
	if source != TitleSourceDerived && source != TitleSourceLLM && source != TitleSourceUser {
		return false, fmt.Errorf("%w: %q", ErrInvalidTitleSource, source)
	}
	n, err := s.WriteRowcount(ctx, WritePatience, "UPDATE sessions SET title_source = ? WHERE id = ? AND title IS NOT NULL", source, sessionID)
	return n > 0, err
}

// Session with exactly this title, false when none
func (s *Store) GetSessionByTitle(ctx context.Context, title string) (Session, bool, error) {
	var found []Session
	err := s.Read(ctx, func(c Conn) error {
		var err error
		found, err = querySessions(c.QueryContext(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved "+
			"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
			"WHERE s.title = ?", title))
		return err
	})
	if err != nil || len(found) == 0 {
		return Session{}, false, err
	}
	return found[0], true, nil
}

// Session a title resolves to, the latest numbered continuation preferred except for the canonical Bot Chat
func (s *Store) ResolveSessionByTitle(ctx context.Context, title string) (string, bool, error) {
	exact, ok, err := s.GetSessionByTitle(ctx, title)
	if err != nil {
		return "", false, err
	}
	if ok && title == CanonicalBotChatTitle {
		return exact.ID, true, nil
	}
	var numbered []string
	err = s.Read(ctx, func(c Conn) error {
		var err error
		numbered, err = queryStrings(ctx, c, "SELECT id FROM sessions "+
			`WHERE title LIKE ? ESCAPE '\' ORDER BY started_at DESC`, EscapeLike(title)+" #%")
		return err
	})
	if err != nil {
		return "", false, err
	}
	if len(numbered) > 0 {
		return numbered[0], true, nil
	}
	return exact.ID, ok, nil
}

// Next title in a lineage, my session becoming my session #2
func (s *Store) GetNextTitleInLineage(ctx context.Context, baseTitle string) (string, error) {
	var next string
	err := s.Read(ctx, func(c Conn) error {
		var err error
		next, err = nextTitleInLineage(ctx, c, baseTitle)
		return err
	})
	return next, err
}

// Next lineage title inside an authority owned transaction, counting siblings that transaction committed
func LineageTitleIn(ctx context.Context, c Conn, baseTitle string) (string, error) {
	return nextTitleInLineage(ctx, c, baseTitle)
}
