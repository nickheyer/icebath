package app

import (
	"connectrpc.com/connect/v2"

	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/config"
	copilotcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// The GitHub device code sign-in of a Copilot account
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, githubcopilotconfig.CopilotName, copilotcredentials.SignIn)}
}
