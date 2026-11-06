package observability

import (
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// DefaultEndpoint is the production ingest endpoint, overridable through config only
const DefaultEndpoint = "https://telemetry.nousresearch.com/v1/telemetry"

var localHosts = Set("localhost", "127.0.0.1", "::1", "[::1]")

// Reports the send without collection misconfiguration once per process
var warnedSendWithoutCollection sync.Once

// SendConfig is the resolved transmission settings, send true only when collection is on too
type SendConfig struct {
	Enabled  bool
	Send     bool
	Endpoint string
}

// Accepts https, or plain http only for a loopback host
func endpointIsSafe(endpoint string) bool {
	parsed, err := pyurl.Parse(endpoint)
	if err != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return parsed.Scheme == "http" && localHosts.Has(parsed.Hostname())
}

// Resolves collection, transmission and endpoint from the telemetry.shared_metrics config section
func ResolveSendConfig(cfg any) SendConfig {
	if tree, ok := cfg.(config.Tree); ok {
		cfg = map[string]any(tree)
	}
	raw, _ := asDict(cfg)
	telemetry, _ := asDict(raw["telemetry"])
	shared, _ := asDict(telemetry["shared_metrics"])
	enabled := shared["enabled"] == true
	sendRequested := shared["send"] == true
	if sendRequested && !enabled {
		warnedSendWithoutCollection.Do(func() {
			slog.Error("telemetry.shared_metrics.send is true but " +
				"telemetry.shared_metrics.enabled is false — nothing is " +
				"collected, so nothing can be sent. Enable collection or " +
				"turn sending off.")
		})
		return SendConfig{Enabled: false, Send: false, Endpoint: DefaultEndpoint}
	}
	endpoint, ok := shared["endpoint"].(string)
	if !ok || pystr.Strip(endpoint) == "" {
		endpoint = DefaultEndpoint
	}
	endpoint = pystr.Strip(endpoint)
	if sendRequested && !endpointIsSafe(endpoint) {
		slog.Error("Refusing to send shared metrics to " + pyrepr.Repr(endpoint) + ": telemetry must use https " +
			"(or a localhost http endpoint for testing).")
		return SendConfig{Enabled: enabled, Send: false, Endpoint: endpoint}
	}
	return SendConfig{Enabled: enabled, Send: sendRequested, Endpoint: endpoint}
}
