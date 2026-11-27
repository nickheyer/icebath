package app

import (
	"connectrpc.com/connect/v2"

	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
	minimaxcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// The sign-ins of a MiniMax OAuth account at the global and the China portal
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, minimaxconfig.OAuthName, minimaxcredentials.SignIn, minimaxcredentials.SignInCN)}
}
