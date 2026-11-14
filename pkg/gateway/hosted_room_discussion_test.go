package gateway

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	hdLocalProfiles = []string{"research", "build", "review", "ops", "qa", "docs"}
	hdMembers       = func() []any {
		var out []any
		for _, profile := range hdLocalProfiles[:3] {
			out = append(out, map[string]any{"member_id": "member-" + profile, "profile": profile, "handle": profile, "display_name": strings.ToUpper(profile[:1]) + profile[1:]})
		}
		return out
	}()
)

func hdNow() *float64 {
	now := float64(time.Now().UnixNano()) / 1e9
	return &now
}

// Room store holding the Release room of the discussion cases
func hdRoomDB(t *testing.T) (string, map[string]any) {
	t.Helper()
	db := hrDB(t)
	room := hrOK(CreateRoom(context.Background(), db, CreateRoomInput{RoomID: "room-1", Name: "Release", Members: hdMembers, AuthorityGatewayID: "gateway-a", Now: hrFloatPtr(1)}))
	return db, room
}

func hdEvents(t *testing.T, db string) []any {
	t.Helper()
	return hrOK(ReadEvents(context.Background(), db, "room-1", 0, int64(MaxRoomLogLimit), false))["events"].([]any)
}

func hdAppendUser(t *testing.T, db, eventID, text, threadID string) map[string]any {
	t.Helper()
	return hrOK(AppendEvent(context.Background(), db, AppendEventInput{RoomID: "room-1", EventID: eventID, Kind: "message.user", Actor: map[string]any{"kind": "user", "id": "local-user"},
		AuthorityGatewayID: "gateway-a", AuthorityEpoch: 1, Payload: map[string]any{"text": text, "thread_id": threadID}, Now: hdNow()}))
}

func hdAppendPublication(t *testing.T, db string, plan PublicationPlan) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, event := range plan.Events {
		in := event.AppendInput("room-1")
		in.Now = hdNow()
		out = append(out, hrOK(AppendEvent(context.Background(), db, in)))
	}
	return out
}

func hdAppendActivity(t *testing.T, db, eventID, discussionEventID, threadID string) {
	t.Helper()
	hrOK(AppendEvent(context.Background(), db, AppendEventInput{RoomID: "room-1", EventID: eventID, Kind: "room.activity", Actor: map[string]any{"kind": "gateway", "id": "gateway-a"},
		Payload: map[string]any{"status": "settled", "reason_code": "silent_round", "thread_id": threadID, "discussion_event_id": discussionEventID}, AuthorityGatewayID: "gateway-a", AuthorityEpoch: 1}))
}

func hdDecide(t *testing.T, room map[string]any, db string) DiscussionDecision {
	t.Helper()
	return hrOK(PlanNextTask(room, hdEvents(t, db), hdLocalProfiles, nil, false, DefaultDiscussionLimits()))
}

func hdNextTask(t *testing.T, room map[string]any, db string) DiscussionTaskPlan {
	t.Helper()
	decision := hdDecide(t, room, db)
	if decision.Status != "task" || decision.Task == nil {
		t.Fatalf("decision %+v", decision)
	}
	return *decision.Task
}

func hdPublish(t *testing.T, room map[string]any, db string, task DiscussionTaskPlan, status string, result any, generation *int64) PublicationPlan {
	t.Helper()
	return hrOK(PlanPublication(room, hdEvents(t, db), task, status, result, generation, hdLocalProfiles))
}

func hdSettleNext(t *testing.T, room map[string]any, db, text string) DiscussionTaskPlan {
	t.Helper()
	task := hdNextTask(t, room, db)
	hdAppendPublication(t, db, hdPublish(t, room, db, task, "settled", map[string]any{"text": text}, nil))
	return task
}

func TestDeferredMemberAllowsNextMentionedMemberAndLaterTerminalResult(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-1", "Report.", "thread-1")
	first := hdNextTask(t, room, db)
	generation := int64(1)
	hdAppendPublication(t, db, hdPublish(t, room, db, first, "deferred", map[string]any{"reason": "member_unavailable"}, &generation))
	second := hdNextTask(t, room, db)
	if second.Member.MemberID == first.Member.MemberID {
		t.Fatal("deferred member planned again")
	}
	hdAppendPublication(t, db, hdPublish(t, room, db, first, "settled", map[string]any{"text": "Recovered on explicit retry."}, nil))
	decision := hdDecide(t, room, db)
	if decision.Status != "task" || decision.Task == nil || decision.Task.Member.MemberID != second.Member.MemberID {
		t.Fatalf("decision %+v", decision)
	}
}

func TestDistinctThreadsArePlannedFifoWithoutSkipping(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-1", "First", "thread-1")
	hdAppendUser(t, db, "user-2", "Second", "thread-2")
	if first := hdNextTask(t, room, db); first.DiscussionEventID != "user-1" {
		t.Fatal(first.DiscussionEventID)
	}
	hdAppendActivity(t, db, "activity-1", "user-1", "thread-1")
	if second := hdNextTask(t, room, db); second.DiscussionEventID != "user-2" {
		t.Fatal(second.DiscussionEventID)
	}
}

func TestRoomStopFencesOldWorkButAllowsALaterMessage(t *testing.T) {
	db, room := hdRoomDB(t)
	hdAppendUser(t, db, "user-1", "First", "thread-1")
	stop := hrOK(RequestRoomStop(context.Background(), db, "room-1", "user-stop-1", room["authority_gateway_id"], room["authority_epoch"]))
	if decision := hdDecide(t, room, db); decision.Status != "idle" || stop["kind"] != "room.stop_requested" {
		t.Fatalf("decision %+v stop %v", decision, stop)
	}
	hdAppendUser(t, db, "user-2", "Continue", "thread-2")
	if resumed := hdNextTask(t, room, db); resumed.DiscussionEventID != "user-2" {
		t.Fatal(resumed.DiscussionEventID)
	}
}

func TestDeterministicTaskFitsExistingDriverAndReconstructsAfterRestart(t *testing.T) {
	db, room := hdRoomDB(t)
	ctx := context.Background()
	user := hdAppendUser(t, db, "user-1", "Check the release.", "thread-1")
	first := hdNextTask(t, room, db)
	repeated := hdNextTask(t, room, db)
	if !reflect.DeepEqual(first, repeated) || first.Identity.ThreadID != "thread-1" {
		t.Fatalf("first %+v repeated %+v", first, repeated)
	}
	want := map[string]any{"target_member_id": "member-research", "target_profile": "research", "prompt": first.Payload["prompt"], "source_event_seq": user["seq"]}
	if !reflect.DeepEqual(first.Payload, want) {
		t.Fatalf("payload %v", first.Payload)
	}
	wall := func() any { return float64(time.Now().UnixNano()) / 1e9 }
	admitted := hrOK(DriverAdmitTask(ctx, db, first.Identity, first.Payload, wall))
	stored := hrOK(DriverGetTask(ctx, db, first.Identity))
	reconstructed := hrOK(ReconstructTaskPlan(room, hdEvents(t, db), stored, hdLocalProfiles))
	if admitted["status"] != "queued" || !reflect.DeepEqual(reconstructed, first) {
		t.Fatalf("reconstructed %+v first %+v", reconstructed, first)
	}
	if again := hrOK(ReconstructTaskPlan(room, hdEvents(t, db), hrOK(DriverGetTask(ctx, db, first.Identity)), hdLocalProfiles)); !reflect.DeepEqual(again, first) {
		t.Fatalf("again %+v", again)
	}
}

func TestMentionsSelectHandlesOrEveryone(t *testing.T) {
	for _, tc := range []struct{ text, profile string }{{"@build please inspect this", "build"}, {"@all inspect this", "research"}, {"@everyone inspect this", "research"},
		{"inspect this", "research"}, {"@unknown inspect this", "research"}} {
		t.Run(tc.text, func(t *testing.T) {
			db, room := hdRoomDB(t)
			hdAppendUser(t, db, "user-1", tc.text, "thread-1")
			if profile := hdNextTask(t, room, db).Member.Profile; profile != tc.profile {
				t.Fatal(profile)
			}
		})
	}
}
