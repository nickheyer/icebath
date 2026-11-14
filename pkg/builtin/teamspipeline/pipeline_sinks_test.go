package teamspipeline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Transport double answering every sink request with one status and JSON body, recording what was sent
type sinkTransport struct {
	status   int
	body     string
	requests []*http.Request
	bodies   []string
}

func (s *sinkTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	s.requests, s.bodies = append(s.requests, r), append(s.bodies, string(raw))
	return &http.Response{StatusCode: s.status, Status: strconv.Itoa(s.status) + " " + http.StatusText(s.status), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(s.body)), Request: r}, nil
}

func sinkPayload() *TeamsMeetingSummaryPayload {
	return &TeamsMeetingSummaryPayload{MeetingRef: &TeamsMeetingRef{MeetingID: "m-1"}, Summary: "S",
		KeyDecisions: []any{"A", "B"}, ActionItems: []any{}, Risks: []any{}}
}

func TestNotionWriterCreatesPageThenUpdatesOnlyProperties(t *testing.T) {
	transport := &sinkTransport{status: 200, body: `{"id": "page-9", "url": "https://notion.so/page-9"}`}
	writer, err := NewNotionWriter(context.Background(), " secret-key ", transport)
	if err != nil {
		t.Fatal(err)
	}
	config := obj("database_id", " db-1 ", "summary_property", "Summary", "meeting_id_property", "Meeting")

	record, err := writer.WriteSummary(context.Background(), sinkPayload(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(record, obj("page_id", "page-9", "url", "https://notion.so/page-9")) {
		t.Fatalf("record %v", pyrepr.Repr(record))
	}
	request := transport.requests[0]
	if request.Method != "POST" || request.URL.String() != "https://api.notion.com/v1/pages" {
		t.Fatalf("request %s %s", request.Method, request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer secret-key" || request.Header.Get("Notion-Version") != "2025-09-03" ||
		request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", request.Header)
	}
	text := func(content string) string { return `{"rich_text":[{"text":{"content":"` + content + `"}}]}` }
	block := func(heading, body string) string {
		return `{"object":"block","type":"heading_2","heading_2":` + text(heading) + `},{"object":"block","type":"paragraph","paragraph":` + text(body) + `}`
	}
	properties := `{"Name":{"title":[{"text":{"content":"Meeting m-1"}}]},"Summary":` + text("S") + `,"Meeting":` + text("m-1") + `}`
	want := `{"parent":{"database_id":"db-1"},"properties":` + properties + `,"children":[` + block("Summary", "S") + "," +
		block("Key Decisions", `- A\n- B`) + "," + block("Action Items", "None") + "," + block("Risks", "None") + `]}`
	if transport.bodies[0] != want {
		t.Fatalf("body\n got %s\nwant %s", transport.bodies[0], want)
	}

	record, err = writer.WriteSummary(context.Background(), sinkPayload(), obj("title_property", "Title"), obj("page_id", "page-9"))
	if err != nil {
		t.Fatal(err)
	}
	if get(record, "page_id") != "page-9" {
		t.Fatalf("record %v", pyrepr.Repr(record))
	}
	if request = transport.requests[1]; request.Method != "PATCH" || request.URL.String() != "https://api.notion.com/v1/pages/page-9" {
		t.Fatalf("request %s %s", request.Method, request.URL)
	}
	if want = `{"properties":{"Title":{"title":[{"text":{"content":"Meeting m-1"}}]}}}`; transport.bodies[1] != want {
		t.Fatalf("body\n got %s\nwant %s", transport.bodies[1], want)
	}
}

func TestNotionWriterFailures(t *testing.T) {
	t.Setenv("NOTION_API_KEY", "")
	unkeyed, err := NewNotionWriter(context.Background(), "", &sinkTransport{status: 200, body: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	var sinkErr *TeamsPipelineSinkError
	if _, err := unkeyed.WriteSummary(context.Background(), sinkPayload(), obj("database_id", "db"), nil); !errors.As(err, &sinkErr) || err.Error() != "NOTION_API_KEY is not configured." {
		t.Fatalf("unkeyed %v", err)
	}
	t.Setenv("NOTION_API_KEY", " from-env ")
	keyed, err := NewNotionWriter(context.Background(), "", &sinkTransport{status: 200, body: `{"url": "u"}`})
	if err != nil || keyed.APIKey != "from-env" {
		t.Fatalf("key %q err %v", keyed.APIKey, err)
	}
	if _, err := keyed.WriteSummary(context.Background(), sinkPayload(), obj(), nil); !errors.As(err, &sinkErr) || err.Error() != "Notion sink requires database_id or an existing page_id." {
		t.Fatalf("no target %v", err)
	}
	if _, err := keyed.WriteSummary(context.Background(), sinkPayload(), obj("databaseId", "db"), nil); pyrepr.ExceptionName(err) != "KeyError" || pyrepr.ExceptionMessage(err) != "'id'" {
		t.Fatalf("missing id %v", err)
	}
	refused, _ := NewNotionWriter(context.Background(), "k", &sinkTransport{status: 400, body: `{"message": "bad"}`})
	_, err = refused.WriteSummary(context.Background(), sinkPayload(), obj("database_id", "db"), nil)
	want := "Client error '400 Bad Request' for url 'https://api.notion.com/v1/pages'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/400"
	if pyrepr.ExceptionName(err) != "HTTPStatusError" || err.Error() != want {
		t.Fatalf("status error %v", err)
	}
}

func TestLinearWriterCreatesThenUpdatesIssue(t *testing.T) {
	transport := &sinkTransport{status: 200, body: `{"data": {"issueCreate": {"success": true, "issue": {"id": "iss-1", "identifier": "ENG-1", "url": "https://linear.app/i/ENG-1"}}}}`}
	writer, err := NewLinearWriter(context.Background(), "lin-key", transport)
	if err != nil {
		t.Fatal(err)
	}
	markdown := "# Meeting m-1\n\n## Summary\nS\n\n## Key Decisions\n- A\n- B\n\n## Action Items\n- None\n\n## Risks\n- None\n\nConfidence: unknown"
	if got, err := renderSummaryMarkdown(sinkPayload()); err != nil || got != markdown {
		t.Fatalf("markdown %q err %v", got, err)
	}

	record, err := writer.WriteSummary(context.Background(), sinkPayload(), obj("teamId", " team-1 "), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(record, obj("issue_id", "iss-1", "identifier", "ENG-1", "url", "https://linear.app/i/ENG-1")) {
		t.Fatalf("record %v", pyrepr.Repr(record))
	}
	request := transport.requests[0]
	if request.Method != "POST" || request.URL.String() != "https://api.linear.app/graphql" || request.Header.Get("Authorization") != "lin-key" {
		t.Fatalf("request %s %s %v", request.Method, request.URL, request.Header)
	}
	sent, err := pyjson.Loads(transport.bodies[0])
	if err != nil {
		t.Fatal(err)
	}
	wantBody := obj("query", "mutation($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier url } } }",
		"variables", obj("input", obj("teamId", "team-1", "title", "Meeting Summary: m-1", "description", markdown)))
	if !pyjson.Equal(sent, wantBody) || !reflect.DeepEqual(get(get(sent.(*pyjson.Object), "variables").(*pyjson.Object), "input").(*pyjson.Object).Keys, []string{"teamId", "title", "description"}) {
		t.Fatalf("body %s", transport.bodies[0])
	}

	transport.body = `{"data": {"issueUpdate": {"issue": {"id": "iss-1", "identifier": "ENG-1", "url": null}}}}`
	titled := sinkPayload()
	titled.Title, titled.Confidence, titled.ConfidenceNotes = "Weekly", "high", "Transcript available."
	if record, err = writer.WriteSummary(context.Background(), titled, obj(), obj("issue_id", "iss-1")); err != nil || get(record, "url") != nil {
		t.Fatalf("update record %v err %v", pyrepr.Repr(record), err)
	}
	sent, _ = pyjson.Loads(transport.bodies[1])
	variables := get(sent.(*pyjson.Object), "variables").(*pyjson.Object)
	if get(variables, "id") != "iss-1" || get(get(variables, "input").(*pyjson.Object), "title") != "Weekly" ||
		!strings.HasSuffix(get(get(variables, "input").(*pyjson.Object), "description").(string), "\n\nConfidence: high\nTranscript available.") ||
		!strings.HasPrefix(get(sent.(*pyjson.Object), "query").(string), "mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input)") {
		t.Fatalf("update body %s", transport.bodies[1])
	}

	var sinkErr *TeamsPipelineSinkError
	if _, err := writer.WriteSummary(context.Background(), sinkPayload(), obj(), nil); !errors.As(err, &sinkErr) || err.Error() != "Linear sink requires team_id when creating a new issue." {
		t.Fatalf("no team %v", err)
	}
	transport.body = `{"errors": [{"message": "denied"}]}`
	if _, err := writer.WriteSummary(context.Background(), sinkPayload(), obj("team_id", "t"), nil); !errors.As(err, &sinkErr) ||
		err.Error() != "Linear write failed: {'errors': [{'message': 'denied'}]}" {
		t.Fatalf("failed write %v", err)
	}
	transport.body = `[1]`
	if _, err := writer.WriteSummary(context.Background(), sinkPayload(), obj("team_id", "t"), nil); pyrepr.ExceptionName(err) != "AttributeError" {
		t.Fatalf("list payload %v", err)
	}
}

func TestSummaryParsingAndHeuristicFallback(t *testing.T) {
	parsed, err := parseSummaryJSON("```json\n{\"summary\": \" S \", \"key_decisions\": [\" a \", \"\", 3], \"confidence\": \"\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	want := summaryFields{Summary: "S", KeyDecisions: []any{"a", "3"}, ActionItems: []any{}, Risks: []any{}, Confidence: "medium"}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("parsed %+v", parsed)
	}
	empty, err := parseSummaryJSON("  ")
	if err != nil || empty.Summary != "Transcript unavailable or too sparse for a confident summary." || empty.Confidence != "low" ||
		empty.ConfidenceNotes != "Generated with heuristic fallback because no LLM summary response was available." {
		t.Fatalf("empty %+v err %v", empty, err)
	}
	if _, err := parseSummaryJSON("[1]"); pyrepr.ExceptionName(err) != "AttributeError" || pyrepr.ExceptionMessage(err) != "'list' object has no attribute 'get'" {
		t.Fatalf("list %v", err)
	}
	if _, err := parseSummaryJSON(`{"summary": "x", "risks": null}`); pyrepr.ExceptionName(err) != "TypeError" || pyrepr.ExceptionMessage(err) != "'NoneType' object is not iterable" {
		t.Fatalf("null risks %v", err)
	}
	if _, err := parseSummaryJSON("no json here"); err == nil {
		t.Fatal("prose parsed")
	}

	heuristic := heuristicSummary("- Action: do x\n* We decided to ship\nRISK: late\n\n\tfollow up: y\nline5")
	wantHeuristic := summaryFields{Summary: "Action: do x We decided to ship RISK: late", KeyDecisions: []any{"We decided to ship"},
		ActionItems: []any{"Action: do x", "follow up: y"}, Risks: []any{"RISK: late"}, Confidence: "low",
		ConfidenceNotes: "Generated with heuristic fallback because no LLM summary response was available."}
	if !reflect.DeepEqual(heuristic, wantHeuristic) {
		t.Fatalf("heuristic %+v", heuristic)
	}
	if long := heuristicSummary(strings.Repeat("decision ", 40)); long.Confidence != "medium" || len(long.KeyDecisions) != 1 {
		t.Fatalf("long %+v", long)
	}
	many := heuristicSummary(strings.Repeat("risk here\n", 9))
	if len(many.Risks) != 6 || many.Summary != "risk here risk here risk here" {
		t.Fatalf("many %+v", many)
	}
}

func TestTeamsPipelineConfigFromDictReadsSnakeAndCamelKeys(t *testing.T) {
	defaults, err := TeamsPipelineConfigFromDict(nil)
	if err != nil || !reflect.DeepEqual(defaults, NewTeamsPipelineConfig()) {
		t.Fatalf("defaults %+v err %v", defaults, err)
	}
	cfg, err := TeamsPipelineConfigFromDict(map[string]any{"transcript_preferred": 0, "transcript_required": "yes", "transcription_fallback": nil,
		"ffmpeg_extract_audio": false, "sttModel": "whisper-1", "transcript_min_chars": "12", "tmpDir": "/var/tmp//teams/",
		"notion": map[string]any{"enabled": true}, "teamsDelivery": map[string]any{"chat_id": "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TranscriptPreferred || !cfg.TranscriptRequired || cfg.TranscriptionFallback || cfg.FFmpegExtractAudio || cfg.STTModel != "whisper-1" ||
		cfg.TranscriptMinChars != 12 || cfg.TmpDir != filepath.FromSlash("/var/tmp/teams") || cfg.Linear != nil ||
		!reflect.DeepEqual(cfg.Notion, map[string]any{"enabled": true}) || !reflect.DeepEqual(cfg.TeamsDelivery, map[string]any{"chat_id": "c"}) {
		t.Fatalf("config %+v", cfg)
	}
	if _, err := TeamsPipelineConfigFromDict(obj("transcript_min_chars", "many")); err == nil {
		t.Fatal("bad transcript_min_chars accepted")
	}
	if _, err := TeamsPipelineConfigFromDict(obj("tmp_dir", int64(5))); pyrepr.ExceptionName(err) != "TypeError" {
		t.Fatalf("tmp_dir %v", err)
	}
	if _, err := TeamsPipelineConfigFromDict("on"); pyrepr.ExceptionName(err) != "ValueError" || pyrepr.ExceptionMessage(err) != "dictionary update sequence element #0 has length 1; 2 is required" {
		t.Fatalf("string config %v", err)
	}
	kept := NewTeamsPipelineConfig()
	kept.TranscriptMinChars = 5
	if pipeline, err := NewTeamsMeetingPipeline(&scriptGraph{}, nil, kept); err != nil || pipeline.Config != kept {
		t.Fatalf("config instance replaced, err %v", err)
	}
}

func TestMeetingIDsFromNotificationFallbacks(t *testing.T) {
	id, organizer, extra, err := meetingIDsFromNotification(obj("id", "n-1", "resource", "communications/onlineMeetings/getAllRecordings",
		"resourceData", obj("id", "rec-blob", "@odata.type", "#microsoft.graph.callRecording", "meetingOrganizer", obj("user", obj("id", " org-9 ")))))
	if err != nil || id != "id:n-1" || organizer != "org-9" || extra.Len() != 0 {
		t.Fatalf("recording blob: %q %q %v %v", id, organizer, pyrepr.Repr(extra), err)
	}
	id, organizer, extra, err = meetingIDsFromNotification(obj("resource", "users/u1/onlineMeetings/m%201/recordings/r1",
		"resourceData", "not a dict", "organizer", obj("identity", obj("user", obj("id", "flat")))))
	if err != nil || id != "m 1" || organizer != "u1" || !pyjson.Equal(extra, obj("recording_id", "r1")) {
		t.Fatalf("recording path: %q %q %v %v", id, organizer, pyrepr.Repr(extra), err)
	}
	id, organizer, _, err = meetingIDsFromNotification(obj("resourceData", obj("meetingId", " mid ", "organizerUserId", " o2 ")))
	if err != nil || id != "mid" || organizer != "o2" {
		t.Fatalf("flat ids: %q %q %v", id, organizer, err)
	}
	id, _, extra, err = meetingIDsFromNotification(obj("resourceData", obj("id", "abcTranscriptV2=")))
	if err != nil || id != "abcTranscriptV2=" || get(extra, "transcript_id") != "abcTranscriptV2=" {
		t.Fatalf("transcript only: %q %v %v", id, pyrepr.Repr(extra), err)
	}
	id, _, _, err = meetingIDsFromNotification(obj("changeType", "created"))
	if err != nil || !strings.HasPrefix(id, "sha256:") || len(id) != len("sha256:")+64 {
		t.Fatalf("receipt key fallback %q %v", id, err)
	}
}
