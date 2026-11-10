package mcp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func catalogInstallFixture(t *testing.T) (context.Context, string) {
	t.Helper()
	root := isolateHome(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tools.LiveResetForTests()
	saved := []any{TUILaunchHome, TUIProfileHome, TUISessionProfileRuntimeScope, tools.WatchIntervalSeconds}
	TUILaunchHome = func() string { return root }
	TUIProfileHome = func(_ context.Context, name string) (string, error) {
		if name = strings.TrimSpace(name); name == "" || name == "default" {
			return "", nil
		}
		dir, err := profiles.GetProfileDir(name)
		if st, statErr := os.Stat(dir); err != nil || statErr != nil || !st.IsDir() {
			return "", errors.New("Profile '" + name + "' does not exist.")
		}
		return dir, nil
	}
	TUISessionProfileRuntimeScope = func(ctx context.Context, session map[string]any) (context.Context, func()) {
		return home.WithOverride(ctx, session["profile_home"].(string)), func() {}
	}
	tools.WatchIntervalSeconds = 0.01
	t.Cleanup(func() {
		TUILaunchHome = saved[0].(func() string)
		TUIProfileHome = saved[1].(func(context.Context, string) (string, error))
		TUISessionProfileRuntimeScope = saved[2].(func(context.Context, map[string]any) (context.Context, func()))
		tools.WatchIntervalSeconds = saved[3].(float64)
		tools.LiveResetForTests()
	})
	return context.Background(), root
}

func TestCatalogClawHubInstallIsReadBackThroughItsCardID(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	var installs []string
	saved := catalogDoInstall
	catalogDoInstall = func(_ context.Context, x *skills.HubCLI, identifier string, _ skills.HubInstallOptions) (skills.HubOutcome, error) {
		installs = append(installs, identifier)
		err := x.Hub.LockFile("").RecordInstall(ctx, "agent-git-oracle", "clawhub", "@tmstudio667-commits/agent-git-oracle", "community",
			"safe", strings.Repeat("0", 64), "agent-git-oracle", []string{"SKILL.md"}, nil, nil)
		return skills.HubOK, err
	}
	t.Cleanup(func() { catalogDoInstall = saved })
	installer := HostInstaller{}
	first, err := installer.InstallSkill(ctx, "agent-git-oracle", false, "clawhub")
	if err != nil || !reflect.DeepEqual(first, map[string]any{"name": "agent-git-oracle", "already_installed": false}) {
		t.Fatalf("first %v %v", first, err)
	}
	for _, c := range []struct{ id, source string }{{"agent-git-oracle", "clawhub"}, {"clawhub/agent-git-oracle", ""}} {
		again, err := installer.InstallSkill(ctx, c.id, false, c.source)
		if err != nil || again["already_installed"] != true {
			t.Fatalf("%v: %v %v", c, again, err)
		}
	}
	if !reflect.DeepEqual(installs, []string{"agent-git-oracle"}) {
		t.Fatalf("installs %v", installs)
	}
}
