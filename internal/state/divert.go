package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

// Appends records the store refused to sessions/<id>.jsonl in the home, one JSON line each, empty when nothing was due
func (e *Engine) DivertTranscript(ctx context.Context, sessionID string, records []any) (string, error) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" || len(records) == 0 {
		return "", nil
	}
	h, err := e.home.Home(ctx)
	if err != nil {
		return "", err
	}
	dir := home.Sessions.In(h)
	if err := e.home.MakeUserDir(ctx, dir, 0o777, true); err != nil {
		return "", err
	}
	path := filepath.Join(dir, sid+".jsonl")
	lineEnd := "\n"
	if runtime.GOOS == "windows" {
		lineEnd = "\r\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	var encodeErr error
	for _, rec := range records {
		if rec == nil {
			continue
		}
		line, err := divertLine(rec)
		if err != nil {
			encodeErr = err
			break
		}
		buf.Write(line)
		buf.WriteString(lineEnd)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return "", errors.Join(err, f.Close())
	}
	if err := errors.Join(encodeErr, f.Close()); err != nil {
		return "", err
	}
	return path, nil
}

// One record as raw UTF-8 JSON, anything that is not an object written as its text under content
func divertLine(rec any) ([]byte, error) {
	line, err := encodeRecord(rec)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] != '{' {
		return encodeRecord(map[string]string{"content": fmt.Sprint(rec)})
	}
	return line, nil
}

// Record in the default separators, raw UTF-8 and escapes only for text that is not valid UTF-8
func encodeRecord(v any) ([]byte, error) {
	line, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{})
	if errors.Is(err, fileutil.ErrJSONNotUTF8) {
		return fileutil.EncodeJSON(v, fileutil.JSONStyle{ASCII: true})
	}
	return line, err
}
