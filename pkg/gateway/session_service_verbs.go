package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// ServiceVerb answers one verb of the service table with its whole reply frame, nil when the reply goes to the transport later
type ServiceVerb func(ctx context.Context, actor Principal, transport tuihost.Transport, request map[string]any) *pyjson.Object

// ServiceVerbs finds a verb of the service table by method name, nil for none; the package holding the hubs, panels and setup flows installs it
var ServiceVerbs = func(method string) ServiceVerb { return nil }

// Reply of a service table verb, refused unless the connection holds every interactive capability
func (c *AuthorityConnection) dispatchServiceVerb(ctx context.Context, verb ServiceVerb, method string, rid any, request *pyjson.Object) *pyjson.Object {
	for capability := range purposeCapabilities["interactive"] {
		if !c.actor.Capabilities[capability] {
			err := runtimeRefusal("permission_denied")
			logRefusal(c.actor.ProfileID, method, err)
			return refusalReply(rid, 4001, "permission_denied", refusalData(err))
		}
	}
	plain, _ := pyjson.Plain(request).(map[string]any)
	return verb(ctx, c.actor, c.transport, plain)
}
