package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var deadErrorKinds = map[string]bool{"forbidden": true, "not_found": true}

func normalizeDeadKey(platform, chatID string) string {
	return strings.ToLower(strings.TrimSpace(platform)) + ":" + strings.TrimSpace(chatID)
}

// Dead target kind from a raised error text, only whole chat deaths counting
func ClassifyDeadError(errorText string) string {
	if errorText == "" {
		return ""
	}
	kind := ClassifySendError(nil, errorText)
	if deadErrorKinds[kind] && (kind != "not_found" || IsChatLevelNotFound(nil, errorText)) {
		return kind
	}
	return ""
}

// DeadTargetRegistry is the persistent per profile set of delivery targets confirmed unreachable
type DeadTargetRegistry struct {
	mu   sync.Mutex
	dead *pyjson.Object
	path string
}

// Registry loaded from the path or the profile gateway dead_targets.json, starting empty on a bad file
func NewDeadTargetRegistry(ctx context.Context, path string) *DeadTargetRegistry {
	if path == "" {
		path = filepath.Join(home.Dir(ctx), "gateway", "dead_targets.json")
	}
	r := &DeadTargetRegistry{dead: pyjson.NewObject(), path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug(fmt.Sprintf("dead_targets: could not load %s (%s) — starting empty", path, pypath.OSErrorText(err)))
		}
		return r
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		slog.Debug(fmt.Sprintf("dead_targets: could not load %s (%v) — starting empty", path, err))
		return r
	}
	raw, err := pyjson.Loads(text)
	if err != nil {
		slog.Debug(fmt.Sprintf("dead_targets: could not load %s (%v) — starting empty", path, err))
		return r
	}
	if obj, ok := raw.(*pyjson.Object); ok {
		for _, k := range obj.Keys {
			if v, ok := obj.Vals[k].(*pyjson.Object); ok {
				r.dead.Set(k, v)
			}
		}
	}
	return r
}

// Writes the set through a temp file, keeping memory state when the disk refuses
func (r *DeadTargetRegistry) flushLocked() {
	text, err := pyjson.Dumps(r.dead, pyjson.Indent(2))
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(r.path), 0o755); err == nil {
			tmp := r.path + ".tmp"
			if err = os.WriteFile(tmp, []byte(text), 0o644); err == nil {
				err = os.Rename(tmp, r.path)
			}
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("dead_targets: could not persist %s (%v)", r.path, err))
	}
}

// True when the target was confirmed dead
func (r *DeadTargetRegistry) IsDead(platform, chatID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if chatID == "" {
		return false
	}
	_, ok := r.dead.Get(normalizeDeadKey(platform, chatID))
	return ok
}

// Records a confirmed dead target and reports whether it was newly added
func (r *DeadTargetRegistry) MarkDead(platform, chatID, reason string) bool {
	if chatID == "" {
		return false
	}
	key := normalizeDeadKey(platform, chatID)
	r.mu.Lock()
	_, existed := r.dead.Get(key)
	entry := pyjson.NewObject()
	entry.Set("platform", strings.ToLower(strings.TrimSpace(platform)))
	entry.Set("chat_id", chatID)
	if rs := []rune(reason); len(rs) > 200 {
		reason = string(rs[:200])
	}
	entry.Set("reason", reason)
	entry.Set("marked_at", float64(time.Now().UnixNano())/1e9)
	r.dead.Set(key, entry)
	r.flushLocked()
	r.mu.Unlock()
	if !existed {
		why := reason
		if why == "" {
			why = "no reason given"
		}
		slog.Info(fmt.Sprintf("dead_targets: marked %s as unreachable (%s) — future deliveries to this target will be skipped until a send succeeds", key, why))
	}
	return !existed
}

// Removes a target's dead flag after a successful send and reports whether it was set
func (r *DeadTargetRegistry) Clear(platform, chatID string) bool {
	if chatID == "" {
		return false
	}
	key := normalizeDeadKey(platform, chatID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dead.Delete(key) {
		return false
	}
	r.flushLocked()
	slog.Info(fmt.Sprintf("dead_targets: cleared %s (delivery succeeded again)", key))
	return true
}
