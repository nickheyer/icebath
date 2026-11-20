package discord

import (
	"sync"

	"github.com/bwmarrin/discordgo"
)

const semanticRenameMaxRecords = 2000

// One guarded title rename, the name it replaced and the event name last seen
type titleRename struct {
	replaced      string
	title         string
	lastEventName string
	titleSeen     bool
}

// Latest guarded title rename per thread id, oldest evicted first
type semanticThreadRenames struct {
	mu      sync.Mutex
	order   []string
	records map[string]*titleRename
}

func newSemanticThreadRenames() *semanticThreadRenames {
	return &semanticThreadRenames{records: map[string]*titleRename{}}
}

// Removes a thread id from the eviction order
func (s *semanticThreadRenames) unlink(threadID string) {
	for i, id := range s.order {
		if id == threadID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

// Records a rename before its REST edit and drops it when the edit fails unless a newer attempt replaced it
func (s *semanticThreadRenames) attempt(threadID, replaced, title string, edit func() error) error {
	record := &titleRename{replaced: replaced, title: title, lastEventName: replaced}
	s.mu.Lock()
	if _, ok := s.records[threadID]; ok {
		s.unlink(threadID)
	}
	s.records[threadID] = record
	s.order = append(s.order, threadID)
	for len(s.records) > semanticRenameMaxRecords {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.records, oldest)
	}
	s.mu.Unlock()
	err := edit()
	if err != nil {
		s.mu.Lock()
		if s.records[threadID] == record {
			delete(s.records, threadID)
			s.unlink(threadID)
		}
		s.mu.Unlock()
	}
	return err
}

// Applies one gateway rename event, only the own replaced to title step keeping the record
func (s *semanticThreadRenames) observeEvent(threadID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[threadID]
	if record == nil || name == record.lastEventName {
		return
	}
	if record.lastEventName == record.replaced && name == record.title {
		record.lastEventName = name
		return
	}
	delete(s.records, threadID)
	s.unlink(threadID)
}

// Name to render for a thread currently called name
func (s *semanticThreadRenames) displayName(threadID, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[threadID]
	if record == nil {
		return name
	}
	if name == record.title {
		record.titleSeen = true
		return record.replaced
	}
	if record.titleSeen || name != record.replaced {
		delete(s.records, threadID)
		s.unlink(threadID)
	}
	return name
}

// Feeds a raw thread update's own name to the rename tracker
func (a *DiscordAdapter) onPlatformRawThreadUpdate(threadID string, data map[string]any) {
	if name, ok := data["name"].(string); ok {
		a.semanticThreadRenames.observeEvent(threadID, name)
	}
}

// Readable chat name of a thread with its guild and parent, forum parents named without a hash
func (a *DiscordAdapter) formatThreadChatName(th *discordgo.Channel) string {
	threadName := th.Name
	if threadName == "" {
		threadName = th.ID
		if threadName == "" {
			threadName = "thread"
		}
	}
	threadName = a.semanticThreadRenames.displayName(th.ID, threadName)
	parent := a.stateChannel(th.ParentID)
	guildName := ""
	guildID := th.GuildID
	if guildID == "" && parent != nil {
		guildID = parent.GuildID
	}
	if client := a.liveClient(); client != nil && client.State != nil && guildID != "" {
		if g, err := client.State.Guild(guildID); err == nil {
			guildName = g.Name
		}
	}
	parentName := ""
	if parent != nil {
		parentName = parent.Name
	}
	switch {
	case isForumParent(parent) && guildName != "" && parentName != "":
		return guildName + " / " + parentName + " / " + threadName
	case parentName != "" && guildName != "":
		return guildName + " / #" + parentName + " / " + threadName
	case parentName != "":
		return parentName + " / " + threadName
	}
	return threadName
}
