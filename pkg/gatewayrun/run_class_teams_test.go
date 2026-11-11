package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestGatewayRunnerSkipsWiringWithoutMsgraphAdapter(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled: [teams_pipeline]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.adapters = adapterDict(gateway.PlatformTelegram, nil)
	called := false
	previous := BindTeamsPipelineRuntime
	BindTeamsPipelineRuntime = func(context.Context, *GatewayRunner) (bool, error) {
		called = true
		return true, nil
	}
	t.Cleanup(func() { BindTeamsPipelineRuntime = previous })
	r.wireTeamsPipelineRuntime(context.Background())
	if called {
		t.Fatal("bound without a msgraph adapter")
	}
}
