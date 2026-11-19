package app

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

const scriptSID = "local-abc"

type scriptedCall struct {
	method string
	params map[string]any
}

// Gateway agent of the scripted session with the defaults the Python constructor has
func scriptedAgentOn(t *testing.T, conn *gatewaycli.GatewayConnection, o gatewayAgentOptions) *GatewayAgent {
	t.Helper()
	o.Connection, o.SessionID = conn, scriptSID
	if o.MaxIterations == 0 {
		o.MaxIterations = 90
	}
	if o.ToolProgressMode == "" {
		o.ToolProgressMode = "all"
	}
	a, err := newGatewayAgent(context.Background(), o)
	if err != nil {
		t.Fatalf("newGatewayAgent: %v", err)
	}
	return a
}
