package app

import (
	"connectrpc.com/connect/v2"

	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	openroutercredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// The OpenRouter sign-in that gives an account its API key
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, openrouterconfig.Name, openroutercredentials.SignIn)}
}
