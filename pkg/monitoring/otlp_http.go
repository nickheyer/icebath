package monitoring

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Span is one finished monitoring span with its attributes
type Span struct {
	Name       string
	Attributes map[string]any
	TraceID    string
	SpanID     string
	Start, End time.Time
}

// SpanExporter receives finished spans in batches
type SpanExporter interface {
	Export(spans []*Span) error
	Shutdown() error
}

// InMemorySpanExporter keeps every exported span, the test double of the OTLP exporter
type InMemorySpanExporter struct {
	mu    sync.Mutex
	spans []*Span
}

func (m *InMemorySpanExporter) Export(spans []*Span) error {
	m.mu.Lock()
	m.spans = append(m.spans, spans...)
	m.mu.Unlock()
	return nil
}

func (m *InMemorySpanExporter) Shutdown() error { return nil }

// FinishedSpans returns a copy of every span exported so far
func (m *InMemorySpanExporter) FinishedSpans() []*Span {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Span{}, m.spans...)
}

// BatchSpanProcessor queues spans and exports them in bounded batches on a schedule, on flush and on shutdown
type BatchSpanProcessor struct {
	exporter      SpanExporter
	maxQueue      int
	maxBatch      int
	scheduleDelay time.Duration
	mu            sync.Mutex
	queue         []*Span
	stop          chan struct{}
	done          chan struct{}
	once          sync.Once
	dropped       int
}

// NewBatchSpanProcessor wraps an exporter with the OpenTelemetry batch defaults
func NewBatchSpanProcessor(exporter SpanExporter) *BatchSpanProcessor {
	p := &BatchSpanProcessor{exporter: exporter, maxQueue: 2048, maxBatch: 512, scheduleDelay: 5 * time.Second,
		stop: make(chan struct{}), done: make(chan struct{})}
	go p.run()
	return p
}

// Queues a finished span, dropping it when the queue is full
func (p *BatchSpanProcessor) OnEnd(span *Span) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) >= p.maxQueue {
		p.dropped++
		return
	}
	p.queue = append(p.queue, span)
}

func (p *BatchSpanProcessor) run() {
	defer close(p.done)
	ticker := time.NewTicker(p.scheduleDelay)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.exportBatches(false)
		case <-p.stop:
			p.exportBatches(true)
			return
		}
	}
}

// Exports queued spans in maxBatch slices, one slice unless draining everything
func (p *BatchSpanProcessor) exportBatches(drain bool) {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		n := min(p.maxBatch, len(p.queue))
		batch := append([]*Span{}, p.queue[:n]...)
		p.queue = p.queue[n:]
		p.mu.Unlock()
		if err := p.exporter.Export(batch); err != nil {
			otlpLog().Debug("OTLP export failed: " + err.Error())
		}
		if !drain {
			return
		}
	}
}

// ForceFlush exports everything queued before returning
func (p *BatchSpanProcessor) ForceFlush() { p.exportBatches(true) }

// Shutdown stops the schedule, drains the queue and shuts the exporter down
func (p *BatchSpanProcessor) Shutdown() {
	p.once.Do(func() {
		close(p.stop)
		select {
		case <-p.done:
		case <-time.After(otlpFlushTimeout):
		}
		_ = p.exporter.Shutdown()
	})
}

// TracerProvider owns the resource and the processor every tracer records spans into
type TracerProvider struct {
	resource  map[string]string
	processor *BatchSpanProcessor
	exporter  SpanExporter
}

// NewTracerProvider pairs a resource with a batch processor
func NewTracerProvider(resource map[string]string, processor *BatchSpanProcessor) *TracerProvider {
	return &TracerProvider{resource: resource, processor: processor}
}

// NewSimpleTracerProvider exports every span as it ends, the in memory test provider
func NewSimpleTracerProvider(resource map[string]string, exporter SpanExporter) *TracerProvider {
	return &TracerProvider{resource: resource, exporter: exporter}
}

// Resource returns the provider's resource attributes
func (tp *TracerProvider) Resource() map[string]string { return tp.resource }

// Tracer returns a tracer recording spans under the instrumentation scope name
func (tp *TracerProvider) Tracer(scope string) *Tracer { return &Tracer{provider: tp, scope: scope} }

// Shutdown drains the processor or the simple exporter
func (tp *TracerProvider) Shutdown() {
	if tp.processor != nil {
		tp.processor.Shutdown()
	} else if tp.exporter != nil {
		_ = tp.exporter.Shutdown()
	}
}

// Tracer records zero duration spans for monitoring events
type Tracer struct {
	provider *TracerProvider
	scope    string
}

// RecordSpan starts and ends one span with the attributes
func (t *Tracer) RecordSpan(name string, attrs map[string]any) error {
	now := time.Now()
	span := &Span{Name: name, Attributes: attrs, TraceID: randomHex(16), SpanID: randomHex(8), Start: now, End: now}
	if t.provider.processor != nil {
		t.provider.processor.OnEnd(span)
		return nil
	}
	if t.provider.exporter != nil {
		return t.provider.exporter.Export([]*Span{span})
	}
	return errors.New("tracer provider has no exporter")
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString(bytes.Repeat([]byte{1}, n))
	}
	return hex.EncodeToString(buf)
}

// OTLPHTTPSpanExporter posts spans to an OTLP/HTTP traces endpoint as JSON
type OTLPHTTPSpanExporter struct {
	Endpoint string
	Headers  map[string]string
	Client   *http.Client
	Resource map[string]string
}

// NewOTLPHTTPSpanExporter targets the endpoint with the resolved headers and a ten second timeout
func NewOTLPHTTPSpanExporter(endpoint string, headers map[string]string) *OTLPHTTPSpanExporter {
	return &OTLPHTTPSpanExporter{Endpoint: endpoint, Headers: headers, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Export posts one ExportTraceServiceRequest, a non 2xx status an error
func (e *OTLPHTTPSpanExporter) Export(spans []*Span) error {
	body, err := pyjson.Dumps(exportTraceRequest(e.Resource, spans))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.Endpoint, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OTLP endpoint answered HTTP %d", resp.StatusCode)
	}
	return nil
}

func (e *OTLPHTTPSpanExporter) Shutdown() error { return nil }

// The OTLP JSON body of a resource with its spans
func exportTraceRequest(resource map[string]string, spans []*Span) *pyjson.Object {
	resAttrs := []any{}
	for _, key := range sortedKeys(resource) {
		resAttrs = append(resAttrs, pyjson.ObjectOf("key", key, "value", pyjson.ObjectOf("stringValue", resource[key])))
	}
	out := []any{}
	for _, s := range spans {
		attrs := []any{}
		for _, key := range sortedAnyKeys(s.Attributes) {
			attrs = append(attrs, pyjson.ObjectOf("key", key, "value", anyValue(s.Attributes[key])))
		}
		out = append(out, pyjson.ObjectOf("traceId", s.TraceID, "spanId", s.SpanID, "name", s.Name, "kind", 1,
			"startTimeUnixNano", strconv.FormatInt(s.Start.UnixNano(), 10), "endTimeUnixNano", strconv.FormatInt(s.End.UnixNano(), 10),
			"attributes", attrs))
	}
	scope := pyjson.ObjectOf("scope", pyjson.ObjectOf("name", "icebath.monitoring"), "spans", out)
	return pyjson.ObjectOf("resourceSpans", []any{pyjson.ObjectOf("resource", pyjson.ObjectOf("attributes", resAttrs), "scopeSpans", []any{scope})})
}

// OTLP AnyValue of a Go attribute value
func anyValue(v any) *pyjson.Object {
	switch t := v.(type) {
	case bool:
		return pyjson.ObjectOf("boolValue", t)
	case int:
		return pyjson.ObjectOf("intValue", strconv.Itoa(t))
	case int64:
		return pyjson.ObjectOf("intValue", strconv.FormatInt(t, 10))
	case float64:
		if t == float64(int64(t)) {
			return pyjson.ObjectOf("intValue", strconv.FormatInt(int64(t), 10))
		}
		return pyjson.ObjectOf("doubleValue", t)
	case string:
		return pyjson.ObjectOf("stringValue", t)
	}
	return pyjson.ObjectOf("stringValue", fmt.Sprint(v))
}

// Keys of a string map in sorted order
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Keys of an attribute map in sorted order
func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
