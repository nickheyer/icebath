package state

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Role of the export rows that show a shell line, never a role of a stored message
const shellLineRole = "shell"

// File of the shell lines a person ran in a session, beside its other files in a sessions folder
func ShellLinesPath(sessionsDir, sessionID string) string {
	return filepath.Join(sessionsDir, sessionID+".shell.jsonl")
}

// Sessions folder of the profile home this store belongs to
func (d *SessionDB) sessionsDir() string { return home.SessionsDir(filepath.Dir(d.Path())) }

// Time a shell line ran, false when it carries none
func shellLineAt(line *pyjson.Object) (float64, bool) {
	switch at := rowValue(line, "at").(type) {
	case float64:
		return at, true
	case int64:
		return float64(at), true
	}
	return 0, false
}

// Shell lines kept for a session in the order they ran, each with its time, command and result, none without a file
func ReadShellLines(sessionsDir, sessionID string) ([]*pyjson.Object, error) {
	data, err := os.ReadFile(ShellLinesPath(sessionsDir, sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []*pyjson.Object
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		decoded, err := pyjson.Loads(string(raw))
		if err != nil {
			return nil, err
		}
		line, isObject := decoded.(*pyjson.Object)
		if !isObject {
			return nil, pyValueError("a shell line of session " + sessionID + " is not an object")
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// Copies the shell lines of a session that ran before a time to another session, every line without a time bound
func CopyShellLines(sessionsDir, fromID, toID string, before *float64) error {
	data, err := os.ReadFile(ShellLinesPath(sessionsDir, fromID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept [][]byte
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		if before != nil {
			decoded, err := pyjson.Loads(string(raw))
			if err != nil {
				return err
			}
			line, _ := decoded.(*pyjson.Object)
			if at, timed := shellLineAt(line); !timed || at >= *before {
				continue
			}
		}
		kept = append(kept, raw)
	}
	if len(kept) == 0 {
		return nil
	}
	path := ShellLinesPath(sessionsDir, toID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(bytes.Join(kept, []byte("\n")), '\n'), 0o600)
}

// Time of the first message a branch leaves behind in its parent, nil when the branch copies every message
func branchLeftBehindAt(ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) (*float64, error) {
	_, target, err := savedLocalPolicyTarget(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	through, _ := payload.Get("through_message_id")
	boundary, err := branchBoundary(ctx, conn, target, through)
	if err != nil {
		return nil, err
	}
	first, found, err := fetchScalar(ctx, conn, "SELECT MIN(timestamp) FROM messages WHERE session_id=? AND active=1 AND id>?", target, boundary)
	if err != nil || !found {
		return nil, err
	}
	switch at := first.(type) {
	case float64:
		return &at, nil
	case int64:
		left := float64(at)
		return &left, nil
	}
	return nil, nil
}

// Whether a sessions folder keeps shell lines under the id of a session row or of the conversation it belongs to
func shellLinesKept(sessionsDir string, session *pyjson.Object) (bool, error) {
	ids := []string{pyStr(rowValue(session, "id"))}
	if owner, isText := rowValue(session, "chat_id").(string); isText && owner != "" && owner != ids[0] {
		ids = append(ids, owner)
	}
	for _, id := range ids {
		_, err := os.Stat(ShellLinesPath(sessionsDir, id))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// Shell lines an export of one session row carries: those of its conversation that ran while the row was the conversation's current one
func (d *SessionDB) exportShellLines(ctx context.Context, session *pyjson.Object) ([]any, error) {
	if kept, err := shellLinesKept(d.sessionsDir(), session); err != nil || !kept {
		return nil, err
	}
	sessionID := pyStr(rowValue(session, "id"))
	var ids []string
	starts := map[string]float64{}
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		if ids, err = localConversationIDs(ctx, conn, sessionID); err != nil {
			return err
		}
		rows, err := queryObjects(ctx, conn, "SELECT id, started_at FROM sessions WHERE id IN ("+placeholders(len(ids))+")", stringsToAny(ids)...)
		if err != nil {
			return err
		}
		for _, row := range rows {
			switch at := rowValue(row, "started_at").(type) {
			case float64:
				starts[pyStr(rowValue(row, "id"))] = at
			case int64:
				starts[pyStr(rowValue(row, "id"))] = float64(at)
			}
		}
		return nil
	})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	lines, err := ReadShellLines(d.sessionsDir(), ids[0])
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	from, until := math.Inf(-1), math.Inf(1)
	if own, known := starts[sessionID]; known {
		earliest := true
		for id, at := range starts {
			if id == sessionID {
				continue
			}
			if at < own || (at == own && id < sessionID) {
				earliest = false
			} else if at < until {
				until = at
			}
		}
		if !earliest {
			from = own
		}
	}
	out := []any{}
	for _, line := range lines {
		if at, timed := shellLineAt(line); !timed || (at >= from && at < until) {
			out = append(out, line)
		}
	}
	return out, nil
}

// Puts the shell lines an export of a session row carries on the exported session
func (d *SessionDB) attachShellLines(ctx context.Context, session, exported *pyjson.Object) error {
	shell, err := d.exportShellLines(ctx, session)
	if err != nil {
		return err
	}
	if len(shell) > 0 {
		exported.Set("shell_lines", shell)
	}
	return nil
}

// Shell lines of an exported session as rows
func exportShellLineRows(session *pyjson.Object) []*pyjson.Object {
	list, _ := rowValue(session, "shell_lines").([]any)
	return objectsOf(list)
}

// Text an export shows for a shell line: the command, what it printed and its exit code when it failed
func shellLineText(line *pyjson.Object) string {
	result, _ := rowValue(line, "result").(*pyjson.Object)
	parts := []string{"$ " + pyrepr.Str(pyrepr.Or(rowValue(line, "command"), ""))}
	for _, key := range []string{"stdout", "stderr"} {
		if text := strings.TrimRight(pyrepr.Str(pyrepr.Or(rowValue(result, key), "")), "\n"); text != "" {
			parts = append(parts, text)
		}
	}
	if code, isInt := rowValue(result, "code").(int64); isInt && code != 0 {
		parts = append(parts, "[exit "+strconv.FormatInt(code, 10)+"]")
	}
	return strings.Join(parts, "\n")
}

// Rows an export renders, the messages of the session with its shell lines placed among them by time
func exportDisplayRows(session *pyjson.Object) []*pyjson.Object {
	messages := exportMessageRows(session)
	lines := exportShellLineRows(session)
	if len(lines) == 0 {
		return messages
	}
	slices.SortStableFunc(lines, func(a, b *pyjson.Object) int {
		x, _ := shellLineAt(a)
		y, _ := shellLineAt(b)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
	out := make([]*pyjson.Object, 0, len(messages)+len(lines))
	next := 0
	place := func(before float64) {
		for ; next < len(lines); next++ {
			at, _ := shellLineAt(lines[next])
			if at >= before {
				return
			}
			out = append(out, pyjson.ObjectOf("role", shellLineRole, "content", shellLineText(lines[next]), "timestamp", at))
		}
	}
	for _, message := range messages {
		switch at := rowValue(message, "timestamp").(type) {
		case float64:
			place(at)
		case int64:
			place(float64(at))
		}
		out = append(out, message)
	}
	place(math.Inf(1))
	return out
}
