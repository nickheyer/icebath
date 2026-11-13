package buzz

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	seenCap             = 500
	cursorStateSubdir   = "buzz"
	cursorStateFilename = "channel-cursors.json"
)

// Insertion ordered string keyed map with oldest first eviction
type orderedMap[V any] struct {
	keys []string
	vals map[string]V
}

func newOrderedMap[V any]() *orderedMap[V] { return &orderedMap[V]{vals: map[string]V{}} }

func (m *orderedMap[V]) get(k string) (V, bool) {
	v, ok := m.vals[k]
	return v, ok
}

func (m *orderedMap[V]) has(k string) bool {
	_, ok := m.vals[k]
	return ok
}

// Stores the value keeping the key's position when it already exists
func (m *orderedMap[V]) set(k string, v V) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// Stores the value and makes the key the newest
func (m *orderedMap[V]) put(k string, v V) {
	m.remove(k)
	m.set(k, v)
}

func (m *orderedMap[V]) remove(k string) {
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	for i, key := range m.keys {
		if key == k {
			m.keys = append(m.keys[:i:i], m.keys[i+1:]...)
			return
		}
	}
}

func (m *orderedMap[V]) len() int { return len(m.keys) }

// Drops the oldest keys until at most limit remain
func (m *orderedMap[V]) trim(limit int) {
	if extra := len(m.keys) - limit; extra > 0 {
		for _, k := range m.keys[:extra] {
			delete(m.vals, k)
		}
		m.keys = append([]string(nil), m.keys[extra:]...)
	}
}

// Newest key, false when empty
func (m *orderedMap[V]) last() (string, bool) {
	if len(m.keys) == 0 {
		return "", false
	}
	return m.keys[len(m.keys)-1], true
}

// Author and clipped content of one event kept for reply parent lookup
type eventMeta struct {
	author, snippet string
}

// Receive cursor, de-dupe window and reply parent cache of one watched conversation
type channelState struct {
	chatType  string
	lastTS    int64
	seen      *orderedMap[struct{}]
	eventMeta *orderedMap[eventMeta]
}

func newChannelState(chatType string) *channelState {
	return &channelState{chatType: chatType, seen: newOrderedMap[struct{}](), eventMeta: newOrderedMap[eventMeta]()}
}

// Bounds the de-dupe window and the reply parent cache
func (st *channelState) trim() {
	st.seen.trim(seenCap)
	st.eventMeta.trim(seenCap)
}

// Cheap change detector of one channel cursor
type cursorMark struct {
	lastTS int64
	count  int
	last   string
}

func (st *channelState) mark() cursorMark {
	last, _ := st.seen.last()
	return cursorMark{st.lastTS, st.seen.len(), last}
}

// Persisted cursor of one channel
type savedCursor struct {
	ChatType string   `json:"chat_type"`
	LastTS   int64    `json:"last_ts"`
	Seen     []string `json:"seen"`
}

// Cursor file contents, bound to one identity on one relay
type cursorFile struct {
	Identity string                 `json:"identity"`
	Relay    string                 `json:"relay"`
	Channels map[string]savedCursor `json:"channels"`
}

// Cursor file under the active icebath home
func cursorPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), cursorStateSubdir, cursorStateFilename)
}

// Cursors saved for this identity and relay, unreadable files and malformed entries skipped
func loadCursors(ctx context.Context, identity, relay string) map[string]savedCursor {
	out := map[string]savedCursor{}
	raw, err := os.ReadFile(cursorPath(ctx))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("Buzz: could not read channel cursors", "error", err)
		}
		return out
	}
	decoded, err := decodeJSON(bytes.TrimPrefix(raw, []byte("﻿")))
	if err != nil {
		slog.Debug("Buzz: could not read channel cursors", "error", err)
		return out
	}
	data, ok := decoded.(map[string]any)
	if !ok || data["identity"] != identity || data["relay"] != relay {
		return out
	}
	channels, ok := data["channels"].(map[string]any)
	if !ok {
		return out
	}
	for channelID, rawEntry := range channels {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		var lastTS int64
		if truthy(entry["last_ts"]) {
			n, ok := intOf(entry["last_ts"])
			if !ok {
				continue
			}
			lastTS = n
		}
		seen := []string{}
		if list, ok := entry["seen"].([]any); ok {
			for _, id := range list {
				seen = append(seen, textOf(id))
			}
			if len(seen) > seenCap {
				seen = seen[len(seen)-seenCap:]
			}
		}
		chatType := ""
		if truthy(entry["chat_type"]) {
			chatType = textOf(entry["chat_type"])
		}
		out[channelID] = savedCursor{ChatType: chatType, LastTS: lastTS, Seen: seen}
	}
	return out
}

// Writes the cursor file through a synced temp file and rename, failures logged at debug
func writeCursors(path string, payload cursorFile) {
	if err := writeFileAtomic(path, payload); err != nil {
		slog.Debug("Buzz: could not persist channel cursors", "error", err)
	}
}

// JSON of the payload replacing the file at path only once fully written and synced
func writeFileAtomic(path string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+cursorStateFilename+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
