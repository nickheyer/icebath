package commands

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRunApprovalModeCommandInspectsPersistsAndRefuses(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := home.WithOverride(context.Background(), h)
	env := &tools.Env{Home: h, Config: config.Defaults()}
	initial := tools.GetApprovalMode(env)
	got, err := RunApprovalModeCommand(ctx, h, env, "")
	if err != nil || !got.OK || got.Mode != initial || got.Changed || got.Message != "Approval mode: "+initial+" (persistent profile setting)." {
		t.Fatalf("inspect %+v %v", got, err)
	}
	got, err = RunApprovalModeCommand(ctx, h, env, "bogus")
	if err != nil || got.OK || got.Mode != initial || got.Message != "Usage: /approvals [manual|smart|off]" {
		t.Fatalf("invalid %+v %v", got, err)
	}
	got, err = RunApprovalModeCommand(ctx, h, env, " OFF ")
	if err != nil || !got.OK || got.Mode != "off" || !got.Changed || got.Message != "Approval mode: off (persistent profile setting)." {
		t.Fatalf("persist %+v %v", got, err)
	}
	raw, err := os.ReadFile(home.ConfigPath(h))
	if err != nil || !strings.Contains(string(raw), "mode: \"off\"") {
		t.Fatalf("config %q %v", raw, err)
	}
	got, err = RunApprovalModeCommand(ctx, h, env, "off")
	if err != nil || !got.OK || got.Changed || got.Mode != "off" {
		t.Fatalf("unchanged %+v %v", got, err)
	}
	got, err = RunApprovalModeCommand(ctx, h, env, "smart")
	if err != nil || !got.OK || !got.Changed || got.Mode != "smart" {
		t.Fatalf("second change %+v %v", got, err)
	}
}
