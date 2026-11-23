package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Connector methods whose owner session is captured before they leave the reader for the pool
var connectorRPCMethods = map[string]bool{"connectors.connect": true, "connection.respond": true}

// Session a connector RPC was issued for and that session's profile home, captured on the reader
type connectorRPCOrigin struct {
	Session     *tuihost.Session
	ProfileHome any
}

type connectorRPCOriginKey struct{}

// Context carrying the owner session of a connector RPC when the bound transport steers it
func captureConnectorRPCOwner(ctx context.Context, params map[string]any) context.Context {
	sid := ""
	if owner, isDict := params["owner"].(map[string]any); isDict && owner["type"] == "session" {
		sid, _ = owner["session_id"].(string)
	}
	_, session := currentSessionSteerAuthority(ctx, sid)
	origin := &connectorRPCOrigin{Session: session}
	if session != nil {
		origin.ProfileHome = session.Get("profile_home")
	}
	return context.WithValue(ctx, connectorRPCOriginKey{}, origin)
}
