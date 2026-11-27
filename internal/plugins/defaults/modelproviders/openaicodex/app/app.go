package app

import (
	"connectrpc.com/connect/v2"

	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	codexcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// The ChatGPT sign-in of a Codex account
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, openaicodexconfig.Name, codexcredentials.SignIn)}
}
