package app

import (
	"connectrpc.com/connect/v2"

	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
	azurefoundrycredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// An Azure Foundry account signed in with Microsoft Entra ID
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, azurefoundryconfig.Name, azurefoundrycredentials.Entra)}
}
