package app

import (
	"connectrpc.com/connect/v2"

	qwenoauthconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/config"
	qwencredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// The Qwen account sign-in, and the Qwen CLI's own login
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, qwenoauthconfig.Name, qwencredentials.SignIn, qwencredentials.QwenCLI)}
}
