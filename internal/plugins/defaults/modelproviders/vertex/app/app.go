package app

import (
	"connectrpc.com/connect/v2"

	vertexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/config"
	vertexcredentials "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/credentials"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
)

type connectors struct{}

func Register(registry *providerapp.Registry) {
	registry.Register(connectors{})
}

// A Vertex account signed in with a Google service account key
func (connectors) Connectors(conn *connect.Client) []providerapp.Connector {
	return []providerapp.Connector{providerapp.SignIns(conn, vertexconfig.Name, vertexcredentials.ServiceAccount)}
}
