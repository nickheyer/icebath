package gatewayrun

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Mark streamed delivery over a consumer that never confirmed the final, the harness of test_interim_only_consumer_warning.py
func runMarkStreamedDeliveryWarnings(t *testing.T, streamDeltasEnabled bool) string {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	logs := captureWarnings(t)
	consumer := gateway.NewGatewayStreamConsumer(newCleanupCaptureAdapter(), "-1001", gateway.StreamConsumerOptions{})
	consumer.StreamDeltasEnabled = streamDeltasEnabled
	tc := newTurnContext()
	tc.source = &gateway.SessionSource{Platform: gateway.PlatformTelegram}
	tc.sessionKey = "sess-105341"
	tc.setStreamConsumer(consumer)
	allocGatewayRunner().runAgentMarkStreamedDelivery(context.Background(), map[string]any{"final_response": "olá"}, tc)
	return logs.String()
}

// test_interim_only_consumer_warning.py test_stream_capable_consumer_still_warns
func TestStreamCapableConsumerStillWarns(t *testing.T) {
	if logs := runMarkStreamedDeliveryWarnings(t, true); !strings.Contains(logs, "possible duplicate send") {
		t.Fatalf("logs = %q", logs)
	}
}

// test_interim_only_consumer_warning.py test_interim_only_consumer_skips_duplicate_warning
func TestInterimOnlyConsumerSkipsDuplicateWarning(t *testing.T) {
	if logs := runMarkStreamedDeliveryWarnings(t, false); strings.Contains(logs, "possible duplicate send") {
		t.Fatalf("logs = %q", logs)
	}
}
