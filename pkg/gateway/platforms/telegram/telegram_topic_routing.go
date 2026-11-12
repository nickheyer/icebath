package telegram

import (
	"context"
	"fmt"
	"github.com/go-telegram/bot/models"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	newTopicFollowSeconds    = 5.0
	newTopicOpenerMaxPending = 256
	newTopicRecordTTLSeconds = 600.0
)

// TopicUser is the from_user of a Telegram message, fields untyped like the bot API payload
type TopicUser struct {
	ID    any
	IsBot any
}

// TopicMessage carries the Message fields topic routing reads, nil standing for an absent attribute
type TopicMessage struct {
	MessageID         any
	MessageThreadID   any
	FromUser          *TopicUser
	Date              *time.Time
	ForumTopicCreated any
	Chat              any

	raw     *models.Message
	authCtx context.Context
}

// TopicRoutingHost is the adapter surface the topic routing relies on
type TopicRoutingHost interface {
	Name() string
	ChatTypeStr(chat any) string
	ChatIDStr(message *TopicMessage) string
	IsUserAuthorizedFromMessage(message *TopicMessage) bool
}

type topicKey struct{ chatID, creator string }

type topicRecord struct {
	threadID string
	sentAt   float64
	recorded float64
}

// TopicRouting records client created DM topics so their thread less openers adopt the new topic
type TopicRouting struct {
	mu      sync.Mutex
	order   []topicKey
	pending map[topicKey]topicRecord
}

// Swappable time.monotonic in seconds
var topicMonotonic = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func creatorKey(message *TopicMessage) string {
	if message.FromUser == nil || message.FromUser.ID == nil {
		return ""
	}
	return pyrepr.Str(message.FromUser.ID)
}

// Telegram send stamp in seconds, ok false when the message carries no date
func sentAt(message *TopicMessage) (float64, bool) {
	if message.Date == nil {
		return 0, false
	}
	return float64(message.Date.Unix()) + float64(message.Date.Nanosecond())*1e-9, true
}

func (r *TopicRouting) del(key topicKey) {
	delete(r.pending, key)
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

func (r *TopicRouting) set(key topicKey, rec topicRecord) {
	if r.pending == nil {
		r.pending = map[topicKey]topicRecord{}
	}
	if _, ok := r.pending[key]; !ok {
		r.order = append(r.order, key)
	}
	r.pending[key] = rec
}

// HandleForumTopicCreated records an authorized non bot private chat topic creation as a routing signal
func (r *TopicRouting) HandleForumTopicCreated(host TopicRoutingHost, message *TopicMessage) {
	if message == nil || message.ForumTopicCreated == nil {
		return
	}
	if host.ChatTypeStr(message.Chat) != "private" {
		return
	}
	stamp, ok := sentAt(message)
	if message.MessageThreadID == nil || !ok || message.FromUser == nil || message.FromUser.IsBot == true {
		return
	}
	if !host.IsUserAuthorizedFromMessage(message) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := topicMonotonic()
	for _, k := range append([]topicKey(nil), r.order...) {
		if now-r.pending[k].recorded > newTopicRecordTTLSeconds {
			r.del(k)
		}
	}
	for len(r.order) >= newTopicOpenerMaxPending {
		oldest := r.order[0]
		for _, k := range r.order[1:] {
			if r.pending[k].recorded < r.pending[oldest].recorded {
				oldest = k
			}
		}
		r.del(oldest)
	}
	chatID := host.ChatIDStr(message)
	r.set(topicKey{chatID, creatorKey(message)}, topicRecord{pyrepr.Str(message.MessageThreadID), stamp, now})
	slog.Debug(fmt.Sprintf("[%s] Recorded new Telegram topic %s (chat=%s)", host.Name(), pyrepr.Str(message.MessageThreadID), chatID))
}

// AdoptCreatedTopicThread returns the topic this sender just created for its thread less follow ups, else the given thread
func (r *TopicRouting) AdoptCreatedTopicThread(host TopicRoutingHost, message *TopicMessage, chatType string, threadID *string) *string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if chatType != "dm" || len(r.pending) == 0 {
		return threadID
	}
	key := topicKey{host.ChatIDStr(message), creatorKey(message)}
	record, ok := r.pending[key]
	stamp, hasStamp := sentAt(message)
	if !ok || !hasStamp || stamp < record.sentAt {
		return threadID
	}
	r.del(key)
	if threadID != nil || message.MessageThreadID != nil {
		return threadID
	}
	if stamp-record.sentAt > newTopicFollowSeconds {
		return threadID
	}
	r.set(key, topicRecord{record.threadID, stamp, topicMonotonic()})
	messageID := any("?")
	if message.MessageID != nil {
		messageID = message.MessageID
	}
	slog.Info(fmt.Sprintf("[%s] Thread-less message %s adopted by new Telegram topic %s (chat=%s)", host.Name(), pyrepr.Str(messageID), record.threadID, key.chatID))
	created := record.threadID
	return &created
}
