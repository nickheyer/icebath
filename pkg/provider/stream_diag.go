package provider

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// StreamDiagHeaders lists the lowercased upstream headers captured per attempt
var StreamDiagHeaders = []string{
	"cf-ray", "cf-cache-status", "x-openrouter-provider", "x-openrouter-model", "x-openrouter-id",
	"x-request-id", "x-vercel-id", "via", "server", "x-forwarded-for",
}

// LargeRequestHintBytes is the body size above which a refused connect may be a size limit
const LargeRequestHintBytes = 256 * 1024

var streamDiagNow = time.Now

// StreamDiag is the per attempt diagnostic record the streaming functions fill and the retry block reads
type StreamDiag struct {
	StartedAt        time.Time
	FirstChunkAt     *time.Time
	Chunks           int
	Bytes            int
	Headers          [][2]string
	HTTPStatus       *int
	ServingProvider  string
	FinishReasonSeen bool
}

// StreamDiagAgent is the agent surface the stream drop log and status line read
type StreamDiagAgent interface {
	StreamProvider() string
	StreamBaseURL() string
	BufferDiagnosticStatus(text string)
	TouchActivity(desc string)
}

type apiErrorSummarizer interface{ SummarizeAPIError(err error) string }

type subagentIdentity interface {
	SubagentID() string
	DelegateDepth() int
}

type retryStatusBufferReader interface{ RetryStatusMessages() []string }

// FailedStreamRequest is the buffered request a failed stream attempt sent, Content nil when unknown
type FailedStreamRequest struct {
	URL     string
	Content []byte
}

type failedRequestCarrier interface{ FailedRequest() *FailedStreamRequest }

// StreamRequestError attaches the buffered request to a stream open failure
type StreamRequestError struct {
	Err     error
	Request *FailedStreamRequest
}

func (e *StreamRequestError) Error() string                       { return e.Err.Error() }
func (e *StreamRequestError) Unwrap() error                       { return e.Err }
func (e *StreamRequestError) FailedRequest() *FailedStreamRequest { return e.Request }
func (e *StreamRequestError) PyTypeName() string                  { return pyrepr.TypeName(e.Err) }

// Fresh diagnostic record for one stream attempt
func StreamDiagInit() *StreamDiag {
	return &StreamDiag{StartedAt: streamDiagNow(), Headers: [][2]string{}}
}

// Snapshots the HTTP status and the known headers at stream open so they survive an early drop
func StreamDiagCaptureResponse(diag *StreamDiag, resp *http.Response) {
	if resp == nil || diag == nil {
		return
	}
	status := resp.StatusCode
	diag.HTTPStatus = &status
	captured := [][2]string{}
	for _, name := range StreamDiagHeaders {
		if val := resp.Header.Get(name); val != "" {
			captured = append(captured, [2]string{name, pystr.Slice(val, 0, 120)})
		}
	}
	diag.Headers = captured
}

// Renders Outer(msg) <- Inner(msg) walking the wrapped causes, deduplicated and at most four deep
func FlattenExceptionChain(err error) string {
	var seen []error
	link := err
	for link != nil && len(seen) < 4 && !errorSeen(seen, link) {
		seen = append(seen, link)
		next := unwrapOne(link)
		if next == nil || sameError(next, link) {
			break
		}
		link = next
	}
	if len(seen) == 0 {
		return pyrepr.TypeName(err)
	}
	parts := make([]string, len(seen))
	for i, e := range seen {
		msg := strings.ReplaceAll(pystr.Strip(e.Error()), "\n", " ")
		if pystr.Len(msg) > 140 {
			msg = pystr.Slice(msg, 0, 140) + "…"
		}
		if msg != "" {
			parts[i] = fmt.Sprintf("%s(%s)", pyrepr.TypeName(e), msg)
		} else {
			parts[i] = pyrepr.TypeName(e)
		}
	}
	return strings.Join(parts, " <- ")
}

func unwrapOne(err error) error {
	if next := errors.Unwrap(err); next != nil {
		return next
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range multi.Unwrap() {
			if e != nil {
				return e
			}
		}
	}
	return nil
}

func sameError(a, b error) bool {
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	return ta == tb && ta.Comparable() && a == b
}

func errorSeen(seen []error, err error) bool {
	for _, e := range seen {
		if sameError(e, err) {
			return true
		}
	}
	return false
}

type streamDiagFields struct {
	httpStatus       string
	bytes            int
	chunks           int
	elapsed          float64
	ttfb             string
	servingProvider  string
	finishReasonSeen bool
	upstream         string
}

// Retry log fields of an attempt, a dash where a value is unknown
func diagFields(diag *StreamDiag) streamDiagFields {
	f := streamDiagFields{httpStatus: "-", ttfb: "-", servingProvider: "-", upstream: "-"}
	if diag == nil {
		return f
	}
	now := streamDiagNow()
	f.bytes, f.chunks = diag.Bytes, diag.Chunks
	started := diag.StartedAt
	if started.IsZero() {
		started = now
	}
	f.elapsed = math.Max(0, now.Sub(started).Seconds())
	if diag.FirstChunkAt != nil {
		f.ttfb = fmt.Sprintf("%.2fs", math.Max(0, diag.FirstChunkAt.Sub(started).Seconds()))
	}
	if len(diag.Headers) > 0 {
		pairs := make([]string, len(diag.Headers))
		for i, kv := range diag.Headers {
			pairs[i] = kv[0] + "=" + kv[1]
		}
		f.upstream = strings.Join(pairs, " ")
	}
	if diag.HTTPStatus != nil {
		f.httpStatus = fmt.Sprint(*diag.HTTPStatus)
	}
	if diag.ServingProvider != "" {
		f.servingProvider = diag.ServingProvider
	}
	f.finishReasonSeen = diag.FinishReasonSeen
	return f
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Logs the structured warning for a transient stream drop and retry with the dying attempt's upstream facts
func LogStreamRetry(agent StreamDiagAgent, kind string, err error, attempt, maxAttempts int, midToolCall bool, diag *StreamDiag) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("stream-retry log emit failed", "panic", r)
		}
	}()
	summary := summarizeForRetryLog(agent, err)
	if summary != "" && pystr.Len(summary) > 240 {
		summary = pystr.Slice(summary, 0, 240) + "…"
	}
	subagentID, depth := "-", 0
	if ident, ok := agent.(subagentIdentity); ok {
		subagentID, depth = orDash(ident.SubagentID()), ident.DelegateDepth()
	}
	f := diagFields(diag)
	slog.Warn(fmt.Sprintf("Stream %s on attempt %d/%d — retrying. subagent_id=%s depth=%d provider=%s base_url=%s "+
		"error_type=%s error=%s chain=%s http_status=%s bytes=%d chunks=%d elapsed=%.2fs ttfb=%s "+
		"serving_provider=%s finish_reason_seen=%s upstream=[%s]",
		kind, attempt, maxAttempts, subagentID, depth, orDash(agent.StreamProvider()), orDash(agent.StreamBaseURL()),
		pyrepr.TypeName(err), summary, FlattenExceptionChain(err), f.httpStatus, f.bytes, f.chunks, f.elapsed, f.ttfb,
		f.servingProvider, pyBoolText(f.finishReasonSeen), f.upstream), "mid_tool_call", midToolCall)
}

func pyBoolText(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Summary of the error the agent renders, its plain text when the agent has no summarizer or it panics
func summarizeForRetryLog(agent StreamDiagAgent, err error) (summary string) {
	s, ok := agent.(apiErrorSummarizer)
	if !ok {
		return err.Error()
	}
	defer func() {
		if r := recover(); r != nil {
			summary = err.Error()
		}
	}()
	return s.SummarizeAPIError(err)
}

// Buffers one compact status line for a stream drop and retry after logging the full warning
func EmitStreamDrop(agent StreamDiagAgent, err error, attempt, maxAttempts int, midToolCall bool, diag *StreamDiag) {
	kind := "drop"
	if midToolCall {
		kind = "drop mid tool-call"
	}
	LogStreamRetry(agent, kind, err, attempt, maxAttempts, midToolCall, diag)
	provider := agent.StreamProvider()
	if provider == "" {
		provider = "provider"
	}
	suffix := ""
	if diag != nil && !diag.StartedAt.IsZero() {
		suffix = fmt.Sprintf(" after %.1fs", math.Max(0, streamDiagNow().Sub(diag.StartedAt).Seconds()))
	}
	defer func() { _ = recover() }()
	typeName := pyrepr.TypeName(err)
	agent.BufferDiagnosticStatus(fmt.Sprintf("⚠️ %s stream %s (%s)%s — attempt %d/%d dropped, reconnecting", provider, kind, typeName, suffix, attempt, maxAttempts))
	agent.TouchActivity(fmt.Sprintf("stream retry %d/%d after %s", attempt, maxAttempts, typeName))
}

// Buffered request carried by the error chain, nil when no link carries one
func failedRequest(err error) *FailedStreamRequest {
	link := err
	for i := 0; i < 8; i++ {
		if link == nil {
			return nil
		}
		if carrier, ok := link.(failedRequestCarrier); ok {
			if req := carrier.FailedRequest(); req != nil {
				return req
			}
		}
		link = unwrapOne(link)
	}
	return nil
}

func attemptsPlural(attempts int) string {
	if attempts != 1 {
		return "s"
	}
	return ""
}

// User facing line for a stream that never opened, naming host, attempts and request size
func ConnectExhaustedNotice(err error, attempts int, baseURL string) string {
	var lifetime *StreamLifetimeExceededError
	if errors.As(err, &lifetime) {
		return fmt.Sprintf("❌ %s (%d attempt%s)", lifetime.Error(), attempts, attemptsPlural(attempts))
	}
	req := failedRequest(err)
	raw := baseURL
	if req != nil && req.URL != "" {
		raw = req.URL
	}
	host := "the endpoint"
	if u, parseErr := pyurl.Split(raw); parseErr == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	line := fmt.Sprintf("❌ Could not open a stream to %s after %d attempt%s", host, attempts, attemptsPlural(attempts))
	if req == nil || req.Content == nil {
		return line + "; the endpoint looks unreachable — try again in a moment."
	}
	size := len(req.Content)
	line += fmt.Sprintf(" (request %d KB)", max(1, int(math.RoundToEven(float64(size)/1024))))
	if size >= LargeRequestHintBytes {
		return line + "; the endpoint or a proxy in front of it may reject requests this large."
	}
	return line + "; the endpoint looks unreachable — try again in a moment."
}

// Buffers the connect exhausted notice once per turn however many passes exhaust the same budget
func BufferConnectExhaustedNotice(agent StreamDiagAgent, err error, attempts int, baseURL string) {
	text := ConnectExhaustedNotice(err, attempts, baseURL)
	if reader, ok := agent.(retryStatusBufferReader); ok {
		for _, msg := range reader.RetryStatusMessages() {
			if msg == text {
				return
			}
		}
	}
	agent.BufferDiagnosticStatus(text)
}
