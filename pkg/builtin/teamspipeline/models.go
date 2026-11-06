package teamspipeline

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Aware time of an ISO 8601 value with Z accepted, naive values taken as UTC, nil for None or blank
func parseDatetime(value any) (*time.Time, error) {
	switch t := value.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return &t, nil
	case *time.Time:
		return t, nil
	}
	text := pystr.Strip(pyrepr.Str(value))
	if text == "" {
		return nil, nil
	}
	if strings.HasSuffix(text, "Z") {
		text = text[:len(text)-1] + "+00:00"
	}
	parsed, aware, err := pydatetime.ParseISOFormat(text)
	if err != nil {
		return nil, err
	}
	if !aware {
		parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.UTC)
	}
	return &parsed, nil
}

// UTC ISO text with a Z suffix, nil for a missing time
func serializeDatetime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return strings.ReplaceAll(pydatetime.ISOFormatUTC(*value), "+00:00", "Z")
}

// camelCase spelling of a snake_case field name
func camel(name string) string {
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		parts[i] = pystr.Title(parts[i])
	}
	return strings.Join(parts, "")
}

// First truthy value among the keys, else the value of the last key
func pick(payload *pyjson.Object, keys ...string) any {
	for _, k := range keys {
		if v, _ := payload.Get(k); pyrepr.Truthy(v) {
			return v
		}
	}
	v, _ := payload.Get(keys[len(keys)-1])
	return v
}

// Field value under its snake and camel keys
func pickField(payload *pyjson.Object, name string) any { return pick(payload, name, camel(name)) }

// str(value or "").strip()
func strField(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(value))
}

// list(value or []) over JSON values
func listField(value any) ([]any, error) {
	if !pyrepr.Truthy(value) {
		return []any{}, nil
	}
	switch t := value.(type) {
	case []any:
		return append([]any{}, t...), nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := []any{}
		for _, k := range t.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(value))
}

// dict(value or {}) over JSON values
func dictField(value any) (*pyjson.Object, error) {
	if !pyrepr.Truthy(value) {
		return pyjson.NewObject(), nil
	}
	if o, ok := value.(*pyjson.Object); ok {
		return o.Copy(), nil
	}
	if items, ok := value.([]any); ok {
		out := pyjson.NewObject()
		for i, it := range items {
			pair, ok := it.([]any)
			if !ok {
				return nil, fmt.Errorf("cannot convert dictionary update sequence element #%d to a sequence", i)
			}
			if len(pair) != 2 {
				return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))
			}
			out.Set(pyrepr.Str(pair[0]), pair[1])
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(value))
}

// Payload as a dict, failing like attribute access on a non dict
func asDict(value any) (*pyjson.Object, error) {
	if o, ok := value.(*pyjson.Object); ok {
		return o, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(value))
}

// Wire value of a field with empty lists and dicts dropped and nested models as dicts
func serializeValue(value any) any {
	switch t := value.(type) {
	case *time.Time:
		return serializeDatetime(t)
	case []any:
		if len(t) == 0 {
			return nil
		}
		return t
	case *pyjson.Object:
		if t == nil || t.Len() == 0 {
			return nil
		}
		return t
	case interface{ ToDict() *pyjson.Object }:
		if isNilModel(t) {
			return nil
		}
		return t.ToDict()
	}
	return value
}

// Whether a model pointer stored in an interface is nil
func isNilModel(m any) bool {
	switch t := m.(type) {
	case *TeamsMeetingRef:
		return t == nil
	case *TeamsMeetingSummaryPayload:
		return t == nil
	case *MeetingArtifact:
		return t == nil
	}
	return false
}

// Dict of the given field pairs with None values dropped
func modelDict(pairs ...any) *pyjson.Object {
	out := pyjson.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		if v := serializeValue(pairs[i+1]); v != nil {
			out.Set(pairs[i].(string), v)
		}
	}
	return out
}

// Error raised for a blank required string field
func requiredError(model, field string) error {
	return fmt.Errorf("%s.%s is required.", model, field)
}

// Graph change notification subscription
type GraphSubscription struct {
	SubscriptionID     string
	Resource           string
	ChangeType         string
	NotificationURL    string
	ExpirationDatetime *time.Time
	ClientState        any
	LatestRenewalAt    *time.Time
	Status             any
}

// Subscription from snake or camel case keys with required fields validated
func GraphSubscriptionFromDict(payload *pyjson.Object) (*GraphSubscription, error) {
	s := &GraphSubscription{
		SubscriptionID:  strField(pick(payload, "subscription_id", "id")),
		Resource:        strField(pickField(payload, "resource")),
		ChangeType:      strField(pickField(payload, "change_type")),
		NotificationURL: strField(pickField(payload, "notification_url")),
		ClientState:     pickField(payload, "client_state"),
		Status:          pickField(payload, "status"),
	}
	for _, f := range [][2]string{{"subscription_id", s.SubscriptionID}, {"resource", s.Resource}, {"change_type", s.ChangeType}, {"notification_url", s.NotificationURL}} {
		if pystr.Strip(f[1]) == "" {
			return nil, requiredError("GraphSubscription", f[0])
		}
	}
	var err error
	if s.ExpirationDatetime, err = parseDatetime(pick(payload, "expiration_datetime", "expirationDateTime")); err != nil {
		return nil, err
	}
	if s.LatestRenewalAt, err = parseDatetime(pickField(payload, "latest_renewal_at")); err != nil {
		return nil, err
	}
	if s.ExpirationDatetime == nil {
		return nil, errors.New("GraphSubscription.expiration_datetime is required.")
	}
	return s, nil
}

// Snake case dict with None fields dropped
func (s *GraphSubscription) ToDict() *pyjson.Object {
	return modelDict("subscription_id", s.SubscriptionID, "resource", s.Resource, "change_type", s.ChangeType, "notification_url", s.NotificationURL,
		"expiration_datetime", s.ExpirationDatetime, "client_state", s.ClientState, "latest_renewal_at", s.LatestRenewalAt, "status", s.Status)
}

// Reference to one Teams online meeting
type TeamsMeetingRef struct {
	MeetingID       string
	OrganizerUserID any
	JoinWebURL      any
	CalendarEventID any
	ThreadID        any
	TenantID        any
	Metadata        *pyjson.Object
}

// Meeting reference from snake or camel case keys with the meeting id required
func TeamsMeetingRefFromDict(payload *pyjson.Object) (*TeamsMeetingRef, error) {
	meta, err := dictField(pickField(payload, "metadata"))
	if err != nil {
		return nil, err
	}
	r := &TeamsMeetingRef{
		MeetingID:       strField(pick(payload, "meeting_id", "id")),
		OrganizerUserID: pickField(payload, "organizer_user_id"),
		JoinWebURL:      pickField(payload, "join_web_url"),
		CalendarEventID: pickField(payload, "calendar_event_id"),
		ThreadID:        pickField(payload, "thread_id"),
		TenantID:        pickField(payload, "tenant_id"),
		Metadata:        meta,
	}
	if pystr.Strip(r.MeetingID) == "" {
		return nil, requiredError("TeamsMeetingRef", "meeting_id")
	}
	return r, nil
}

// Snake case dict with None fields and an empty metadata dropped
func (r *TeamsMeetingRef) ToDict() *pyjson.Object {
	return modelDict("meeting_id", r.MeetingID, "organizer_user_id", r.OrganizerUserID, "join_web_url", r.JoinWebURL, "calendar_event_id", r.CalendarEventID,
		"thread_id", r.ThreadID, "tenant_id", r.TenantID, "metadata", r.Metadata)
}

// Transcript, recording or call record artifact of a meeting
type MeetingArtifact struct {
	ArtifactType string
	ArtifactID   string
	DisplayName  any
	ContentType  any
	SourceURL    any
	DownloadURL  any
	CreatedAt    *time.Time
	AvailableAt  *time.Time
	SizeBytes    *int64
	Metadata     *pyjson.Object
}

// Artifact from Graph or snake case keys with the type and id validated
func MeetingArtifactFromDict(payload *pyjson.Object) (*MeetingArtifact, error) {
	kind := pickField(payload, "artifact_type")
	ks, _ := kind.(string)
	meta, err := dictField(pickField(payload, "metadata"))
	if err != nil {
		return nil, err
	}
	if ks != "transcript" && ks != "recording" && ks != "call_record" {
		return nil, errors.New("MeetingArtifact.artifact_type must be transcript, recording, or call_record.")
	}
	a := &MeetingArtifact{
		ArtifactType: ks,
		ArtifactID:   strField(pick(payload, "artifact_id", "id")),
		DisplayName:  pick(payload, "display_name", "displayName", "name"),
		ContentType:  pickField(payload, "content_type"),
		SourceURL:    pick(payload, "source_url", "sourceUrl", "webUrl"),
		DownloadURL:  pick(payload, "download_url", "downloadUrl", "@microsoft.graph.downloadUrl"),
		Metadata:     meta,
	}
	if pystr.Strip(a.ArtifactID) == "" {
		return nil, requiredError("MeetingArtifact", "artifact_id")
	}
	if a.CreatedAt, err = parseDatetime(pick(payload, "created_at", "createdDateTime")); err != nil {
		return nil, err
	}
	if a.AvailableAt, err = parseDatetime(pick(payload, "available_at", "availableDateTime", "lastModifiedDateTime")); err != nil {
		return nil, err
	}
	if size := pick(payload, "size_bytes", "size"); size != nil {
		n, err := pyint.Of(size)
		if err != nil {
			return nil, err
		}
		v := n.Int64()
		a.SizeBytes = &v
	}
	return a, nil
}

// Snake case dict with None fields and an empty metadata dropped
func (a *MeetingArtifact) ToDict() *pyjson.Object {
	var size any
	if a.SizeBytes != nil {
		size = *a.SizeBytes
	}
	return modelDict("artifact_type", a.ArtifactType, "artifact_id", a.ArtifactID, "display_name", a.DisplayName, "content_type", a.ContentType,
		"source_url", a.SourceURL, "download_url", a.DownloadURL, "created_at", a.CreatedAt, "available_at", a.AvailableAt, "size_bytes", size, "metadata", a.Metadata)
}

// Summary payload a pipeline job produces for one meeting
type TeamsMeetingSummaryPayload struct {
	MeetingRef      *TeamsMeetingRef
	Title           any
	StartTime       *time.Time
	EndTime         *time.Time
	Participants    []any
	TranscriptText  any
	Summary         any
	KeyDecisions    []any
	ActionItems     []any
	Risks           []any
	CallMetrics     *pyjson.Object
	SourceArtifacts []*MeetingArtifact
	Confidence      any
	ConfidenceNotes any
	NotionTarget    any
	LinearTarget    any
	TeamsTarget     any
}

// Summary payload whose meeting_ref key is mandatory and nested payloads read from snake case keys only
func TeamsMeetingSummaryPayloadFromDict(payload *pyjson.Object) (*TeamsMeetingSummaryPayload, error) {
	refRaw, ok := payload.Get("meeting_ref")
	if !ok {
		return nil, errors.New("KeyError: 'meeting_ref'")
	}
	refDict, err := asDict(refRaw)
	if err != nil {
		return nil, err
	}
	p := &TeamsMeetingSummaryPayload{
		Title: pickField(payload, "title"), TranscriptText: pickField(payload, "transcript_text"), Summary: pickField(payload, "summary"),
		Confidence: pickField(payload, "confidence"), ConfidenceNotes: pickField(payload, "confidence_notes"), NotionTarget: pickField(payload, "notion_target"),
		LinearTarget: pickField(payload, "linear_target"), TeamsTarget: pickField(payload, "teams_target"),
	}
	if p.MeetingRef, err = TeamsMeetingRefFromDict(refDict); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		dst  *[]any
	}{{"participants", &p.Participants}, {"key_decisions", &p.KeyDecisions}, {"action_items", &p.ActionItems}, {"risks", &p.Risks}} {
		if *f.dst, err = listField(pickField(payload, f.name)); err != nil {
			return nil, err
		}
	}
	if p.CallMetrics, err = dictField(pickField(payload, "call_metrics")); err != nil {
		return nil, err
	}
	items, err := listField(pick(payload, "source_artifacts"))
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		d, err := asDict(it)
		if err != nil {
			return nil, err
		}
		a, err := MeetingArtifactFromDict(d)
		if err != nil {
			return nil, err
		}
		p.SourceArtifacts = append(p.SourceArtifacts, a)
	}
	if p.StartTime, err = parseDatetime(pickField(payload, "start_time")); err != nil {
		return nil, err
	}
	if p.EndTime, err = parseDatetime(pickField(payload, "end_time")); err != nil {
		return nil, err
	}
	return p, nil
}

// Snake case dict with None fields and empty collections dropped
func (p *TeamsMeetingSummaryPayload) ToDict() *pyjson.Object {
	arts := make([]any, 0, len(p.SourceArtifacts))
	for _, a := range p.SourceArtifacts {
		arts = append(arts, a.ToDict())
	}
	var ref any
	if p.MeetingRef != nil {
		ref = p.MeetingRef
	}
	return modelDict("meeting_ref", ref, "title", p.Title, "start_time", p.StartTime, "end_time", p.EndTime, "participants", p.Participants,
		"transcript_text", p.TranscriptText, "summary", p.Summary, "key_decisions", p.KeyDecisions, "action_items", p.ActionItems, "risks", p.Risks,
		"call_metrics", p.CallMetrics, "source_artifacts", arts, "confidence", p.Confidence, "confidence_notes", p.ConfidenceNotes,
		"notion_target", p.NotionTarget, "linear_target", p.LinearTarget, "teams_target", p.TeamsTarget)
}

// Durable pipeline job for one meeting notification
type TeamsMeetingPipelineJob struct {
	JobID                    string
	EventID                  string
	SourceEventType          string
	DedupeKey                string
	Status                   string
	RetryCount               int64
	CreatedAt                *time.Time
	UpdatedAt                *time.Time
	MeetingRef               *TeamsMeetingRef
	SelectedArtifactStrategy any
	SummaryPayload           *TeamsMeetingSummaryPayload
	ErrorInfo                *pyjson.Object
}

// Job from snake or camel case keys with required ids validated and nested models built when present
func TeamsMeetingPipelineJobFromDict(payload *pyjson.Object) (*TeamsMeetingPipelineJob, error) {
	j := &TeamsMeetingPipelineJob{
		JobID: strField(pickField(payload, "job_id")), EventID: strField(pickField(payload, "event_id")),
		SourceEventType: strField(pickField(payload, "source_event_type")), DedupeKey: strField(pickField(payload, "dedupe_key")),
		Status: strField(pickField(payload, "status")), SelectedArtifactStrategy: pickField(payload, "selected_artifact_strategy"),
	}
	var err error
	if v := pickField(payload, "meeting_ref"); pyrepr.Truthy(v) {
		d, err := asDict(v)
		if err != nil {
			return nil, err
		}
		if j.MeetingRef, err = TeamsMeetingRefFromDict(d); err != nil {
			return nil, err
		}
	}
	if v := pickField(payload, "summary_payload"); pyrepr.Truthy(v) {
		d, err := asDict(v)
		if err != nil {
			return nil, err
		}
		if j.SummaryPayload, err = TeamsMeetingSummaryPayloadFromDict(d); err != nil {
			return nil, err
		}
	}
	if j.ErrorInfo, err = dictField(pickField(payload, "error_info")); err != nil {
		return nil, err
	}
	for _, f := range [][2]string{{"job_id", j.JobID}, {"event_id", j.EventID}, {"source_event_type", j.SourceEventType}, {"dedupe_key", j.DedupeKey}, {"status", j.Status}} {
		if pystr.Strip(f[1]) == "" {
			return nil, requiredError("TeamsMeetingPipelineJob", f[0])
		}
	}
	if j.CreatedAt, err = parseDatetime(pickField(payload, "created_at")); err != nil {
		return nil, err
	}
	if j.UpdatedAt, err = parseDatetime(pickField(payload, "updated_at")); err != nil {
		return nil, err
	}
	retry := pickField(payload, "retry_count")
	if !pyrepr.Truthy(retry) {
		retry = int64(0)
	}
	n, err := pyint.Of(retry)
	if err != nil {
		return nil, err
	}
	j.RetryCount = n.Int64()
	return j, nil
}

// Snake case dict with None fields and an empty error_info dropped
func (j *TeamsMeetingPipelineJob) ToDict() *pyjson.Object {
	var ref, summary any
	if j.MeetingRef != nil {
		ref = j.MeetingRef
	}
	if j.SummaryPayload != nil {
		summary = j.SummaryPayload
	}
	return modelDict("job_id", j.JobID, "event_id", j.EventID, "source_event_type", j.SourceEventType, "dedupe_key", j.DedupeKey, "status", j.Status,
		"retry_count", j.RetryCount, "created_at", j.CreatedAt, "updated_at", j.UpdatedAt, "meeting_ref", ref, "selected_artifact_strategy", j.SelectedArtifactStrategy,
		"summary_payload", summary, "error_info", j.ErrorInfo)
}
