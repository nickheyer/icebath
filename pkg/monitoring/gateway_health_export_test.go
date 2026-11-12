package monitoring

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestOTLPAttrsRedactStringsAndNeverExportProfile(t *testing.T) {
	attrs := SpanAttrs(pyjson.ObjectOf(
		"event", "gateway_health",
		"name", "gateway.lifecycle",
		"profile", "user@example.com",
		"exit_reason", "Bearer top-secret-token-0123456789 for user@example.com",
	))
	if _, ok := attrs["icebath.profile"]; ok {
		t.Fatalf("profile exported %v", attrs)
	}
	text := fmt.Sprint(attrs)
	if strings.Contains(text, "top-secret-token-0123456789") || strings.Contains(text, "user@example.com") {
		t.Fatalf("attrs leak %s", text)
	}
}

func TestResourceAttributesAreAllowlistedAndSanitized(t *testing.T) {
	attrs := SafeResourceAttributes(map[string]any{
		"service.name":                "hermes-gateway",
		"service.instance.id":         "install-1",
		"deployment.environment.name": "staging",
		"user.email":                  "user@example.com",
		"authorization":               "Bearer top-secret-token-0123456789",
		"custom.request.id":           "unbounded",
	})
	if len(attrs) != 3 || attrs["service.name"] != "hermes-gateway" || attrs["deployment.environment.name"] != "staging" {
		t.Fatalf("attrs %v", attrs)
	}
	id := attrs["service.instance.id"]
	if !strings.HasPrefix(id, "sha256:") || strings.Contains(id, "install-1") {
		t.Fatalf("instance id %q", id)
	}
}

func TestDiagnosticLogAttributesAreAllowlistedRedactedAndProfileFree(t *testing.T) {
	attrs := diagnosticLogAttributes(pyjson.ObjectOf(
		"event", "gateway_diagnostic",
		"name", "platform.fatal",
		"subsystem", "platform.slack",
		"profile", "user@example.com",
		"error_code", "Bearer top-secret-token-0123456789",
		"custom", "must-not-egress",
	))
	for _, key := range []string{"icebath.profile", "icebath.custom"} {
		if _, ok := attrs[key]; ok {
			t.Fatalf("%s exported %v", key, attrs)
		}
	}
	if strings.Contains(fmt.Sprint(attrs), "top-secret-token-0123456789") {
		t.Fatalf("attrs leak %v", attrs)
	}
}

func TestGatewayHealthMetricsReachLoopbackCollector(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	type delivery struct {
		path  string
		names []string
	}
	var mu sync.Mutex
	var received []delivery
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Errorf("decode: %v", err)
		}
		d := delivery{path: r.URL.Path}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					d.names = append(d.names, m.Name)
				}
			}
		}
		mu.Lock()
		received = append(received, d)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	runtime := StartGatewayHealthExport(map[string]any{"monitoring": map[string]any{
		"install_id": "test-install",
		"export":     map[string]any{"otlp": map[string]any{"enabled": true, "endpoint": server.URL + "/v1/traces"}},
		"gateway_health_export": map[string]any{
			"enabled": true, "metrics_enabled": true, "diagnostic_events_enabled": false,
		},
	}})
	defer runtime.Shutdown()
	if !runtime.Enabled {
		t.Fatalf("runtime disabled: %s", runtime.Reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.MetricProvider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("collector never received metrics")
	}
	mu.Lock()
	defer mu.Unlock()
	names := map[string]bool{}
	for _, d := range received {
		if d.path != "/v1/metrics" {
			t.Fatalf("posted to %s", d.path)
		}
		for _, n := range d.names {
			names[n] = true
		}
	}
	if !names["hermes.gateway.up"] {
		t.Fatalf("names %v", names)
	}
}

func TestRegisteredObservableMetricNamesCoverSnapshotMetrics(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := GatewayReadRuntimeStatus
	t.Cleanup(func() { GatewayReadRuntimeStatus = saved })
	GatewayReadRuntimeStatus = func() *pyjson.Object {
		return pyjson.ObjectOf("gateway_state", "running", "active_agents", 1,
			"platforms", pyjson.ObjectOf("slack", pyjson.ObjectOf("state", "fatal")))
	}
	snap, err := readRuntimeSnapshot(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, n := range observableMetricNames {
		registered[n] = true
	}
	var missing []string
	for _, m := range snap.Metrics {
		if !registered[m.Name] {
			missing = append(missing, m.Name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("gauges emitted but NOT registered in metric_names (will be silently dropped): %v", missing)
	}
}
