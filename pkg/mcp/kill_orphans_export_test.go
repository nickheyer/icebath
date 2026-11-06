package mcp

import "testing"

func TestKillOrphanedMCPChildrenReturnsWithoutLiveServers(t *testing.T) {
	KillOrphanedMCPChildren(false, "no-such-server")
	KillOrphanedMCPChildren(true, "")
}
