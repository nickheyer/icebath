package state

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Longest title a session may carry
const MaxTitleLength = 200

// Provenance of a title derived from the first message
const TitleSourceDerived = "derived"

// Provenance of a title a model wrote
const TitleSourceLLM = "llm"

// Provenance of a title the person typed
const TitleSourceUser = "user"

var titleRank = map[string]int{TitleSourceDerived: 1, TitleSourceLLM: 2, TitleSourceUser: 3}

var numberedTitleRe = regexp.MustCompile(`^(.*?) #(\d+)$`)

// Error of a title another session holds
type TitleInUseError struct {
	Title     string
	SessionID string
}

func (e *TitleInUseError) Error() string {
	return "title " + strconv.Quote(e.Title) + " is already in use by session " + e.SessionID
}

// Title without control, zero width and bidi characters and with whitespace collapsed
func SanitizeTitle(title string) (string, error) {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(' ')
		case unicode.IsControl(r):
		case r >= 0x200b && r <= 0x200f, r >= 0x2028 && r <= 0x202e, r >= 0x2060 && r <= 0x2069, r == 0xfeff, r == 0xfffc, r >= 0xfff9 && r <= 0xfffb:
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.Join(strings.Fields(b.String()), " ")
	if len([]rune(cleaned)) > MaxTitleLength {
		return "", fmt.Errorf("title too long (%d characters, max %d)", len([]rune(cleaned)), MaxTitleLength)
	}
	return cleaned, nil
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
		return q.QueryRow("SELECT id FROM sessions WHERE title LIKE ? ESCAPE '\\' ORDER BY started_at DESC LIMIT 1", escapeLike(title)+" #%").Scan(&numbered)
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if numbered != "" {
		return numbered, nil
	}
	return exact, nil
}

// Next free title in a lineage, the unnumbered original counting as number one
func (d *DB) NextTitleInLineage(base string) (string, error) {
	if m := numberedTitleRe.FindStringSubmatch(base); m != nil {
		base = m[1]
	}
	var titles []string
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT title FROM sessions WHERE title = ? OR title LIKE ? ESCAPE '\\'", base, escapeLike(base)+" #%")
		if err != nil {
			return err
		}
		defer rows.Close()
		titles = nil
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			titles = append(titles, t)
		}
		return rows.Err()
	})
	if err != nil {
		return "", err
	}
	if len(titles) == 0 {
		return base, nil
	}
	highest := 1
	for _, t := range titles {
		if m := numberedTitleRe.FindStringSubmatch(t); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil && n > highest {
				highest = n
			}
		}
	}
	return fmt.Sprintf("%s #%d", base, highest+1), nil
}
