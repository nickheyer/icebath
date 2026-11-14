package teamspipeline

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestGenerateSummaryPayloadUsesHeuristicWhenNoModelAnswers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pipeline, _ := testPipeline(t, obj("notion", obj("database_id", "db-1"), "linear", map[string]any{"team_id": "team-9"}, "teams_delivery", obj("chat_id", "chat-1")))
	resolved, err := newMeetingRef("meeting-1", nil, nil, nil, nil, nil, obj("subject", "Weekly Sync",
		"startDateTime", "2026-03-01T10:00:00Z", "endDateTime", "2026-03-01T11:00:00+00:00",
		"participants", []any{obj("displayName", "Ada"), obj("identity", obj("user", obj("displayName", "Grace"))), "skip", obj("identity", nil)}))
	if err != nil {
		t.Fatal(err)
	}
	artifacts := []*MeetingArtifact{testArtifact(t, "call_record", "call-1", nil, nil, obj("metrics", obj("participant_count", int64(4)))),
		testArtifact(t, "transcript", "tx-1", "meeting.vtt", nil, nil)}
	transcript := "Decision: ship it\nAction: write docs\nrisk: none"

	raw, err := pipeline.SummarizeFn(context.Background(), resolved, transcript, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	payload := raw.(*TeamsMeetingSummaryPayload)

	if payload.MeetingRef != resolved || payload.Title != "Weekly Sync" || payload.TranscriptText != transcript || len(payload.SourceArtifacts) != 2 {
		t.Fatalf("payload %+v", payload)
	}
	if !reflect.DeepEqual(payload.Participants, []any{"Ada", "Grace"}) || !pyjson.Equal(payload.CallMetrics, obj("participant_count", int64(4), "artifact_count", int64(2))) {
		t.Fatalf("participants %v metrics %v", payload.Participants, pyrepr.Repr(payload.CallMetrics))
	}
	if payload.Summary != "Decision: ship it Action: write docs risk: none" || !reflect.DeepEqual(payload.KeyDecisions, []any{"Decision: ship it"}) ||
		!reflect.DeepEqual(payload.ActionItems, []any{"Action: write docs"}) || !reflect.DeepEqual(payload.Risks, []any{"risk: none"}) ||
		payload.Confidence != "low" || payload.ConfidenceNotes != "Generated with heuristic fallback because no LLM summary response was available." {
		t.Fatalf("summary fields %+v", payload)
	}
	if payload.NotionTarget != "db-1" || payload.LinearTarget != "team-9" || payload.TeamsTarget != "chat-1" {
		t.Fatalf("targets %v %v %v", payload.NotionTarget, payload.LinearTarget, payload.TeamsTarget)
	}
	start, end := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	if payload.StartTime == nil || !payload.StartTime.Equal(start) || payload.EndTime == nil || !payload.EndTime.Equal(end) {
		t.Fatalf("times %v %v", payload.StartTime, payload.EndTime)
	}
	if prompt := buildSummaryPrompt(resolved, transcript, artifacts); prompt != "Meeting ID: meeting-1\nTitle: Weekly Sync\nArtifacts:\n- call_record:call-1:\n- transcript:tx-1:meeting.vtt\n\nTranscript:\n"+transcript {
		t.Fatalf("prompt %q", prompt)
	}
	if prompt := buildSummaryPrompt(&TeamsMeetingRef{MeetingID: "m"}, "", nil); prompt != "Meeting ID: m\nTitle: Unknown\nArtifacts:\n- none\n\nTranscript:\n" {
		t.Fatalf("bare prompt %q", prompt)
	}

	pipeline.Config.Notion = "db"
	if _, err := pipeline.SummarizeFn(context.Background(), resolved, transcript, artifacts); pyrepr.ExceptionName(err) != "AttributeError" {
		t.Fatalf("string notion config %v", err)
	}
	pipeline.Config.Notion = nil
	resolved.Metadata.Set("startDateTime", "not a time")
	if _, err := pipeline.SummarizeFn(context.Background(), resolved, transcript, artifacts); err == nil {
		t.Fatal("bad start time accepted")
	}
}
