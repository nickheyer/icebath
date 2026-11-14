package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type DebugLog struct {
	tool    string
	enabled bool
	id      string
	dir     string
	start   string
	mu      sync.Mutex
	calls   [][]Field
}

func NewDebugLog(tool, envVar, logsDir string) (*DebugLog, error) {
	d := &DebugLog{tool: tool, dir: logsDir, enabled: strings.ToLower(os.Getenv(envVar)) == "true"}
	if !d.enabled {
		return d, nil
	}
	id, err := newUUID()
	if err != nil {
		return nil, err
	}
	d.id = id
	d.start = isoNow()
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, err
	}
	slog.Debug(fmt.Sprintf("%s debug mode enabled - Session ID: %s", tool, d.id))
	return d, nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func isoNow() string {
	now := time.Now()
	text := now.Format("2006-01-02T15:04:05")
	if us := now.Nanosecond() / 1000; us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text
}

func (d *DebugLog) Active() bool {
	return d != nil && d.enabled
}

func (d *DebugLog) Record(name string, data ...Field) {
	if !d.Active() {
		return
	}
	entry := []Field{{"timestamp", isoNow()}, {"tool_name", name}}
	for _, f := range data {
		if i := slices.IndexFunc(entry, func(e Field) bool { return e.Key == f.Key }); i >= 0 {
			entry[i].Value = f.Value
			continue
		}
		entry = append(entry, f)
	}
	d.mu.Lock()
	d.calls = append(d.calls, entry)
	d.mu.Unlock()
}

func objectOf(fields []Field) (json.RawMessage, error) {
	members := make([]wire.Member, 0, len(fields))
	for _, f := range fields {
		var v json.RawMessage
		switch t := f.Value.(type) {
		case json.RawMessage:
			v = t
		default:
			enc, err := fileutil.EncodeJSON(f.Value, fileutil.JSONStyle{Tight: true})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Key, err)
			}
			v = enc
		}
		members = append(members, wire.Member{Key: f.Key, Value: v})
	}
	return wire.Object(members), nil
}

func (d *DebugLog) Save(ctx context.Context) {
	if !d.Active() {
		return
	}
	d.mu.Lock()
	calls := slices.Clone(d.calls)
	d.mu.Unlock()
	if err := d.write(ctx, calls); err != nil {
		slog.Error(fmt.Sprintf("Error saving %s debug log: %v", d.tool, err))
	}
}

func (d *DebugLog) write(ctx context.Context, calls [][]Field) error {
	items := make([]json.RawMessage, 0, len(calls))
	for _, c := range calls {
		obj, err := objectOf(c)
		if err != nil {
			return err
		}
		items = append(items, obj)
	}
	payload, err := objectOf([]Field{
		{"session_id", d.id},
		{"start_time", d.start},
		{"end_time", isoNow()},
		{"debug_enabled", true},
		{"total_calls", len(calls)},
		{"tool_calls", wire.Array(items...)},
	})
	if err != nil {
		return err
	}
	path := filepath.Join(d.dir, d.tool+"_debug_"+d.id+".json")
	if err := fileutil.WriteJSON(ctx, path, payload, fileutil.JSONStyle{Indent: 2}); err != nil {
		return err
	}
	slog.Debug(fmt.Sprintf("%s debug log saved: %s", d.tool, path))
	return nil
}
