package tools

import (
	"slices"
	"testing"
)

// Every shipped backend name is listed once in a stable order
func TestEveryShippedTerminalBackendIsListed(t *testing.T) {
	want := []string{"daytona", "docker", "local", "managed_modal", "modal", "ssh", "vercel_sandbox"}
	if got := BuiltinTerminalBackends(); !slices.Equal(got, want) {
		t.Fatalf("backends %q", got)
	}
}
