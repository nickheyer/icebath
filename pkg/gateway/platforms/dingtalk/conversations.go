package dingtalk

import (
	"context"
	"encoding/json"
	"log/slog"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const maxConversations = 5000

// What the adapter learned about one conversation so replies reach it without a fresh inbound message
type conversation struct {
	Group   bool   `json:"group"`
	StaffID string `json:"staff_id,omitempty"`
	Title   string `json:"title,omitempty"`
	Seen    int64  `json:"seen"`
}

// Conversations of the profile persisted under its home so proactive sends survive restarts
type conversationBook struct {
	path string

	mu      sync.Mutex
	entries map[string]conversation
	writeMu sync.Mutex
}

// Book of the active profile home, loaded from disk when present
func loadConversationBook(ctx context.Context) *conversationBook {
	b := &conversationBook{path: filepath.Join(home.Dir(ctx), "platforms", "dingtalk", "conversations.json"),
		entries: map[string]conversation{}}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return b
	}
	if err := json.Unmarshal(raw, &b.entries); err != nil {
		slog.Warn(fmt.Sprintf("[Dingtalk] Ignoring unreadable conversation book %s: %v", b.path, err))
		b.entries = map[string]conversation{}
	}
	return b
}

// Known facts of a conversation
func (b *conversationBook) lookup(chatID string) (conversation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.entries[chatID]
	return c, ok
}

// Records a conversation, writing the book only when what is known about it changed
func (b *conversationBook) record(chatID string, c conversation) {
	if chatID == "" {
		return
	}
	b.mu.Lock()
	prev, ok := b.entries[chatID]
	if c.StaffID == "" {
		c.StaffID = prev.StaffID
	}
	c.Seen = time.Now().Unix()
	b.entries[chatID] = c
	changed := !ok || prev.Group != c.Group || prev.StaffID != c.StaffID || prev.Title != c.Title
	if changed && len(b.entries) > maxConversations {
		b.evictLocked()
	}
	b.mu.Unlock()
	if !changed {
		return
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.mu.Lock()
	snapshot, err := json.Marshal(b.entries)
	b.mu.Unlock()
	if err == nil {
		err = b.write(snapshot)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Dingtalk] Could not save conversation book %s: %v", b.path, err))
	}
}

// Drops the least recently seen conversations down to the cap
func (b *conversationBook) evictLocked() {
	ids := make([]string, 0, len(b.entries))
	for id := range b.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return b.entries[ids[i]].Seen < b.entries[ids[j]].Seen })
	for _, id := range ids[:len(ids)-maxConversations] {
		delete(b.entries, id)
	}
}

// Atomically replaces the book file with the snapshot
func (b *conversationBook) write(snapshot []byte) error {
	dir := filepath.Dir(b.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".conversations-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(snapshot); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), b.path)
}
