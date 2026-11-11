package teamspipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Graph resource patterns with a quoted key group and a slash key group per segment
var (
	usersMeetingRE = regexp.MustCompile(`(?i)(?:^|/)users(?:\('([^']+)'\)|/([^/'()]+))/onlineMeetings(?:\('([^']+)'\)|/([^/'?]+))`)
	commMeetingRE  = regexp.MustCompile(`(?i)(?:^|/)communications/onlineMeetings(?:\('([^']+)'\)|/([^/'?]+))`)
	transcriptRE   = regexp.MustCompile(`(?i)/transcripts(?:\('([^']+)'\)|/([^/'?]+))`)
	recordingRE    = regexp.MustCompile(`(?i)/recordings(?:\('([^']+)'\)|/([^/'?]+))`)
)

// Collection function names the meeting pattern captures in place of a real meeting id
var resourceSentinels = map[string]bool{"getalltranscripts": true, "getallrecordings": true, "transcripts": true, "recordings": true}

// Base failure of the Teams meeting helpers
type TeamsMeetingError struct{ Msg string }

func (e *TeamsMeetingError) Error() string { return e.Msg }

// Meeting that Graph cannot resolve
type TeamsMeetingNotFoundError struct{ TeamsMeetingError }

func (e *TeamsMeetingNotFoundError) Unwrap() error { return &e.TeamsMeetingError }

// Transcript or recording that cannot be found
type TeamsMeetingArtifactNotFoundError struct{ TeamsMeetingError }

func (e *TeamsMeetingArtifactNotFoundError) Unwrap() error { return &e.TeamsMeetingError }

// Graph access denied for the requested resource
type TeamsMeetingPermissionError struct{ TeamsMeetingError }

func (e *TeamsMeetingPermissionError) Unwrap() error { return &e.TeamsMeetingError }

// Organizer, meeting, transcript and recording ids of a Graph resource, empty when absent
type graphMeetingResource struct {
	OrganizerUserID, MeetingID, TranscriptID, RecordingID string
}

// Unquoted stripped value of the first non empty capture group, empty when none matched
func matchID(m []string, groups ...int) string {
	if m == nil {
		return ""
	}
	for _, g := range groups {
		if m[g] != "" {
			return pystr.Strip(pyurl.Unquote(m[g]))
		}
	}
	return ""
}

// Organizer, meeting and artifact ids parsed from a Graph resource or odata id
func parseGraphMeetingResource(resource any) graphMeetingResource {
	text := strField(resource)
	users := usersMeetingRE.FindStringSubmatch(text)
	meetingID := matchID(users, 3, 4)
	if meetingID == "" {
		meetingID = matchID(commMeetingRE.FindStringSubmatch(text), 1, 2)
	}
	if meetingID != "" && resourceSentinels[pystr.Lower(meetingID)] {
		meetingID = ""
	}
	return graphMeetingResource{
		OrganizerUserID: matchID(users, 1, 2), MeetingID: meetingID,
		TranscriptID: matchID(transcriptRE.FindStringSubmatch(text), 1, 2),
		RecordingID:  matchID(recordingRE.FindStringSubmatch(text), 1, 2),
	}
}

// True when a Graph id names a callTranscript artifact rather than an onlineMeeting
func looksLikeTranscriptID(value, odataType any) bool {
	if strings.Contains(pystr.Lower(textOrEmpty(odataType)), "calltranscript") {
		return true
	}
	v := textOrEmpty(value)
	return strings.Contains(pystr.Lower(v), "transcript") || strings.Contains(decodedIDHint(v), "transcript")
}

// str(value or "") of a loosely typed value
func textOrEmpty(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Lowercase base64 decoded text of a Graph id, empty when too short or undecodable
func decodedIDHint(value string) string {
	stripped := pystr.Strip(value)
	n := utf8.RuneCountInString(stripped)
	if n < 16 {
		return ""
	}
	padded := stripped + strings.Repeat("=", (4-n%4)%4)
	for _, decode := range []func(string) ([]byte, error){pybase64.URLSafeB64Decode, pybase64.B64Decode} {
		if raw, err := decode(padded); err == nil {
			return pystr.Lower(strings.ToValidUTF8(string(raw), ""))
		}
	}
	return ""
}

// Organizer scoped meetings collection when the organizer is known, required for app only access
func meetingsCollection(organizerUserID any) string {
	if pyrepr.Truthy(organizerUserID) {
		return "/users/" + pyurl.Quote(pyrepr.Str(organizerUserID), "") + "/onlineMeetings"
	}
	return "/communications/onlineMeetings"
}

// Graph path of the referenced meeting
func meetingPath(ref *TeamsMeetingRef) string {
	return meetingsCollection(ref.OrganizerUserID) + "/" + pyurl.Quote(ref.MeetingID, "")
}

// Permission, not found or generic meeting error for a Graph API failure
func wrapGraphError(exc *messagetools.MicrosoftGraphAPIError, missingMessage string) error {
	switch exc.StatusCode {
	case 401, 403:
		return &TeamsMeetingPermissionError{TeamsMeetingError{exc.Error()}}
	case 404:
		return &TeamsMeetingNotFoundError{TeamsMeetingError{missingMessage}}
	}
	return &TeamsMeetingError{exc.Error()}
}

// Graph call error with a Graph API failure translated into a meeting error
func graph(err error, missingMessage string) error {
	var api *messagetools.MicrosoftGraphAPIError
	if errors.As(err, &api) {
		return wrapGraphError(api, missingMessage)
	}
	return err
}

// Dict valued key of a payload, nil for anything else
func dictGet(payload any, key string) *pyjson.Object {
	o, ok := payload.(*pyjson.Object)
	if !ok || o == nil {
		return nil
	}
	v, _ := o.Get(key)
	d, _ := v.(*pyjson.Object)
	return d
}

// Organizer user id nested under organizer identity user
func parseOrganizerUserID(payload *pyjson.Object) any {
	if user := dictGet(dictGet(payload, "organizer"), "identity"); user != nil {
		if u := dictGet(user, "user"); u != nil {
			return get(u, "id")
		}
	}
	return nil
}

// Meeting reference built like the dataclass constructor, the meeting id required
func newMeetingRef(meetingID string, organizerUserID, joinWebURL, calendarEventID, threadID, tenantID any, metadata *pyjson.Object) (*TeamsMeetingRef, error) {
	if pystr.Strip(meetingID) == "" {
		return nil, requiredError("TeamsMeetingRef", "meeting_id")
	}
	return &TeamsMeetingRef{MeetingID: meetingID, OrganizerUserID: organizerUserID, JoinWebURL: joinWebURL, CalendarEventID: calendarEventID,
		ThreadID: threadID, TenantID: tenantID, Metadata: metadata}, nil
}

// Meeting reference of a Graph onlineMeeting payload
func normalizeMeetingRef(payload *pyjson.Object, tenantID, organizerUserID string) (*TeamsMeetingRef, error) {
	metadata := pyjson.NewObject()
	for _, key := range []string{"subject", "startDateTime", "endDateTime", "createdDateTime", "participants"} {
		if v := get(payload, key); v != nil {
			metadata.Set(key, v)
		}
	}
	threadID := get(payload, "threadId")
	if chat := dictGet(payload, "chatInfo"); chat != nil && pyrepr.Truthy(get(chat, "threadId")) {
		threadID = pyrepr.Str(get(chat, "threadId"))
	}
	return newMeetingRef(strField(get(payload, "id")), or(organizerUserID, parseOrganizerUserID(payload)), get(payload, "joinWebUrl"),
		get(payload, "calendarEventId"), threadID, or(tenantID, get(payload, "tenantId")), metadata)
}

// Artifact built like the dataclass constructor with the type, id, datetimes and size validated
func newMeetingArtifact(kind, id string, displayName, contentType, sourceURL, downloadURL, createdAt, availableAt, size any, metadata *pyjson.Object) (*MeetingArtifact, error) {
	if kind != "transcript" && kind != "recording" && kind != "call_record" {
		return nil, errors.New("MeetingArtifact.artifact_type must be transcript, recording, or call_record.")
	}
	if pystr.Strip(id) == "" {
		return nil, requiredError("MeetingArtifact", "artifact_id")
	}
	a := &MeetingArtifact{ArtifactType: kind, ArtifactID: id, DisplayName: displayName, ContentType: contentType, SourceURL: sourceURL,
		DownloadURL: downloadURL, Metadata: metadata}
	var err error
	if a.CreatedAt, err = parseDatetime(createdAt); err != nil {
		return nil, err
	}
	if a.AvailableAt, err = parseDatetime(availableAt); err != nil {
		return nil, err
	}
	if size != nil {
		n, err := pyint.Of(size)
		if err != nil {
			return nil, err
		}
		v := n.Int64()
		a.SizeBytes = &v
	}
	return a, nil
}

// Artifact of a Graph transcript or recording payload
func normalizeArtifact(kind string, payload *pyjson.Object) (*MeetingArtifact, error) {
	return newMeetingArtifact(kind, strField(get(payload, "id")), or(get(payload, "displayName"), get(payload, "name")),
		or(get(payload, "contentType"), get(payload, "fileMimeType")), or(get(payload, "webUrl"), get(payload, "contentUrl")),
		or(get(payload, "@microsoft.graph.downloadUrl"), get(payload, "downloadUrl"), get(payload, "recordingContentUrl"), get(payload, "transcriptContentUrl")),
		get(payload, "createdDateTime"), or(get(payload, "lastModifiedDateTime"), get(payload, "meetingEndDateTime")), get(payload, "size"), payload.Copy())
}

// Completed, downloadable and stamp ranks of a transcript in priority order
type transcriptRank struct {
	completed, download int
	stamp               string
}

// Rank preferring completed, downloadable, most recent transcripts
func transcriptSortKey(a *MeetingArtifact) transcriptRank {
	status := pystr.Lower(textOrEmpty(get(a.Metadata, "status")))
	r := transcriptRank{}
	if pyrepr.Truthy(or(a.DownloadURL, a.SourceURL)) {
		r.download = 1
	}
	if status == "available" || status == "completed" || status == "succeeded" {
		r.completed = 1
	}
	if stamp := a.AvailableAt; stamp != nil {
		r.stamp = pydatetime.ISOFormat(*stamp)
	} else if a.CreatedAt != nil {
		r.stamp = pydatetime.ISOFormat(*a.CreatedAt)
	}
	return r
}

// Tuple order of two ranks
func (r transcriptRank) less(o transcriptRank) bool {
	if r.completed != o.completed {
		return r.completed < o.completed
	}
	if r.download != o.download {
		return r.download < o.download
	}
	return r.stamp < o.stamp
}

// Meeting reference from a meeting id or join URL, refusing transcript ids without a join URL
func resolveMeetingReference(ctx context.Context, client graphClient, meetingID, joinWebURL, tenantID, organizerUserID string) (*TeamsMeetingRef, error) {
	if meetingID != "" && looksLikeTranscriptID(meetingID, nil) {
		if joinWebURL == "" {
			return nil, &TeamsMeetingError{"Refusing to GET /communications/onlineMeetings/{id} with a transcript id. " +
				"Graph v1.0 does not support that id format; use the organizer-scoped meeting " +
				"id from the notification @odata.id, or a join URL."}
		}
		meetingID = ""
	}
	collection := meetingsCollection(organizerUserID)
	var payload *pyjson.Object
	switch {
	case meetingID != "":
		missing := "Teams meeting not found: " + meetingID
		got, err := client.GetJSON(ctx, collection+"/"+pyurl.Quote(meetingID, ""), nil, nil)
		if err != nil {
			return nil, graph(err, missing)
		}
		o, ok := got.(*pyjson.Object)
		if !ok || !pyrepr.Truthy(get(o, "id")) {
			return nil, &TeamsMeetingNotFoundError{TeamsMeetingError{missing}}
		}
		payload = o
	case joinWebURL != "":
		missing := "Teams meeting not found for join URL: " + joinWebURL
		params := map[string]any{"$filter": "JoinWebUrl eq '" + strings.ReplaceAll(joinWebURL, "'", "''") + "'"}
		listing, err := client.GetJSON(ctx, collection, params, nil)
		if err != nil {
			return nil, graph(err, missing)
		}
		var candidates []any
		if o, ok := listing.(*pyjson.Object); ok {
			candidates, _ = get(o, "value").([]any)
		}
		if len(candidates) == 0 {
			return nil, &TeamsMeetingNotFoundError{TeamsMeetingError{missing}}
		}
		o, err := asDict(candidates[0])
		if err != nil {
			return nil, err
		}
		payload = o
	default:
		return nil, errors.New("Either meeting_id or join_web_url is required.")
	}
	return normalizeMeetingRef(payload, tenantID, organizerUserID)
}

// Every transcript or recording artifact of a meeting across Graph pages
func listArtifacts(ctx context.Context, client graphClient, ref *TeamsMeetingRef, kind string) ([]*MeetingArtifact, error) {
	collection := kind + "s"
	payloads, err := client.CollectPaginated(ctx, meetingPath(ref)+"/"+collection, nil, nil)
	if err != nil {
		return nil, graph(err, fmt.Sprintf("No %s found for Teams meeting %s", collection, ref.MeetingID))
	}
	out := []*MeetingArtifact{}
	for _, p := range payloads {
		if o, ok := p.(*pyjson.Object); ok {
			a, err := normalizeArtifact(kind, o)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// Transcript artifacts of a meeting
func listTranscriptArtifacts(ctx context.Context, client graphClient, ref *TeamsMeetingRef) ([]*MeetingArtifact, error) {
	return listArtifacts(ctx, client, ref, "transcript")
}

// Recording artifacts of a meeting
func listRecordingArtifacts(ctx context.Context, client graphClient, ref *TeamsMeetingRef) ([]*MeetingArtifact, error) {
	return listArtifacts(ctx, client, ref, "recording")
}

// Downloads an artifact from its download URL or its content endpoint into destination
func downloadArtifact(ctx context.Context, client graphClient, ref *TeamsMeetingRef, a *MeetingArtifact, destination, kind string, headers map[string]string) (map[string]any, error) {
	path := meetingPath(ref) + "/" + kind + "s/" + pyurl.Quote(a.ArtifactID, "") + "/content"
	if pyrepr.Truthy(a.DownloadURL) {
		path = pyrepr.Str(a.DownloadURL)
	}
	res, err := client.DownloadToFile(ctx, path, destination, headers)
	if err != nil {
		return nil, graph(err, fmt.Sprintf("%s %s not found for meeting %s", pystr.Capitalize(kind), a.ArtifactID, ref.MeetingID))
	}
	return res, nil
}

// Most preferred transcript among the candidates, nil when none is a transcript
func selectPreferredTranscript(candidates []*MeetingArtifact) *MeetingArtifact {
	var best *MeetingArtifact
	var bestRank transcriptRank
	for _, c := range candidates {
		if c.ArtifactType != "transcript" {
			continue
		}
		if r := transcriptSortKey(c); best == nil || bestRank.less(r) {
			best, bestRank = c, r
		}
	}
	return best
}

// Stripped text of a transcript downloaded as VTT into a scratch directory, an artifact error when empty
func downloadTranscriptText(ctx context.Context, client graphClient, ref *TeamsMeetingRef, transcript *MeetingArtifact, encoding string) (string, error) {
	name := "transcript.vtt"
	if pyrepr.Truthy(transcript.DisplayName) {
		name = pyrepr.Str(transcript.DisplayName)
	}
	suffix := pypath.PathSuffix(name)
	if suffix == "" {
		suffix = ".txt"
	}
	dir, err := os.MkdirTemp("", "teams-transcript-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	destination := filepath.Join(dir, "transcript"+suffix)
	if _, err := downloadArtifact(ctx, client, ref, transcript, destination, "transcript", map[string]string{"Accept": "text/vtt"}); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		return "", err
	}
	decoded, err := pycodecs.Decode(raw, encoding)
	if err != nil {
		return "", err
	}
	text := pystr.Strip(pystr.TranslateNewlines(decoded))
	if text == "" {
		return "", &TeamsMeetingArtifactNotFoundError{TeamsMeetingError{fmt.Sprintf("Transcript %s for meeting %s was empty.", transcript.ArtifactID, ref.MeetingID)}}
	}
	return text, nil
}

// Preferred transcript and its text, both empty when none exists or its download is missing or empty
func fetchPreferredTranscriptText(ctx context.Context, client graphClient, ref *TeamsMeetingRef) (*MeetingArtifact, string, error) {
	candidates, err := listTranscriptArtifacts(ctx, client, ref)
	if err != nil {
		return nil, "", err
	}
	transcript := selectPreferredTranscript(candidates)
	if transcript == nil {
		return nil, "", nil
	}
	text, err := downloadTranscriptText(ctx, client, ref, transcript, "utf-8")
	var missing *TeamsMeetingArtifactNotFoundError
	if errors.As(err, &missing) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return transcript, text, nil
}

// Downloads a recording to destination and reports its path, size and content type
func downloadRecordingArtifact(ctx context.Context, client graphClient, ref *TeamsMeetingRef, recording *MeetingArtifact, destination string) (*pyjson.Object, error) {
	path := pypath.PurePathStr(destination)
	res, err := downloadArtifact(ctx, client, ref, recording, path, "recording", nil)
	if err != nil {
		return nil, err
	}
	var size any
	if recording.SizeBytes != nil {
		size = *recording.SizeBytes
	}
	return pyjson.ObjectOf("artifact", recording.ToDict(), "path", path,
		"size_bytes", or(res["size_bytes"], size), "content_type", or(res["content_type"], recording.ContentType)), nil
}

// len() of a JSON collection or string, a TypeError for an unsized value
func jsonLen(v any) (int, error) {
	switch t := v.(type) {
	case []any:
		return len(t), nil
	case *pyjson.Object:
		return t.Len(), nil
	case string:
		return utf8.RuneCountInString(t), nil
	}
	return 0, fmt.Errorf("object of type '%s' has no len()", pyrepr.TypeName(v))
}

// Call record artifact with its metrics, nil when absent, missing or denied while permission errors are allowed
func enrichMeetingWithCallRecord(ctx context.Context, client graphClient, ref *TeamsMeetingRef, callRecordID string, allowPermissionErrors bool) (*MeetingArtifact, error) {
	id := textOrEmpty(or(callRecordID, get(ref.Metadata, "call_record_id")))
	if id == "" {
		return nil, nil
	}
	got, err := client.GetJSON(ctx, "/communications/callRecords/"+pyurl.Quote(id, ""), nil, nil)
	if err != nil {
		var api *messagetools.MicrosoftGraphAPIError
		if !errors.As(err, &api) {
			return nil, err
		}
		if api.StatusCode == 404 || ((api.StatusCode == 401 || api.StatusCode == 403) && allowPermissionErrors) {
			return nil, nil
		}
		return nil, wrapGraphError(api, "Call record not found: "+id)
	}
	payload, ok := got.(*pyjson.Object)
	if !ok || !pyrepr.Truthy(get(payload, "id")) {
		return nil, nil
	}
	participants, err := jsonLen(or(get(payload, "participants"), []any{}))
	if err != nil {
		return nil, err
	}
	metrics := pyjson.ObjectOf("version", get(payload, "version"), "modalities", get(payload, "modalities"),
		"participant_count", participants, "organizer", parseOrganizerUserID(payload))
	if sessions := get(payload, "sessions"); pyrepr.Truthy(sessions) {
		n, err := jsonLen(sessions)
		if err != nil {
			return nil, err
		}
		metrics.Set("session_count", n)
	}
	return newMeetingArtifact("call_record", pyrepr.Str(get(payload, "id")), or(get(payload, "type"), "call_record"), nil, get(payload, "webUrl"), nil,
		get(payload, "startDateTime"), get(payload, "endDateTime"), nil, pyjson.ObjectOf("call_record", payload, "metrics", metrics))
}
