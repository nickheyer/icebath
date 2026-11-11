package mcp

import "github.com/nickheyer/icebath/pkg/tools"

// Swaps the managed client a card retry mints through the way hermes tests patch ConnectorClient, returning the restore
func SwapReissueManagedClient(fn func(env *tools.Env, createsGuest bool, initiator string, call *tools.CallRef) (tools.ManagedClient, error)) func() {
	saved := reissueManagedClient
	reissueManagedClient = fn
	return func() { reissueManagedClient = saved }
}
