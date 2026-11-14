package state

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func projectionCompact(t *testing.T, epochs, tail int) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "chat", Source: "desktop"})
	for epoch := range epochs {
		for i := range 4 {
			readAppend(t, db, "chat", map[string]any{"role": "user", "content": fmt.Sprintf("e%d user %d", epoch, i)})
			readAppend(t, db, "chat", map[string]any{"role": "assistant", "content": fmt.Sprintf("e%d assistant %d", epoch, i)})
		}
		live := archiveConversation(t, db, "chat", false)
		archiveCompact(t, db, "chat", append([]map[string]any{{"role": "user", "content": fmt.Sprintf("[summary %d]", epoch)}}, live[len(live)-tail:]...), ArchiveCompactOptions{TailCount: tail})
	}
	return db
}

func projectionTexts(msgs []map[string]any) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = fmt.Sprint(m["role"], ":", m["content"])
	}
	return out
}

func projectionHas(texts []string, want string) bool {
	for _, s := range texts {
		if s == want {
			return true
		}
	}
	return false
}

func projectionResume(t *testing.T, db *SessionDB, sid string) ([]string, []string) {
	t.Helper()
	model, display, err := db.GetResumeConversations(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return projectionTexts(model), projectionTexts(display)
}

func TestResumeAndWarmDisplayMatchTheRestTranscript(t *testing.T) {
	db := projectionCompact(t, 3, 2)
	rest := fmt.Sprint(projectionTexts(displayPaged(t, db, "chat")))
	_, display := projectionResume(t, db, "chat")
	warm := projectionTexts(displayConversation(t, db, "chat", ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true, IncludeCompacted: true}))
	if fmt.Sprint(display) != rest || fmt.Sprint(warm) != rest {
		t.Fatalf("display %v\nwarm %v\nrest %v", display, warm, rest)
	}
	if !projectionHas(display, "user:e0 user 0") || !projectionHas(display, "assistant:e0 assistant 0") {
		t.Fatalf("pre compaction turns missing: %v", display)
	}
}

func TestDisplayReadDedupesCarriedForwardTail(t *testing.T) {
	db := projectionCompact(t, 4, 2)
	_, display := projectionResume(t, db, "chat")
	seen := map[string]bool{}
	for _, s := range display {
		if seen[s] {
			t.Fatalf("duplicate %q in %v", s, display)
		}
		seen[s] = true
	}
}

func TestModelProjectionStaysCompressed(t *testing.T) {
	db := projectionCompact(t, 3, 2)
	model, display := projectionResume(t, db, "chat")
	if len(model) >= len(display) || projectionHas(model, "user:e0 user 0") {
		t.Fatalf("model %v", model)
	}
	active := projectionTexts(displayConversation(t, db, "chat", ConversationOptions{RepairAlternation: true}))
	if fmt.Sprint(model) != fmt.Sprint(active) {
		t.Fatalf("model %v\nactive %v", model, active)
	}
}

func TestRewoundRowsAreExcludedFromTheDisplayProjections(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "chat", Source: "desktop"})
	var target int64
	for _, pair := range [][2]string{{"user", "kept"}, {"assistant", "kept reply"}, {"user", "taken back"}, {"assistant", "taken back reply"}} {
		id := readAppend(t, db, "chat", map[string]any{"role": pair[0], "content": pair[1]})
		if pair[0] == "user" {
			target = id
		}
	}
	if _, err := db.RewindToMessage(context.Background(), "chat", target, RewindOptions{}); err != nil {
		t.Fatal(err)
	}
	_, display := projectionResume(t, db, "chat")
	warm := projectionTexts(displayConversation(t, db, "chat", ConversationOptions{IncludeAncestors: true, IncludeCompacted: true}))
	for _, projection := range [][]string{display, warm} {
		if projectionHas(projection, "user:taken back") || !projectionHas(projection, "user:kept") {
			t.Fatalf("projection %v", projection)
		}
	}
}

func TestPrefixIncludesACompactedAncestorsArchivedRows(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "parent", Source: "desktop"})
	for i := range 3 {
		readAppend(t, db, "parent", map[string]any{"role": "user", "content": fmt.Sprintf("P user %d", i)})
		readAppend(t, db, "parent", map[string]any{"role": "assistant", "content": fmt.Sprintf("P assistant %d", i)})
	}
	archiveCompact(t, db, "parent", []map[string]any{{"role": "user", "content": "[parent summary]"}}, ArchiveCompactOptions{})
	if err := db.EndSession("parent", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "child", Source: "desktop", ParentSessionID: "parent"})
	readAppend(t, db, "child", map[string]any{"role": "user", "content": "C user 0"})
	readAppend(t, db, "child", map[string]any{"role": "assistant", "content": "C assistant 0"})
	prefix, err := db.GetAncestorDisplayPrefix(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	_, display := projectionResume(t, db, "child")
	texts := projectionTexts(prefix)
	if !projectionHas(texts, "user:P user 0") || !projectionHas(display, "user:P user 0") || projectionHas(texts, "user:C user 0") {
		t.Fatalf("prefix %v\ndisplay %v", texts, display)
	}
}

func TestExplicitBranchHasNoAncestorPrefix(t *testing.T) {
	ctx := context.Background()
	db := projectionCompact(t, 3, 2)
	readSession(t, db, NewSession{ID: "branch", Source: "desktop", ParentSessionID: "chat", ModelConfig: map[string]any{"_branched_from": "chat"}})
	readAppend(t, db, "branch", map[string]any{"role": "user", "content": "branch turn"})
	prefix, err := db.GetAncestorDisplayPrefix(ctx, "branch")
	if err != nil || len(prefix) != 0 {
		t.Fatalf("prefix %v err %v", prefix, err)
	}
	if _, display := projectionResume(t, db, "branch"); fmt.Sprint(display) != "[user:branch turn]" {
		t.Fatalf("display %v", display)
	}
}

func TestResumeGuardBoundsWhatResumeLoads(t *testing.T) {
	ctx := context.Background()
	db := projectionCompact(t, 4, 2)
	_, display := projectionResume(t, db, "chat")
	full, err := db.GetResumeMessageCount(ctx, "chat", false)
	if err != nil || full < int64(len(display)) {
		t.Fatalf("count %d display %d err %v", full, len(display), err)
	}
	two := 2
	var tooLarge *SessionResumeTooLargeError
	if _, err := db.AssertResumeSafe(ctx, "chat", &two, false); !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v", err)
	}
	tip, err := db.GetResumeMessageCount(ctx, "chat", true)
	if err != nil || tip >= full {
		t.Fatalf("tip %d full %d err %v", tip, full, err)
	}
	limit := int(tip)
	if n, err := db.AssertResumeSafe(ctx, "chat", &limit, true); err != nil || n == 0 {
		t.Fatalf("tip guard n=%d err=%v", n, err)
	}
}
