package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const pruneRPCTimeout = 600 * time.Second

// Ready gateway endpoint serving the home, nil when none is ready, discovery never starting one
func liveOwner(ctx context.Context, h string) *gateway.GatewayEndpoint {
	resolved, err := pypath.Resolve(h)
	if err != nil {
		resolved = h
	}
	found := gateway.DiscoverGatewayEndpoint(ctx, resolved, 2*time.Second)
	if found.State == "ready" {
		return found.Endpoint
	}
	return nil
}

// Runs one session.prune on the owner endpoint, a client error on refusal or loss
func ownerPrune(ctx context.Context, endpoint *gateway.GatewayEndpoint, params map[string]any) (map[string]any, error) {
	client, err := gateway.ConnectGatewayWith(ctx, gateway.ConnectGatewayOptions{Endpoint: endpoint})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	result, err := client.RPCTimeout(ctx, "session.prune", params, pruneRPCTimeout)
	if err != nil {
		return nil, err
	}
	plain, ok := pyjson.Plain(result).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gateway session.prune answered %T, not an object", result)
	}
	return plain, nil
}

// Says the gateway did not complete the prune and returns the failing exit code
func reportOwnerFailure(w io.Writer, err error) int {
	fmt.Fprintf(w, "Error: the gateway did not complete the prune (%s). Run `icebath sessions prune --dry-run` "+
		"with the same filters to see what is left.\n", sessionsExcText(err))
	return 1
}
