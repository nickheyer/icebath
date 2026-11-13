package mcp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func catalogInstallFixture(t *testing.T) (context.Context, string) {
	t.Helper()
	root := isolateHome(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tools.LiveResetForTests()
	saved := []any{TUILaunchHome, TUIProfileHome, TUIProfileRuntimeScope, tools.WatchIntervalSeconds}
	TUILaunchHome = func() string { return root }
	TUIProfileHome = func(name string) (string, error) {
		if name = strings.TrimSpace(name); name == "" || name == "default" {
			return "", nil
		}
		dir, err := profiles.GetProfileDir(name)
		if st, statErr := os.Stat(dir); err != nil || statErr != nil || !st.IsDir() {
			return "", errors.New("Profile '" + name + "' does not exist.")
		}
		return dir, nil
	}
	TUIProfileRuntimeScope = func(ctx context.Context, profileHome string) context.Context {
		return home.WithOverride(ctx, profileHome)
	}
	tools.WatchIntervalSeconds = 0.01
	t.Cleanup(func() {
		TUILaunchHome = saved[0].(func() string)
		TUIProfileHome = saved[1].(func(string) (string, error))
		TUIProfileRuntimeScope = saved[2].(func(context.Context, string) context.Context)
		tools.WatchIntervalSeconds = saved[3].(float64)
		tools.LiveResetForTests()
	})
	return context.Background(), root
}

// A fresh runner targets the calling home with its profile label, and declared env comes from plugin facts only
func TestCatalogRunnerConstructionAndDeclaredEnv(t *testing.T) {
	root := isolateHome(t)
	r := OpenCatalogRunner(context.Background(), nil, nil)
	if r.installer != (HostInstaller{}) || r.home != home.Dir(context.Background()) || r.label != "default" || r.opID != "" ||
		r.facts == nil || r.work == nil || r.approvedEnv == nil || len(r.facts)+len(r.work)+len(r.approvedEnv) != 0 {
		t.Fatalf("default runner %+v under %s", r, root)
	}
	profile := root + "/profiles/work"
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	if named := newCatalogRunner(home.WithOverride(context.Background(), profile), nil, HostInstaller{}); named.home != profile || named.label != "work" {
		t.Fatalf("profile runner home %q label %q", named.home, named.label)
	}
	entry := &plugins.PluginCatalogEntry{Name: "p", Capabilities: plugins.CatalogCapabilities{RequiresEnv: []string{"P_KEY"}}}
	if got := catalogDeclaredEnv(entry); !reflect.DeepEqual(got, []string{"P_KEY"}) {
		t.Fatalf("plugin declared %v", got)
	}
	if got := catalogDeclaredEnv(map[string]any{"name": "s"}); len(got) != 0 {
		t.Fatalf("skill declared %v", got)
	}
	r.facts["p"] = entry
	env := pyjson.NewObject()
	env.Set("P_KEY", "v")
	env.Set("OTHER", "x")
	if msg := r.checkAnswer(&tools.ConnTarget{Name: "p", Kind: "plugin"}, env); msg != "'p' does not declare OTHER" {
		t.Fatalf("check %q", msg)
	}
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
