package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

var titleRank = map[string]int{TitleSourceDerived: 1, TitleSourceLLM: 2, TitleSourceUser: 3}

// Error of a title another session holds
type TitleInUseError struct {
	Title     string
	SessionID string
}

func (e *TitleInUseError) Error() string {
	return "title " + strconv.Quote(e.Title) + " is already in use by session " + e.SessionID
}

// Rank of a stored provenance, legacy rows without one ranking as user titles
func rankOf(source sql.NullString) int {
	if !source.Valid {
		return titleRank[TitleSourceUser]
	}
	return titleRank[source.String]
}

// Writes a title under a provenance, lower authority never replacing higher
func (d *DB) setTitle(sessionID, title, source string) (bool, error) {
	cleaned, err := SanitizeTitle(title)
	if err != nil {
		return false, err
	}
	changed := false
	err = d.write(func(tx *Tx) error {
		var current, currentSource sql.NullString
		err := tx.QueryRow("SELECT title, title_source FROM sessions WHERE id = ?", sessionID).Scan(&current, &currentSource)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if source != TitleSourceUser && current.Valid && rankOf(currentSource) >= titleRank[source] {
			return nil
		}
		if cleaned != "" {
			if err := resolveTitleConflict(tx, sessionID, cleaned); err != nil {
				return err
			}
		}
		var newSource any
		if cleaned != "" {
			newSource = source
		}
		res, err := tx.Exec("UPDATE sessions SET title = ?, title_source = ? WHERE id = ?", nullStr(cleaned), newSource, sessionID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed = n > 0
		return nil
	})
	return changed, err
}

// Frees a title held by a compression ancestor or an empty ended ghost, failing on a live holder
func resolveTitleConflict(tx *Tx, sessionID, title string) error {
	var holder string
	var ended sql.NullFloat64
	var count int
	err := tx.QueryRow("SELECT id, ended_at, message_count FROM sessions WHERE title = ? AND id != ?", title, sessionID).Scan(&holder, &ended, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ancestor, err := isCompressionAncestor(tx, holder, sessionID)
	if err != nil {
		return err
	}
	if ancestor || (ended.Valid && count == 0) {
		_, err := tx.Exec("UPDATE sessions SET title = NULL, title_source = NULL WHERE id = ?", holder)
		return err
	}
	return &TitleInUseError{Title: title, SessionID: holder}
}

// Reports whether ancestor precedes descendant along compression continuations
func isCompressionAncestor(tx *Tx, ancestor, descendant string) (bool, error) {
	cur := descendant
	seen := map[string]bool{}
	for !seen[cur] {
		seen[cur] = true
		var parent string
		err := tx.QueryRow(`SELECT parent.id FROM sessions child JOIN sessions parent ON parent.id = child.parent_session_id
			WHERE child.id = ? AND parent.end_reason = 'compression'`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if parent == ancestor {
			return true, nil
		}
		cur = parent
	}
	return false, nil
}

// Sets a title on behalf of the person, an empty title clearing it
func (d *DB) SetTitle(sessionID, title string) (bool, error) {
	return d.setTitle(sessionID, title, TitleSourceUser)
}

// Sets an automatic title, untouched when a higher authority title holds the row
func (d *DB) SetAutoTitle(sessionID, title, source string) (bool, error) {
	if source != TitleSourceDerived && source != TitleSourceLLM {
		return false, fmt.Errorf("invalid automatic title source %q", source)
	}
	return d.setTitle(sessionID, title, source)
}

// Title of a session and its provenance
func (d *DB) Title(sessionID string) (string, string, error) {
	var title, source sql.NullString
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT title, title_source FROM sessions WHERE id = ?", sessionID).Scan(&title, &source)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return title.String, source.String, err
}

// Session id a title names, the newest numbered continuation preferred
func (d *DB) ResolveByTitle(title string) (string, error) {
	var exact string
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT id FROM sessions WHERE title = ?", title).Scan(&exact)
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var numbered string
	err = d.read(func(q querier) error {
		return q.QueryRow("SELECT id FROM sessions WHERE title LIKE ? ESCAPE '\\' ORDER BY started_at DESC LIMIT 1", EscapeLike(title)+" #%").Scan(&numbered)
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if numbered != "" {
		return numbered, nil
	}
	return exact, nil
}
