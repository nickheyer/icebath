package kanban

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Subscriptions of a task, failing the test on error
func subsOf(t *testing.T, conn state.SQLiteConn, tid string) []*pyjson.Object {
	t.Helper()
	subs, err := ListNotifySubs(context.Background(), conn, &tid, NotifyOwnerScope{})
	if err != nil {
		t.Fatal(err)
	}
	return subs
}

// Field of a subscription object
func subField(sub *pyjson.Object, key string) any {
	v, _ := sub.Get(key)
	return v
}

// Adds a subscription or fails the test
func mustAddSub(t *testing.T, conn state.SQLiteConn, o NotifySubOptions) {
	t.Helper()
	if err := AddNotifySub(context.Background(), conn, o); err != nil {
		t.Fatal(err)
	}
}

func TestNotifySubDeliveryModePersistsAndLastWriteWins(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "mode sub task", Assignee: sp("worker1")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", NotifierProfile: sp("owner-a")})
	subs := subsOf(t, conn, tid)
	if len(subs) != 1 || subField(subs[0], "delivery_mode") != "notify" || subField(subs[0], "notifier_profile") != "owner-a" {
		t.Fatal("fresh sub", subs)
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", NotifierProfile: sp("owner-b"), DeliveryMode: sp("wake")})
	subs = subsOf(t, conn, tid)
	if len(subs) != 1 || subField(subs[0], "delivery_mode") != "wake" || subField(subs[0], "notifier_profile") != "owner-a" {
		t.Fatal("re-subscribe", subs)
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1"})
	if subField(subsOf(t, conn, tid)[0], "delivery_mode") != "wake" {
		t.Fatal("None re-subscribe clobbered the mode")
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", DeliveryMode: sp("bogus")})
	if subField(subsOf(t, conn, tid)[0], "delivery_mode") != "wake" {
		t.Fatal("unknown mode clobbered the mode")
	}
}

func TestNotifySubChatTypePersistsAndLastWriteWins(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "chat_type sub", Assignee: sp("worker1")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1"})
	if subField(subsOf(t, conn, tid)[0], "chat_type") != "dm" {
		t.Fatal("default chat_type")
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", ChatType: sp("group")})
	if subField(subsOf(t, conn, tid)[0], "chat_type") != "group" {
		t.Fatal("chat_type not last-write-wins")
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", DeliveryMode: sp("wake")})
	sub := subsOf(t, conn, tid)[0]
	if subField(sub, "chat_type") != "group" || subField(sub, "delivery_mode") != "wake" {
		t.Fatal("None re-subscribe clobbered chat_type", sub)
	}
}

func TestNotifySubUserIdBackfillsLegacySenderlessRows(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "legacy sub", Assignee: sp("worker1")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1"})
	if subField(subsOf(t, conn, tid)[0], "user_id") != nil {
		t.Fatal("user_id set on a senderless sub")
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", UserID: sp("640466638")})
	if subField(subsOf(t, conn, tid)[0], "user_id") != "640466638" {
		t.Fatal("user_id not backfilled")
	}
}

func TestNotifySubUserIdAltPersistsAndBackfillsLegacyRows(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "alt sub", Assignee: sp("worker1")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", UserID: sp("open-id")})
	sub := subsOf(t, conn, tid)[0]
	if subField(sub, "user_id") != "open-id" || subField(sub, "user_id_alt") != nil {
		t.Fatal(sub)
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat1", UserID: sp("open-id"), UserIDAlt: sp("union-id")})
	sub = subsOf(t, conn, tid)[0]
	if subField(sub, "user_id") != "open-id" || subField(sub, "user_id_alt") != "union-id" {
		t.Fatal(sub)
	}
}

func TestMigrationBackfillsLegacyGatewaySubsToNotifyWake(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "legacy sub upgrade"})
	mustExec(t, conn, "ALTER TABLE kanban_notify_subs DROP COLUMN delivery_mode")
	mustExecArgs(t, conn, "INSERT INTO kanban_notify_subs (task_id, platform, chat_id, thread_id, created_at) VALUES (?, 'telegram', 'legacy-chat', '', 1)", tid)
	mustExecArgs(t, conn, "INSERT INTO kanban_notify_subs (task_id, platform, chat_id, thread_id, created_at) VALUES (?, 'tui', 'tui-session-1', '', 1)", tid)
	if err := migrateAddOptionalColumns(ctx, conn); err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, r := range mustRows(t, conn, "SELECT platform, delivery_mode FROM kanban_notify_subs WHERE task_id = ?", tid) {
		modes[cellStr(r.Get("platform"))] = cellStr(r.Get("delivery_mode"))
	}
	if modes["telegram"] != "notify+wake" {
		t.Fatal("Legacy gateway subscription lost active wake across the upgrade", modes)
	}
	if modes["tui"] != "notify" {
		t.Fatal(modes)
	}
}

func TestMigrationBackfillRunsOnlyOnFirstAdd(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "explicit downgrade survives"})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-x", DeliveryMode: sp("notify")})
	if err := migrateAddOptionalColumns(ctx, conn); err != nil {
		t.Fatal(err)
	}
	rows := mustRows(t, conn, "SELECT delivery_mode FROM kanban_notify_subs WHERE task_id = ?", tid)
	if len(rows) != 1 || rows[0].Get("delivery_mode") != "notify" {
		t.Fatal(rows)
	}
}

// Full routing subscription on a parent task
func addFullParentSub(t *testing.T, conn state.SQLiteConn, parent string) {
	t.Helper()
	mustAddSub(t, conn, NotifySubOptions{TaskID: parent, Platform: "telegram", ChatID: "chat1", ThreadID: sp("topic1"), UserID: sp("user1"),
		UserIDAlt: sp("alt-1"), ChatType: sp("dm"), NotifierProfile: sp("default"), DeliveryMode: sp("notify+wake"),
		DeliveryMetadata: pyjson.ObjectOf("reply_fallback", "general", "topic_name", "ops")})
}

// Asserts every routing column survived inheritance
func assertFullInheritedSub(t *testing.T, subs []*pyjson.Object) {
	t.Helper()
	if len(subs) != 1 {
		t.Fatal("subs", len(subs))
	}
	s := subs[0]
	if subField(s, "platform") != "telegram" || subField(s, "chat_id") != "chat1" || subField(s, "thread_id") != "topic1" || subField(s, "user_id") != "user1" {
		t.Fatal(s)
	}
	if subField(s, "user_id_alt") != "alt-1" {
		t.Fatal("user_id_alt dropped during inheritance")
	}
	if subField(s, "chat_type") != "dm" {
		t.Fatal("chat_type dropped during inheritance")
	}
	if subField(s, "delivery_mode") != "notify+wake" {
		t.Fatal(s)
	}
	md, _ := subField(s, "delivery_metadata").(*pyjson.Object)
	if fallback, _ := md.Get("reply_fallback"); md == nil || fallback != "general" {
		t.Fatal("delivery_metadata dropped during inheritance")
	}
}

func TestLinkTasksInheritsAllRoutingColumns(t *testing.T) {
	conn := openTestBoard(t)
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "root"})
	addFullParentSub(t, conn, parent)
	child := mustCreate(t, conn, CreateTaskOptions{Title: "existing child", Assignee: sp("w1")})
	if _, err := LinkTasks(context.Background(), conn, parent, child, nil); err != nil {
		t.Fatal(err)
	}
	assertFullInheritedSub(t, subsOf(t, conn, child))
}

func TestCreateWithParentsInheritsDeliveryMetadata(t *testing.T) {
	conn := openTestBoard(t)
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "root"})
	addFullParentSub(t, conn, parent)
	child := mustCreate(t, conn, CreateTaskOptions{Title: "graph child", Assignee: sp("w1"), Parents: []string{parent}})
	assertFullInheritedSub(t, subsOf(t, conn, child))
}

// Done task carrying a telegram subscription
func makeDoneTaskWithSub(t *testing.T, conn state.SQLiteConn, title, chatID string) string {
	t.Helper()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: title, Assignee: sp("worker1")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: chatID, NotifierProfile: sp("default")})
	if ok, err := CompleteTask(context.Background(), conn, tid, CompleteTaskOptions{Summary: sp("done")}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	return tid
}

// Pushes a task event history and completion into the past
func backdateTask(t *testing.T, conn state.SQLiteConn, tid string, days int64) {
	t.Helper()
	ctx := context.Background()
	past := time.Now().Unix() - days*86400
	if err := WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE task_events SET created_at = ? WHERE task_id = ?", past, tid); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET completed_at = ?, created_at = ? WHERE id = ?", past, past, tid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Purges stale subs or fails the test
func purge(t *testing.T, conn state.SQLiteConn, days int64) int64 {
	t.Helper()
	n, err := PurgeStaleDoneNotifySubs(context.Background(), conn, days)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGcPurgesStaleDoneSubKeepsFreshOne(t *testing.T) {
	conn := openTestBoard(t)
	stale := makeDoneTaskWithSub(t, conn, "old done", "c-stale")
	fresh := makeDoneTaskWithSub(t, conn, "new done", "c-fresh")
	backdateTask(t, conn, stale, 45)
	if purge(t, conn, 30) != 1 || len(subsOf(t, conn, stale)) != 0 || len(subsOf(t, conn, fresh)) != 1 {
		t.Fatal("purge")
	}
}

func TestGcHonorsConfiguredRetentionDays(t *testing.T) {
	conn := openTestBoard(t)
	defaultDays := int64(30)
	tid := makeDoneTaskWithSub(t, conn, "ten days old", "c-10d")
	backdateTask(t, conn, tid, 10)
	if purge(t, conn, defaultDays) != 0 || len(subsOf(t, conn, tid)) != 1 {
		t.Fatal("default retention purged a fresh sub")
	}
	if purge(t, conn, 7) != 1 || len(subsOf(t, conn, tid)) != 0 {
		t.Fatal("tight retention kept the sub")
	}
	tid2 := makeDoneTaskWithSub(t, conn, "ancient", "c-anc")
	backdateTask(t, conn, tid2, 3650)
	if purge(t, conn, 0) != 0 || len(subsOf(t, conn, tid2)) != 1 {
		t.Fatal("zero retention swept")
	}
}

// Forces a status with a matching status event
func setTaskStatus(t *testing.T, conn state.SQLiteConn, tid, status string) {
	t.Helper()
	ctx := context.Background()
	if err := WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ? WHERE id = ?", status, tid); err != nil {
			return err
		}
		return appendEvent(ctx, conn, tid, "status", payload("status", status), nil)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGcSparesReopenedTaskEvenWhenOld(t *testing.T) {
	conn := openTestBoard(t)
	tid := makeDoneTaskWithSub(t, conn, "reopened", "c-reopen")
	backdateTask(t, conn, tid, 90)
	setTaskStatus(t, conn, tid, "ready")
	backdateTask(t, conn, tid, 90)
	if purge(t, conn, 30) != 0 || len(subsOf(t, conn, tid)) != 1 {
		t.Fatal("reopened task purged")
	}
}

func TestGcPurgesBlockedTaskThatNeverDone(t *testing.T) {
	conn := openTestBoard(t)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "stuck blocked", Assignee: sp("worker1")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "c-blocked", NotifierProfile: sp("default")})
	setTaskStatus(t, conn, tid, "blocked")
	backdateTask(t, conn, tid, 45)
	if purge(t, conn, 30) != 1 || len(subsOf(t, conn, tid)) != 0 {
		t.Fatal("blocked task kept its sub")
	}
}

// Counts subscriptions or fails the test
func countSubs(t *testing.T, dbPath string, board *string, scope NotifyOwnerScope, platform, chatID, threadID *string) int64 {
	t.Helper()
	n, err := CountNotifySubs(context.Background(), dbPath, board, scope, platform, chatID, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMissingDbCountsZeroAndCreatesNothing(t *testing.T) {
	setupKanbanHome(t)
	dbPath := mustDBPath(t, BoardArg("default"))
	if pathExists(dbPath) {
		t.Fatal("db exists before the probe")
	}
	if countSubs(t, "", BoardArg("default"), NotifyOwnerScope{}, nil, nil, nil) != 0 || countSubs(t, "", BoardArg("default"), NotifyOwnerScope{}, sp("tui"), sp("session-1"), nil) != 0 {
		t.Fatal("count")
	}
	if pathExists(dbPath) {
		t.Fatal("read-only probe must not create the DB")
	}
}

func TestOptionalFiltersNarrowCountWithoutChangingUnfilteredCount(t *testing.T) {
	setupKanbanHome(t)
	conn := mustConnect(t, "", BoardArg("default"))
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "t", Assignee: sp("w")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "tui", ChatID: "session-1"})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "TUI", ChatID: "session-2", ThreadID: sp("thread-2")})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "session-1"})
	conn.Close()
	board := BoardArg("default")
	count := func(platform, chatID, threadID *string) int64 {
		return countSubs(t, "", board, NotifyOwnerScope{}, platform, chatID, threadID)
	}
	for _, tc := range []struct {
		platform, chatID, threadID *string
		want                       int64
	}{
		{nil, nil, nil, 3}, {sp("tui"), nil, nil, 2}, {nil, sp("session-1"), nil, 2}, {nil, nil, sp("thread-2"), 1},
		{sp("tui"), sp("session-1"), nil, 1}, {sp("tui"), sp("session-1"), sp(""), 1}, {sp("tui"), sp("session-2"), sp("thread-2"), 1},
		{sp("tui"), sp("other-session"), nil, 0},
	} {
		if got := count(tc.platform, tc.chatID, tc.threadID); got != tc.want {
			t.Errorf("count(%v, %v, %v) = %d, want %d", pyStrOpt(tc.platform), pyStrOpt(tc.chatID), pyStrOpt(tc.threadID), got, tc.want)
		}
	}
}

func TestLegacyDbWithoutSubsTableCountsZeroAndStaysUnmigrated(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	raw := rawSQLite(t, legacy)
	mustExec(t, raw, "CREATE TABLE something_else (id INTEGER)")
	raw.Close()
	if countSubs(t, legacy, nil, NotifyOwnerScope{}, nil, nil, nil) != 0 || countSubs(t, legacy, nil, NotifyOwnerScope{}, sp("tui"), sp("session-1"), nil) != 0 {
		t.Fatal("legacy count")
	}
	raw = rawSQLite(t, legacy)
	defer raw.Close()
	for _, r := range mustRows(t, raw, "SELECT name FROM sqlite_master WHERE type='table'") {
		if cellStr(r.Get("name")) == "kanban_notify_subs" {
			t.Fatal("read-only probe must never create schema")
		}
	}
}

func TestCountNotifySubsFiltersProfileOwners(t *testing.T) {
	setupKanbanHome(t)
	dbPath := filepath.Join(t.TempDir(), "owners.db")
	if _, err := InitDB(context.Background(), dbPath, nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, dbPath, nil)
	for _, profile := range []*string{sp("default"), sp("writer"), nil} {
		tid := mustCreate(t, conn, CreateTaskOptions{Title: "owned by " + pyStrOpt(profile)})
		mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-" + pyStrOpt(profile), NotifierProfile: profile})
	}
	conn.Close()
	if countSubs(t, dbPath, nil, NotifyOwnerScope{Profiles: []string{"writer"}, Scoped: true}, nil, nil, nil) != 1 {
		t.Fatal("writer count")
	}
	if countSubs(t, dbPath, nil, NotifyOwnerScope{Profiles: []string{"default"}, Scoped: true, IncludeUnowned: true}, nil, nil, nil) != 2 {
		t.Fatal("default plus unowned count")
	}
}

func TestNotifySubCrud(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "x"})
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "123", UserID: sp("u1"), NotifierProfile: sp("default"),
		DeliveryMetadata: pyjson.ObjectOf("chat_type", "dm", "telegram_reply_to_message_id", "42")})
	subs := subsOf(t, conn, tid)
	if len(subs) != 1 || subField(subs[0], "platform") != "telegram" || subField(subs[0], "notifier_profile") != "default" ||
		!pyjson.Equal(subField(subs[0], "delivery_metadata"), pyjson.ObjectOf("chat_type", "dm", "telegram_reply_to_message_id", "42")) {
		t.Fatal(subs)
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "123", DeliveryMetadata: pyjson.ObjectOf("chat_type", "dm", "telegram_reply_to_message_id", "43")})
	subs = subsOf(t, conn, tid)
	md, _ := subField(subs[0], "delivery_metadata").(*pyjson.Object)
	if reply, _ := md.Get("telegram_reply_to_message_id"); len(subs) != 1 || reply != "43" {
		t.Fatal("duplicate add", subs)
	}
	mustAddSub(t, conn, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "123", ThreadID: sp("5")})
	if len(subsOf(t, conn, tid)) != 2 {
		t.Fatal("distinct thread not a new row")
	}
	if ok, err := RemoveNotifySub(ctx, conn, tid, "telegram", "123", nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if len(subsOf(t, conn, tid)) != 1 {
		t.Fatal("remove")
	}
}

func TestNotifyClaimIsSingleOwnerAndRewindable(t *testing.T) {
	conn1 := openTestBoard(t)
	conn2 := mustConnect(t, "", nil)
	defer conn2.Close()
	ctx := context.Background()
	tid := mustCreate(t, conn1, CreateTaskOptions{Title: "x", Assignee: sp("w")})
	mustAddSub(t, conn1, NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "123"})
	initialCursor := cellInt(subField(subsOf(t, conn1, tid)[0], "last_event_id"))
	if _, err := CompleteTask(ctx, conn1, tid, CompleteTaskOptions{Result: sp("ok")}); err != nil {
		t.Fatal(err)
	}
	kinds := []string{"completed", "blocked"}
	oldCursor, claimedCursor, events, err := ClaimUnseenEventsForSub(ctx, conn1, tid, "telegram", "123", nil, kinds)
	if err != nil || oldCursor != initialCursor || claimedCursor <= oldCursor || len(events) != 1 || events[0].Kind != "completed" {
		t.Fatal(oldCursor, claimedCursor, len(events), err)
	}
	_, _, duplicate, err := ClaimUnseenEventsForSub(ctx, conn2, tid, "telegram", "123", nil, kinds)
	if err != nil || len(duplicate) != 0 {
		t.Fatal("duplicate claim", len(duplicate), err)
	}
	if ok, err := RewindNotifyCursor(ctx, conn1, tid, "telegram", "123", nil, claimedCursor, oldCursor); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, retried, err := UnseenEventsForSub(ctx, conn2, tid, "telegram", "123", nil, kinds)
	if err != nil || len(retried) != 1 || retried[0].Kind != "completed" {
		t.Fatal("retry", len(retried), err)
	}
}
