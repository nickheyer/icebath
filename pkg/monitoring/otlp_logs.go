package monitoring

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	respb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	otlpMaxRetries         = 6
	otlpDefaultTimeout     = 10.0
	otlpDefaultJitter      = 0.2
	otlpMaxRequestSize     = 64 * 1024 * 1024
	otlpExporterUserAgent  = "OTel-OTLP-Exporter-Python/1.45.1"
	blrpScheduleDelay      = 1000 * time.Millisecond
	blrpMaxExportBatchSize = 512
	blrpMaxQueueSize       = 2048
	blrpShutdownTimeout    = 30 * time.Second
)

var (
	otlpRetryableStatus = map[int]bool{429: true, 502: true, 503: true, 504: true}
	envHeaderOWS        = `[ \t]*`
	envHeaderKey        = `[\x21\x23-\x27\x2a\x2b\x2d\x2e\x30-\x39\x41-\x5a\x5e-\x7a\x7c\x7e]+`
	envHeaderPattern    = regexp.MustCompile(`^(?:` + envHeaderOWS + envHeaderKey + envHeaderOWS + `=` + envHeaderOWS + `[\x21\x23-\x2b\x2d-\x3a\x3c-\x5b\x5d-\x7e]*` + envHeaderOWS + `)$`)
	envHeaderLiberal    = regexp.MustCompile(`^(?:` + envHeaderOWS + envHeaderKey + envHeaderOWS + `=` + envHeaderOWS + `[\x20\x21\x23-\x2b\x2d-\x3a\x3c-\x5b\x5d-\x7e]*` + envHeaderOWS + `)$`)
	envHeaderDelimiter  = regexp.MustCompile(`[ \t]*,[ \t]*`)
)

func otlpHTTPLog() *slog.Logger {
	return slog.Default().With("logger", "opentelemetry.exporter.otlp.common.http")
}

// Headers of a W3C baggage formatted environment value, unencoded values accepted in liberal mode
func parseEnvHeaders(s string, liberal bool) map[string]string {
	headers := map[string]string{}
	for _, header := range envHeaderDelimiter.Split(s, -1) {
		if header == "" {
			continue
		}
		trimmed := strings.TrimSpace(header)
		if envHeaderPattern.MatchString(trimmed) {
			name, value, _ := strings.Cut(trimmed, "=")
			headers[strings.ToLower(strings.TrimSpace(unquote(name)))] = strings.TrimSpace(unquote(value))
			continue
		}
		if !liberal {
			otlpHTTPLog().Warn(fmt.Sprintf("Header format invalid! Header values in environment variables must be URL encoded per the OpenTelemetry Protocol Exporter specification: %s", header))
			continue
		}
		if !envHeaderLiberal.MatchString(trimmed) {
			otlpHTTPLog().Warn(fmt.Sprintf("Header format invalid! Header values in environment variables must be URL encoded per the OpenTelemetry Protocol Exporter specification or a comma separated list of name=value occurrences: %s", header))
			continue
		}
		name, value, _ := strings.Cut(trimmed, "=")
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	return headers
}

// Percent decoded text, the text unchanged when it holds an invalid escape
func unquote(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// Default protobuf headers overlaid with the signal or generic environment headers and the explicit ones
func resolveOTLPHeaders(headers map[string]string, signalEnv string) map[string]string {
	out := map[string]string{"content-type": "application/x-protobuf", "user-agent": otlpExporterUserAgent}
	env := os.Getenv(signalEnv)
	if env == "" {
		env = os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	for k, v := range parseEnvHeaders(env, true) {
		out[k] = v
	}
	for k, v := range headers {
		out[strings.ToLower(k)] = v
	}
	return out
}

// Export timeout seconds from the signal or generic environment variable, ten when unset or invalid
func resolveOTLPTimeout(signalEnv string) float64 {
	raw := os.Getenv(signalEnv)
	if raw == "" {
		raw = os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT")
	}
	if raw == "" {
		return otlpDefaultTimeout
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		otlpHTTPLog().Warn(fmt.Sprintf("Invalid timeout value '%s', using default of 10 seconds", raw))
		return otlpDefaultTimeout
	}
	return v
}

// Compression named by the signal or generic environment variable, none when unset or unsupported
func resolveOTLPCompression(signalEnv string) string {
	value := os.Getenv(signalEnv)
	if value == "" {
		value = os.Getenv("OTEL_EXPORTER_OTLP_COMPRESSION")
	}
	if value == "" {
		value = "none"
	}
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "none", "gzip", "deflate":
		return value
	}
	otlpHTTPLog().Warn("Unsupported compression type: " + value)
	return "none"
}

// otlpHTTPClient posts serialized OTLP requests with compression, bounded retries and Retry-After backoff
type otlpHTTPClient struct {
	client      *http.Client
	endpoint    string
	kind        string
	timeout     float64
	compression string
	headers     map[string]string
	jitter      float64
	shutdownMu  sync.Mutex
	isShutdown  bool
	shutdownSig chan struct{}
}

// Client for one signal endpoint with headers, timeout and compression resolved from the environment
func newOTLPHTTPClient(endpoint, kind, signal string, headers map[string]string) *otlpHTTPClient {
	upper := strings.ToUpper(signal)
	c := &otlpHTTPClient{
		client:      &http.Client{},
		endpoint:    endpoint,
		kind:        kind,
		timeout:     resolveOTLPTimeout("OTEL_EXPORTER_OTLP_" + upper + "_TIMEOUT"),
		compression: resolveOTLPCompression("OTEL_EXPORTER_OTLP_" + upper + "_COMPRESSION"),
		headers:     resolveOTLPHeaders(headers, "OTEL_EXPORTER_OTLP_"+upper+"_HEADERS"),
		jitter:      otlpDefaultJitter,
		shutdownSig: make(chan struct{}),
	}
	if c.compression != "none" {
		has := false
		for k := range c.headers {
			has = has || strings.ToLower(k) == "content-encoding"
		}
		if !has {
			c.headers["Content-Encoding"] = c.compression
		}
	}
	return c
}

func (c *otlpHTTPClient) computeBackoff(retry int) float64 {
	return math.Pow(2, float64(retry)) * (1 - c.jitter + rand.Float64()*2*c.jitter)
}

func (c *otlpHTTPClient) compress(data []byte) []byte {
	var buf bytes.Buffer
	switch c.compression {
	case "gzip":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(data)
		_ = w.Close()
		return buf.Bytes()
	case "deflate":
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(data)
		_ = w.Close()
		return buf.Bytes()
	}
	return data
}

type otlpHTTPResult struct {
	status int
	reason string
	header http.Header
	err    error
}

// Whether an error is a refused, reset or unreachable connection rather than an HTTP answer
func isConnectionError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial" || !opErr.Timeout()
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func (c *otlpHTTPClient) request(data []byte, timeout float64) otlpHTTPResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return otlpHTTPResult{err: err}
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return otlpHTTPResult{err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	res := otlpHTTPResult{status: resp.StatusCode, reason: http.StatusText(resp.StatusCode), header: resp.Header}
	if resp.StatusCode >= 400 {
		res.err = fmt.Errorf("%d %s for url: %s", resp.StatusCode, res.reason, c.endpoint)
	}
	return res
}

// One request, retried once at once on a connection error while time remains
func (c *otlpHTTPClient) submit(data []byte, timeout float64) otlpHTTPResult {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	res := c.request(data, timeout)
	if res.err != nil && res.status == 0 && isConnectionError(res.err) {
		if remaining := time.Until(deadline).Seconds(); remaining > 0 {
			res = c.request(data, remaining)
		}
	}
	return res
}

// Seconds a Retry-After header asks to wait, as delay seconds or an HTTP date, ok false when absent or invalid
func extractRetryAfter(header http.Header) (float64, bool) {
	value, present := header["Retry-After"]
	if !present || len(value) == 0 {
		return 0, false
	}
	v := strings.TrimSpace(value[0])
	if seconds, err := strconv.ParseFloat(v, 64); err == nil {
		if math.IsInf(seconds, 0) || math.IsNaN(seconds) {
			return 0, false
		}
		return max(seconds, 0), true
	}
	at, err := mail.ParseDate(v)
	if err != nil {
		return 0, false
	}
	return max(time.Until(at).Seconds(), 0), true
}

// Sends one serialized request, retrying retryable answers with jittered backoff until the deadline
func (c *otlpHTTPClient) export(data []byte) bool {
	data = c.compress(data)
	deadline := time.Now().Add(time.Duration(c.timeout * float64(time.Second)))
	for retry := range otlpMaxRetries {
		backoff := c.computeBackoff(retry)
		res := c.submit(data, max(time.Until(deadline).Seconds(), 0))
		if res.status >= 200 && res.status < 400 {
			return true
		}
		retryable := isConnectionError(res.err)
		if res.status != 0 {
			retryable = otlpRetryableStatus[res.status]
			if after, ok := extractRetryAfter(res.header); retryable && ok {
				backoff = after
			}
		}
		reason := res.reason
		if reason == "" && res.err != nil {
			reason = res.err.Error()
		}
		if reason == "" {
			reason = "unknown"
		}
		if !retryable {
			status := "None"
			if res.status != 0 {
				status = strconv.Itoa(res.status)
			}
			otlpHTTPLog().Error(fmt.Sprintf("Failed to export %s batch code: %s, reason: %s", c.kind, status, reason))
			return false
		}
		if retry+1 == otlpMaxRetries || backoff > time.Until(deadline).Seconds() || c.shutdownSet() {
			otlpHTTPLog().Error(fmt.Sprintf("Failed to export %s batch due to timeout, max retries or shutdown.", c.kind))
			return false
		}
		otlpHTTPLog().Warn(fmt.Sprintf("Transient error %s encountered while exporting %s batch, retrying in %.2fs.", reason, c.kind, backoff))
		select {
		case <-c.shutdownSig:
			otlpHTTPLog().Warn("Shutdown in progress, aborting retry.")
			return false
		case <-time.After(time.Duration(backoff * float64(time.Second))):
		}
	}
	return false
}

func (c *otlpHTTPClient) shutdownSet() bool {
	select {
	case <-c.shutdownSig:
		return true
	default:
		return false
	}
}

// Stops pending retries and closes idle connections, warning when already shut down
func (c *otlpHTTPClient) shutdown() {
	c.shutdownMu.Lock()
	defer c.shutdownMu.Unlock()
	if c.isShutdown {
		otlpHTTPLog().Warn("OTLP client already shutdown, ignoring call")
		return
	}
	c.isShutdown = true
	close(c.shutdownSig)
	c.client.CloseIdleConnections()
}

// otlpLogRecord is one emitted log record with the scope that emitted it
type otlpLogRecord struct {
	scope  string
	record *logspb.LogRecord
}

// otlpLogExporter encodes log batches as ExportLogsServiceRequest protobuf and posts them
type otlpLogExporter struct {
	client   *otlpHTTPClient
	resource *respb.Resource
	mu       sync.Mutex
	shut     bool
}

// Log exporter for the logs endpoint with the explicit headers
func newOTLPLogExporter(endpoint string, headers map[string]string, res *resource.Resource) *otlpLogExporter {
	return &otlpLogExporter{client: newOTLPHTTPClient(endpoint, "logs", "logs", headers), resource: resourceProto(res)}
}

// Proto resource of an SDK resource, its attributes in key order
func resourceProto(res *resource.Resource) *respb.Resource {
	out := &respb.Resource{}
	for _, kv := range res.Attributes() {
		out.Attributes = append(out.Attributes, &commonpb.KeyValue{Key: string(kv.Key), Value: attributeValueProto(kv.Value)})
	}
	return out
}

func attributeValueProto(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.Emit()}}
}

// Proto AnyValue of a Python attribute value, an error for an unsupported type
func anyValueProto(value any) (*commonpb.AnyValue, error) {
	switch v := value.(type) {
	case nil:
		return &commonpb.AnyValue{}, nil
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v}}, nil
	case string:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}, nil
	case int:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(v)}}, nil
	case int64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}, nil
	case float64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}, nil
	}
	return nil, fmt.Errorf("Invalid type %T of value %v", value, value)
}

// Posts one batch grouped by scope in first appearance order
func (e *otlpLogExporter) export(batch []otlpLogRecord) bool {
	e.mu.Lock()
	shut := e.shut
	e.mu.Unlock()
	if shut {
		otlpHTTPLog().Warn("Exporter already shutdown, ignoring batch")
		return false
	}
	var scopes []*logspb.ScopeLogs
	index := map[string]*logspb.ScopeLogs{}
	for _, rec := range batch {
		sl, ok := index[rec.scope]
		if !ok {
			sl = &logspb.ScopeLogs{Scope: &commonpb.InstrumentationScope{Name: rec.scope}}
			index[rec.scope] = sl
			scopes = append(scopes, sl)
		}
		sl.LogRecords = append(sl.LogRecords, rec.record)
	}
	req := &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{Resource: e.resource, ScopeLogs: scopes}}}
	data, err := proto.Marshal(req)
	if err != nil {
		otlpHTTPLog().Error(fmt.Sprintf("Failed to encode logs batch: %s", err))
		return false
	}
	if len(data) > otlpMaxRequestSize {
		otlpHTTPLog().Warn(fmt.Sprintf("Dropping logs batch: serialized size %d bytes exceeds max_request_size %d bytes.", len(data), otlpMaxRequestSize))
		return false
	}
	return e.client.export(data)
}

func (e *otlpLogExporter) shutdown() {
	e.mu.Lock()
	if e.shut {
		e.mu.Unlock()
		otlpHTTPLog().Warn("Exporter already shutdown, ignoring call")
		return
	}
	e.shut = true
	e.mu.Unlock()
	e.client.shutdown()
}

// batchLogProcessor queues records and exports them in batches from a worker every schedule delay
type batchLogProcessor struct {
	exporter  *otlpLogExporter
	mu        sync.Mutex
	exportMu  sync.Mutex
	queue     []otlpLogRecord
	awaken    chan struct{}
	shutdown  bool
	timedOut  bool
	workerEnd chan struct{}
}

// Processor with its worker started
func newBatchLogProcessor(exporter *otlpLogExporter) *batchLogProcessor {
	p := &batchLogProcessor{exporter: exporter, awaken: make(chan struct{}, 1), workerEnd: make(chan struct{})}
	go p.worker()
	return p
}

func (p *batchLogProcessor) worker() {
	defer close(p.workerEnd)
	for {
		interrupted := false
		select {
		case <-p.awaken:
			interrupted = true
		case <-time.After(blrpScheduleDelay):
		}
		p.mu.Lock()
		done := p.shutdown
		p.mu.Unlock()
		if done {
			break
		}
		if interrupted {
			p.exportBatches("while_exceeds")
		} else {
			p.exportBatches("at_least_one")
		}
	}
	p.exportBatches("all")
}

func (p *batchLogProcessor) shouldExport(strategy string, iteration int) bool {
	if len(p.queue) == 0 || p.timedOut {
		return false
	}
	if len(p.queue) >= blrpMaxExportBatchSize || strategy == "all" {
		return true
	}
	return strategy == "at_least_one" && iteration == 0
}

func (p *batchLogProcessor) exportBatches(strategy string) {
	p.exportMu.Lock()
	defer p.exportMu.Unlock()
	for iteration := 0; ; iteration++ {
		p.mu.Lock()
		if !p.shouldExport(strategy, iteration) {
			p.mu.Unlock()
			return
		}
		count := min(blrpMaxExportBatchSize, len(p.queue))
		batch := append([]otlpLogRecord(nil), p.queue[:count]...)
		p.queue = p.queue[count:]
		p.mu.Unlock()
		p.exporter.export(batch)
	}
}

// Queues one record, dropping the oldest when full and waking the worker at a full batch
func (p *batchLogProcessor) emit(rec otlpLogRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdown {
		otlpHTTPLog().Info("Shutdown called, ignoring Log.")
		return
	}
	if len(p.queue) == blrpMaxQueueSize {
		otlpHTTPLog().Warn("Queue full, dropping Log.")
		p.queue = p.queue[1:]
	}
	p.queue = append(p.queue, rec)
	if len(p.queue) >= blrpMaxExportBatchSize {
		select {
		case p.awaken <- struct{}{}:
		default:
		}
	}
}

// Exports everything queued, false once shut down
func (p *batchLogProcessor) forceFlush() bool {
	p.mu.Lock()
	done := p.shutdown
	p.mu.Unlock()
	if done {
		return false
	}
	p.exportBatches("all")
	return true
}

// Stops the worker after its final export, bounded by the shutdown timeout, then shuts the exporter
func (p *batchLogProcessor) close() {
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return
	}
	p.shutdown = true
	p.mu.Unlock()
	select {
	case p.awaken <- struct{}{}:
	default:
	}
	select {
	case <-p.workerEnd:
	case <-time.After(blrpShutdownTimeout):
	}
	p.mu.Lock()
	p.timedOut = true
	p.mu.Unlock()
	p.exporter.shutdown()
}
