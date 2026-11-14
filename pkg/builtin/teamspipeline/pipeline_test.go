package teamspipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Puts the Graph seams of the pipeline back once the test ends
func keepPipelineSeams(t *testing.T) {
	t.Helper()
	resolve, fetch, enrich, list, download, prepare := pipelineResolveMeetingReference, pipelineFetchPreferredTranscriptText,
		pipelineEnrichMeetingWithCallRecord, pipelineListRecordingArtifacts, pipelineDownloadRecordingArtifact, pipelinePrepareAudioPath
	t.Cleanup(func() {
		pipelineResolveMeetingReference, pipelineFetchPreferredTranscriptText, pipelineEnrichMeetingWithCallRecord = resolve, fetch, enrich
		pipelineListRecordingArtifacts, pipelineDownloadRecordingArtifact, pipelinePrepareAudioPath = list, download, prepare
	})
}

// None for an empty keyword argument
func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func transcriptMeetingResolver(_ context.Context, _ graphClient, meetingID, _, tenantID, organizerUserID string) (*TeamsMeetingRef, error) {
	return newMeetingRef(meetingID, noneIfEmpty(organizerUserID), nil, nil, nil, noneIfEmpty(tenantID),
		obj("subject", "Weekly Sync", "participants", []any{obj("displayName", "Ada")}))
}

func noCallRecord(context.Context, graphClient, *TeamsMeetingRef, string, bool) (*MeetingArtifact, error) {
	return nil, nil
}

func testArtifact(t *testing.T, kind, id string, displayName, downloadURL any, metadata *pyjson.Object) *MeetingArtifact {
	t.Helper()
	a, err := newMeetingArtifact(kind, id, displayName, nil, nil, downloadURL, nil, nil, nil, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testPipeline(t *testing.T, config any) (*TeamsMeetingPipeline, *TeamsPipelineStore) {
	t.Helper()
	store, err := NewTeamsPipelineStore(filepath.Join(t.TempDir(), "teams-store.json"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := NewTeamsMeetingPipeline(&scriptGraph{}, store, config)
	if err != nil {
		t.Fatal(err)
	}
	return pipeline, store
}

func meetingNotification(id, changeType, resource string, resourceData *pyjson.Object) *pyjson.Object {
	return obj("id", id, "changeType", changeType, "resource", resource, "resourceData", resourceData)
}

func TestTeamsMeetingPipelineTranscriptFirstPathPersistsStateAndSkipsRecording(t *testing.T) {
	keepPipelineSeams(t)
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
		return testArtifact(t, "transcript", "tx-1", "meeting.vtt", nil, nil),
			"Action: Send draft by Friday.\nDecision: Ship the transcript-first path.\nDetailed transcript content.", nil
	}
	pipelineEnrichMeetingWithCallRecord = func(context.Context, graphClient, *TeamsMeetingRef, string, bool) (*MeetingArtifact, error) {
		return testArtifact(t, "call_record", "call-1", nil, nil, obj("metrics", obj("participant_count", int64(4)))), nil
	}
	pipeline, store := testPipeline(t, obj("transcript_min_chars", int64(20)))
	pipeline.SummarizeFn = func(_ context.Context, resolved *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error) {
		return &TeamsMeetingSummaryPayload{MeetingRef: resolved, Title: "Weekly Sync", TranscriptText: transcriptText, Summary: "Short summary",
			KeyDecisions: []any{"Ship the transcript-first path."}, ActionItems: []any{"Send draft by Friday."}, Risks: []any{"Timeline risk."},
			Confidence: "high", ConfidenceNotes: "Transcript available.", SourceArtifacts: artifacts}, nil
	}

	job, err := pipeline.RunNotification(context.Background(),
		meetingNotification("notif-1", "updated", "communications/onlineMeetings/meeting-123", obj("id", "meeting-123")))
	if err != nil {
		t.Fatal(err)
	}

	if job.Status != "completed" || job.SelectedArtifactStrategy != "transcript_first" {
		t.Fatalf("status %q strategy %v error %v", job.Status, job.SelectedArtifactStrategy, job.ErrorInfo)
	}
	if job.SummaryPayload == nil || job.SummaryPayload.Summary != "Short summary" {
		t.Fatalf("summary payload %+v", job.SummaryPayload)
	}
	stored := store.GetJob(job.JobID)
	if stored == nil || get(stored, "status") != "completed" {
		t.Fatalf("stored %v", stored)
	}
}

type fakeNotionWriter struct{}

func (fakeNotionWriter) WriteSummary(_ context.Context, _ *TeamsMeetingSummaryPayload, _, existingRecord *pyjson.Object) (*pyjson.Object, error) {
	var pageID any = "page-1"
	if existingRecord != nil && existingRecord.Len() > 0 {
		pageID = get(existingRecord, "page_id")
	}
	return obj("page_id", pageID, "url", "https://notion.so/page-1"), nil
}

func TestTeamsMeetingPipelineRecordingFallbackUsesSTTAndUpdatesSinkRecords(t *testing.T) {
	keepPipelineSeams(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
		return nil, "", nil
	}
	pipelineListRecordingArtifacts = func(context.Context, graphClient, *TeamsMeetingRef) ([]*MeetingArtifact, error) {
		return []*MeetingArtifact{testArtifact(t, "recording", "rec-1", "../../nested/recording.mp4", "https://files.example/recording.mp4", nil)}, nil
	}
	var downloadedTargets []string
	pipelineDownloadRecordingArtifact = func(_ context.Context, _ graphClient, _ *TeamsMeetingRef, _ *MeetingArtifact, destination string) (*pyjson.Object, error) {
		downloadedTargets = append(downloadedTargets, destination)
		if err := os.WriteFile(destination, []byte("video-bytes"), 0o644); err != nil {
			return nil, err
		}
		return obj("path", destination, "size_bytes", int64(11), "content_type", "video/mp4"), nil
	}
	pipelinePrepareAudioPath = func(_ *TeamsMeetingPipeline, _ context.Context, recordingPath string) (string, error) {
		audioPath := strings.TrimSuffix(recordingPath, filepath.Ext(recordingPath)) + ".wav"
		return audioPath, os.WriteFile(audioPath, []byte("audio-bytes"), 0o644)
	}
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipeline, store := testPipeline(t, obj("notion", obj("enabled", true, "database_id", "db-1"), "teams_delivery", obj("enabled", true, "channel_id", "channel-1")))
	pipeline.TranscribeFn = func(context.Context, string, string) (map[string]any, error) {
		return map[string]any{"success": true, "transcript": "Action: Follow up with Legal.\nRisk: Budget approval pending.", "provider": "local"}, nil
	}
	pipeline.SummarizeFn = func(_ context.Context, resolved *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error) {
		return &TeamsMeetingSummaryPayload{MeetingRef: resolved, Title: "Weekly Sync", TranscriptText: transcriptText, Summary: "Fallback summary",
			KeyDecisions: []any{}, ActionItems: []any{"Follow up with Legal."}, Risks: []any{"Budget approval pending."},
			Confidence: "medium", ConfidenceNotes: "Generated from STT fallback.", SourceArtifacts: artifacts}, nil
	}
	pipeline.NotionWriter = fakeNotionWriter{}
	pipeline.TeamsSender = SinkFn(func(_ context.Context, _ *TeamsMeetingSummaryPayload, _, existingRecord *pyjson.Object) (*pyjson.Object, error) {
		var messageID any = "msg-1"
		if existingRecord != nil && existingRecord.Len() > 0 {
			messageID = get(existingRecord, "message_id")
		}
		return obj("message_id", messageID), nil
	})

	job, err := pipeline.RunNotification(context.Background(),
		meetingNotification("notif-2", "updated", "communications/onlineMeetings/meeting-456", obj("id", "meeting-456")))
	if err != nil {
		t.Fatal(err)
	}

	if job.Status != "completed" || job.SelectedArtifactStrategy != "recording_stt_fallback" {
		t.Fatalf("status %q strategy %v error %v", job.Status, job.SelectedArtifactStrategy, job.ErrorInfo)
	}
	if job.SummaryPayload == nil || job.SummaryPayload.Summary != "Fallback summary" {
		t.Fatalf("summary payload %+v", job.SummaryPayload)
	}
	if len(downloadedTargets) == 0 || filepath.Base(downloadedTargets[0]) != "recording.mp4" || strings.Contains(downloadedTargets[0], "nested") {
		t.Fatalf("downloaded targets %v", downloadedTargets)
	}
	notionRecord, teamsRecord := store.GetSinkRecord("notion:meeting-456"), store.GetSinkRecord("teams:meeting-456")
	if notionRecord == nil || get(notionRecord, "page_id") != "page-1" {
		t.Fatalf("notion record %v", notionRecord)
	}
	if teamsRecord == nil || get(teamsRecord, "message_id") != "msg-1" {
		t.Fatalf("teams record %v", teamsRecord)
	}
}

func TestTeamsMeetingPipelineMissingTranscriptAndRecordingSchedulesRetry(t *testing.T) {
	keepPipelineSeams(t)
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
		return nil, "", nil
	}
	pipelineListRecordingArtifacts = func(context.Context, graphClient, *TeamsMeetingRef) ([]*MeetingArtifact, error) {
		return []*MeetingArtifact{}, nil
	}
	pipeline, _ := testPipeline(t, obj())
	pipeline.SummarizeFn = func(context.Context, *TeamsMeetingRef, string, []*MeetingArtifact) (any, error) { return nil, nil }

	job, err := pipeline.RunNotification(context.Background(),
		meetingNotification("notif-3", "updated", "communications/onlineMeetings/meeting-789", obj("id", "meeting-789")))
	if err != nil {
		t.Fatal(err)
	}

	if job.Status != "retry_scheduled" {
		t.Fatalf("status %q error %v", job.Status, job.ErrorInfo)
	}
	if get(job.ErrorInfo, "retryable") != true {
		t.Fatalf("error info %v", job.ErrorInfo)
	}
	if message, _ := get(job.ErrorInfo, "message").(string); !strings.Contains(message, "Recording unavailable") {
		t.Fatalf("message %q", message)
	}
}

func TestTeamsMeetingPipelineDuplicateNotificationReusesCompletedJob(t *testing.T) {
	keepPipelineSeams(t)
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
		return testArtifact(t, "transcript", "tx-dup", "meeting.vtt", nil, nil),
			"Decision: Keep duplicate notifications idempotent.\nAction: Verify the cached job is reused.", nil
	}
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	summarizeCalls := 0
	pipeline, store := testPipeline(t, obj("transcript_min_chars", int64(20)))
	pipeline.SummarizeFn = func(_ context.Context, resolved *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error) {
		summarizeCalls++
		return &TeamsMeetingSummaryPayload{MeetingRef: resolved, Title: "Weekly Sync", TranscriptText: transcriptText, Summary: "Duplicate-safe summary",
			KeyDecisions: []any{"Keep duplicate notifications idempotent."}, ActionItems: []any{"Verify the cached job is reused."},
			Confidence: "high", ConfidenceNotes: "Transcript available.", SourceArtifacts: artifacts}, nil
	}
	notification := meetingNotification("notif-dup", "updated", "communications/onlineMeetings/meeting-dup", obj("id", "meeting-dup"))

	firstJob, err := pipeline.RunNotification(context.Background(), notification)
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := pipeline.RunNotification(context.Background(), notification)
	if err != nil {
		t.Fatal(err)
	}

	if firstJob.Status != "completed" || secondJob.Status != "completed" {
		t.Fatalf("statuses %q %q error %v", firstJob.Status, secondJob.Status, firstJob.ErrorInfo)
	}
	if secondJob.JobID != firstJob.JobID {
		t.Fatalf("job ids %q %q", firstJob.JobID, secondJob.JobID)
	}
	if summarizeCalls != 1 {
		t.Fatalf("summarize calls %d", summarizeCalls)
	}
	if n := store.ListJobs().Len(); n != 1 {
		t.Fatalf("jobs %d", n)
	}
	receiptKey, err := BuildNotificationReceiptKey(notification)
	if err != nil {
		t.Fatal(err)
	}
	if recorded, err := store.RecordNotificationReceipt(receiptKey, nil, ""); err != nil || recorded {
		t.Fatalf("recorded %v err %v", recorded, err)
	}
}

const testTranscriptID = "ktVizInGAAAAi_B6lATZRTE5Om1lZXRpbmdfTranscriptV2="

func TestCreateJobFromGetAllTranscriptsUsesMeetingIDNotTranscriptID(t *testing.T) {
	pipeline, _ := testPipeline(t, nil)

	job, err := pipeline.CreateJobFromNotification(obj("id", "notif-tx", "changeType", "created",
		"resource", "communications/onlineMeetings/getAllTranscripts",
		"resourceData", obj("id", testTranscriptID, "@odata.type", "#Microsoft.Graph.callTranscript",
			"@odata.id", "users/976f4b31-fd01-4e0b-9178-29cc40c14438/onlineMeetings('MSo-meeting-id')/transcripts('"+testTranscriptID+"')"),
		"tenantId", "tenant-1"))
	if err != nil {
		t.Fatal(err)
	}

	if job.MeetingRef == nil {
		t.Fatal("no meeting_ref")
	}
	if job.MeetingRef.MeetingID != "MSo-meeting-id" || job.MeetingRef.MeetingID == testTranscriptID {
		t.Fatalf("meeting id %q", job.MeetingRef.MeetingID)
	}
	if job.MeetingRef.OrganizerUserID != "976f4b31-fd01-4e0b-9178-29cc40c14438" {
		t.Fatalf("organizer %v", job.MeetingRef.OrganizerUserID)
	}
	if got := get(job.MeetingRef.Metadata, "transcript_id"); got != testTranscriptID {
		t.Fatalf("transcript id %v", got)
	}
}

func TestCreateJobFromQuotedUserOdataIDUsesMeetingIDNotTranscriptID(t *testing.T) {
	pipeline, _ := testPipeline(t, nil)

	job, err := pipeline.CreateJobFromNotification(obj("id", "notif-quoted-user", "changeType", "created",
		"resource", "communications/onlineMeetings/getAllTranscripts",
		"resourceData", obj("id", testTranscriptID, "@odata.type", "#Microsoft.Graph.callTranscript",
			"@odata.id", "users('976f4b31-fd01-4e0b-9178-29cc40c14438')/onlineMeetings('MSo-meeting-id')/transcripts('"+testTranscriptID+"')"),
		"tenantId", "tenant-1"))
	if err != nil {
		t.Fatal(err)
	}

	if job.MeetingRef == nil {
		t.Fatal("no meeting_ref")
	}
	if job.MeetingRef.MeetingID != "MSo-meeting-id" || job.MeetingRef.MeetingID == testTranscriptID {
		t.Fatalf("meeting id %q", job.MeetingRef.MeetingID)
	}
	if job.MeetingRef.OrganizerUserID != "976f4b31-fd01-4e0b-9178-29cc40c14438" {
		t.Fatalf("organizer %v", job.MeetingRef.OrganizerUserID)
	}
	if got := get(job.MeetingRef.Metadata, "transcript_id"); got != testTranscriptID {
		t.Fatalf("transcript id %v", got)
	}
}

func TestRunJobReparsesQuotedUserOdataIDWhenStoredMeetingIDIsTranscript(t *testing.T) {
	keepPipelineSeams(t)
	captured := map[string]any{}
	pipelineResolveMeetingReference = func(_ context.Context, _ graphClient, meetingID, _, tenantID, organizerUserID string) (*TeamsMeetingRef, error) {
		captured["meeting_id"], captured["organizer_user_id"] = noneIfEmpty(meetingID), noneIfEmpty(organizerUserID)
		return newMeetingRef(meetingID, noneIfEmpty(organizerUserID), nil, nil, nil, noneIfEmpty(tenantID), obj("subject", "Standup"))
	}
	pipelineFetchPreferredTranscriptText = func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
		return testArtifact(t, "transcript", "tx-1", "meeting.vtt", nil, nil),
			"Decision: Re-parse stored Graph notifications on replay.\nAction: Confirm the meeting id is used.", nil
	}
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipeline, store := testPipeline(t, obj("transcript_min_chars", int64(20)))
	pipeline.SummarizeFn = func(_ context.Context, resolved *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error) {
		return &TeamsMeetingSummaryPayload{MeetingRef: resolved, Title: "Standup", TranscriptText: transcriptText, Summary: "Reparsed", SourceArtifacts: artifacts}, nil
	}
	job, err := pipeline.CreateJobFromNotification(obj("id", "notif-replay", "changeType", "created",
		"resource", "communications/onlineMeetings/getAllTranscripts",
		"resourceData", obj("id", testTranscriptID, "@odata.type", "#Microsoft.Graph.callTranscript",
			"@odata.id", "users('org-1')/onlineMeetings('MSo-from-odata')/transcripts('"+testTranscriptID+"')")))
	if err != nil {
		t.Fatal(err)
	}
	if job.MeetingRef == nil {
		t.Fatal("no meeting_ref")
	}
	job.MeetingRef.MeetingID = testTranscriptID
	if _, err := store.UpsertJob(job.JobID, job.ToDict()); err != nil {
		t.Fatal(err)
	}

	ran, err := pipeline.RunJob(context.Background(), job.JobID)
	if err != nil {
		t.Fatal(err)
	}

	if captured["meeting_id"] != "MSo-from-odata" || captured["organizer_user_id"] != "org-1" {
		t.Fatalf("captured %v", captured)
	}
	if ran.Status != "completed" {
		t.Fatalf("status %q error %v", ran.Status, ran.ErrorInfo)
	}
}

func TestCreateJobFromUsersResourcePathWithoutOdataID(t *testing.T) {
	pipeline, _ := testPipeline(t, nil)

	job, err := pipeline.CreateJobFromNotification(obj("id", "notif-resource-path", "changeType", "created",
		"resource", "users/org-2/onlineMeetings('MSo-from-resource')/transcripts('tx-99')",
		"resourceData", obj("id", "tx-99", "@odata.type", "#Microsoft.Graph.callTranscript")))
	if err != nil {
		t.Fatal(err)
	}

	if job.MeetingRef == nil {
		t.Fatal("no meeting_ref")
	}
	if job.MeetingRef.MeetingID != "MSo-from-resource" || job.MeetingRef.OrganizerUserID != "org-2" {
		t.Fatalf("meeting ref %+v", job.MeetingRef)
	}
	if got := get(job.MeetingRef.Metadata, "transcript_id"); got != "tx-99" {
		t.Fatalf("transcript id %v", got)
	}
}
