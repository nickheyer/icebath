package langfuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	mediaQueueSize  = 100_000
	mediaMaxLevels  = 10
	mediaMaxRetries = 3
)

var (
	inputMediaKeys = map[string]bool{attrTraceInput: true, attrObsInput: true, "ai.prompt.messages": true, "ai.prompt": true,
		"ai.toolCall.args": true, "gcp.vertex.agent.llm_request": true, "gcp.vertex.agent.tool_call_args": true, "prompt": true,
		"lk.input_text": true, "lk.user_transcript": true, "lk.chat_ctx": true, "lk.user_input": true, "mlflow.spanInputs": true,
		"traceloop.entity.input": true, "input.value": true, "pydantic_ai.all_messages": true, "gen_ai.system_instructions": true,
		"input": true, "gen_ai.input.messages": true, "gen_ai.tool.call.arguments": true, "genkit:input": true, "tool_arguments": true}
	outputMediaKeys = map[string]bool{attrTraceOutput: true, attrObsOutput: true, "ai.response.text": true, "ai.result.text": true,
		"ai.toolCall.result": true, "ai.response.object": true, "ai.result.object": true, "ai.response.toolCalls": true,
		"ai.result.toolCalls": true, "gcp.vertex.agent.llm_response": true, "gcp.vertex.agent.tool_response": true,
		"all_messages_events": true, "lk.function_tool.output": true, "lk.response.text": true, "mlflow.spanOutputs": true,
		"traceloop.entity.output": true, "output.value": true, "final_result": true, "output": true, "gen_ai.output.messages": true,
		"gen_ai.tool.call.result": true, "genkit:output": true, "tool_response": true}
	inputMediaPrefixes  = []string{"gen_ai.prompt", "llm.input_messages"}
	outputMediaPrefixes = []string{"gen_ai.completion", "llm.output_messages"}
)

// Media found in a traced value, its decoded bytes, content type, source and content addressed id
type lfMedia struct {
	content     []byte
	hasContent  bool
	contentType string
	hasType     bool
	source      string
	mediaID     string
	hasID       bool
}

// Media parsed from a base64 data URI, an unparsable URI logged and left without content
func mediaFromDataURI(uri string) *lfMedia {
	m := &lfMedia{source: "base64_data_uri"}
	content, contentType, err := parseDataURI(uri)
	if err != nil {
		pkgLog().Error("Error parsing base64 data URI", "error", err)
	} else {
		m.content, m.hasContent, m.contentType, m.hasType = content, true, contentType, true
	}
	if m.hasContent {
		m.mediaID = strings.NewReplacer("+", "-", "/", "_").Replace(m.sha256Hash())[:22]
		m.hasID = true
	}
	return m
}

// Bytes and content type of a base64 data URI
func parseDataURI(data string) ([]byte, string, error) {
	if data == "" {
		return nil, "", errors.New("Data URI is not a string")
	}
	if !strings.HasPrefix(data, "data:") {
		return nil, "", errors.New("Data URI does not start with 'data:'")
	}
	header, actual, found := strings.Cut(data[5:], ",")
	if !found {
		return nil, "", errors.New("not enough values to unpack (expected 2, got 1)")
	}
	if header == "" || actual == "" {
		return nil, "", errors.New("Invalid URI")
	}
	parts := strings.Split(header, ";")
	isBase64 := false
	for _, p := range parts {
		if p == "base64" {
			isBase64 = true
		}
	}
	if !isBase64 {
		return nil, "", errors.New("Data is not base64 encoded")
	}
	if parts[0] == "" {
		return nil, "", errors.New("Content type is empty")
	}
	decoded, err := pybase64.B64Decode(strings.NewReplacer("-", "+", "_", "/").Replace(actual))
	if err != nil {
		return nil, "", err
	}
	return decoded, parts[0], nil
}

// Base64 SHA-256 of the content
func (m *lfMedia) sha256Hash() string {
	sum := sha256.Sum256(m.content)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Reference string Langfuse resolves to the uploaded media, false when the media is invalid
func (m *lfMedia) reference() (string, bool) {
	if !m.hasType || m.source == "" || !m.hasID {
		return "", false
	}
	return fmt.Sprintf("@@@langfuseMedia:type=%s|id=%s|source=%s@@@", m.contentType, m.mediaID, m.source), true
}

// Serialized form of the media, its reference or the upload failure marker
func (m *lfMedia) serialized() string {
	if ref, ok := m.reference(); ok {
		return ref
	}
	return uploadFailedMarker(m)
}

// Marker serialized in place of media whose upload could not be prepared
func uploadFailedMarker(m *lfMedia) string {
	contentType := "None"
	if m.hasType {
		contentType = m.contentType
	}
	return fmt.Sprintf("<Upload handling failed for LangfuseMedia of type %s>", contentType)
}

// Reports a Langfuse media reference string
func isMediaReference(v any) bool {
	s, ok := v.(string)
	return ok && strings.HasPrefix(s, "@@@langfuseMedia:") && strings.HasSuffix(s, "@@@")
}

// One queued media upload
type uploadJob struct {
	mediaID       string
	content       []byte
	contentType   string
	contentLength int
	sha256Hash    string
	traceID       string
	observationID string
	field         string
}

// Finds media in traced values and uploads it to Langfuse from background workers
type mediaManager struct {
	client  *lfClient
	enabled bool
	threads int
	http    *http.Client
	queue   chan *uploadJob
	mu      sync.Mutex
	idle    *sync.Cond
	pending int
	stopCh  chan struct{}
	stopped sync.Once
	workers sync.WaitGroup
}

// Media manager reading the upload switch and worker count from the SDK environment
func newMediaManager(c *lfClient) (*mediaManager, error) {
	threads := 1
	if text, ok := os.LookupEnv("LANGFUSE_MEDIA_UPLOAD_THREAD_COUNT"); ok {
		n, err := parseIntText("LANGFUSE_MEDIA_UPLOAD_THREAD_COUNT", text)
		if err != nil {
			return nil, err
		}
		threads = max(clampInt(n), 1)
	}
	switch strings.ToLower(envOr("LANGFUSE_MEDIA_UPLOAD_ENABLED", "True")) {
	case "false", "0":
		threads = 0
	}
	m := &mediaManager{client: c, enabled: threads > 0, threads: threads, queue: make(chan *uploadJob, mediaQueueSize),
		stopCh: make(chan struct{}),
		http:   &http.Client{Timeout: c.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	m.idle = sync.NewCond(&m.mu)
	return m, nil
}

// Starts the upload workers
func (m *mediaManager) start() {
	for i := range m.threads {
		m.workers.Add(1)
		go m.consume(i)
	}
}

// Stops the upload workers after the jobs they hold
func (m *mediaManager) stop() {
	m.stopped.Do(func() { close(m.stopCh) })
	m.workers.Wait()
}

// Waits until every queued upload has been processed, running them on the caller once the workers stopped
func (m *mediaManager) join() {
	select {
	case <-m.stopCh:
		for {
			select {
			case job := <-m.queue:
				m.run(job)
			default:
				return
			}
		}
	default:
	}
	m.mu.Lock()
	for m.pending > 0 {
		m.idle.Wait()
	}
	m.mu.Unlock()
}

// Uploads one job and marks it done whatever its outcome
func (m *mediaManager) run(job *uploadJob) {
	sdkDebug(fmt.Sprintf("Media: Processing upload for media_id=%s in trace_id=%s", job.mediaID, job.traceID))
	if err := m.upload(job); err != nil {
		pkgLog().Error(fmt.Sprintf("Media upload error: Failed to upload media due to unexpected error. Queue item marked as done. Error: %v", err))
	}
	m.mu.Lock()
	m.pending--
	if m.pending == 0 {
		m.idle.Broadcast()
	}
	m.mu.Unlock()
}

// Upload worker loop marking each job done whatever its outcome
func (m *mediaManager) consume(id int) {
	defer m.workers.Done()
	sdkDebug(fmt.Sprintf("Thread: Media upload consumer thread #%d started and actively processing queue items", id))
	for {
		select {
		case <-m.stopCh:
			sdkDebug(fmt.Sprintf("Thread: Pausing media upload consumer thread #%d", id))
			return
		case job := <-m.queue:
			m.run(job)
		}
	}
}

// Queues an upload for valid media
func (m *mediaManager) enqueue(media *lfMedia, traceID, observationID, field string) {
	if !media.hasContent || len(media.content) == 0 || !media.hasType {
		return
	}
	if !media.hasID {
		pkgLog().Error("Media ID is None. Skipping upload.")
		return
	}
	job := &uploadJob{mediaID: media.mediaID, content: media.content, contentType: media.contentType,
		contentLength: len(media.content), sha256Hash: media.sha256Hash(), traceID: traceID, observationID: observationID, field: field}
	m.mu.Lock()
	m.pending++
	m.mu.Unlock()
	select {
	case m.queue <- job:
		sdkDebug(fmt.Sprintf("Queue: Enqueued media ID %s for upload processing | trace_id=%s | field=%s", media.mediaID, traceID, field))
	default:
		m.mu.Lock()
		m.pending--
		if m.pending == 0 {
			m.idle.Broadcast()
		}
		m.mu.Unlock()
		pkgLog().Warn(fmt.Sprintf("Queue capacity: Media queue is full. Failed to process media_id=%s for trace_id=%s. Consider increasing queue capacity.",
			media.mediaID, traceID))
	}
}

// Value with every media occurrence replaced by media objects whose uploads are queued, true when any was found
func (m *mediaManager) findAndProcess(data any, traceID, observationID, field string) (any, bool) {
	if !m.enabled {
		return data, false
	}
	return m.walk(data, 1, traceID, observationID, field)
}

// One level of the media search
func (m *mediaManager) walk(data any, level int, traceID, observationID, field string) (any, bool) {
	if level > mediaMaxLevels {
		return data, false
	}
	if media, ok := data.(*lfMedia); ok {
		m.enqueue(media, traceID, observationID, field)
		return data, false
	}
	if s, ok := data.(string); ok {
		if head, _, found := strings.Cut(s, ","); strings.HasPrefix(s, "data:") && found && strings.HasSuffix(head, ";base64") {
			media := mediaFromDataURI(s)
			m.enqueue(media, traceID, observationID, field)
			return media, true
		}
		return data, false
	}
	if items, ok := dictItems(data); ok {
		kind, hasKind := dictGet(data, "type")
		_, hasData := dictGet(data, "data")
		for _, shape := range [][2]string{{"base64", "media_type"}, {"media", "mime_type"}} {
			_, hasMediaType := dictGet(data, shape[1])
			if hasKind && strEq(kind, shape[0]) && hasMediaType && hasData {
				raw := dget(data, "data")
				if isMediaReference(raw) {
					return data, false
				}
				media := mediaFromDataURI(concatStr("data:"+anyutil.String(dget(data, shape[1]))+";base64,", raw))
				m.enqueue(media, traceID, observationID, field)
				return replaceKey(items, "data", media), true
			}
		}
		for _, key := range []string{"inline_data", "inlineData"} {
			inline := dget(data, key)
			inlineItems, isInline := dictItems(inline)
			if !isInline {
				continue
			}
			raw, hasRaw := dictGet(inline, "data")
			if !hasRaw {
				continue
			}
			contentType := dget(inline, "mime_type")
			if !anyutil.IsTrue(contentType) {
				contentType = dget(inline, "mimeType")
			}
			if !anyutil.IsTrue(contentType) {
				continue
			}
			text, isStr := raw.(string)
			if !isStr {
				continue
			}
			if isMediaReference(text) {
				return data, false
			}
			media := mediaFromDataURI("data:" + anyutil.String(contentType) + ";base64," + text)
			m.enqueue(media, traceID, observationID, field)
			return replaceKey(items, key, replaceKey(inlineItems, "data", media)), true
		}
		out := jsonutil.NewObject()
		changed := false
		for _, it := range items {
			v, c := m.walk(it.val, level+1, traceID, observationID, field)
			out.Set(it.key, v)
			changed = changed || c
		}
		return out, changed
	}
	if elems, ok := listItems(data); ok {
		out := make([]any, len(elems))
		changed := false
		for i, e := range elems {
			v, c := m.walk(e, level+1, traceID, observationID, field)
			out[i] = v
			changed = changed || c
		}
		return out, changed
	}
	return data, false
}

// String concatenation that raises like Python for a non string operand
func concatStr(prefix string, v any) string {
	s, ok := v.(string)
	if !ok {
		panic(fmt.Sprintf("expected text, got %s", errchain.Kind(v)))
	}
	return prefix + s
}

// Copy of dict entries with one key set to a new value in place
func replaceKey(items []item, key string, val any) *jsonutil.Object {
	out := jsonutil.NewObject()
	for _, it := range items {
		out.Set(it.key, it.val)
	}
	out.Set(key, val)
	return out
}

// Upload URL answer of the media API
type uploadURLResponse struct {
	UploadURL *string `json:"uploadUrl"`
	MediaID   string  `json:"mediaId"`
}

// Status error of a non 2xx HTTP answer
type httpStatusError struct {
	status int
	body   string
	api    bool
}

func (e *httpStatusError) Error() string {
	if e.api {
		return fmt.Sprintf("status_code: %d, body: %s", e.status, e.body)
	}
	return fmt.Sprintf("Client error '%d' for url", e.status)
}

// Runs fn up to three times with jittered exponential waits, giving up at once on a 4xx other than 429
func requestWithBackoff[T any](fn func() (T, error)) (T, error) {
	var out T
	var err error
	wait := 1.0
	for attempt := 1; attempt <= mediaMaxRetries; attempt++ {
		out, err = fn()
		if err == nil {
			return out, nil
		}
		var se *httpStatusError
		if errors.As(err, &se) && se.status >= 400 && se.status < 500 && se.status != 429 {
			return out, err
		}
		if errors.As(err, &se) && !se.api && se.status < 500 && se.status != 429 {
			return out, err
		}
		if attempt < mediaMaxRetries {
			time.Sleep(time.Duration(rand.Float64() * wait * float64(time.Second)))
			wait *= 2
		}
	}
	return out, err
}

// JSON call to the Langfuse public API with the SDK authentication and headers
func (m *mediaManager) api(method, path string, body any) ([]byte, error) {
	payload, err := jsonutil.Encode(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(context.Background(), method, m.client.baseURL+"/"+path, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(m.client.publicKey, m.client.secretKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-langfuse-sdk-name", "python")
	req.Header.Set("x-langfuse-sdk-version", sdkVersion)
	req.Header.Set("x-langfuse-public-key", m.client.publicKey)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpStatusError{status: resp.StatusCode, body: string(data), api: true}
	}
	return data, nil
}

// Upload of one job, presigned URL request, the PUT and the status patch
func (m *mediaManager) upload(job *uploadJob) error {
	answer, err := requestWithBackoff(func() (*uploadURLResponse, error) {
		data, err := m.api(http.MethodPost, "api/public/media", jsonutil.ObjectOf("traceId", nullable(job.traceID), "observationId", nullable(job.observationID),
			"datasetId", nil, "datasetItemId", nil, "contentType", job.contentType, "contentLength", job.contentLength,
			"sha256Hash", job.sha256Hash, "field", job.field))
		if err != nil {
			return nil, err
		}
		var r uploadURLResponse
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return &r, nil
	})
	if err != nil {
		return err
	}
	if answer.UploadURL == nil || *answer.UploadURL == "" {
		sdkDebug(fmt.Sprintf("Media status: Media with ID %s already uploaded. Skipping duplicate upload.", job.mediaID))
		return nil
	}
	if answer.MediaID != job.mediaID {
		pkgLog().Error(fmt.Sprintf("Media integrity error: Media ID mismatch between SDK (%s) and Server (%s). Upload cancelled. Please check media ID generation logic.",
			job.mediaID, answer.MediaID))
		return nil
	}
	uploadURL := *answer.UploadURL
	headers := map[string]string{"Content-Type": job.contentType}
	if !strings.Contains(uploadURL, "storage.googleapis.com") {
		headers["x-ms-blob-type"] = "BlockBlob"
		headers["x-amz-checksum-sha256"] = job.sha256Hash
	}
	type putResult struct {
		status int
		text   string
	}
	started := time.Now()
	put, err := requestWithBackoff(func() (putResult, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, uploadURL, bytes.NewReader(job.content))
		if err != nil {
			return putResult{}, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := m.http.Do(req)
		if err != nil {
			return putResult{}, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		res := putResult{resp.StatusCode, string(data)}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return res, &httpStatusError{status: resp.StatusCode, body: res.text}
		}
		return res, nil
	})
	elapsed := int(time.Since(started).Milliseconds())
	var statusErr *httpStatusError
	if err != nil && !errors.As(err, &statusErr) {
		return err
	}
	if err != nil {
		put = putResult{statusErr.status, statusErr.body}
	}
	_, patchErr := requestWithBackoff(func() ([]byte, error) {
		return m.api(http.MethodPatch, "api/public/media/"+job.mediaID, jsonutil.ObjectOf("uploadedAt", isoformatUTC(time.Now().UTC()),
			"uploadHttpStatus", put.status, "uploadHttpError", put.text, "uploadTimeMs", elapsed))
	})
	if patchErr != nil {
		return patchErr
	}
	if err != nil {
		return err
	}
	sdkDebug(fmt.Sprintf("Media upload: Successfully uploaded media_id=%s for trace_id=%s | status_code=%d | duration=%dms | size=%d bytes",
		job.mediaID, job.traceID, put.status, elapsed, job.contentLength))
	return nil
}

// Nil for an empty identifier
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Media field a span attribute key feeds
func mediaField(key string) string {
	if inputMediaKeys[key] || hasAnyPrefix(key, inputMediaPrefixes) {
		return "input"
	}
	if outputMediaKeys[key] || hasAnyPrefix(key, outputMediaPrefixes) {
		return "output"
	}
	return "metadata"
}

// Reports a prefix match
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Exporter stage that uploads media still inline in string attributes before the OTLP export
type mediaExporter struct {
	next  sdktrace.SpanExporter
	media *mediaManager
}

// Exports spans with media in their string attributes replaced by references
func (e *mediaExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		attrs := span.Attributes()
		changed := false
		processed := make([]attribute.KeyValue, len(attrs))
		for j, kv := range attrs {
			processed[j] = kv
			if v, ok := e.processAttribute(span, kv); ok {
				processed[j], changed = v, true
			}
		}
		if !changed {
			out[i] = span
			continue
		}
		stub := tracetest.SpanStubFromReadOnlySpan(span)
		stub.Attributes = processed
		out[i] = stub.Snapshot()
	}
	return e.next.ExportSpans(ctx, out)
}

// Shuts the wrapped exporter down
func (e *mediaExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

// Attribute with its media processed, false when nothing changed or processing failed
func (e *mediaExporter) processAttribute(span sdktrace.ReadOnlySpan, kv attribute.KeyValue) (out attribute.KeyValue, changed bool) {
	key := string(kv.Key)
	defer func() {
		if r := recover(); r != nil {
			sc := span.SpanContext()
			pkgLog().Warn(fmt.Sprintf("Media processing error: Failed to process span attribute before export. Leaving attribute unchanged. "+
				"span_name='%s' trace_id='%s' span_id='%s' attribute_key='%s' error='%v'", span.Name(), sc.TraceID(), sc.SpanID(), key, r))
			out, changed = kv, false
		}
	}()
	switch kv.Value.Type() {
	case attribute.STRING:
		s := kv.Value.AsString()
		if p := e.processString(span, key, s); p != s {
			return attribute.String(key, p), true
		}
	case attribute.STRINGSLICE:
		items := kv.Value.AsStringSlice()
		next := make([]string, len(items))
		differs := false
		for i, s := range items {
			next[i] = e.processString(span, key, s)
			differs = differs || next[i] != s
		}
		if differs {
			return attribute.StringSlice(key, next), true
		}
	}
	return kv, false
}

// String attribute with an inline data URI or serialized media replaced by references
func (e *mediaExporter) processString(span sdktrace.ReadOnlySpan, key, value string) string {
	field := mediaField(key)
	ids := func() (string, string) {
		sc := span.SpanContext()
		if !sc.TraceID().IsValid() || !sc.SpanID().IsValid() {
			panic("Trace ID and span ID are required for media processing")
		}
		return sc.TraceID().String(), sc.SpanID().String()
	}
	if head, _, found := strings.Cut(value, ","); strings.HasPrefix(value, "data:") && found && strings.HasSuffix(head, ";base64") {
		traceID, spanID := ids()
		processed, changed := e.media.findAndProcess(value, traceID, spanID, field)
		if media, ok := processed.(*lfMedia); ok {
			if ref, ok := media.reference(); ok {
				return ref
			}
		}
		if changed {
			return serializeMediaValue(processed)
		}
	}
	stripped := strings.TrimLeftFunc(value, unicode.IsSpace)
	if !strings.HasPrefix(stripped, "{") && !strings.HasPrefix(stripped, "[") {
		return value
	}
	if !mayContainSerializedMedia(value) {
		return value
	}
	parsed, err := jsonutil.Decode(value)
	if err != nil {
		return value
	}
	if !isDict(parsed) {
		if _, ok := listItems(parsed); !ok {
			return value
		}
	}
	traceID, spanID := ids()
	processed, changed := e.media.findAndProcess(parsed, traceID, spanID, field)
	if !changed {
		return value
	}
	return serializeMediaValue(processed)
}

// Reports a string that may hold serialized media worth parsing
func mayContainSerializedMedia(value string) bool {
	if strings.Contains(value, "data:") || strings.Contains(value, "inline_data") || strings.Contains(value, "inlineData") {
		return true
	}
	hasMediaKey := strings.Contains(value, `"media_type"`) || strings.Contains(value, `"mime_type"`) || strings.Contains(value, `"mimeType"`)
	return hasMediaKey && strings.Contains(value, `"data"`)
}

// Text of a processed attribute value, media as its reference or failure marker
func serializeMediaValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case *lfMedia:
		return t.serialized()
	}
	return eventJSON(v)
}
