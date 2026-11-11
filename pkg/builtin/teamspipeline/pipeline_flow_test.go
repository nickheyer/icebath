package teamspipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const longTranscript = "Decision: keep the transcript first path.\nAction: write the remaining cases.\nMore detail for the summary."

func transcriptFetcher(t *testing.T, text string) func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
	return func(context.Context, graphClient, *TeamsMeetingRef) (*MeetingArtifact, string, error) {
		if text == "" {
			return nil, "", nil
		}
		return testArtifact(t, "transcript", "tx-1", "meeting.vtt", nil, nil), text, nil
	}
}

func echoSummarizer(_ context.Context, resolved *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error) {
	return &TeamsMeetingSummaryPayload{MeetingRef: resolved, Title: "T", TranscriptText: transcriptText, Summary: "ok", SourceArtifacts: artifacts}, nil
}

func flowNotification(id string) *pyjson.Object {
	return meetingNotification(id, "updated", "communications/onlineMeetings/meeting-1", obj("id", "meeting-1"))
}

func TestRunJobRecordsFailedAndRetryStates(t *testing.T) {
	keepPipelineSeams(t)
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipelineResolveMeetingReference = func(context.Context, graphClient, string, string, string, string) (*TeamsMeetingRef, error) {
		return nil, &TeamsMeetingNotFoundError{TeamsMeetingError{"Teams meeting not found: meeting-1"}}
	}
	pipeline, store := testPipeline(t, nil)
	job, err := pipeline.RunNotification(context.Background(), flowNotification("n-fail"))
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "failed" || !pyjson.Equal(job.ErrorInfo, obj("message", "Teams meeting not found: meeting-1", "type", "TeamsMeetingNotFoundError")) {
		t.Fatalf("failed job %q %v", job.Status, pyrepr.Repr(job.ErrorInfo))
	}
	if again, err := pipeline.RunNotification(context.Background(), flowNotification("n-fail")); err != nil || again.JobID != job.JobID || again.Status != "failed" {
		t.Fatalf("terminal duplicate %+v err %v", again, err)
	}

	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = transcriptFetcher(t, "")
	required, _ := testPipeline(t, obj("transcript_required", true))
	if job, err = required.RunNotification(context.Background(), flowNotification("n-req")); err != nil || job.Status != "retry_scheduled" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "Transcript unavailable for meeting meeting-1.", "retryable", true)) {
		t.Fatalf("required %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
	noFallback, _ := testPipeline(t, obj("transcription_fallback", false))
	if job, err = noFallback.RunNotification(context.Background(), flowNotification("n-nofb")); err != nil || job.Status != "retry_scheduled" ||
		get(job.ErrorInfo, "message") != "No transcript available and transcription fallback disabled for meeting-1." {
		t.Fatalf("no fallback %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}

	var pipelineErr *TeamsPipelineError
	if _, err := pipeline.RunJob(context.Background(), "nope"); !errors.As(err, &pipelineErr) || err.Error() != "Unknown Teams pipeline job: nope" {
		t.Fatalf("unknown job %v", err)
	}
	if _, err := pipeline.RunJob(context.Background(), &TeamsMeetingPipelineJob{JobID: "j-1"}); !errors.As(err, &pipelineErr) || err.Error() != "Job j-1 has no meeting_ref." {
		t.Fatalf("no meeting_ref %v", err)
	}
	if store.GetJob("nope") != nil || store.GetJob("j-1") != nil {
		t.Fatal("refused job stored")
	}
}

func TestRunJobSummaryResultShapesAndStoredNotification(t *testing.T) {
	keepPipelineSeams(t)
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = transcriptFetcher(t, longTranscript)
	pipeline, store := testPipeline(t, obj("transcript_min_chars", int64(20)))
	pipeline.SummarizeFn = func(_ context.Context, resolved *TeamsMeetingRef, _ string, _ []*MeetingArtifact) (any, error) {
		return map[string]any{"meeting_ref": map[string]any{"meeting_id": resolved.MeetingID, "metadata": resolved.Metadata}, "summary": "from dict", "keyDecisions": []any{"d"}}, nil
	}
	job, err := pipeline.RunNotification(context.Background(), flowNotification("n-dict"))
	if err != nil || job.Status != "completed" || job.SummaryPayload.Summary != "from dict" || !reflect.DeepEqual(job.SummaryPayload.KeyDecisions, []any{"d"}) {
		t.Fatalf("dict summary %+v err %v", job, err)
	}
	if got := get(get(store.GetJob(job.JobID), "meeting_ref").(*pyjson.Object), "metadata").(*pyjson.Object); get(got, "subject") != "Weekly Sync" || get(got, "notification") == nil {
		t.Fatalf("merged metadata %v", pyrepr.Repr(got))
	}

	pipeline.SummarizeFn = func(context.Context, *TeamsMeetingRef, string, []*MeetingArtifact) (any, error) { return nil, nil }
	if job, err = pipeline.RunNotification(context.Background(), flowNotification("n-none")); err != nil || job.Status != "failed" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "'NoneType' object is not a mapping", "type", "TypeError")) {
		t.Fatalf("none summary %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
	pipeline.SummarizeFn = func(context.Context, *TeamsMeetingRef, string, []*MeetingArtifact) (any, error) {
		return obj("summary", "x"), nil
	}
	if job, err = pipeline.RunNotification(context.Background(), flowNotification("n-noref")); err != nil || job.Status != "failed" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "'meeting_ref'", "type", "KeyError")) {
		t.Fatalf("summary without meeting_ref %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}

	pipeline.SummarizeFn = echoSummarizer
	bare := &TeamsMeetingPipelineJob{JobID: "teams-job-bare", EventID: "e", DedupeKey: "e", SourceEventType: "manual", Status: "received",
		MeetingRef: &TeamsMeetingRef{MeetingID: "meeting-1", Metadata: obj("join_web_url", nil)}}
	if job, err = pipeline.RunJob(context.Background(), bare); err != nil || job.Status != "failed" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "'NoneType' object has no attribute 'get'", "type", "AttributeError")) {
		t.Fatalf("job without stored notification %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pipelineResolveMeetingReference = func(ctx context.Context, _ graphClient, _, _, _, _ string) (*TeamsMeetingRef, error) {
		cancel()
		return nil, ctx.Err()
	}
	created, err := pipeline.CreateJobFromNotification(flowNotification("n-cancel"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunJob(ctx, created.JobID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run %v", err)
	}
	if status := get(store.GetJob(created.JobID), "status"); status != "resolving_meeting" {
		t.Fatalf("cancelled job status %v", status)
	}
}

func TestShortTranscriptFallsBackToRecordingAndSTTFailuresRetry(t *testing.T) {
	keepPipelineSeams(t)
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = transcriptFetcher(t, "  too short  ")
	displayName := any("..")
	pipelineListRecordingArtifacts = func(context.Context, graphClient, *TeamsMeetingRef) ([]*MeetingArtifact, error) {
		return []*MeetingArtifact{testArtifact(t, "recording", "rec-7", displayName, nil, nil), testArtifact(t, "recording", "rec-8", "second.mp4", nil, nil)}, nil
	}
	var targets []string
	pipelineDownloadRecordingArtifact = func(_ context.Context, _ graphClient, _ *TeamsMeetingRef, recording *MeetingArtifact, destination string) (*pyjson.Object, error) {
		targets = append(targets, recording.ArtifactID+"="+destination)
		return obj("path", destination), os.WriteFile(destination, []byte("v"), 0o644)
	}
	tmpRoot := filepath.Join(t.TempDir(), "scratch", "deep")
	pipeline, store := testPipeline(t, obj("tmp_dir", tmpRoot, "ffmpeg_extract_audio", false, "stt_model", "whisper-x"))
	pipeline.SummarizeFn = echoSummarizer
	result := map[string]any{"success": true, "transcript": "  spoken words  "}
	var transcribed []string
	pipeline.TranscribeFn = func(_ context.Context, filePath, model string) (map[string]any, error) {
		transcribed = append(transcribed, model+"="+filePath)
		inFlight := 0
		for _, stored := range store.ListJobs().Vals {
			if get(stored.(*pyjson.Object), "status") == "transcribing_audio" {
				inFlight++
			}
		}
		if inFlight != 1 {
			t.Errorf("%d jobs transcribing during speech to text", inFlight)
		}
		return result, nil
	}

	job, err := pipeline.RunNotification(context.Background(), flowNotification("n-short"))
	if err != nil || job.Status != "completed" || job.SelectedArtifactStrategy != "recording_stt_fallback" || job.SummaryPayload.TranscriptText != "spoken words" {
		t.Fatalf("job %+v err %v", job, err)
	}
	if len(job.SummaryPayload.SourceArtifacts) != 2 || job.SummaryPayload.SourceArtifacts[0].ArtifactID != "tx-1" || job.SummaryPayload.SourceArtifacts[1].ArtifactID != "rec-7" {
		t.Fatalf("artifacts %+v", job.SummaryPayload.SourceArtifacts)
	}
	if len(targets) != 1 || !strings.HasPrefix(targets[0], "rec-7="+filepath.Join(tmpRoot, "teams-recording-")) || filepath.Base(targets[0]) != "rec-7.mp4" {
		t.Fatalf("targets %v", targets)
	}
	if len(transcribed) != 1 || transcribed[0] != "whisper-x="+strings.TrimPrefix(targets[0], "rec-7=") {
		t.Fatalf("transcribed %v", transcribed)
	}
	if left, err := os.ReadDir(tmpRoot); err != nil || len(left) != 0 {
		t.Fatalf("scratch left %v err %v", left, err)
	}

	displayName = "clip.MP4"
	result = map[string]any{"success": false, "error": "provider down"}
	if job, err = pipeline.RunNotification(context.Background(), flowNotification("n-stt")); err != nil || job.Status != "retry_scheduled" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "provider down", "retryable", true)) {
		t.Fatalf("stt failure %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
	if filepath.Base(targets[1]) != "clip.MP4" {
		t.Fatalf("targets %v", targets)
	}
	result = map[string]any{"success": false}
	if job, _ = pipeline.RunNotification(context.Background(), flowNotification("n-stt2")); get(job.ErrorInfo, "message") != "Unknown STT failure" {
		t.Fatalf("stt failure without error %v", pyrepr.Repr(job.ErrorInfo))
	}
	result = map[string]any{"success": true, "transcript": "   "}
	if job, _ = pipeline.RunNotification(context.Background(), flowNotification("n-stt3")); job.Status != "retry_scheduled" || get(job.ErrorInfo, "message") != "STT returned an empty transcript." {
		t.Fatalf("empty transcript %q %v", job.Status, pyrepr.Repr(job.ErrorInfo))
	}
}

func TestPrepareAudioPathKeepsAudioAndNeedsFFmpeg(t *testing.T) {
	pipeline, _ := testPipeline(t, nil)
	for _, name := range []string{"/x/a.MP3", "/x/b.webm", "/x/c.wav"} {
		if got, err := pipeline.prepareAudioPath(context.Background(), name); err != nil || got != name {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	var retryable *TeamsPipelineRetryableError
	if _, err := pipeline.prepareAudioPath(context.Background(), "/x/video.mp4"); !errors.As(err, &retryable) ||
		err.Error() != "Recording fallback requires ffmpeg for audio extraction, but ffmpeg was not found." {
		t.Fatalf("missing ffmpeg %v", err)
	}
	pipeline.Config.FFmpegExtractAudio = false
	if got, err := pipeline.prepareAudioPath(context.Background(), "/x/video.mp4"); err != nil || got != "/x/video.mp4" {
		t.Fatalf("extraction off %q %v", got, err)
	}
}

type recordingSink struct {
	name  string
	store *TeamsPipelineStore
	seen  *[]string
	fail  error
}

func (s recordingSink) WriteSummary(_ context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error) {
	jobs := s.store.ListJobs()
	*s.seen = append(*s.seen, s.name+":"+pyrepr.Str(get(jobs.Vals[jobs.Keys[0]].(*pyjson.Object), "status"))+":"+pyrepr.Str(get(existingRecord, "n"))+":"+pyrepr.Str(get(config, "tag")))
	if s.fail != nil {
		return nil, s.fail
	}
	n := int64(1)
	if existingRecord != nil {
		n = get(existingRecord, "n").(int64) + 1
	}
	return obj("n", n, "meeting", payload.MeetingRef.MeetingID), nil
}

func TestWriteSinksRunsEnabledSinksInOrderWithExistingRecords(t *testing.T) {
	keepPipelineSeams(t)
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = transcriptFetcher(t, longTranscript)
	pipeline, store := testPipeline(t, map[string]any{"transcript_min_chars": int64(20), "notion": map[string]any{"enabled": true, "tag": "n"},
		"linear": map[string]any{"enabled": false, "tag": "l"}, "teams_delivery": obj("enabled", "yes", "tag", "t")})
	pipeline.SummarizeFn = echoSummarizer
	var seen []string
	pipeline.NotionWriter = recordingSink{name: "notion", store: store, seen: &seen}
	pipeline.LinearWriter = recordingSink{name: "linear", store: store, seen: &seen}
	pipeline.TeamsSender = recordingSink{name: "teams", store: store, seen: &seen}

	job, err := pipeline.RunNotification(context.Background(), flowNotification("n-sinks"))
	if err != nil || job.Status != "completed" {
		t.Fatalf("job %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
	if want := []string{"notion:writing_notion:None:n", "teams:sending_teams:None:t"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen %v", seen)
	}
	if record := store.GetSinkRecord("notion:meeting-1"); get(record, "n") != int64(1) || get(record, "sink_key") != "notion:meeting-1" || store.GetSinkRecord("linear:meeting-1") != nil {
		t.Fatalf("records %v", pyrepr.Repr(record))
	}

	seen = nil
	if job, err = pipeline.RunJob(context.Background(), job.JobID); err != nil || job.Status != "completed" {
		t.Fatalf("replay %q err %v", job.Status, err)
	}
	if want := []string{"notion:writing_notion:1:n", "teams:sending_teams:1:t"}; !reflect.DeepEqual(seen, want) || get(store.GetSinkRecord("teams:meeting-1"), "n") != int64(2) {
		t.Fatalf("replay seen %v", seen)
	}

	pipeline.NotionWriter = recordingSink{name: "notion", store: store, seen: &seen, fail: &TeamsPipelineSinkError{TeamsPipelineError{"NOTION_API_KEY is not configured."}}}
	if job, err = pipeline.RunJob(context.Background(), job.JobID); err != nil || job.Status != "failed" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "NOTION_API_KEY is not configured.", "type", "TeamsPipelineSinkError")) {
		t.Fatalf("sink failure %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
	pipeline.Config.Notion = "on"
	if job, err = pipeline.RunJob(context.Background(), job.JobID); err != nil || job.Status != "failed" || get(job.ErrorInfo, "type") != "AttributeError" ||
		get(job.ErrorInfo, "message") != "'str' object has no attribute 'get'" {
		t.Fatalf("string sink config %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
}

func TestRunJobRecordsRaisedPanicsAndMissingTranscriptionResult(t *testing.T) {
	keepPipelineSeams(t)
	pipelineEnrichMeetingWithCallRecord = noCallRecord
	pipelineResolveMeetingReference = transcriptMeetingResolver
	pipelineFetchPreferredTranscriptText = transcriptFetcher(t, longTranscript)
	pipeline, store := testPipeline(t, obj("transcript_min_chars", int64(20), "ffmpeg_extract_audio", false, "tmp_dir", t.TempDir()))
	pipeline.SummarizeFn = func(context.Context, *TeamsMeetingRef, string, []*MeetingArtifact) (any, error) {
		panic(pyError("ValueError", "summarizer exploded"))
	}
	job, err := pipeline.RunNotification(context.Background(), flowNotification("n-panic"))
	if err != nil || job.Status != "failed" || !pyjson.Equal(job.ErrorInfo, obj("message", "summarizer exploded", "type", "ValueError")) {
		t.Fatalf("raised error %+v err %v", job, err)
	}
	pipeline.SummarizeFn = func(context.Context, *TeamsMeetingRef, string, []*MeetingArtifact) (any, error) { panic("plain text") }
	if job, err = pipeline.RunNotification(context.Background(), flowNotification("n-panic-text")); err != nil || job.Status != "failed" || get(job.ErrorInfo, "message") != "plain text" {
		t.Fatalf("raised text %+v err %v", job, err)
	}
	if get(store.GetJob(job.JobID), "status") != "failed" {
		t.Fatal("failed state not stored")
	}

	pipelineFetchPreferredTranscriptText = transcriptFetcher(t, "")
	pipelineListRecordingArtifacts = func(context.Context, graphClient, *TeamsMeetingRef) ([]*MeetingArtifact, error) {
		return []*MeetingArtifact{testArtifact(t, "recording", "rec-1", "r.mp4", nil, nil)}, nil
	}
	pipelineDownloadRecordingArtifact = func(_ context.Context, _ graphClient, _ *TeamsMeetingRef, _ *MeetingArtifact, destination string) (*pyjson.Object, error) {
		return obj("path", destination), nil
	}
	pipeline.TranscribeFn = func(context.Context, string, string) (map[string]any, error) { return nil, nil }
	if job, err = pipeline.RunNotification(context.Background(), flowNotification("n-nil-stt")); err != nil || job.Status != "failed" ||
		!pyjson.Equal(job.ErrorInfo, obj("message", "'NoneType' object has no attribute 'get'", "type", "AttributeError")) {
		t.Fatalf("missing transcription result %q %v err %v", job.Status, pyrepr.Repr(job.ErrorInfo), err)
	}
}

func TestSummarySinksKeepNonTextSummaryErrorsOfTheWriters(t *testing.T) {
	payload := sinkPayload()
	payload.Summary = int64(5)
	if _, err := renderSummaryMarkdown(payload); pyrepr.ExceptionName(err) != "TypeError" || pyrepr.ExceptionMessage(err) != "sequence item 3: expected str instance, int found" {
		t.Fatalf("markdown %v", err)
	}
	transport := &sinkTransport{status: 200, body: `{"id": "p"}`}
	writer, err := NewNotionWriter(context.Background(), "k", transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteSummary(context.Background(), payload, obj("database_id", "db", "summary_property", "Summary"), nil); pyrepr.ExceptionName(err) != "TypeError" ||
		pyrepr.ExceptionMessage(err) != "'int' object is not subscriptable" {
		t.Fatalf("summary property %v", err)
	}
	if _, err := writer.WriteSummary(context.Background(), payload, obj("database_id", "db"), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transport.bodies[0], `"paragraph":{"rich_text":[{"text":{"content":5}}]}`) {
		t.Fatalf("body %s", transport.bodies[0])
	}
	payload.Summary, payload.ConfidenceNotes = "S", []any{"n"}
	if _, err := renderSummaryMarkdown(payload); pyrepr.ExceptionMessage(err) != "sequence item 15: expected str instance, list found" {
		t.Fatalf("notes %v", err)
	}
	payload.ConfidenceNotes, payload.MeetingRef = nil, nil
	if _, err := renderSummaryMarkdown(payload); pyrepr.ExceptionName(err) != "AttributeError" {
		t.Fatalf("payload without meeting_ref %v", err)
	}
	payload.Title = "Named"
	if text, err := renderSummaryMarkdown(payload); err != nil || !strings.HasPrefix(text, "# Named\n\n## Summary\nS\n") {
		t.Fatalf("titled %q %v", text, err)
	}
}
