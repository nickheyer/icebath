package buzz

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	seenCap               = 500
	cursorStateSubdir     = "buzz"
	cursorStateFilename   = "channel-cursors.json"
	defaultCursorChatType = "group"
)

// Read position of one watched channel
type ChannelCursor struct {
	ChatType string
	LastTS   int64
	Seen     []string
}

// Change detector of one channel cursor, the newest seen id included when there is one
type CursorMark struct {
	LastTS  int64
	Count   int
	Last    string
	HasLast bool
}

// Per channel cursors persisted under the icebath home so a restart resumes where the reader stopped
type CursorStore struct {
	SelfPubkey      string
	RelayURL        string
	ChannelOrder    []string
	ChannelState    map[string]*ChannelCursor
	RestoredCursors map[string]ChannelCursor

	snapshotMu sync.Mutex
	snapshots  int
	writeMu    sync.Mutex
	written    int
}

// Records a watched channel keeping first watch order for the persisted payload
func (s *CursorStore) Watch(channelID string, cursor *ChannelCursor) {
	if s.ChannelState == nil {
		s.ChannelState = map[string]*ChannelCursor{}
	}
	if _, ok := s.ChannelState[channelID]; !ok {
		s.ChannelOrder = append(s.ChannelOrder, channelID)
	}
	s.ChannelState[channelID] = cursor
}

// Stops persisting a channel, dropping it from the watch order
func (s *CursorStore) Forget(channelID string) {
	if _, ok := s.ChannelState[channelID]; !ok {
		return
	}
	delete(s.ChannelState, channelID)
	for i, id := range s.ChannelOrder {
		if id == channelID {
			s.ChannelOrder = append(s.ChannelOrder[:i], s.ChannelOrder[i+1:]...)
			break
		}
	}
}

// Cursor file under the icebath home
func CursorPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), cursorStateSubdir, cursorStateFilename)
}

// Restores cursors written for this identity and relay, ignoring unreadable files and malformed entries
func (s *CursorStore) LoadCursors(ctx context.Context) error {
	s.RestoredCursors = map[string]ChannelCursor{}
	path := CursorPath(ctx)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var data any
	if err == nil {
		var text string
		if text, err = textutil.DecodeUTF8(raw); err == nil {
			data, err = jsonutil.Decode(text)
		}
	}
	if err != nil {
		pkgLog().Debug("Buzz: could not read channel cursors", "error", err)
		return nil
	}
	obj, ok := data.(*jsonutil.Object)
	if !ok {
		return nil
	}
	if identity, _ := obj.Get("identity"); identity != s.SelfPubkey {
		return nil
	}
	if relay, _ := obj.Get("relay"); relay != s.RelayURL {
		return nil
	}
	channelsRaw, _ := obj.Get("channels")
	channels, ok := channelsRaw.(*jsonutil.Object)
	if !ok {
		return nil
	}
	for _, channelID := range channels.Keys() {
		entry, ok := channels.Value(channelID).(*jsonutil.Object)
		if !ok {
			continue
		}
		var lastTS int64
		if v, _ := entry.Get("last_ts"); anyutil.IsTrue(v) {
			n, err := anyutil.ToInt(v)
			if errors.Is(err, anyutil.ErrRange) {
				return err
			}
			if err != nil {
				continue
			}
			lastTS = n
		}
		seen := []string{}
		if list, ok := entry.Value("seen").([]any); ok {
			for _, id := range list {
				seen = append(seen, anyutil.String(id))
			}
			if len(seen) > seenCap {
				seen = seen[len(seen)-seenCap:]
			}
		}
		chatType := ""
		if v, _ := entry.Get("chat_type"); anyutil.IsTrue(v) {
			chatType = anyutil.String(v)
		}
		s.RestoredCursors[channelID] = ChannelCursor{ChatType: chatType, LastTS: lastTS, Seen: seen}
	}
	return nil
}

// Snapshot of every watched channel cursor in watch order
func (s *CursorStore) cursorPayload() *jsonutil.Object {
	channels := jsonutil.NewObject()
	for _, id := range s.ChannelOrder {
		state := s.ChannelState[id]
		chatType := state.ChatType
		if chatType == "" {
			chatType = defaultCursorChatType
		}
		seen := make([]any, len(state.Seen))
		for i, e := range state.Seen {
			seen[i] = e
		}
		channels.Set(id, jsonutil.ObjectOf("chat_type", chatType, "last_ts", state.LastTS, "seen", seen))
	}
	return jsonutil.ObjectOf("identity", s.SelfPubkey, "relay", s.RelayURL, "channels", channels)
}

// Persists every watched channel cursor, logging instead of failing
func (s *CursorStore) SaveCursors(ctx context.Context) {
	writeCursors(ctx, CursorPath(ctx), s.cursorPayload())
}

// Atomic compact JSON write of the payload, a failure only logged
func writeCursors(ctx context.Context, path string, payload *jsonutil.Object) {
	if err := config.AtomicJSONWriteWith(ctx, path, payload, jsonutil.Spaced()); err != nil {
		pkgLog().Debug("Buzz: could not persist channel cursors", "error", err)
	}
}

// Last timestamp, seen count and newest seen id of a channel cursor
func CursorMarkOf(state *ChannelCursor) CursorMark {
	m := CursorMark{LastTS: state.LastTS, Count: len(state.Seen)}
	if len(state.Seen) > 0 {
		m.Last, m.HasLast = state.Seen[len(state.Seen)-1], true
	}
	return m
}

// Write of the cursor as it stands now that skips itself when a newer snapshot already landed
func (s *CursorStore) CursorWriter(ctx context.Context) func() {
	payload := s.cursorPayload()
	s.snapshotMu.Lock()
	s.snapshots++
	generation := s.snapshots
	s.snapshotMu.Unlock()
	path := CursorPath(ctx)
	return func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		if generation > s.written {
			s.written = generation
			writeCursors(ctx, path, payload)
		}
	}
}
