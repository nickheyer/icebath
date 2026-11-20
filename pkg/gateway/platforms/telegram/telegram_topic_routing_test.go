package telegram

import (
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type fakeTopicHost struct{ allowed string }

func (fakeTopicHost) Name() string                { return "telegram" }
func (fakeTopicHost) ChatTypeStr(chat any) string { return chat.(map[string]any)["type"].(string) }
func (fakeTopicHost) ChatIDStr(message *TopicMessage) string {
	return pyrepr.Str(message.Chat.(map[string]any)["id"])
}
func (h fakeTopicHost) IsUserAuthorizedFromMessage(message *TopicMessage) bool {
	return creatorKey(message) == h.allowed
}

var topicT0 = time.Date(2026, 10, 14, 13, 21, 2, 0, time.UTC)

func topicMsg(messageID int64, sent float64, threadID any, created bool, userID, chatID int64) *TopicMessage {
	date := topicT0.Add(time.Duration(sent * float64(time.Second)))
	m := &TopicMessage{MessageID: messageID, MessageThreadID: threadID, FromUser: &TopicUser{ID: userID, IsBot: false},
		Date: &date, Chat: map[string]any{"id": chatID, "type": "private"}}
	if created {
		m.ForumTopicCreated = map[string]any{"name": "New topic"}
	}
	return m
}

func topicThread(r *TopicRouting, host TopicRoutingHost, m *TopicMessage) *string {
	var tid *string
	if m.MessageThreadID != nil {
		s := pyrepr.Str(m.MessageThreadID)
		tid = &s
	}
	return r.AdoptCreatedTopicThread(host, m, "dm", tid)
}

func withTopicClock(t *testing.T) *float64 {
	clock := 1000.0
	orig := topicMonotonic
	topicMonotonic = func() float64 { return clock }
	t.Cleanup(func() { topicMonotonic = orig })
	return &clock
}

func TestThreadLessMessagesAfterCreationLandInTheNewTopic(t *testing.T) {
	clock := withTopicClock(t)
	host, r := fakeTopicHost{allowed: "42"}, &TopicRouting{}
	r.HandleForumTopicCreated(host, topicMsg(9379, 0, int64(600), true, 42, 111))
	if got := topicThread(r, host, topicMsg(9382, 0.2, nil, false, 42, 111)); got == nil || *got != "600" {
		t.Fatalf("opener thread = %v", got)
	}
	*clock += 30
	if got := topicThread(r, host, topicMsg(9383, 0.7, nil, false, 42, 111)); got == nil || *got != "600" {
		t.Fatalf("second chunk thread = %v", got)
	}
	if got := topicThread(r, host, topicMsg(9384, 1.0, int64(600), false, 42, 111)); got == nil || *got != strconv.Itoa(600) {
		t.Fatalf("threaded message = %v", got)
	}
	if got := topicThread(r, host, topicMsg(9385, 1.2, nil, false, 42, 111)); got != nil {
		t.Fatalf("lobby message adopted into %s", *got)
	}
}

func TestCreationIsNotAdoptedLateOrByOthers(t *testing.T) {
	withTopicClock(t)
	host, r := fakeTopicHost{allowed: "42"}, &TopicRouting{}
	r.HandleForumTopicCreated(host, topicMsg(10, 0, int64(600), true, 42, 111))
	for _, m := range []*TopicMessage{
		topicMsg(11, 0.1, nil, false, 7, 111),
		topicMsg(12, 0.1, nil, false, 42, 222),
		topicMsg(5, -60, nil, false, 42, 111),
		topicMsg(13, newTopicFollowSeconds+1, nil, false, 42, 111),
	} {
		if got := topicThread(r, host, m); got != nil {
			t.Fatalf("message %v adopted into %s", m.MessageID, *got)
		}
	}
	r.HandleForumTopicCreated(host, topicMsg(20, 0, int64(600), true, 7, 111))
	if len(r.pending) != 0 {
		t.Fatalf("pending = %v", r.pending)
	}
}

func TestPendingCreationsAreBoundedAndExpire(t *testing.T) {
	clock := withTopicClock(t)
	host, r := fakeTopicHost{allowed: "42"}, &TopicRouting{}
	for i := range newTopicOpenerMaxPending + 3 {
		*clock += 1
		r.HandleForumTopicCreated(host, topicMsg(int64(i), 0, int64(600), true, 42, int64(i)))
	}
	if len(r.pending) != newTopicOpenerMaxPending || r.order[0].chatID != "3" {
		t.Fatalf("pending %d, oldest %v", len(r.pending), r.order[0])
	}
	*clock += newTopicRecordTTLSeconds + 1
	r.HandleForumTopicCreated(host, topicMsg(1, 0, int64(600), true, 42, 9999))
	if len(r.pending) != 1 {
		t.Fatalf("expired records kept, %d pending", len(r.pending))
	}
}
