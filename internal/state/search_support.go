package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func DecodeContent(cell any) Content { return decodeContent(cell) }

func LenientScan(dst any) sql.Scanner { return lenient{dst: dst} }

func IsCallFailure(err error) bool { return familyOf(err) == callFamily }

func IsEngineFailure(err error) bool { return familyOf(err) != notSQLite }

func (s *Store) EnterFTSFailOpen(ctx context.Context, cause error) (bool, error) {
	return enterFTSFailOpen(ctx, s, cause, time.Now().Add(WritePatience), WritePatience)
}

func (s *Store) RefreshFTSStale(ctx context.Context) error {
	f := s.fts()
	if ftsGet(f, func(f *ftsFlags) bool { return f.stale || !f.enabled }) {
		return nil
	}
	stale := false
	err := s.Read(ctx, func(c Conn) error {
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key = ? LIMIT 1", ftsStaleKey).Scan(&one)
		stale = err == nil
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		if IsEngineFailure(err) {
			return nil
		}
		return err
	}
	if stale {
		f.set(func(f *ftsFlags) { f.stale, f.enabled, f.trigram, f.cjkAvailable = true, false, false, false })
	}
	return nil
}

type AnchoredView struct {
	Window         []MessageRow
	MessagesBefore int
	MessagesAfter  int
	BookendStart   []MessageRow
	BookendEnd     []MessageRow
}

func (s *Store) GetAnchoredView(ctx context.Context, sessionID string, aroundMessageID int64, window, bookend int, keepRoles []string) (AnchoredView, error) {
	bookend = max(bookend, 0)
	around, err := s.GetMessagesAround(ctx, sessionID, aroundMessageID, window)
	if err != nil {
		return AnchoredView{}, err
	}
	if len(around.Window) == 0 {
		return AnchoredView{}, nil
	}
	view := AnchoredView{Window: around.Window, MessagesBefore: around.MessagesBefore, MessagesAfter: around.MessagesAfter}
	if keepRoles != nil {
		view.Window = nil
		for _, m := range around.Window {
			if m.ID == aroundMessageID || containsString(keepRoles, m.Role) {
				view.Window = append(view.Window, m)
			}
		}
	}
	if bookend == 0 {
		return view, nil
	}
	roleClause := ""
	if keepRoles != nil {
		roleClause = " AND role IN (" + bindMarks(len(keepRoles)) + ")"
	}
	first, last := around.Window[0].ID, around.Window[len(around.Window)-1].ID
	err = s.Read(ctx, func(c Conn) error {
		read := func(op string, boundary int64, order string) ([]MessageRow, error) {
			args := append(append([]any{sessionID, boundary}, stringArgs(keepRoles)...), bookend)
			rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages "+
				"WHERE session_id = ? AND id "+op+" ?"+roleClause+" "+
				"AND length(content) > 0 "+
				"ORDER BY id "+order+" LIMIT ?", args...))
			if err != nil {
				return nil, err
			}
			return rowsToMessageRows(rows, "get_anchored_view"), nil
		}
		var err error
		if view.BookendStart, err = read("<", first, "ASC"); err != nil {
			return err
		}
		end, err := read(">", last, "DESC")
		if err != nil {
			return err
		}
		view.BookendEnd = make([]MessageRow, len(end))
		for i, m := range end {
			view.BookendEnd[len(end)-1-i] = m
		}
		return nil
	})
	if err != nil {
		return AnchoredView{}, err
	}
	return view, nil
}
