package codeexec

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestComputerUseBackendSetIsWhatTheSelectorReads(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", h)
	env := cuPluginHome(t, h, nil, "", "")
	var out bytes.Buffer
	prev := config.FacadeStdout
	config.FacadeStdout = &out
	t.Cleanup(func() { config.FacadeStdout = prev; config.ResetCache() })
	config.ResetCache()
	src := config.ConfigCommandSources{
		RuntimeKeyOwners: config.RuntimeKeyOwners{
			SelectionConfigKeys: tools.SelectionConfigKeys,
			ActiveManifests:     func() []config.PluginConfigKeys { return plugins.ActiveConfigKeyManifests(h) },
		},
		LeftCoreTerminalKeys: provider.LeftCoreTerminalKeys,
		IsEnvSettingKey:      provider.IsEnvSettingKey,
		SaveEnvSetting:       provider.SaveEnvSetting,
		ReadEnvSetting:       provider.ReadEnvSetting,
		RemoveEnvSetting:     provider.RemoveEnvSetting,
		DropStaleModelRoute:  provider.DropStaleModelRoute,
	}
	if err := config.SetConfigValue(context.Background(), h, "computer_use.backend", "widget-driver", false, src); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "not a recognized config key") {
		t.Fatalf("computer_use.backend noticed: %s", out.String())
	}
	if got := configuredBackendName(env); got != "widget-driver" {
		t.Fatalf("selector reads %q", got)
	}
}
