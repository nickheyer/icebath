package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Holds the project's dependency lock until the test releases it
func pubRuntimeLock(t *testing.T, ctx context.Context, project string) func() {
	t.Helper()
	held, release, err := RuntimeLock(ctx, project, WaitForever)
	if err != nil || !held {
		t.Fatalf("runtime lock %v %v", held, err)
	}
	return release
}

// Publishes a plugin selection for one home through the real selection type
func pubPublishSelection(t *testing.T, ctx context.Context, project, homeDir string, enabled ...string) {
	t.Helper()
	selection, err := newPluginSelection(ctx, map[string]any{"home": homeDir, "enabled": pubList(enabled...), "disabled": pubList()})
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.Publish(ctx, project); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRecoveryRefusesToReplaceNewerConfig(t *testing.T) {
	tmp := isolateHome(t)
	ctx := context.Background()
	t.Setenv(home.EnvHome, tmp)
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o777); err != nil {
		t.Fatal(err)
	}
	peSwap(t, &repoRoot, func() string { return repo })
	config := filepath.Join(tmp, "config.yaml")
	peWrite(t, config, "plugins:\n  enabled: [old]\n")
	release := pubRuntimeLock(t, ctx, repo)
	pubPublishSelection(t, ctx, repo, tmp, "new")
	peWrite(t, config, peRead(t, config)+"model: user-choice\n")
	changed := peRead(t, config)
	err := recoverPublication(ctx, repo)
	release()
	svWantError(t, err, "config changed")
	var py *PyError
	if !errors.As(err, &py) || py.Class != "RuntimeError" {
		t.Fatalf("recovery error class %T %v", err, err)
	}
	if peRead(t, config) != changed || !isRegularFile(pubState(t, repo, "publication.json")) {
		t.Fatal("refused recovery touched the config or dropped the journal")
	}
}

func TestContextHomePublicationRecoversFromItsOwnProcess(t *testing.T) {
	tmp := isolateHome(t)
	processHome := filepath.Join(tmp, "process-home")
	contextHome := filepath.Join(tmp, "context-home")
	project := filepath.Join(tmp, "repo")
	for _, dir := range []string{contextHome, project} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(contextHome, "config.yaml")
	original := "plugins:\n  enabled: [old]\n"
	peWrite(t, config, original)
	t.Setenv(home.EnvHome, processHome)
	peSwap(t, &repoRoot, func() string { return project })
	processState := InstallStateDir(context.Background(), project)
	ctx := home.WithOverride(context.Background(), contextHome)
	contextState := InstallStateDir(ctx, project)
	if contextState == processState {
		t.Fatal("a context home must own its dependency state")
	}
	ordered, err := EnabledPluginsOrdered(ctx, EnabledPluginsOptions{})
	if err != nil || !reflect.DeepEqual(ordered, []HomePlugins{{PluginsDir: filepath.Join(contextHome, "plugins"), Names: []string{"old"}}}) {
		t.Fatalf("context selection %v %v", ordered, err)
	}
	release := pubRuntimeLock(t, ctx, project)
	pubPublishSelection(t, ctx, project, contextHome, "new")
	release()
	if peRead(t, config) == original {
		t.Fatal("selection not published")
	}
	if pathExists(processState) {
		t.Fatal("context publication wrote the process home's state")
	}
	t.Setenv(home.EnvHome, contextHome)
	process := context.Background()
	if InstallStateDir(process, project) != contextState {
		t.Fatal("the context home's own process reads another state dir")
	}
	release = pubRuntimeLock(t, process, project)
	err = recoverPublication(process, project)
	release()
	if err != nil {
		t.Fatal(err)
	}
	if peRead(t, config) != original || pathExists(filepath.Join(contextState, "publication.json")) {
		t.Fatalf("recovery left %q", peRead(t, config))
	}
}
