package commands

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Root home under a fake user home, the fixture of the onboarding run cases
func onboardingRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return root
}

// Raw user config of a file
func onboardingRaw(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := config.ReadUserConfigRaw(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSetRunFalsePersistsAndKeepsComments(t *testing.T) {
	root := onboardingRoot(t)
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("# my notes\nmodel:\n  default: some-model  # pinned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tools.SetOnboardingRun(ctx, false, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if !strings.Contains(text, "# my notes") || !strings.Contains(text, "# pinned") {
		t.Fatalf("comments lost:\n%s", text)
	}
	if got := onboardingRaw(t, path)["model"]; !reflect.DeepEqual(got, map[string]any{"default": "some-model"}) {
		t.Fatalf("model = %#v", got)
	}
	if tools.OnboardingShouldRun(ctx, false) {
		t.Fatal("should_run after set_run(False)")
	}
}

func TestNamedProfileLaunchUsesTheRootFile(t *testing.T) {
	root := onboardingRoot(t)
	ctx := context.Background()
	work, err := manage.CreateProfile(ctx, "work", manage.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", work)
	if home.Root() != root {
		t.Fatalf("default root %s, want %s", home.Root(), root)
	}
	if err := tools.SetOnboardingRun(ctx, true, false); err != nil {
		t.Fatal(err)
	}
	if section, _ := onboardingRaw(t, filepath.Join(root, "config.yaml"))["onboarding"].(map[string]any); section["run"] != true {
		t.Fatalf("root onboarding = %#v", section)
	}
	if section, _ := onboardingRaw(t, filepath.Join(work, "config.yaml"))["onboarding"].(map[string]any); section != nil {
		if _, has := section["run"]; has {
			t.Fatalf("work onboarding = %#v", section)
		}
	}
	if !tools.OnboardingShouldRun(ctx, false) {
		t.Fatal("should_run after set_run(True)")
	}
	if err := os.WriteFile(filepath.Join(work, "config.yaml"), []byte("onboarding:\n  run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	if err := tools.SetOnboardingRun(ctx, false, false); err != nil {
		t.Fatal(err)
	}
	if tools.OnboardingShouldRun(ctx, false) {
		t.Fatal("should_run after set_run(False)")
	}
}
