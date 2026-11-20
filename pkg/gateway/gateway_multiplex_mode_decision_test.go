package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardRefusalIsRecordedInRuntimeStatusAndClearedOnDefault(t *testing.T) {
	dir := statusHome(t)
	ctx := context.Background()
	if !AcquireGatewayRuntimeLock(ctx) {
		t.Fatal("could not take the runtime lock")
	}
	t.Cleanup(ReleaseGatewayRuntimeLock)
	statePath := filepath.Join(dir, "gateway_state.json")
	RecordMultiplexDecision(ctx, MultiplexDecision{Enabled: false, Source: "guard", Reason: "profile(s) 'coder' still run their own gateway"})
	reason, _ := ReadRuntimeStatus(statePath).Get("multiplex_standalone_reason")
	if s, _ := reason.(string); !strings.Contains(s, "coder") {
		t.Fatalf("reason %v", reason)
	}
	RecordMultiplexDecision(ctx, MultiplexDecision{Enabled: true, Source: "default", Reason: "unset; default applies"})
	if reason, _ := ReadRuntimeStatus(statePath).Get("multiplex_standalone_reason"); reason != nil {
		t.Fatalf("reason %v", reason)
	}
}
