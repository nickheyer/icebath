package monitoring

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/settings"
)

// OTelSDKModule is the Go OpenTelemetry SDK module the exporter is linked against
const OTelSDKModule = "go.opentelemetry.io/otel/sdk"

// ResolveHeaders maps header names to the values of the env vars named for them, unset vars skipped
func ResolveHeaders(headersEnv any) map[string]string {
	resolved := map[string]string{}
	entries, ok := asConfigMap(headersEnv)
	if !ok {
		return resolved
	}
	for headerName, envName := range entries {
		name := pyrepr.Str(envName)
		if val := os.Getenv(name); val != "" {
			resolved[pyrepr.Str(headerName)] = val
		} else {
			pkgLog().Debug(fmt.Sprintf("OTLP header %s: env var %s not set; skipping", headerName, name))
		}
	}
	return resolved
}

var signalSuffixes = []string{"/v1/traces", "/v1/metrics"}

// SignalEndpoint rewrites a traces or metrics OTLP path to the named signal, other paths passing through
func SignalEndpoint(endpoint, signal string) string {
	target := "/v1/" + signal
	for _, suffix := range signalSuffixes {
		if suffix != target && strings.HasSuffix(endpoint, suffix) {
			return endpoint[:len(endpoint)-len(suffix)] + target
		}
	}
	return endpoint
}

var resourceAttributeKeys = map[string]bool{
	"service.name": true, "service.namespace": true, "service.version": true, "service.instance.id": true,
	"deployment.environment.name": true, "cloud.provider": true, "cloud.platform": true, "cloud.region": true, "telemetry.scope": true,
}

var safeResourceValue = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// The install id of the profile, unknown when the policy cannot mint or read one
func otlpInstallID(ctx context.Context) (id string) {
	defer func() {
		if r := recover(); r != nil {
			id = "unknown"
		}
	}()
	return EnsureInstallID(ctx)
}

// SafeResourceAttributes keeps allowlisted bounded resource labels, rejecting values redaction would change
func SafeResourceAttributes(raw any) map[string]string {
	attrs := map[string]string{}
	entries, ok := asConfigMap(raw)
	if !ok {
		return attrs
	}
	for key, value := range entries {
		if !resourceAttributeKeys[key] || value == nil {
			continue
		}
		text := pyrepr.Str(value)
		if key == "service.instance.id" {
			attrs[key] = safeInstanceID(value)
		} else if safeResourceValue.MatchString(text) && RedactBoundedWith(text, 128, "[redacted]", RedactionUnavailable) == text {
			attrs[key] = text
		}
	}
	return attrs
}

// RuntimeResourceAttributes builds the safe OTLP resource shared by spans, metrics and diagnostic logs
func RuntimeResourceAttributes(ctx context.Context, telemetryScope string) map[string]string {
	attrs := SafeResourceAttributes(settings.MonitoringGatewayHealthExportResourceAttributes.Get(ctx))
	attrs["service.name"] = "icebath-gateway"
	attrs["service.instance.id"] = safeInstanceID(otlpInstallID(ctx))
	attrs["telemetry.scope"] = telemetryScope
	return attrs
}

func resourceAttributes(ctx context.Context) map[string]string {
	return RuntimeResourceAttributes(ctx, "gateway_monitoring")
}

// BuildExporter constructs the OTLP span exporter the profile names, an error without a collector address
func BuildExporter(ctx context.Context) (*OTLPHTTPSpanExporter, error) {
	endpoint := settings.MonitoringExportOtlpEndpoint.Get(ctx)
	if endpoint == "" {
		return nil, errors.New("monitoring.export.otlp.endpoint is not set")
	}
	return NewOTLPHTTPSpanExporter(endpoint, ResolveHeaders(settings.MonitoringExportOtlpHeadersEnv.Get(ctx))), nil
}

// Tracer provider and batch processor seam the streamer tests replace
var makeProvider = func(ctx context.Context) (*TracerProvider, *BatchSpanProcessor, error) {
	exporter, err := BuildExporter(ctx)
	if err != nil {
		return nil, nil, err
	}
	processor := NewBatchSpanProcessor(exporter)
	provider := NewTracerProvider(resourceAttributes(ctx), processor)
	return provider, processor, nil
}

// Per kind attribute allowlists, everything else never egresses
var keepByKind = map[string][]string{
	"gateway_health": {"name", "gateway_state", "old_state", "new_state", "exit_reason", "restart_requested", "active_agents",
		"gateway_busy", "gateway_drainable", "platform_count", "fatal_platform_count", "version", "supervision_mode", "pid"},
	"gateway_diagnostic": {"name", "subsystem", "error_class", "error_code", "platform", "old_state", "new_state", "version", "severity"},
	"cron_execution":     {"status", "job_key", "source", "duration_ms", "delivery_outcome", "error_class"},
}

// icebath.<key> attributes for the present keys, string values redacted and bounded
func allowlistedAttrs(ev *pyjson.Object, keys []string) map[string]any {
	attrs := map[string]any{}
	for _, col := range keys {
		v, has := ev.Get(col)
		if !has || v == nil {
			continue
		}
		if s, ok := v.(string); ok {
			attrs["icebath."+col] = RedactBounded(s)
		} else {
			attrs["icebath."+col] = v
		}
	}
	return attrs
}

// SpanAttrs maps a monitoring event to span attributes, content free by construction
func SpanAttrs(ev *pyjson.Object) map[string]any {
	kind, _ := ev.Get("event")
	attrs := map[string]any{"icebath.event": "unknown"}
	if pyrepr.Truthy(kind) {
		attrs["icebath.event"] = kind
	}
	for k, v := range allowlistedAttrs(ev, keepByKind[pyrepr.Str(kind)]) {
		attrs[k] = v
	}
	return attrs
}

// ExportBatch maps a batch of events to spans on the provider and returns how many it created
func ExportBatch(provider *TracerProvider, batch []*pyjson.Object) int {
	tracer := provider.Tracer("icebath.monitoring")
	n := 0
	for _, ev := range batch {
		name := "event"
		if kind, has := ev.Get("event"); has && kind != nil {
			name = pyrepr.Str(kind)
		}
		if err := tracer.RecordSpan("icebath."+name, SpanAttrs(ev)); err != nil {
			pkgLog().Debug("OTLP span map failed: " + err.Error())
			continue
		}
		n++
	}
	return n
}

// OTLPStreamer pushes each emitter batch to OTLP as spans, scoped to its plane by the event filter
type OTLPStreamer struct {
	provider  *TracerProvider
	processor *BatchSpanProcessor
	filter    func(*pyjson.Object) bool
	sub       *Subscriber
	mu        sync.Mutex
	exported  int
}

// NewOTLPStreamer builds the provider the profile names and the subscriber that exports every batch
func NewOTLPStreamer(ctx context.Context, eventFilter func(*pyjson.Object) bool) (*OTLPStreamer, error) {
	provider, processor, err := makeProvider(ctx)
	if err != nil {
		return nil, err
	}
	s := &OTLPStreamer{provider: provider, processor: processor, filter: eventFilter}
	s.sub = NewSubscriber(s.Call)
	return s, nil
}

// Call exports the batch after the filter, the emitter callable
func (s *OTLPStreamer) Call(batch []*pyjson.Object) {
	if s.filter != nil {
		kept := batch[:0:0]
		for _, ev := range batch {
			if s.filter(ev) {
				kept = append(kept, ev)
			}
		}
		batch = kept
	}
	if len(batch) == 0 {
		return
	}
	n := ExportBatch(s.provider, batch)
	s.mu.Lock()
	s.exported += n
	s.mu.Unlock()
}

// Subscriber is the emitter registration of this streamer
func (s *OTLPStreamer) Subscriber() *Subscriber { return s.sub }

// Exported counts the spans the streamer created
func (s *OTLPStreamer) Exported() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exported
}

// Shutdown unsubscribes from the singleton emitter, flushes the processor and shuts the provider down
func (s *OTLPStreamer) Shutdown() {
	GetEmitter().Unsubscribe(s.sub)
	if s.processor != nil {
		s.processor.ForceFlush()
	}
	if s.provider != nil {
		s.provider.Shutdown()
	}
}

// IsOTLPEnabled reports an enabled export with a collector address
func IsOTLPEnabled(ctx context.Context) bool {
	return settings.MonitoringExportOtlpEnabled.Get(ctx) && settings.MonitoringExportOtlpEndpoint.Get(ctx) != ""
}

// StartStreaming attaches a streamer to the singleton emitter when OTLP is enabled, nil otherwise or when the provider cannot build
func StartStreaming(ctx context.Context, eventFilter func(*pyjson.Object) bool) *OTLPStreamer {
	if !IsOTLPEnabled(ctx) {
		return nil
	}
	streamer, err := NewOTLPStreamer(ctx, eventFilter)
	if err != nil {
		pkgLog().Warn("monitoring.export.otlp.enabled but the OTLP exporter could not start: " + err.Error())
		return nil
	}
	GetEmitter().Subscribe(streamer.sub)
	return streamer
}

// Flush deadline the shutdown path gives the processor
const otlpFlushTimeout = 30 * time.Second
