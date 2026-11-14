package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Removes global MCP tools other tests left registered so the session scopes start from a fresh registry
func isolateMCPRegistry(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, d := range tools.Default.Snapshot(ctx, "") {
		if strings.HasPrefix(d.Name, "mcp__") {
			if err := tools.Default.Deregister(d.Name, "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
}
