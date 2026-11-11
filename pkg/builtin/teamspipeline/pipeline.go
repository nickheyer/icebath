package teamspipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Job states that end a run, states of a run in flight and recording suffixes that need no audio extraction
var (
	terminalPipelineStates = map[string]bool{"completed": true, "failed": true, "retry_scheduled": true}
	activePipelineStates   = map[string]bool{"received": true, "resolving_meeting": true, "fetching_transcript": true, "downloading_recording": true,
		"transcribing_audio": true, "summarizing": true, "writing_notion": true, "writing_linear": true, "sending_teams": true}
	audioSuffixes = map[string]bool{".wav": true, ".mp3": true, ".m4a": true, ".ogg": true, ".flac": true, ".aac": true, ".webm": true}
)

const summarySystemPrompt = "You summarize meeting transcripts. Return only valid JSON with keys: " +
	"summary, key_decisions, action_items, risks, confidence, confidence_notes."

// Graph meeting helpers and the audio preparation step the pipeline calls, swapped by tests for the Graph side
var (
	pipelineResolveMeetingReference      = resolveMeetingReference
	pipelineFetchPreferredTranscriptText = fetchPreferredTranscriptText
	pipelineEnrichMeetingWithCallRecord  = enrichMeetingWithCallRecord
	pipelineListRecordingArtifacts       = listRecordingArtifacts
	pipelineDownloadRecordingArtifact    = downloadRecordingArtifact
	pipelinePrepareAudioPath             = (*TeamsMeetingPipeline).prepareAudioPath
)

// Base failure of the Teams meeting pipeline
type TeamsPipelineError struct{ Msg string }

func (e *TeamsPipelineError) Error() string { return e.Msg }

// Failure the pipeline retries later
type TeamsPipelineRetryableError struct{ TeamsPipelineError }

func (e *TeamsPipelineRetryableError) Unwrap() error { return &e.TeamsPipelineError }

// Failure of an output sink
type TeamsPipelineSinkError struct{ TeamsPipelineError }

func (e *TeamsPipelineSinkError) Unwrap() error { return &e.TeamsPipelineError }

// Meeting artifacts that are not yet available
type TeamsPipelineArtifactNotFoundError struct{ TeamsPipelineRetryableError }

func (e *TeamsPipelineArtifactNotFoundError) Unwrap() error { return &e.TeamsPipelineRetryableError }

// Speech to text call returning the transcription result dict for an audio file and optional model
type TranscribeFn func(ctx context.Context, filePath, model string) (map[string]any, error)

// Summary builder returning a summary payload or its dict for a resolved meeting, transcript and artifacts
type SummarizeFn func(ctx context.Context, resolvedMeeting *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error)

// Output sink that writes one summary and returns its sink record
type SummarySink interface {
	WriteSummary(ctx context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error)
}

// Bare function used as an output sink
type SinkFn func(ctx context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error)

func (f SinkFn) WriteSummary(ctx context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error) {
	return f(ctx, payload, config, existingRecord)
}

// Error standing for a Python builtin exception class with its message
func pyError(class, msg string) error { return &pyrepr.FormatError{Class: class, Msg: msg} }

// Mapping get that fails with the AttributeError of calling get on a non dict
func dictGetAttr(v any, key string) (any, error) {
	val, err := attrGet(v, key)
	if err != nil {
		return nil, pyError("AttributeError", err.Error())
	}
	return val, nil
}

// Key of a falsy or dict value, nil for a falsy value and an AttributeError for a truthy non dict
func orDictGet(v any, key string) (any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	return dictGetAttr(v, key)
}

// Value under a required key of a dict, the KeyError or TypeError subscripting raises otherwise
func subscript(v any, key string) (any, error) {
	switch t := v.(type) {
	case *pyjson.Object:
		if t != nil {
			if val, ok := t.Get(key); ok {
				return val, nil
			}
			return nil, pyError("KeyError", pyrepr.Repr(key))
		}
	case []any:
		return nil, pyError("TypeError", "list indices must be integers or slices, not str")
	case string:
		return nil, pyError("TypeError", "string indices must be integers, not 'str'")
	}
	return nil, pyError("TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(v)))
}

// Items of an iterable JSON value, the TypeError of iterating anything else
func iterItems(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		if t != nil {
			out := make([]any, 0, len(t.Keys))
			for _, k := range t.Keys {
				out = append(out, k)
			}
			return out, nil
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, pyError("TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v)))
}

// dict(value or {}) over a config value that may be a Go map or an ordered dict
func configDict(v any) (*pyjson.Object, error) {
	if !pyrepr.Truthy(v) {
		return pyjson.NewObject(), nil
	}
	switch t := v.(type) {
	case map[string]any:
		return mapping(t)
	case string:
		return nil, pyError("ValueError", "dictionary update sequence element #0 has length 1; 2 is required")
	}
	return dictField(v)
}

// JSON value with every Go map turned into an ordered dict, key order sorted
func orderedValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := pyjson.NewObject()
		for _, k := range keys {
			out.Set(k, orderedValue(t[k]))
		}
		return out
	case *pyjson.Object:
		if t == nil {
			return t
		}
		out := pyjson.NewObject()
		for _, k := range t.Keys {
			out.Set(k, orderedValue(t.Vals[k]))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = orderedValue(item)
		}
		return out
	}
	return v
}

// Pipeline settings, the transcript policy, speech to text model, scratch directory and sink configs
type TeamsPipelineConfig struct {
	TranscriptPreferred   bool
	TranscriptRequired    bool
	TranscriptionFallback bool
	STTModel              any
	FFmpegExtractAudio    bool
	TranscriptMinChars    int64
	TmpDir                string
	Notion                any
	Linear                any
	TeamsDelivery         any
}

// Settings with the dataclass defaults
func NewTeamsPipelineConfig() *TeamsPipelineConfig {
	return &TeamsPipelineConfig{TranscriptPreferred: true, TranscriptionFallback: true, FFmpegExtractAudio: true, TranscriptMinChars: 80}
}

// Settings from a snake or camel case dict, missing flags keeping their defaults
func TeamsPipelineConfigFromDict(payload any) (*TeamsPipelineConfig, error) {
	data, err := configDict(payload)
	if err != nil {
		return nil, err
	}
	flag := func(name string, def bool) bool {
		if v, ok := data.Get(name); ok {
			return pyrepr.Truthy(v)
		}
		return def
	}
	cfg := &TeamsPipelineConfig{
		TranscriptPreferred: flag("transcript_preferred", true), TranscriptRequired: flag("transcript_required", false),
		TranscriptionFallback: flag("transcription_fallback", true), FFmpegExtractAudio: flag("ffmpeg_extract_audio", true),
		STTModel: or(get(data, "stt_model"), get(data, "sttModel")), Notion: get(data, "notion"), Linear: get(data, "linear"),
		TeamsDelivery: or(get(data, "teams_delivery"), get(data, "teamsDelivery")),
	}
	minChars, ok := data.Get("transcript_min_chars")
	if !ok {
		minChars = int64(80)
	}
	n, err := pyint.Of(minChars)
	if err != nil {
		return nil, err
	}
	switch {
	case n.IsInt64():
		cfg.TranscriptMinChars = n.Int64()
	case n.Sign() > 0:
		cfg.TranscriptMinChars = math.MaxInt64
	default:
		cfg.TranscriptMinChars = math.MinInt64
	}
	if tmp := or(get(data, "tmp_dir"), get(data, "tmpDir")); pyrepr.Truthy(tmp) {
		text, ok := tmp.(string)
		if !ok {
			return nil, pyError("TypeError", "expected str, bytes or os.PathLike object, not "+pyrepr.TypeName(tmp))
		}
		cfg.TmpDir = pypath.PurePathStr(text)
	}
	return cfg, nil
}

// Notion rich text property holding one text run
func richText(content any) *pyjson.Object {
	return obj("rich_text", []any{obj("text", obj("content", content))})
}

// Markdown bullet list of the items, one per line
func bullets(items []any) string {
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = "- " + pyrepr.Str(item)
	}
	return strings.Join(lines, "\n")
}

// Text of a summary field, empty for a falsy value
func summaryText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// One summary section, its heading and the body the payload holds for it
type summarySection struct {
	heading string
	body    any
}

// Value itself when truthy, else the fallback
func orDefault(v, fallback any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return fallback
}

// Heading and body of the four summary sections in display order
func sections(payload *TeamsMeetingSummaryPayload) []summarySection {
	return []summarySection{{"Summary", orDefault(payload.Summary, "")}, {"Key Decisions", bullets(payload.KeyDecisions)},
		{"Action Items", bullets(payload.ActionItems)}, {"Risks", bullets(payload.Risks)}}
}

// First 1900 characters or items of a summary, the TypeError slicing raises for other values
func summaryHead(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return pystr.Slice(t, 0, 1900), nil
	case []any:
		return append([]any{}, t[:min(len(t), 1900)]...), nil
	case *pyjson.Object, map[string]any:
		return nil, pyError("TypeError", "unhashable type: 'slice'")
	}
	return nil, pyError("TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(v)))
}

// Lines joined by newlines, the TypeError str.join raises for a line that is not a str
func joinLines(lines []any) (string, error) {
	texts := make([]string, len(lines))
	for i, line := range lines {
		text, ok := line.(string)
		if !ok {
			return "", pyError("TypeError", fmt.Sprintf("sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(line)))
		}
		texts[i] = text
	}
	return strings.Join(texts, "\n"), nil
}

// Meeting id of the payload reference, the AttributeError of a payload with no reference
func payloadMeetingID(payload *TeamsMeetingSummaryPayload) (string, error) {
	if payload.MeetingRef == nil {
		return "", pyError("AttributeError", "'NoneType' object has no attribute 'meeting_id'")
	}
	return payload.MeetingRef.MeetingID, nil
}

// Raised for a non 2xx sink response like httpx raise_for_status
type sinkHTTPStatusError struct{ msg string }

func (e *sinkHTTPStatusError) Error() string      { return e.msg }
func (e *sinkHTTPStatusError) PyTypeName() string { return "HTTPStatusError" }

// API key and HTTP transport shared by the Notion and Linear sinks
type httpSinkWriter struct {
	secretName string
	APIKey     string
	transport  http.RoundTripper
}

// Sink plumbing with the explicit key or the scoped secret of the given name, stripped
func newHTTPSinkWriter(ctx context.Context, secretName, apiKey string, transport http.RoundTripper) (httpSinkWriter, error) {
	if apiKey == "" {
		secret, _, err := provider.GetSecret(ctx, secretName)
		if err != nil {
			return httpSinkWriter{}, err
		}
		apiKey = secret
	}
	return httpSinkWriter{secretName: secretName, APIKey: pystr.Strip(apiKey), transport: transport}, nil
}

// Sink error when no API key is configured
func (w *httpSinkWriter) requireAPIKey() error {
	if w.APIKey == "" {
		return &TeamsPipelineSinkError{TeamsPipelineError{w.secretName + " is not configured."}}
	}
	return nil
}

// Sends one JSON request on a fresh client with a thirty second timeout and returns the decoded JSON body
func (w *httpSinkWriter) request(ctx context.Context, method, rawURL string, headers [][2]string, body *pyjson.Object) (any, error) {
	text, err := pyjson.Dumps(body, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	client, closeClient := &http.Client{Transport: w.transport, CheckRedirect: pyhttpx.RedirectPolicy(false)}, func() {}
	if w.transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		client.Transport, closeClient = t, t.CloseIdleConnections
	}
	resp, err := pyhttpx.Do(client, req, 30*time.Second)
	closeClient()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &sinkHTTPStatusError{resp.StatusErrorText(rawURL)}
	}
	return resp.JSON()
}

const (
	notionAPIBase    = "https://api.notion.com/v1"
	notionAPIVersion = "2025-09-03"
	linearAPIURL     = "https://api.linear.app/graphql"
	linearUpdate     = "mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success issue { id identifier url } } }"
	linearCreate     = "mutation($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier url } } }"
)

// Sink that writes a meeting summary as a Notion database page
type NotionWriter struct{ httpSinkWriter }

// Notion sink keyed by the explicit key or the NOTION_API_KEY secret
func NewNotionWriter(ctx context.Context, apiKey string, transport http.RoundTripper) (*NotionWriter, error) {
	w, err := newHTTPSinkWriter(ctx, "NOTION_API_KEY", apiKey, transport)
	if err != nil {
		return nil, err
	}
	return &NotionWriter{w}, nil
}

// Creates the summary page with its body blocks, or updates only the properties of an existing page
func (w *NotionWriter) WriteSummary(ctx context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error) {
	if err := w.requireAPIKey(); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, pyError("AttributeError", "'NoneType' object has no attribute 'get'")
	}
	databaseID := strField(or(get(config, "database_id"), get(config, "databaseId"), ""))
	pageID := get(existingRecord, "page_id")
	if databaseID == "" && !pyrepr.Truthy(pageID) {
		return nil, &TeamsPipelineSinkError{TeamsPipelineError{"Notion sink requires database_id or an existing page_id."}}
	}
	headers := [][2]string{{"Authorization", "Bearer " + w.APIKey}, {"Notion-Version", notionAPIVersion}, {"Content-Type", "application/json"}}
	properties, err := w.buildProperties(payload, config)
	if err != nil {
		return nil, err
	}
	var record any
	if pyrepr.Truthy(pageID) {
		record, err = w.request(ctx, "PATCH", notionAPIBase+"/pages/"+pyrepr.Str(pageID), headers, obj("properties", properties))
	} else {
		body := obj("parent", obj("database_id", databaseID), "properties", properties, "children", w.buildBlocks(payload))
		record, err = w.request(ctx, "POST", notionAPIBase+"/pages", headers, body)
	}
	if err != nil {
		return nil, err
	}
	id, err := subscript(record, "id")
	if err != nil {
		return nil, err
	}
	return obj("page_id", id, "url", get(record.(*pyjson.Object), "url")), nil
}

// JSON object key of a property name the way a dict key is hashed and dumped
func propertyKey(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case nil:
		return "null", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case []any:
		return "", pyError("TypeError", "unhashable type: 'list'")
	case *pyjson.Object, map[string]any:
		return "", pyError("TypeError", "unhashable type: 'dict'")
	}
	return pyrepr.Str(v), nil
}

// Title property plus the optional summary and meeting id properties named by the config
func (w *NotionWriter) buildProperties(payload *TeamsMeetingSummaryPayload, config *pyjson.Object) (*pyjson.Object, error) {
	title := payload.Title
	if !pyrepr.Truthy(title) {
		id, err := payloadMeetingID(payload)
		if err != nil {
			return nil, err
		}
		title = "Meeting " + id
	}
	titleProperty, ok := config.Get("title_property")
	if !ok {
		titleProperty = "Name"
	}
	key, err := propertyKey(titleProperty)
	if err != nil {
		return nil, err
	}
	properties := obj(key, obj("title", []any{obj("text", obj("content", title))}))
	if summaryProperty := get(config, "summary_property"); pyrepr.Truthy(summaryProperty) {
		if key, err = propertyKey(summaryProperty); err != nil {
			return nil, err
		}
		head, err := summaryHead(orDefault(payload.Summary, ""))
		if err != nil {
			return nil, err
		}
		properties.Set(key, richText(head))
	}
	if meetingIDProperty := get(config, "meeting_id_property"); pyrepr.Truthy(meetingIDProperty) {
		if key, err = propertyKey(meetingIDProperty); err != nil {
			return nil, err
		}
		id, err := payloadMeetingID(payload)
		if err != nil {
			return nil, err
		}
		properties.Set(key, richText(id))
	}
	return properties, nil
}

// Heading and paragraph block pair for each summary section
func (w *NotionWriter) buildBlocks(payload *TeamsMeetingSummaryPayload) []any {
	blocks := []any{}
	for _, s := range sections(payload) {
		blocks = append(blocks, obj("object", "block", "type", "heading_2", "heading_2", richText(s.heading)),
			obj("object", "block", "type", "paragraph", "paragraph", richText(orDefault(s.body, "None"))))
	}
	return blocks
}

// Sink that writes a meeting summary as a Linear issue
type LinearWriter struct{ httpSinkWriter }

// Linear sink keyed by the explicit key or the LINEAR_API_KEY secret
func NewLinearWriter(ctx context.Context, apiKey string, transport http.RoundTripper) (*LinearWriter, error) {
	w, err := newHTTPSinkWriter(ctx, "LINEAR_API_KEY", apiKey, transport)
	if err != nil {
		return nil, err
	}
	return &LinearWriter{w}, nil
}

// Updates the recorded issue or creates one for the configured team, returning its id, identifier and URL
func (w *LinearWriter) WriteSummary(ctx context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error) {
	if err := w.requireAPIKey(); err != nil {
		return nil, err
	}
	headers := [][2]string{{"Authorization", w.APIKey}, {"Content-Type", "application/json"}}
	if config == nil {
		return nil, pyError("AttributeError", "'NoneType' object has no attribute 'get'")
	}
	teamID := strField(or(get(config, "team_id"), get(config, "teamId"), ""))
	title := payload.Title
	if !pyrepr.Truthy(title) {
		id, err := payloadMeetingID(payload)
		if err != nil {
			return nil, err
		}
		title = "Meeting Summary: " + id
	}
	description, err := renderSummaryMarkdown(payload)
	if err != nil {
		return nil, err
	}
	issueInput := obj("title", title, "description", description)
	var body *pyjson.Object
	if existingIssueID := get(existingRecord, "issue_id"); pyrepr.Truthy(existingIssueID) {
		body = obj("query", linearUpdate, "variables", obj("id", existingIssueID, "input", issueInput))
	} else if teamID == "" {
		return nil, &TeamsPipelineSinkError{TeamsPipelineError{"Linear sink requires team_id when creating a new issue."}}
	} else {
		input := obj("teamId", teamID)
		input.Update(issueInput)
		body = obj("query", linearCreate, "variables", obj("input", input))
	}
	payloadJSON, err := w.request(ctx, "POST", linearAPIURL, headers, body)
	if err != nil {
		return nil, err
	}
	data, err := dictGetAttr(payloadJSON, "data")
	if err != nil {
		return nil, err
	}
	var issue any
	for _, op := range []string{"issueUpdate", "issueCreate"} {
		result, err := orDictGet(data, op)
		if err != nil {
			return nil, err
		}
		if issue, err = orDictGet(result, "issue"); err != nil {
			return nil, err
		}
		if pyrepr.Truthy(issue) {
			break
		}
	}
	found, ok := issue.(*pyjson.Object)
	if !ok || found == nil || !pyrepr.Truthy(get(found, "id")) {
		return nil, &TeamsPipelineSinkError{TeamsPipelineError{"Linear write failed: " + pyrepr.Str(payloadJSON)}}
	}
	return obj("issue_id", get(found, "id"), "identifier", get(found, "identifier"), "url", get(found, "url")), nil
}

// Transcript first Teams meeting pipeline with durable lifecycle state
type TeamsMeetingPipeline struct {
	GraphClient  graphClient
	Store        *TeamsPipelineStore
	Config       *TeamsPipelineConfig
	TranscribeFn TranscribeFn
	SummarizeFn  SummarizeFn
	NotionWriter SummarySink
	LinearWriter SummarySink
	TeamsSender  SummarySink
}

// Speech to text of an audio file through the configured transcription provider
func transcribeAudio(ctx context.Context, filePath, model string) (map[string]any, error) {
	return media.TranscribeAudio(ctx, filePath, model, "")
}

// Pipeline over a Graph client and store with the default transcriber, the built in summarizer and no sinks
func NewTeamsMeetingPipeline(client graphClient, store *TeamsPipelineStore, config any) (*TeamsMeetingPipeline, error) {
	cfg, ok := config.(*TeamsPipelineConfig)
	if ok && cfg == nil {
		config = nil
	}
	if cfg == nil {
		var err error
		if cfg, err = TeamsPipelineConfigFromDict(config); err != nil {
			return nil, err
		}
	}
	p := &TeamsMeetingPipeline{GraphClient: client, Store: store, Config: cfg, TranscribeFn: transcribeAudio}
	p.SummarizeFn = p.generateSummaryPayload
	return p, nil
}

// Records the notification receipt and returns its existing job or a newly stored received job
func (p *TeamsMeetingPipeline) CreateJobFromNotification(notification *pyjson.Object) (*TeamsMeetingPipelineJob, error) {
	if notification == nil {
		return nil, pyError("AttributeError", "'NoneType' object has no attribute 'get'")
	}
	eventID, err := BuildNotificationReceiptKey(notification)
	if err != nil {
		return nil, err
	}
	if _, err := p.Store.RecordNotificationReceipt(eventID, notification, ""); err != nil {
		return nil, err
	}
	existing, err := p.findJobByDedupeKey(eventID)
	if err != nil || existing != nil {
		return existing, err
	}
	resourceData := or(get(notification, "resourceData"), pyjson.NewObject())
	meetingID, organizerUserID, extraMetadata, err := meetingIDsFromNotification(notification)
	if err != nil {
		return nil, err
	}
	sourceEventType := pyrepr.Str(or(get(notification, "changeType"), "graph.notification"))
	tenantID, err := dictGetAttr(resourceData, "tenantId")
	if err != nil {
		return nil, err
	}
	joinWebURL, _ := dictGetAttr(resourceData, "joinWebUrl")
	callRecordID, _ := dictGetAttr(resourceData, "callRecordId")
	metadata := obj("notification", notification.Copy(), "join_web_url", joinWebURL, "call_record_id", or(callRecordID, get(notification, "callRecordId")))
	metadata.Update(extraMetadata)
	var organizer any
	if organizerUserID != "" {
		organizer = organizerUserID
	}
	meetingRef, err := newMeetingRef(meetingID, organizer, nil, nil, nil, or(tenantID, get(notification, "tenantId")), metadata)
	if err != nil {
		return nil, err
	}
	job := &TeamsMeetingPipelineJob{JobID: "teams-job-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12], EventID: eventID, DedupeKey: eventID,
		Status: "received", SourceEventType: sourceEventType, MeetingRef: meetingRef, ErrorInfo: pyjson.NewObject()}
	for _, f := range [][2]string{{"event_id", job.EventID}, {"source_event_type", job.SourceEventType}, {"dedupe_key", job.DedupeKey}} {
		if pystr.Strip(f[1]) == "" {
			return nil, requiredError("TeamsMeetingPipelineJob", f[0])
		}
	}
	if _, err := p.Store.UpsertJob(job.JobID, job.ToDict()); err != nil {
		return nil, err
	}
	return job, nil
}

// Accepts a notification and runs its job unless a duplicate already finished or is in flight
func (p *TeamsMeetingPipeline) RunNotification(ctx context.Context, notification *pyjson.Object) (*TeamsMeetingPipelineJob, error) {
	job, err := p.CreateJobFromNotification(notification)
	if err != nil {
		return nil, err
	}
	if terminalPipelineStates[job.Status] || (activePipelineStates[job.Status] && job.Status != "received") {
		return job, nil
	}
	return p.RunJob(ctx, job.JobID)
}

// Runs a job through resolve, transcript, summary and sinks, persisting completed, retry_scheduled or failed
func (p *TeamsMeetingPipeline) RunJob(ctx context.Context, jobOrID any) (*TeamsMeetingPipelineJob, error) {
	job, err := p.coerceJob(jobOrID)
	if err != nil {
		return nil, err
	}
	meetingRef := job.MeetingRef
	if meetingRef == nil {
		return nil, &TeamsPipelineError{fmt.Sprintf("Job %s has no meeting_ref.", job.JobID)}
	}
	artifacts := []*MeetingArtifact{}
	body := func() (*TeamsMeetingPipelineJob, error) {
		next, resolvedMeeting, notification, err := p.resolveMeeting(ctx, job, meetingRef)
		if err != nil {
			return nil, err
		}
		job = next
		next, transcriptText, err := p.obtainTranscript(ctx, job, resolvedMeeting, &artifacts)
		if err != nil {
			return nil, err
		}
		job = next
		callRecordID, err := dictGetAttr(notification, "callRecordId")
		if err != nil {
			return nil, err
		}
		if !pyrepr.Truthy(callRecordID) {
			callRecordID = get(meetingRef.Metadata, "call_record_id")
		}
		callRecord, err := pipelineEnrichMeetingWithCallRecord(ctx, p.GraphClient, resolvedMeeting, textOrEmpty(callRecordID), true)
		if err != nil {
			return nil, err
		}
		if callRecord != nil {
			artifacts = append(artifacts, callRecord)
		}
		if next, err = p.persistJob(job, "status", "summarizing"); err != nil {
			return nil, err
		}
		job = next
		raw, err := p.SummarizeFn(ctx, resolvedMeeting, transcriptText, artifacts)
		if err != nil {
			return nil, err
		}
		summaryPayload, err := coerceSummaryPayload(raw)
		if err != nil {
			return nil, err
		}
		job.SummaryPayload = summaryPayload
		if next, err = p.persistJob(job, "summary_payload", summaryPayload.ToDict()); err != nil {
			return nil, err
		}
		job = next
		if err := p.writeSinks(ctx, job, summaryPayload); err != nil {
			return nil, err
		}
		return p.persistJob(job, "status", "completed")
	}
	done, err := func() (finished *TeamsMeetingPipelineJob, err error) {
		defer func() {
			if raised := recover(); raised != nil {
				if cause, ok := raised.(error); ok {
					finished, err = nil, cause
				} else {
					finished, err = nil, errors.New(fmt.Sprint(raised))
				}
			}
		}()
		return body()
	}()
	if err == nil {
		return done, nil
	}
	if cancelled := ctx.Err(); cancelled != nil && errors.Is(err, cancelled) {
		return nil, err
	}
	var retryable *TeamsPipelineRetryableError
	if errors.As(err, &retryable) {
		return p.persistJob(job, "status", "retry_scheduled", "error_info", obj("message", pyrepr.ExceptionMessage(err), "retryable", true))
	}
	return p.persistJob(job, "status", "failed", "error_info", obj("message", pyrepr.ExceptionMessage(err), "type", pyrepr.ExceptionName(err)))
}

// Summary payload of a summarizer result, a dict result built through the payload constructor
func coerceSummaryPayload(raw any) (*TeamsMeetingSummaryPayload, error) {
	if payload, ok := raw.(*TeamsMeetingSummaryPayload); ok && payload != nil {
		return payload, nil
	} else if ok {
		raw = nil
	}
	dict, err := mapping(orderedValue(raw))
	if err != nil {
		return nil, pyError("TypeError", err.Error())
	}
	return TeamsMeetingSummaryPayloadFromDict(dict)
}

// Resolves the Graph meeting of a job and returns the job, the resolved meeting and the stored notification
func (p *TeamsMeetingPipeline) resolveMeeting(ctx context.Context, job *TeamsMeetingPipelineJob, meetingRef *TeamsMeetingRef) (*TeamsMeetingPipelineJob, *TeamsMeetingRef, any, error) {
	job, err := p.persistJob(job, "status", "resolving_meeting")
	if err != nil {
		return nil, nil, nil, err
	}
	notification := get(meetingRef.Metadata, "notification")
	meetingID, organizerUserID := meetingRef.MeetingID, meetingRef.OrganizerUserID
	if stored, ok := notification.(*pyjson.Object); ok && stored != nil && stored.Len() > 0 {
		parsedID, parsedOrg, _, err := meetingIDsFromNotification(stored)
		if err != nil {
			return nil, nil, nil, err
		}
		if parsedOrg != "" {
			organizerUserID = or(organizerUserID, parsedOrg)
		}
		if parsedID != "" && !looksLikeTranscriptID(parsedID, nil) {
			meetingID = parsedID
		}
	}
	joinWebURL := or(meetingRef.JoinWebURL, get(meetingRef.Metadata, "join_web_url"))
	resolvedMeeting, err := pipelineResolveMeetingReference(ctx, p.GraphClient, meetingID, textOrEmpty(joinWebURL), textOrEmpty(meetingRef.TenantID), textOrEmpty(organizerUserID))
	if err != nil {
		return nil, nil, nil, err
	}
	if meetingRef.Metadata != nil && meetingRef.Metadata.Len() > 0 {
		merged := meetingRef.Metadata.Copy()
		merged.Update(resolvedMeeting.Metadata)
		resolvedMeeting.Metadata = merged
	}
	resolvedMeeting.OrganizerUserID = or(resolvedMeeting.OrganizerUserID, meetingRef.OrganizerUserID)
	job.MeetingRef = resolvedMeeting
	if job, err = p.persistJob(job, "meeting_ref", resolvedMeeting.ToDict()); err != nil {
		return nil, nil, nil, err
	}
	return job, resolvedMeeting, notification, nil
}

// Transcript text from the preferred transcript, else from the first recording through speech to text
func (p *TeamsMeetingPipeline) obtainTranscript(ctx context.Context, job *TeamsMeetingPipelineJob, resolvedMeeting *TeamsMeetingRef, artifacts *[]*MeetingArtifact) (*TeamsMeetingPipelineJob, string, error) {
	transcriptText := ""
	var err error
	if p.Config.TranscriptPreferred {
		if job, err = p.persistJob(job, "status", "fetching_transcript"); err != nil {
			return nil, "", err
		}
		var transcriptArtifact *MeetingArtifact
		if transcriptArtifact, transcriptText, err = pipelineFetchPreferredTranscriptText(ctx, p.GraphClient, resolvedMeeting); err != nil {
			return nil, "", err
		}
		if transcriptArtifact != nil && transcriptText != "" {
			*artifacts = append(*artifacts, transcriptArtifact)
			if int64(utf8.RuneCountInString(pystr.Strip(transcriptText))) < p.Config.TranscriptMinChars {
				transcriptText = ""
			}
		}
	}
	if transcriptText != "" {
		if job, err = p.persistJob(job, "selected_artifact_strategy", "transcript_first"); err != nil {
			return nil, "", err
		}
		return job, transcriptText, nil
	}
	if p.Config.TranscriptRequired {
		return nil, "", &TeamsPipelineRetryableError{TeamsPipelineError{fmt.Sprintf("Transcript unavailable for meeting %s.", resolvedMeeting.MeetingID)}}
	}
	if !p.Config.TranscriptionFallback {
		return nil, "", &TeamsPipelineArtifactNotFoundError{TeamsPipelineRetryableError{TeamsPipelineError{
			fmt.Sprintf("No transcript available and transcription fallback disabled for %s.", resolvedMeeting.MeetingID)}}}
	}
	if job, err = p.persistJob(job, "status", "downloading_recording"); err != nil {
		return nil, "", err
	}
	recordings, err := pipelineListRecordingArtifacts(ctx, p.GraphClient, resolvedMeeting)
	if err != nil {
		return nil, "", err
	}
	if len(recordings) == 0 {
		return nil, "", &TeamsPipelineRetryableError{TeamsPipelineError{fmt.Sprintf("Recording unavailable for meeting %s.", resolvedMeeting.MeetingID)}}
	}
	*artifacts = append(*artifacts, recordings[0])
	if transcriptText, err = p.transcribeRecording(ctx, job, resolvedMeeting, recordings[0]); err != nil {
		return nil, "", err
	}
	if job, err = p.persistJob(job, "selected_artifact_strategy", "recording_stt_fallback"); err != nil {
		return nil, "", err
	}
	return job, transcriptText, nil
}

// Job itself or the stored job of an id, a pipeline error for an unknown id
func (p *TeamsMeetingPipeline) coerceJob(jobOrID any) (*TeamsMeetingPipelineJob, error) {
	if job, ok := jobOrID.(*TeamsMeetingPipelineJob); ok && job != nil {
		return job, nil
	}
	payload := p.Store.GetJob(pyrepr.Str(jobOrID))
	if payload == nil || payload.Len() == 0 {
		return nil, &TeamsPipelineError{"Unknown Teams pipeline job: " + pyrepr.Str(jobOrID)}
	}
	return TeamsMeetingPipelineJobFromDict(payload)
}

// Stored job whose dedupe key equals the given key, nil when none does
func (p *TeamsMeetingPipeline) findJobByDedupeKey(dedupeKey string) (*TeamsMeetingPipelineJob, error) {
	jobs := p.Store.ListJobs()
	for _, id := range jobs.Keys {
		if payload, ok := jobs.Vals[id].(*pyjson.Object); ok && payload != nil && textOrEmpty(get(payload, "dedupe_key")) == dedupeKey {
			return TeamsMeetingPipelineJobFromDict(payload)
		}
	}
	return nil, nil
}

// Merges the job and the key value updates into the store and returns the stored job
func (p *TeamsMeetingPipeline) persistJob(job *TeamsMeetingPipelineJob, updates ...any) (*TeamsMeetingPipelineJob, error) {
	payload := job.ToDict()
	for i := 0; i+1 < len(updates); i += 2 {
		payload.Set(updates[i].(string), updates[i+1])
	}
	stored, err := p.Store.UpsertJob(job.JobID, payload)
	if err != nil {
		return nil, err
	}
	return TeamsMeetingPipelineJobFromDict(stored)
}

// Downloads a recording into a scratch directory under its basename and returns its speech to text transcript
func (p *TeamsMeetingPipeline) transcribeRecording(ctx context.Context, job *TeamsMeetingPipelineJob, meetingRef *TeamsMeetingRef, recording *MeetingArtifact) (string, error) {
	tempRoot := p.Config.TmpDir
	if tempRoot == "" {
		tempRoot = filepath.Join(home.Dir(ctx), "tmp", "teams_pipeline")
	}
	if err := os.MkdirAll(tempRoot, 0o777); err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp(tempRoot, "teams-recording-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	if tmpDir, err = filepath.Abs(tmpDir); err != nil {
		return "", err
	}
	fallbackName := recording.ArtifactID + ".mp4"
	displayName := fallbackName
	if pyrepr.Truthy(recording.DisplayName) {
		text, ok := recording.DisplayName.(string)
		if !ok {
			return "", pyError("TypeError", "expected str, bytes or os.PathLike object, not "+pyrepr.TypeName(recording.DisplayName))
		}
		displayName = text
	}
	recordingName := pypath.PathName(displayName)
	if recordingName == "" || recordingName == "." || recordingName == ".." {
		recordingName = fallbackName
	}
	recordingPath := pypath.PathJoin(tmpDir, recordingName)
	if _, err := pipelineDownloadRecordingArtifact(ctx, p.GraphClient, meetingRef, recording, recordingPath); err != nil {
		return "", err
	}
	audioPath, err := pipelinePrepareAudioPath(p, ctx, recordingPath)
	if err != nil {
		return "", err
	}
	if _, err := p.persistJob(job, "status", "transcribing_audio"); err != nil {
		return "", err
	}
	result, err := p.TranscribeFn(ctx, audioPath, textOrEmpty(p.Config.STTModel))
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", pyError("AttributeError", "'NoneType' object has no attribute 'get'")
	}
	if !pyrepr.Truthy(result["success"]) {
		return "", &TeamsPipelineRetryableError{TeamsPipelineError{pyrepr.Str(or(result["error"], "Unknown STT failure"))}}
	}
	transcript := strField(result["transcript"])
	if transcript == "" {
		return "", &TeamsPipelineRetryableError{TeamsPipelineError{"STT returned an empty transcript."}}
	}
	return transcript, nil
}

// Recording itself when it is audio or extraction is off, else a wav extracted beside it by ffmpeg
func (p *TeamsMeetingPipeline) prepareAudioPath(ctx context.Context, recordingPath string) (string, error) {
	if audioSuffixes[pystr.Lower(pypath.PathSuffix(recordingPath))] || !p.Config.FFmpegExtractAudio {
		return recordingPath, nil
	}
	ffmpeg, ok := pyshutil.Which("ffmpeg", nil)
	if !ok || ffmpeg == "" {
		return "", &TeamsPipelineRetryableError{TeamsPipelineError{"Recording fallback requires ffmpeg for audio extraction, but ffmpeg was not found."}}
	}
	audioPath, err := pypath.WithSuffix(recordingPath, ".wav")
	if err != nil {
		return "", pyError("ValueError", err.Error())
	}
	proc, err := pysubprocess.RunWith(ctx, []string{ffmpeg, "-y", "-i", recordingPath, audioPath}, pysubprocess.Options{})
	if err != nil {
		return "", err
	}
	if proc.Returncode != 0 {
		stderr, err := pycodecs.DecodeReplace(proc.Stderr, "utf-8")
		if err != nil {
			return "", err
		}
		return "", &TeamsPipelineRetryableError{TeamsPipelineError{"ffmpeg audio extraction failed: " + pystr.Strip(stderr)}}
	}
	return audioPath, nil
}

// Summary fields the model returned, or the heuristic fields when the model call or its JSON fails
func (p *TeamsMeetingPipeline) summaryFieldsFor(ctx context.Context, prompt, transcriptText string) (summaryFields, error) {
	response, err := provider.AsyncCallLLM(ctx, map[string]any{"task": "call", "temperature": 0.2, "max_tokens": 900,
		"messages": []any{map[string]any{"role": "system", "content": summarySystemPrompt}, map[string]any{"role": "user", "content": prompt}}})
	var parsed summaryFields
	if err == nil {
		parsed, err = parseSummaryJSON(provider.ExtractContentOrReasoning(response, -1))
	}
	if err != nil {
		if cancelled := ctx.Err(); cancelled != nil && errors.Is(err, cancelled) {
			return summaryFields{}, err
		}
		slog.Info("Teams pipeline LLM summary unavailable, using heuristic summary: " + pyrepr.ExceptionMessage(err))
		parsed = heuristicSummary(transcriptText)
	}
	return parsed, nil
}

// Summary payload from the auxiliary model, the heuristic summary standing in when the model is unavailable
func (p *TeamsMeetingPipeline) generateSummaryPayload(ctx context.Context, resolvedMeeting *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) (any, error) {
	parsed, err := p.summaryFieldsFor(ctx, buildSummaryPrompt(resolvedMeeting, transcriptText, artifacts), transcriptText)
	if err != nil {
		return nil, err
	}
	teamsDelivery := or(p.Config.TeamsDelivery, pyjson.NewObject())
	participants, err := collectParticipants(resolvedMeeting)
	if err != nil {
		return nil, err
	}
	callMetrics, err := collectCallMetrics(artifacts)
	if err != nil {
		return nil, err
	}
	payload := &TeamsMeetingSummaryPayload{
		MeetingRef: resolvedMeeting, TranscriptText: transcriptText, SourceArtifacts: artifacts,
		Title:        pyrepr.Str(or(get(resolvedMeeting.Metadata, "subject"), "Meeting "+resolvedMeeting.MeetingID)),
		Participants: participants, CallMetrics: callMetrics, Summary: parsed.Summary, Confidence: parsed.Confidence, ConfidenceNotes: parsed.ConfidenceNotes,
		KeyDecisions: append([]any{}, parsed.KeyDecisions...), ActionItems: append([]any{}, parsed.ActionItems...), Risks: append([]any{}, parsed.Risks...),
	}
	if payload.NotionTarget, err = orDictGet(p.Config.Notion, "database_id"); err != nil {
		return nil, err
	}
	if payload.LinearTarget, err = orDictGet(p.Config.Linear, "team_id"); err != nil {
		return nil, err
	}
	channelID, err := dictGetAttr(teamsDelivery, "channel_id")
	if err != nil {
		return nil, err
	}
	chatID, _ := dictGetAttr(teamsDelivery, "chat_id")
	payload.TeamsTarget = or(channelID, chatID)
	if payload.StartTime, err = parseDatetime(get(resolvedMeeting.Metadata, "startDateTime")); err != nil {
		return nil, err
	}
	if payload.EndTime, err = parseDatetime(get(resolvedMeeting.Metadata, "endDateTime")); err != nil {
		return nil, err
	}
	return payload, nil
}

// Writes the summary to each enabled sink in Notion, Linear, Teams order and stores every sink record
func (p *TeamsMeetingPipeline) writeSinks(ctx context.Context, job *TeamsMeetingPipelineJob, payload *TeamsMeetingSummaryPayload) error {
	sinks := []struct {
		name, status string
		config       any
		sink         SummarySink
	}{{"notion", "writing_notion", p.Config.Notion, p.NotionWriter}, {"linear", "writing_linear", p.Config.Linear, p.LinearWriter},
		{"teams", "sending_teams", p.Config.TeamsDelivery, p.TeamsSender}}
	for _, s := range sinks {
		if !pyrepr.Truthy(s.config) {
			continue
		}
		enabled, err := dictGetAttr(s.config, "enabled")
		if err != nil {
			return err
		}
		if !pyrepr.Truthy(enabled) || s.sink == nil {
			continue
		}
		if job, err = p.persistJob(job, "status", s.status); err != nil {
			return err
		}
		meetingID, err := payloadMeetingID(payload)
		if err != nil {
			return err
		}
		sinkKey := s.name + ":" + meetingID
		config, err := mapping(s.config)
		if err != nil {
			return err
		}
		record, err := s.sink.WriteSummary(ctx, payload, config, p.Store.GetSinkRecord(sinkKey))
		if err != nil {
			return err
		}
		if record == nil {
			return pyError("TypeError", "'NoneType' object is not a mapping")
		}
		if _, err := p.Store.UpsertSinkRecord(sinkKey, record); err != nil {
			return err
		}
	}
	return nil
}

// Metrics of every call record artifact merged in order plus the artifact count
func collectCallMetrics(artifacts []*MeetingArtifact) (*pyjson.Object, error) {
	metrics := pyjson.NewObject()
	for _, artifact := range artifacts {
		if artifact.ArtifactType == "call_record" {
			found, err := dictField(get(artifact.Metadata, "metrics"))
			if err != nil {
				return nil, err
			}
			metrics.Update(found)
		}
	}
	metrics.Set("artifact_count", int64(len(artifacts)))
	return metrics, nil
}

// Display names of the meeting participants, from the participant or its identity user
func collectParticipants(meetingRef *TeamsMeetingRef) ([]any, error) {
	participants, ok := or(get(meetingRef.Metadata, "participants"), []any{}).([]any)
	if !ok {
		return []any{}, nil
	}
	names := []any{}
	for _, entry := range participants {
		item, ok := entry.(*pyjson.Object)
		if !ok || item == nil {
			continue
		}
		name := get(item, "displayName")
		if !pyrepr.Truthy(name) {
			user, err := orDictGet(get(item, "identity"), "user")
			if err != nil {
				return nil, err
			}
			if name, err = orDictGet(user, "displayName"); err != nil {
				return nil, err
			}
		}
		if pyrepr.Truthy(name) {
			names = append(names, pyrepr.Str(name))
		}
	}
	return names, nil
}

// OData annotation of a payload under its at prefixed name or its bare name
func odataField(payload *pyjson.Object, name string) any {
	return or(get(payload, "@"+name), get(payload, name))
}

// Organizer user id from the organizer user or identity user, else the flat organizer id fields
func organizerUserIDFromPayload(payload *pyjson.Object) string {
	if organizer, ok := or(get(payload, "meetingOrganizer"), get(payload, "organizer")).(*pyjson.Object); ok && organizer != nil {
		user, present := organizer.Get("user")
		if !present || user == nil {
			if identity, ok := get(organizer, "identity").(*pyjson.Object); ok && identity != nil {
				user = get(identity, "user")
			}
		}
		if found, ok := user.(*pyjson.Object); ok && found != nil && pyrepr.Truthy(get(found, "id")) {
			return pystr.Strip(pyrepr.Str(get(found, "id")))
		}
	}
	return strField(or(get(payload, "organizerUserId"), get(payload, "organizer_user_id"), ""))
}

// Whether the resourceData id names a transcript or recording artifact instead of a meeting
func resourceDataIDIsArtifact(notification, resourceData *pyjson.Object) bool {
	odataType := textOrEmpty(odataField(resourceData, "odata.type"))
	lowered := pystr.Lower(odataType)
	if strings.Contains(lowered, "calltranscript") || strings.Contains(lowered, "callrecording") {
		return true
	}
	resource := pystr.Lower(textOrEmpty(get(notification, "resource")))
	for _, marker := range []string{"getalltranscripts", "getallrecordings", "/transcripts", "/recordings"} {
		if strings.Contains(resource, marker) {
			return true
		}
	}
	return looksLikeTranscriptID(textOrEmpty(get(resourceData, "id")), odataType)
}

// Meeting id, organizer user id and artifact id metadata of a Graph change notification, the meeting id never empty
func meetingIDsFromNotification(notification *pyjson.Object) (string, string, *pyjson.Object, error) {
	resourceData, _ := get(notification, "resourceData").(*pyjson.Object)
	if resourceData == nil {
		resourceData = pyjson.NewObject()
	}
	odataType := textOrEmpty(odataField(resourceData, "odata.type"))
	var first graphMeetingResource
	for _, raw := range []any{odataField(resourceData, "odata.id"), get(notification, "resource"), get(resourceData, "transcriptContentUrl")} {
		parsed := parseGraphMeetingResource(textOrEmpty(raw))
		first.OrganizerUserID = firstText(first.OrganizerUserID, parsed.OrganizerUserID)
		first.MeetingID = firstText(first.MeetingID, parsed.MeetingID)
		first.TranscriptID = firstText(first.TranscriptID, parsed.TranscriptID)
		first.RecordingID = firstText(first.RecordingID, parsed.RecordingID)
	}
	organizerUserID := firstText(first.OrganizerUserID, organizerUserIDFromPayload(resourceData), organizerUserIDFromPayload(notification))
	meetingID := firstText(first.MeetingID, strField(or(get(resourceData, "meetingId"), get(notification, "meetingId"), "")))
	transcriptID, recordingID := first.TranscriptID, first.RecordingID
	resourceDataID := strField(get(resourceData, "id"))
	if resourceDataID != "" && !resourceDataIDIsArtifact(notification, resourceData) {
		meetingID = firstText(meetingID, resourceDataID)
	} else if resourceDataID != "" && transcriptID == "" && looksLikeTranscriptID(resourceDataID, odataType) {
		transcriptID = resourceDataID
	}
	if meetingID = firstText(meetingID, transcriptID, recordingID); meetingID == "" {
		key, err := BuildNotificationReceiptKey(notification)
		if err != nil {
			return "", "", nil, err
		}
		meetingID = key
	}
	extraMetadata := pyjson.NewObject()
	if transcriptID != "" {
		extraMetadata.Set("transcript_id", transcriptID)
	}
	if recordingID != "" {
		extraMetadata.Set("recording_id", recordingID)
	}
	return meetingID, organizerUserID, extraMetadata, nil
}

// First non empty text, empty when every value is empty
func firstText(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Model prompt naming the meeting, its artifacts and the first eighteen thousand transcript characters
func buildSummaryPrompt(meetingRef *TeamsMeetingRef, transcriptText string, artifacts []*MeetingArtifact) string {
	artifactLines := make([]string, len(artifacts))
	for i, artifact := range artifacts {
		artifactLines[i] = "- " + artifact.ArtifactType + ":" + artifact.ArtifactID + ":" + summaryText(artifact.DisplayName)
	}
	listing := strings.Join(artifactLines, "\n")
	if listing == "" {
		listing = "- none"
	}
	return "Meeting ID: " + meetingRef.MeetingID + "\n" +
		"Title: " + pyrepr.Str(or(get(meetingRef.Metadata, "subject"), "Unknown")) + "\n" +
		"Artifacts:\n" + listing + "\n\n" +
		"Transcript:\n" + pystr.Slice(transcriptText, 0, 18000)
}

// Stripped text of every item whose text is not blank
func cleanItems(values any) ([]any, error) {
	items, err := iterItems(values)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, item := range items {
		if text := pystr.Strip(pyrepr.Str(item)); text != "" {
			out = append(out, text)
		}
	}
	return out, nil
}

// Summary, decision, action and risk fields with the confidence of one summary
type summaryFields struct {
	Summary                          string
	KeyDecisions, ActionItems, Risks []any
	Confidence, ConfidenceNotes      string
}

// Summary fields of a model reply holding one JSON object, prose or code fences around it ignored
func parseSummaryJSON(content string) (summaryFields, error) {
	text := pystr.Strip(content)
	if text == "" {
		return heuristicSummary(""), nil
	}
	if start, end := strings.Index(text, "{"), strings.LastIndex(text, "}"); start >= 0 && end > start {
		text = text[start : end+1]
	}
	payload, err := pyjson.Loads(text)
	if err != nil {
		return summaryFields{}, err
	}
	field := func(key string) (any, error) { return dictGetAttr(payload, key) }
	summary, err := field("summary")
	if err != nil {
		return summaryFields{}, err
	}
	out := summaryFields{Summary: strField(summary)}
	dict := payload.(*pyjson.Object)
	for _, f := range []struct {
		key string
		dst *[]any
	}{{"key_decisions", &out.KeyDecisions}, {"action_items", &out.ActionItems}, {"risks", &out.Risks}} {
		values, ok := dict.Get(f.key)
		if !ok {
			values = []any{}
		}
		if *f.dst, err = cleanItems(values); err != nil {
			return summaryFields{}, err
		}
	}
	out.Confidence = pystr.Strip(pyrepr.Str(or(get(dict, "confidence"), "medium")))
	out.ConfidenceNotes = strField(get(dict, "confidence_notes"))
	return out, nil
}

// Summary fields derived from the transcript lines without a model
func heuristicSummary(transcriptText string) summaryFields {
	lines, lowered := []string{}, []string{}
	for _, line := range pystr.Splitlines(transcriptText, false) {
		if pystr.Strip(line) != "" {
			stripped := pystr.StripChars(line, " -*\t")
			lines, lowered = append(lines, stripped), append(lowered, pystr.Lower(stripped))
		}
	}
	pickLines := func(limit int, match func(low string) bool) []any {
		out := []any{}
		for i, low := range lowered {
			if match(low) && len(out) < limit {
				out = append(out, lines[i])
			}
		}
		return out
	}
	summary := pystr.Slice(strings.Join(lines[:min(3, len(lines))], " "), 0, 1200)
	if summary == "" {
		summary = "Transcript unavailable or too sparse for a confident summary."
	}
	confidence := "medium"
	if utf8.RuneCountInString(pystr.Strip(transcriptText)) < 300 {
		confidence = "low"
	}
	return summaryFields{
		Summary:      summary,
		KeyDecisions: pickLines(6, func(low string) bool { return strings.Contains(low, "decide") || strings.Contains(low, "decision") }),
		ActionItems: pickLines(8, func(low string) bool {
			for _, prefix := range []string{"action:", "todo:", "next step:", "follow up:"} {
				if strings.HasPrefix(low, prefix) {
					return true
				}
			}
			return false
		}),
		Risks:           pickLines(6, func(low string) bool { return strings.Contains(low, "risk") || strings.Contains(low, "blocker") }),
		Confidence:      confidence,
		ConfidenceNotes: "Generated with heuristic fallback because no LLM summary response was available.",
	}
}

// Markdown document of the summary with its sections and confidence lines
func renderSummaryMarkdown(payload *TeamsMeetingSummaryPayload) (string, error) {
	title := summaryText(payload.Title)
	if title == "" {
		id, err := payloadMeetingID(payload)
		if err != nil {
			return "", err
		}
		title = "Meeting " + id
	}
	lines := []any{"# " + title}
	for _, s := range sections(payload) {
		fallback := "- None"
		if s.heading == "Summary" {
			fallback = "No summary available."
		}
		lines = append(lines, "", "## "+s.heading, orDefault(s.body, fallback))
	}
	lines = append(lines, "", "Confidence: "+pyrepr.Str(orDefault(payload.Confidence, "unknown")), orDefault(payload.ConfidenceNotes, ""))
	text, err := joinLines(lines)
	if err != nil {
		return "", err
	}
	return pystr.Strip(text), nil
}
