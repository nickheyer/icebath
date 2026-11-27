package app

import (
	"connectrpc.com/connect/v2"

	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	anthropiccredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// Ways to sign an Anthropic account in besides an API key
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, anthropicconfig.Name,
		anthropiccredentials.Subscription, anthropiccredentials.Pasted, anthropiccredentials.ClaudeCode)}
}
