package state

import (
	"errors"
	"fmt"
)

// Portable copy of one session
type Export struct {
	Session  *Session   `json:"session"`
	Messages []*Message `json:"messages"`
}

// Session with its rows, the compacted history included on request, nil when absent
func (d *DB) ExportSession(sessionID string, includeCompacted, includeInactive bool) (*Export, error) {
	s, err := d.GetSession(sessionID)
	if err != nil || s == nil {
		return nil, err
	}
	msgs, err := d.fetchMessages([]string{sessionID}, activeClause(includeInactive, includeCompacted))
	if err != nil {
		return nil, err
	}
	return &Export{Session: s, Messages: msgs}, nil
}

// Every session of a source, or every session, with its live rows
func (d *DB) ExportAll(source string) ([]*Export, error) {
	sessions, err := d.ListSessions(ListFilter{Limit: 1 << 30, Source: source, IncludeArchived: true, IncludeHidden: true, IncludeChildren: true, IncludeEmpty: true})
	if err != nil {
		return nil, err
	}
	out := make([]*Export, 0, len(sessions))
	for _, s := range sessions {
		msgs, err := d.fetchMessages([]string{s.ID}, activeClause(false, false))
		if err != nil {
			return nil, err
		}
		out = append(out, &Export{Session: s, Messages: msgs})
	}
	return out, nil
}

// Imports exported sessions, skipping ids that already exist, returning how many landed
func (d *DB) ImportSessions(exports []*Export) (int, error) {
	imported := 0
	for _, e := range exports {
		if e == nil || e.Session == nil || e.Session.ID == "" {
			return imported, errors.New("export without a session id")
		}
		existing, err := d.GetSession(e.Session.ID)
		if err != nil {
			return imported, err
		}
		if existing != nil {
			continue
		}
		s := e.Session
		if err := d.CreateSession(NewSession{ID: s.ID, Source: s.Source, UserID: s.UserID, Model: s.Model, ModelConfig: s.ModelConfig,
			SystemPrompt: s.SystemPrompt, ParentSessionID: s.ParentSessionID, Cwd: s.Cwd, GitRepoRoot: s.GitRepoRoot, ProfileName: s.ProfileName}); err != nil {
			return imported, fmt.Errorf("import session %s: %w", s.ID, err)
		}
		err = d.transcriptWrite(func(tx *Tx) error {
			if _, err := tx.Exec("UPDATE sessions SET started_at = ?, ended_at = ?, end_reason = ?, title = NULL WHERE id = ?",
				s.StartedAt, nullFloat(s.EndedAt), nullStr(s.EndReason), s.ID); err != nil {
				return err
			}
			for _, m := range e.Messages {
				m.RowID, m.Persisted = 0, false
			}
			_, err := insertMessages(tx, s.ID, e.Messages)
			return err
		})
		if err != nil {
			return imported, fmt.Errorf("import messages of %s: %w", s.ID, err)
		}
		if s.Title != "" {
			if _, err := d.setTitle(s.ID, s.Title, titleSourceOr(s.TitleSource)); err != nil {
				var inUse *TitleInUseError
				if !errors.As(err, &inUse) {
					return imported, err
				}
			}
		}
		imported++
	}
	return imported, nil
}

// Provenance of an imported title, user when the export carried none
func titleSourceOr(source string) string {
	if _, ok := titleRank[source]; ok {
		return source
	}
	return TitleSourceUser
}

// Nullable float bound as NULL when zero
func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
