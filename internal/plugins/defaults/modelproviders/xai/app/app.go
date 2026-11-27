package app

import (
	"connectrpc.com/connect/v2"

	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
	xaicredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// The Grok sign-in of an xAI OAuth account
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, xaiconfig.OAuthName, xaicredentials.SignIn)}
}
