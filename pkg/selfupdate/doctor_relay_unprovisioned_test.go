package selfupdate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestCheckRelayPluginsReportsAnUnprovisionedRuntimeWithItsCause(t *testing.T) {
	d, out := doctorRawHome(t, "", "")
	doctorUnsetEnv(t, tools.RelayPluginsConfigEnv)
	cause := &pm.ProvisionDeferred{Cause: "python: install failed: download failed", Retry: 30 * time.Second}
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) {
		return nil, &pm.RuntimeUnprovisioned{Venv: "/install/venv", Err: cause}
	})
	t.Cleanup(func() { tools.RegisterNemoRelayBinding(nil) })
	f, err := checkRelayPlugins(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "NeMo Relay runtime is not provisioned") || !strings.Contains(text, "(Icebath runs without Relay plugins)") ||
		!strings.Contains(text, cause.Error()) || strings.Contains(text, "not available on this platform") {
		t.Fatalf("out = %q", text)
	}
	if len(f.ManualIssues) != 1 || f.ManualIssues[0] != "Run `icebath pm install` to provision the NeMo Relay runtime." {
		t.Fatalf("manual issues = %q", f.ManualIssues)
	}
}
