package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var capabilitiesRegistry = &handlerRegistry{}

func init() {
	capabilitiesRegistry.method("client.capabilities", clientCapabilities)
	capabilitiesRegistry.install()
}

// Records what the calling client handles
func clientCapabilities(ctx context.Context, rid any, params map[string]any) map[string]any {
	p, err := tuigateway.ClientCapabilities.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	if _, present := params["server_requests"]; present {
		tuihost.Advertise(callerTransport(ctx), p.ServerRequests)
	}
	result := tuigateway.ClientCapabilitiesResult{ServerRequests: []string{}, DeclinesNotShown: true, CanonicalClarifyBatch: true}
	for _, entry := range tuigateway.Entries(tuigateway.KindRequest) {
		result.ServerRequests = append(result.ServerRequests, entry.Name)
	}
	return ok(rid, tuigateway.Plain(result))
}
