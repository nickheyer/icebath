package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func memProvider() (*TracerProvider, *InMemorySpanExporter) {
	exporter := &InMemorySpanExporter{}
	return NewSimpleTracerProvider(map[string]string{}, exporter), exporter
}

func TestGatewayHealthEventMapsToSpanWithAttrs(t *testing.T) {
	provider, mem := memProvider()
	n := ExportBatch(provider, []*pyjson.Object{pyjson.ObjectOf("event", "gateway_health", "name", "gateway.lifecycle",
		"old_state", "starting", "new_state", "running", "active_agents", int64(2), "pid", int64(4242), "profile", "secret-profile")})
	if n != 1 {
		t.Fatalf("exported %d", n)
	}
	spans := mem.FinishedSpans()
	if len(spans) != 1 || spans[0].Name != "icebath.gateway_health" {
		t.Fatalf("spans %+v", spans)
	}
	attrs := spans[0].Attributes
	if attrs["icebath.old_state"] != "starting" || attrs["icebath.new_state"] != "running" || attrs["icebath.active_agents"] != int64(2) {
		t.Fatalf("attrs %v", attrs)
	}
	if _, leaked := attrs["icebath.profile"]; leaked {
		t.Fatal("profile egressed")
	}
}

func TestHeadersResolveFromEnvNotValue(t *testing.T) {
	t.Setenv("DD_KEY_ENV", "secret-value")
	os.Unsetenv("NOPE_ENV")
	resolved := ResolveHeaders(map[string]any{"DD-API-KEY": "DD_KEY_ENV", "X-Missing": "NOPE_ENV"})
	if len(resolved) != 1 || resolved["DD-API-KEY"] != "secret-value" {
		t.Fatalf("resolved %v", resolved)
	}
}

func TestTraceResourceIncludesStableHashedInstance(t *testing.T) {
	attrs := resourceAttributes(map[string]any{"monitoring": map[string]any{"install_id": "private-install-id"}})
	if attrs["service.name"] != "icebath-gateway" || !strings.HasPrefix(attrs["service.instance.id"], "sha256:") || len(attrs["service.instance.id"]) != len("sha256:")+24 {
		t.Fatalf("attrs %v", attrs)
	}
	for _, v := range attrs {
		if strings.Contains(v, "private-install-id") {
			t.Fatalf("install id leaked in %v", attrs)
		}
	}
	if attrs["telemetry.scope"] != "gateway_monitoring" {
		t.Fatalf("scope %q", attrs["telemetry.scope"])
	}
}

func TestTraceResourceIncludesConfiguredDeploymentEnvironment(t *testing.T) {
	attrs := resourceAttributes(map[string]any{"monitoring": map[string]any{"install_id": "private-install-id",
		"gateway_health_export": map[string]any{"resource_attributes": map[string]any{"deployment.environment.name": "production", "host.name": "leak", "cloud.region": "bad value!"}}}})
	if attrs["deployment.environment.name"] != "production" || attrs["service.name"] != "icebath-gateway" {
		t.Fatalf("attrs %v", attrs)
	}
	if _, has := attrs["host.name"]; has {
		t.Fatal("unlisted resource key kept")
	}
	if _, has := attrs["cloud.region"]; has {
		t.Fatal("unsafe resource value kept")
	}
}

func TestStreamerReceivesEventsAndRespectsFilter(t *testing.T) {
	provider, mem := memProvider()
	prev := makeProvider
	makeProvider = func(map[string]any) (*TracerProvider, *BatchSpanProcessor, error) { return provider, nil, nil }
	t.Cleanup(func() { makeProvider = prev })
	streamer, err := NewOTLPStreamer(map[string]any{}, func(ev *pyjson.Object) bool {
		kind, _ := ev.Get("event")
		return kind == "gateway_health"
	})
	if err != nil {
		t.Fatal(err)
	}
	em := NewMonitoringEmitter(true)
	em.Subscribe(streamer.Subscriber())
	em.Emit(pyjson.ObjectOf("event", "gateway_health", "name", "gateway.health_snapshot"))
	em.Emit(pyjson.ObjectOf("event", "model_call", "provider", "anthropic"))
	em.Flush(5)
	em.Close()
	spans := mem.FinishedSpans()
	if len(spans) != 1 || spans[0].Name != "icebath.gateway_health" || streamer.Exported() != 1 {
		t.Fatalf("spans %+v exported %d", spans, streamer.Exported())
	}
}

func TestFailingStreamerNeverBreaksEmitter(t *testing.T) {
	em := NewMonitoringEmitter(true)
	var seen []*pyjson.Object
	em.Subscribe(NewSubscriber(func([]*pyjson.Object) { panic("export down") }))
	em.Subscribe(NewSubscriber(func(batch []*pyjson.Object) { seen = append(seen, batch...) }))
	em.Emit(pyjson.ObjectOf("event", "gateway_health", "name", "gateway.lifecycle"))
	em.Flush(5)
	em.Close()
	if len(seen) != 1 {
		t.Fatalf("seen %d", len(seen))
	}
}

func TestSignalEndpointRewritesOnlyKnownSignalPaths(t *testing.T) {
	if got := SignalEndpoint("https://c.example/v1/traces", "metrics"); got != "https://c.example/v1/metrics" {
		t.Fatalf("got %q", got)
	}
	if got := SignalEndpoint("https://c.example/v1/traces", "traces"); got != "https://c.example/v1/traces" {
		t.Fatalf("got %q", got)
	}
	if got := SignalEndpoint("https://c.example/custom", "metrics"); got != "https://c.example/custom" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildExporterRequiresAnEndpoint(t *testing.T) {
	if _, err := BuildExporter(map[string]any{"monitoring": map[string]any{"export": map[string]any{"otlp": map[string]any{"enabled": true}}}}); err == nil || err.Error() != "monitoring.export.otlp.endpoint is not set" {
		t.Fatalf("err %v", err)
	}
	t.Setenv("OTLP_TOKEN", "tok")
	exporter, err := BuildExporter(map[string]any{"monitoring": map[string]any{"export": map[string]any{"otlp": map[string]any{"endpoint": "https://c.example/v1/traces", "headers_env": map[string]any{"Authorization": "OTLP_TOKEN"}}}}})
	if err != nil || exporter.Endpoint != "https://c.example/v1/traces" || exporter.Headers["Authorization"] != "tok" {
		t.Fatalf("exporter %+v err %v", exporter, err)
	}
}

func TestHTTPExporterPostsOTLPJSONWithHeaders(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	exporter := NewOTLPHTTPSpanExporter(srv.URL+"/v1/traces", map[string]string{"Authorization": "Bearer x"})
	exporter.Resource = map[string]string{"service.name": "icebath-gateway"}
	processor := NewBatchSpanProcessor(exporter)
	provider := NewTracerProvider(exporter.Resource, processor)
	if n := ExportBatch(provider, []*pyjson.Object{pyjson.ObjectOf("event", "cron_execution", "status", "ok", "duration_ms", int64(12))}); n != 1 {
		t.Fatalf("exported %d", n)
	}
	processor.ForceFlush()
	provider.Shutdown()
	if auth != "Bearer x" {
		t.Fatalf("auth header %q", auth)
	}
	resourceSpans := got["resourceSpans"].([]any)[0].(map[string]any)
	span := resourceSpans["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)
	if span["name"] != "icebath.cron_execution" || len(span["traceId"].(string)) != 32 || len(span["spanId"].(string)) != 16 {
		t.Fatalf("span %v", span)
	}
	attrs := span["attributes"].([]any)
	found := false
	for _, a := range attrs {
		m := a.(map[string]any)
		if m["key"] == "icebath.duration_ms" && m["value"].(map[string]any)["intValue"] == "12" {
			found = true
		}
	}
	if !found {
		t.Fatalf("attributes %v", attrs)
	}
}

func TestStartStreamingIsOffWithoutAnEnabledEndpoint(t *testing.T) {
	if s := StartStreaming(map[string]any{"monitoring": map[string]any{"export": map[string]any{"otlp": map[string]any{"enabled": true}}}}, nil); s != nil {
		t.Fatal("streamer without endpoint")
	}
	if s := StartStreaming(map[string]any{"monitoring": map[string]any{"export": map[string]any{"otlp": map[string]any{"endpoint": "https://c.example"}}}}, nil); s != nil {
		t.Fatal("streamer while disabled")
	}
}

func writeMonitoringConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCmdMonitoringStatusReportsDisabledExport(t *testing.T) {
	dir := writeMonitoringConfig(t, "monitoring:\n  gateway_health_export:\n    enabled: false\n")
	var out bytes.Buffer
	code, err := CmdMonitoring(context.Background(), dir, map[string]any{"monitoring_action": nil}, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	want := "Gateway monitoring\n  Health export:  disabled (monitoring.gateway_health_export.enabled)\n  OTLP endpoint:  not configured (monitoring.export.otlp)\n  OTel SDK:       " + otelSDKLine(t) + "\n\n  Scope: gateway service health + redacted diagnostics only.\n  No prompts, messages, tool args/results, usage analytics, or traces.\n"
	if out.String() != want {
		t.Fatalf("output %q", out.String())
	}
}

func TestCmdMonitoringStatusReportsEnabledExportDetails(t *testing.T) {
	dir := writeMonitoringConfig(t, "monitoring:\n  gateway_health_export:\n    enabled: true\n    metrics_enabled: false\n    export_interval_seconds: 30\n  export:\n    otlp:\n      enabled: true\n      endpoint: https://collector.example/v1/traces\n")
	var out bytes.Buffer
	code, err := CmdMonitoring(context.Background(), dir, map[string]any{"monitoring_action": "status"}, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	for _, want := range []string{"  Health export:  enabled (monitoring.gateway_health_export.enabled)\n", "    Metrics:            off (interval 30s)\n",
		"    Diagnostic events:  on\n", "    Warning/error logs: on (interval 5s)\n", "    Content safety:     always on (rendered messages are never exported; not configurable)\n",
		"  OTLP endpoint:  https://collector.example/v1/traces\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q missing from %q", want, out.String())
		}
	}
}

func TestCmdMonitoringUnknownActionExitsTwo(t *testing.T) {
	dir := writeMonitoringConfig(t, "")
	var stderr bytes.Buffer
	code, err := CmdMonitoring(context.Background(), dir, map[string]any{"monitoring_action": "bogus"}, io.Discard, &stderr)
	if code != 2 || err != nil || stderr.String() != "Unknown monitoring action: bogus\n" {
		t.Fatalf("code %d err %v stderr %q", code, err, stderr.String())
	}
}

// Module path and build info version of the linked Go OpenTelemetry SDK, read independently of the command
func otelSDKLine(t *testing.T) string {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("test binary carries no build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "go.opentelemetry.io/otel/sdk" {
			if dep.Version == "" || strings.Contains(dep.Version, "Python") {
				t.Fatalf("dep %+v", dep)
			}
			return dep.Path + " " + dep.Version
		}
	}
	t.Fatal("build info lists no go.opentelemetry.io/otel/sdk")
	return ""
}
