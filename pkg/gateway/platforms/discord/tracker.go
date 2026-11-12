package discord

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const nonConversationalMaxTracked = 2000

// Persistent bounded set of Discord message ids that are status noise
type nonConversationalTracker struct {
	mu         sync.Mutex
	persistMu  sync.Mutex
	maxTracked int
	order      []string
	ids        map[string]struct{}
	path       string
}

// Tracker loaded from the gateway state file under the active home
func newNonConversationalTracker(ctx context.Context, maxTracked int) *nonConversationalTracker {
	t := &nonConversationalTracker{maxTracked: maxTracked, ids: map[string]struct{}{},
		path: filepath.Join(home.Dir(ctx), discordCommandSyncStateSubdir, discordNonconversationalStateFilename)}
	for _, id := range t.load() {
		if _, ok := t.ids[id]; !ok {
			t.ids[id] = struct{}{}
			t.order = append(t.order, id)
		}
	}
	return t
}

// Ids persisted on disk, empty when the file is missing or unreadable
func (t *nonConversationalTracker) load() []string {
	raw, err := os.ReadFile(t.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug(fmt.Sprintf("[%s] Failed to load non-conversational Discord IDs", "Discord"))
		}
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err == nil {
		var data any
		if data, err = pyjson.Loads(text); err == nil {
			if items, ok := data.([]any); ok {
				out := []string{}
				for _, item := range items {
					if s := pyrepr.Str(item); pystr.Strip(s) != "" {
						out = append(out, s)
					}
				}
				return out
			}
			return nil
		}
	}
	slog.Debug(fmt.Sprintf("[%s] Failed to load non-conversational Discord IDs", "Discord"))
	return nil
}

// Trims the in memory set to the newest ids and returns the ids to persist
func (t *nonConversationalTracker) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.order) > t.maxTracked {
		t.order = append([]string(nil), t.order[len(t.order)-t.maxTracked:]...)
		t.ids = make(map[string]struct{}, len(t.order))
		for _, id := range t.order {
			t.ids[id] = struct{}{}
		}
	}
	return append([]string(nil), t.order...)
}

// Writes the ids as a compact JSON list, logging a failure
func (t *nonConversationalTracker) save(ctx context.Context, ids []string) {
	if err := config.AtomicJSONWriteWith(ctx, t.path, ids); err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to save non-conversational Discord IDs", "Discord"), "err", err)
	}
}

// Adds ids and persists the set in mutation order when any id was new
func (t *nonConversationalTracker) markMany(ctx context.Context, messageIDs []string) {
	changed := false
	t.mu.Lock()
	for _, messageID := range messageIDs {
		key := pystr.Strip(messageID)
		if _, seen := t.ids[key]; key != "" && !seen {
			t.ids[key] = struct{}{}
			t.order = append(t.order, key)
			changed = true
		}
	}
	t.mu.Unlock()
	if changed {
		t.persistMu.Lock()
		defer t.persistMu.Unlock()
		t.save(ctx, t.snapshot())
	}
}

// Whether a message id is tracked as status noise
func (t *nonConversationalTracker) contains(messageID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.ids[messageID]
	return ok
}
