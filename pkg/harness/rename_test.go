package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestSaveProfileRenamePointsEveryReferenceAtTheNewName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, dir)
	path, err := profiles.UserPath()
	if err != nil {
		t.Fatal(err)
	}
	doc := profiles.Document{Default: "old", Profiles: []profiles.Profile{{Name: "old", Template: "anthropic", Key: profiles.Source{Kind: profiles.SourceStatic, Value: "k"}}}}
	if err := profiles.WriteFile(path, doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Setting{{Key: "renametest.profile", Profile: true}, {Key: "renametest.other"}} {
		if err := RegisterSetting(s); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"profile":"old","settings":{"renametest.profile":"old","renametest.other":"old"},"compaction":{"model":{"profile":"old"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	kinds := filepath.Join(dir, KindsDir)
	if err := os.MkdirAll(kinds, 0o755); err != nil {
		t.Fatal(err)
	}
	kindPath := filepath.Join(kinds, "critic.json")
	if err := os.WriteFile(kindPath, []byte(`{"name":"critic","model":{"profile":"old","model":"claude-fable-5-1"}}`), 0o644); err != nil {
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
	p := a.Profiles[0].Written
	p.Name = "new"
	if err := a.SaveProfile("old", p); err != nil {
		t.Fatal(err)
	}
	if a.Config.Profile != "new" || a.Config.Setting("renametest.profile") != "new" || a.Config.Setting("renametest.other") != "old" || a.Config.Compaction.Model.Profile != "new" {
		t.Fatalf("config after rename: profile %q settings %v compaction %+v", a.Config.Profile, a.Config.Settings, a.Config.Compaction.Model)
	}
	if a.Primary.Profile.Name != "new" {
		t.Fatalf("primary endpoint still on %q", a.Primary.Profile.Name)
	}
	for _, l := range a.Lives() {
		if l.Endpoint.Profile.Name != "new" || l.Session.Labels()[profileLabel] == "old" {
			t.Fatalf("open session on %q labeled %q", l.Endpoint.Profile.Name, l.Session.Labels()[profileLabel])
		}
	}
	if k, ok := a.Kind("critic"); !ok || k.Model == nil || k.Model.Profile != "new" || k.Model.Model != "claude-fable-5-1" {
		t.Fatalf("kind after rename: %+v", k.Model)
	}
	written, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	for _, want := range []string{`"profile": "new"`, `"renametest.profile": "new"`, `"renametest.other": "old"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("config file lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(strings.ReplaceAll(text, `"renametest.other": "old"`, ""), `"old"`) {
		t.Fatalf("config file still names the old profile:\n%s", text)
	}
	kindText, err := os.ReadFile(kindPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kindText), `"new"`) || strings.Contains(string(kindText), `"old"`) {
		t.Fatalf("kind file after rename:\n%s", kindText)
	}
	if _, ok := a.FindProfile("old"); ok {
		t.Fatal("old profile still found")
	}
	if _, ok := a.FindProfile("new"); !ok {
		t.Fatal("new profile not found")
	}
}
