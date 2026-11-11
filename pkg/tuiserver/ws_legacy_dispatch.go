package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Runs the legacy dispatcher with the ticket profile home bound, refusing a ticket whose home stopped resolving
func dispatchLegacy(ctx context.Context, req map[string]any, transport tuigateway.Transport, actor gateway.Principal) map[string]any {
	ticketHome, err := foreignProfileHome(actor.ProfileID)
	if err != nil {
		return profileUnavailable(req)
	}
	scope := ctx
	method, _ := req["method"].(string)
	switch {
	case ticketHome != "" && sessionFallbackAllowed[method]:
		scoped := ownProfileRead(ctx, req, ticketHome)
		_, answered := scoped["result"]
		_, refused := scoped["error"]
		if answered || refused {
			return scoped
		}
		req = scoped
	case ticketHome != "":
		bound, target, ok := ticketProfileRequest(req, ticketHome)
		if !ok {
			return bound
		}
		req = bound
		scope = profileRuntimeScope(ctx, target, true)
	case !sessionFallbackAllowed[method]:
		if resp, target, ok, named := explicitProfileRequest(req); named {
			if !ok {
				return resp
			}
			req = resp
			if target != "" {
				scope = profileRuntimeScope(ctx, target, true)
			}
		}
	}
	return wsServerDispatch(withConnectionProfileHome(scope, ticketHome), req, transport)
}
