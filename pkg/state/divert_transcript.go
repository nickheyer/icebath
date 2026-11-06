package state

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Appends pending messages to sessions/<id>.jsonl under the home, returning the path or empty when nothing was written
func DivertSessionTranscriptJSONL(ctx context.Context, sessionID string, messages []any) (string, error) {
	sid := pystr.Strip(sessionID)
	if sid == "" || len(messages) == 0 {
		return "", nil
	}
	sessionsDir := filepath.Join(home.Dir(ctx), "sessions")
	if _, err := home.MakeUserDir(sessionsDir, 0o777, true, true); err != nil {
		return "", err
	}
	path := filepath.Join(sessionsDir, sid+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		record := msg
		switch msg.(type) {
		case *pyjson.Object, map[string]any:
		default:
			o := pyjson.NewObject()
			o.Set("content", pyrepr.Str(msg))
			record = o
		}
		line, err := pyjson.Dumps(record, pyjson.EnsureASCII(false), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
		if err != nil {
			return "", err
		}
		if _, err := sha256HexUTF8(line); err != nil {
			return "", err
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			return "", err
		}
	}
	return path, nil
}
