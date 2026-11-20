//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestCheckRelayPluginsReportsAnUnprovisionedRuntimeWithItsCause(t *testing.T) {
	d, out := doctorRawHome(t, "", "")
	t.Setenv(tools.RelayPluginsConfigEnv, "/home/x/relay-plugins.toml")
	cause := errors.New("run `icebath pm install --extra nemo-relay` to provision it")
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) {
		return nil, &pm.RuntimeUnprovisioned{Venv: "/install/venv", Err: cause}
	})
	t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
	f, err := checkRelayPlugins(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "Relay plugins are configured but the NeMo Relay runtime is not provisioned") || !strings.Contains(text, "(Icebath runs without Relay plugins)") ||
		!strings.Contains(text, cause.Error()) || strings.Contains(text, "not available on this platform") {
		t.Fatalf("out = %q", text)
	}
	if len(f.ManualIssues) != 1 || f.ManualIssues[0] != "Run `icebath pm install --extra nemo-relay` to provision the NeMo Relay runtime." {
		t.Fatalf("manual issues = %q", f.ManualIssues)
	}
}
