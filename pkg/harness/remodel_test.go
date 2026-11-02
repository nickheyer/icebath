package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestSaveProfileDropsTheModelTheEditRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, dir)
	path, err := profiles.UserPath()
	if err != nil {
		t.Fatal(err)
	}
	doc := profiles.Document{Default: "home", Profiles: []profiles.Profile{{Name: "home", Template: "anthropic", Models: []string{"gone-1", "kept-1"}, Key: profiles.Source{Kind: profiles.SourceStatic, Value: "k"}}}}
	if err := profiles.WriteFile(path, doc); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"profile":"home","model":"gone-1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	cfg, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := Build(ctx, cfg, func(any) {}, BuildOptions{Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if a.Primary.Model != "gone-1" || a.Session.Model() != "gone-1" {
		t.Fatalf("built on %q, session on %q", a.Primary.Model, a.Session.Model())
	}
	p := a.Profiles[0].Written
	p.Models = []string{"kept-1"}
	if err := a.SaveProfile("home", p); err != nil {
		t.Fatal(err)
	}
	if a.Config.Model != "kept-1" || a.Primary.Model != "kept-1" || a.Session.Model() != "kept-1" {
		t.Fatalf("after dropping gone-1: config %q primary %q session %q", a.Config.Model, a.Primary.Model, a.Session.Model())
	}
	written, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"model": "kept-1"`) {
		t.Fatalf("config file after the edit:\n%s", written)
	}
	p.Models = nil
	if err := a.SaveProfile("home", p); err != nil {
		t.Fatal(err)
	}
	base, _ := profiles.Lookup("anthropic")
	if want := base.Model(); want == "" || a.Config.Model != want || a.Primary.Model != want || a.Session.Model() != want {
		t.Fatalf("after dropping every model: config %q primary %q session %q, want the provider default %q", a.Config.Model, a.Primary.Model, a.Session.Model(), want)
	}
	if written, err = os.ReadFile(cfgPath); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"model": "`+base.Model()+`"`) {
		t.Fatalf("config file after dropping every model:\n%s", written)
	}
	ep := a.Primary
	ep.Model = ""
	if _, err := a.newAgent("x", a.Session, ep); err == nil || !strings.Contains(err.Error(), "has no model") {
		t.Fatalf("agent built without a model: %v", err)
	}
	a.Lives()[0].Endpoint.Model = ""
	ep.Model = "picked-1"
	a.SetDefaultEndpoint("home", ep)
	if a.Config.Model != "picked-1" || a.Session.Model() != "picked-1" || a.Lives()[0].Endpoint.Model != "picked-1" {
		t.Fatalf("after picking a model: config %q session %q live %q", a.Config.Model, a.Session.Model(), a.Lives()[0].Endpoint.Model)
	}
}
