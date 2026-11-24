package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuihost"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// ServiceVerb answers one verb of the service table with its whole reply frame, nil when the reply goes to the transport later
type ServiceVerb func(ctx context.Context, actor Principal, transport tuihost.Transport, request map[string]any) *jsonutil.Object

// ServiceVerbs finds a verb of the service table by method name, nil for none; the package holding the hubs, panels and setup flows installs it
var ServiceVerbs = func(method string) ServiceVerb { return nil }

// Reply of a service table verb, refused unless the connection holds every interactive capability
func (c *AuthorityConnection) dispatchServiceVerb(ctx context.Context, verb ServiceVerb, method string, rid any, request *jsonutil.Object) *jsonutil.Object {
	for capability := range purposeCapabilities["interactive"] {
		if !c.actor.Capabilities[capability] {
			err := runtimeRefusal("permission_denied")
			logRefusal(c.actor.ProfileID, method, err)
			return refusalReply(rid, 4001, "permission_denied", refusalData(err))
		}
	}
	plain, _ := jsonutil.Plain(request).(map[string]any)
	return verb(ctx, c.actor, c.transport, plain)
}
