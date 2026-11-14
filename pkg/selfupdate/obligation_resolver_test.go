package selfupdate

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestObligationHostStateDirMatchesTheGatewayResolver(t *testing.T) {
	cases := []map[string]string{
		{"ICEBATH_GATEWAY_LOCK_DIR": "/srv/override/locks"},
		{"XDG_STATE_HOME": "/srv/xdg-state"},
		{"XDG_STATE_HOME": "relative/state"},
		{},
	}
	for _, env := range cases {
		for _, name := range []string{"ICEBATH_GATEWAY_LOCK_DIR", "XDG_STATE_HOME"} {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
		for name, value := range env {
			t.Setenv(name, value)
		}
		if got, want := updaterecord.HostStateDir(), gateway.GatewayLockDir(); got != want {
			t.Fatalf("env %v host state dir %q, gateway resolver %q", env, got, want)
		}
	}
}
