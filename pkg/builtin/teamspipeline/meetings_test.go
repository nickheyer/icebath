package teamspipeline

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Graph client double whose calls are recorded and answered by the optional script functions
type scriptGraph struct {
	calls    []string
	params   []map[string]any
	headers  []map[string]string
	get      func(path string) (any, error)
	collect  func(path string) ([]any, error)
	download func(path, destination string) (map[string]any, error)
}

func (f *scriptGraph) GetJSON(_ context.Context, path string, params map[string]any, _ map[string]string) (any, error) {
	f.calls, f.params = append(f.calls, path), append(f.params, params)
	if f.get == nil {
		panic("Graph should not be called, got " + path)
	}
	return f.get(path)
}
func (f *scriptGraph) PostJSON(_ context.Context, path string, _ any, _ map[string]string) (any, error) {
	panic("unexpected POST " + path)
}
func (f *scriptGraph) PatchJSON(_ context.Context, path string, _ any, _ map[string]string) (any, error) {
	panic("unexpected PATCH " + path)
}
func (f *scriptGraph) Delete(_ context.Context, path string, _ map[string]string) (map[string]any, error) {
	panic("unexpected DELETE " + path)
}
func (f *scriptGraph) CollectPaginated(_ context.Context, path string, _ map[string]any, _ map[string]string) ([]any, error) {
	f.calls = append(f.calls, path)
	return f.collect(path)
}
func (f *scriptGraph) DownloadToFile(_ context.Context, path, destination string, headers map[string]string) (map[string]any, error) {
	f.calls, f.headers = append(f.calls, path), append(f.headers, headers)
	return f.download(path, destination)
}

func TestJoinURLCanUseOrganizerScopedGraphLookup(t *testing.T) {
	g := &scriptGraph{get: func(string) (any, error) {
		return obj("value", []any{obj("id", "meeting-1", "joinWebUrl", "https://teams.microsoft.com/meet/code")}), nil
	}}
	m, err := resolveMeetingReference(context.Background(), g, "", "https://teams.microsoft.com/meet/code", "", "organizer-1")
	if err != nil || m.MeetingID != "meeting-1" || m.OrganizerUserID != "organizer-1" {
		t.Fatal(m, err)
	}
	if !reflect.DeepEqual(g.calls, []string{"/users/organizer-1/onlineMeetings"}) ||
		!reflect.DeepEqual(g.params, []map[string]any{{"$filter": "JoinWebUrl eq 'https://teams.microsoft.com/meet/code'"}}) {
		t.Fatal(g.calls, g.params)
	}
}

func TestTranscriptDownloadRequestsGraphVTTContent(t *testing.T) {
	g := &scriptGraph{download: func(_, destination string) (map[string]any, error) {
		return map[string]any{"content_type": "text/vtt"}, os.WriteFile(destination, []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n<v Speaker>Hello</v>\n"), 0o644)
	}}
	ref, _ := newMeetingRef("meeting-1", "organizer-1", nil, nil, nil, nil, pyjson.NewObject())
	tr, err := newMeetingArtifact("transcript", "transcript-1", "transcript.vtt", nil, nil, nil, nil, nil, nil, pyjson.NewObject())
	if err != nil {
		t.Fatal(err)
	}
	text, err := downloadTranscriptText(context.Background(), g, ref, tr, "utf-8")
	if err != nil || !strings.HasPrefix(text, "WEBVTT") {
		t.Fatal(text, err)
	}
	if !reflect.DeepEqual(g.calls, []string{"/users/organizer-1/onlineMeetings/meeting-1/transcripts/transcript-1/content"}) ||
		!reflect.DeepEqual(g.headers, []map[string]string{{"Accept": "text/vtt"}}) {
		t.Fatal(g.calls, g.headers)
	}
}

func TestParseGraphMeetingResourceReadsQuotedUsersTranscriptPath(t *testing.T) {
	got := parseGraphMeetingResource("users/org-1/onlineMeetings('MSo-meeting')/transcripts('ktViz-transcript-TranscriptV2=')")
	if got != (graphMeetingResource{"org-1", "MSo-meeting", "ktViz-transcript-TranscriptV2=", ""}) {
		t.Fatal(got)
	}
}

func TestParseGraphMeetingResourceReadsQuotedUserKeyOdataID(t *testing.T) {
	got := parseGraphMeetingResource("users('org-1')/onlineMeetings('MSo-meeting')/transcripts('ktViz-transcript-TranscriptV2=')")
	if got != (graphMeetingResource{"org-1", "MSo-meeting", "ktViz-transcript-TranscriptV2=", ""}) {
		t.Fatal(got)
	}
}

func TestParseGraphMeetingResourceReadsGraphURLQuotedUserKey(t *testing.T) {
	got := parseGraphMeetingResource("https://graph.microsoft.com/v1.0/users('org-1')/onlineMeetings('MSo-meeting')/transcripts('tx-1')")
	if got.OrganizerUserID != "org-1" || got.MeetingID != "MSo-meeting" || got.TranscriptID != "tx-1" {
		t.Fatal(got)
	}
}

func TestParseGraphMeetingResourceIgnoresGetAllTranscriptsSentinel(t *testing.T) {
	if got := parseGraphMeetingResource("communications/onlineMeetings/getAllTranscripts"); got != (graphMeetingResource{}) {
		t.Fatal(got)
	}
	if got := parseGraphMeetingResource(" communications/onlineMeetings/%20m%2F1/recordings/r-1 "); got.MeetingID != "m/1" || got.RecordingID != "r-1" {
		t.Fatal(got)
	}
}

func TestLooksLikeTranscriptIDDetectsGraphCallTranscripts(t *testing.T) {
	if !looksLikeTranscriptID("ktVizInGAAAA-TranscriptV2=", "#Microsoft.Graph.callTranscript") || !looksLikeTranscriptID("abcTranscriptV2=", nil) ||
		looksLikeTranscriptID("MSo-meeting", nil) {
		t.Fatal("transcript id detection disagrees with upstream")
	}
}

func TestLooksLikeTranscriptIDDetectsBase64EncodedMarker(t *testing.T) {
	encoded := strings.TrimRight(base64.URLEncoding.EncodeToString([]byte("...meeting_ABC@thread.v2#55d6e479-2db7-4297-8590-98f6011a9613-1787004872-TranscriptV2")), "=") + "="
	if strings.Contains(strings.ToLower(encoded), "transcript") || !looksLikeTranscriptID(encoded, nil) {
		t.Fatal(encoded)
	}
	meeting := base64.StdEncoding.EncodeToString([]byte("0#af8b854e-e592-4d5a-b04c-6e63a3b502e7#0**19:meeting_ABC@thread.v2"))
	if looksLikeTranscriptID(meeting, nil) || decodedIDHint("short") != "" || decodedIDHint("!!!!!!!!!!!!!!!!!!") != "" {
		t.Fatal(meeting)
	}
}

func TestResolveMeetingReferenceUsesOrganizerScopedGraphPath(t *testing.T) {
	g := &scriptGraph{get: func(string) (any, error) {
		return obj("id", "MSo-meeting-id", "subject", "Standup", "organizer", obj("identity", obj("user", obj("id", "org-1")))), nil
	}}
	m, err := resolveMeetingReference(context.Background(), g, "MSo-meeting-id", "", "tenant-1", "org-1")
	if err != nil || !reflect.DeepEqual(g.calls, []string{"/users/org-1/onlineMeetings/MSo-meeting-id"}) {
		t.Fatal(g.calls, err)
	}
	if m.MeetingID != "MSo-meeting-id" || m.OrganizerUserID != "org-1" || m.TenantID != "tenant-1" || get(m.Metadata, "subject") != "Standup" {
		t.Fatal(m)
	}
}

func TestResolveMeetingReferenceRefusesTranscriptIDWithoutJoinURL(t *testing.T) {
	_, err := resolveMeetingReference(context.Background(), &scriptGraph{}, "ktVizInGAAAA-TranscriptV2=", "", "", "")
	var te *TeamsMeetingError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "transcript id") {
		t.Fatal(err)
	}
	if _, err := resolveMeetingReference(context.Background(), &scriptGraph{}, "", "", "", ""); err == nil || err.Error() != "Either meeting_id or join_web_url is required." {
		t.Fatal(err)
	}
}

func TestResolveMeetingReferenceTranslatesMissingMeetings(t *testing.T) {
	g := &scriptGraph{get: func(string) (any, error) { return nil, &messagetools.MicrosoftGraphAPIError{StatusCode: 404} }}
	_, err := resolveMeetingReference(context.Background(), g, "m 1", "", "", "")
	var nf *TeamsMeetingNotFoundError
	if !errors.As(err, &nf) || err.Error() != "Teams meeting not found: m 1" || g.calls[0] != "/communications/onlineMeetings/m%201" {
		t.Fatal(err, g.calls)
	}
	g.get = func(string) (any, error) { return obj("value", []any{}), nil }
	if _, err := resolveMeetingReference(context.Background(), g, "", "https://x/'a'", "", ""); !errors.As(err, &nf) {
		t.Fatal(err)
	}
	if g.params[1]["$filter"] != "JoinWebUrl eq 'https://x/''a'''" {
		t.Fatal(g.params[1])
	}
}

func TestSelectPreferredTranscriptRanksCompletedDownloadableRecent(t *testing.T) {
	g := &scriptGraph{collect: func(string) ([]any, error) {
		return []any{
			obj("id", "old", "status", "completed", "transcriptContentUrl", "https://d/old", "createdDateTime", "2026-01-01T10:00:00Z"),
			obj("id", "new", "status", "completed", "transcriptContentUrl", "https://d/new", "createdDateTime", "2026-01-02T10:00:00Z"),
			obj("id", "pending", "createdDateTime", "2026-01-03T10:00:00Z"),
			"skipped",
		}, nil
	}, download: func(_, destination string) (map[string]any, error) {
		return nil, os.WriteFile(destination, []byte(" \r\n"), 0o644)
	}}
	ref, _ := newMeetingRef("m-1", nil, nil, nil, nil, nil, pyjson.NewObject())
	items, err := listTranscriptArtifacts(context.Background(), g, ref)
	if err != nil || len(items) != 3 || g.calls[0] != "/communications/onlineMeetings/m-1/transcripts" {
		t.Fatal(items, err, g.calls)
	}
	if best := selectPreferredTranscript(items); best.ArtifactID != "new" || best.DownloadURL != "https://d/new" {
		t.Fatal(best)
	}
	tr, text, err := fetchPreferredTranscriptText(context.Background(), g, ref)
	if tr != nil || text != "" || err != nil || g.calls[2] != "https://d/new" {
		t.Fatal(tr, text, err, g.calls)
	}
}

func TestDownloadRecordingArtifactReportsPathSizeAndType(t *testing.T) {
	g := &scriptGraph{download: func(string, string) (map[string]any, error) { return map[string]any{"size_bytes": 0}, nil }}
	ref, _ := newMeetingRef("m-1", "o@x", nil, nil, nil, nil, pyjson.NewObject())
	rec, _ := normalizeArtifact("recording", obj("id", "r 1", "size", "12", "fileMimeType", "video/mp4"))
	out, err := downloadRecordingArtifact(context.Background(), g, ref, rec, "out//rec.mp4")
	if err != nil || g.calls[0] != "/users/o%40x/onlineMeetings/m-1/recordings/r%201/content" {
		t.Fatal(err, g.calls)
	}
	want := `{"artifact": {"artifact_type": "recording", "artifact_id": "r 1", "content_type": "video/mp4", "size_bytes": 12, "metadata": {"id": "r 1", "size": "12", "fileMimeType": "video/mp4"}}, "path": "out/rec.mp4", "size_bytes": 12, "content_type": "video/mp4"}`
	if got, _ := pyjson.Dumps(out); got != want {
		t.Fatal(got)
	}
}

func TestEnrichMeetingWithCallRecordSoftensDenialAndCollectsMetrics(t *testing.T) {
	ref, _ := newMeetingRef("m-1", nil, nil, nil, nil, nil, obj("call_record_id", "cr-1"))
	g := &scriptGraph{get: func(string) (any, error) { return nil, &messagetools.MicrosoftGraphAPIError{StatusCode: 403} }}
	if a, err := enrichMeetingWithCallRecord(context.Background(), g, ref, "", true); a != nil || err != nil {
		t.Fatal(a, err)
	}
	g.get = func(string) (any, error) {
		return obj("id", "cr-1", "version", 2, "modalities", []any{"audio"}, "participants", []any{1, 2}, "sessions", []any{1},
			"organizer", obj("identity", obj("user", obj("id", "org"))), "startDateTime", "2026-01-01T10:00:00Z"), nil
	}
	a, err := enrichMeetingWithCallRecord(context.Background(), g, ref, "", true)
	if err != nil || a.ArtifactType != "call_record" || a.DisplayName != "call_record" || g.calls[1] != "/communications/callRecords/cr-1" {
		t.Fatal(a, err)
	}
	if got, _ := pyjson.Dumps(get(a.Metadata, "metrics")); got != `{"version": 2, "modalities": ["audio"], "participant_count": 2, "organizer": "org", "session_count": 1}` {
		t.Fatal(got)
	}
	empty, _ := newMeetingRef("m-1", nil, nil, nil, nil, nil, pyjson.NewObject())
	if a, err := enrichMeetingWithCallRecord(context.Background(), &scriptGraph{}, empty, "", true); a != nil || err != nil {
		t.Fatal(a, err)
	}
}
