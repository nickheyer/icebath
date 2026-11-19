package app

import (
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

// Gateway connection of the CLI while its session is open, nil off the gateway or once the connection is final
func gatewayConnectionFor(c *CLI) *gatewaycli.GatewayConnection {
	store := c.gatewayStore()
	if store == nil || !c.gatewaySessionOpen || store.Connection.Final() {
		return nil
	}
	return store.Connection
}
