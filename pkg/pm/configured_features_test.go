package pm

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestAutomaticRepairNeverUndoesAWithoutThatLandedAfterItsRead(t *testing.T) {
	b := declBlockingSetup(t)
	repo := RepoRoot()
	peWrite(t, filepath.Join(repo, "pyproject.toml"), peRead(t, filepath.Join(peReferences, "pyproject.toml")))
	peSwap(t, &syncFeaturePolicy, func(context.Context, []string, bool) ([]string, []string, error) { return nil, nil, nil })
	peSwap(t, &ConfiguredFeaturesMissingDeps, func(context.Context) []ConfiguredFeatureGap {
		return []ConfiguredFeatureGap{{Feature: "Matrix", Hint: "Run `icebath setup`.", Extra: "matrix"}}
	})
	uv, python := peUV(t), pePinnedPython(t)
	peSwap(t, &uvToolchain, func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil })
	var out bytes.Buffer
	ctx := withSyncOutput(declAs("main"), &out)
	if err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"all"}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	real, optedOut := dependencyLock, false
	peSwap(t, &dependencyLock, func(ctx context.Context, patient bool) (func(), error) {
		if !optedOut {
			optedOut = true
			if err := DeclineExtras(ctx, []string{"matrix"}); err != nil {
				return nil, err
			}
			if err := syncVenv(ctx, SyncVenvOptions{Extras: []string{"all"}, Explicit: true}); err != nil {
				return nil, err
			}
		}
		return real(ctx, patient)
	})
	if err := InstallConfiguredFeaturesMissingDeps(ctx, repo); err != nil {
		t.Fatalf("repair %v: %s", err, out.String())
	}
	if !optedOut {
		t.Fatal("the opt out never landed between the repair's read and its lock")
	}
	if got := declDeclined(t, ""); !reflect.DeepEqual(got, map[string]bool{"matrix": true}) {
		t.Fatalf("declined %v", got)
	}
	if got := carriedRecorded(t); !reflect.DeepEqual(got, []string{"all"}) {
		t.Fatalf("recorded %v", got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, built := range b.built {
		if slices.Contains(built, "matrix") {
			t.Fatalf("a build re-added matrix %v", b.built)
		}
	}
}

func TestUpdateInstallsTheExtrasConfiguredFeaturesNeed(t *testing.T) {
	tmp := isolateHome(t)
	root := filepath.Join(tmp, "project")
	peWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"x\"\nversion = \"0\"\n"+
		"[project.optional-dependencies]\ndiscord = []\ngoogle-chat = []\nmcp = []\n")
	peSwap(t, &ConfiguredFeaturesMissingDeps, func(context.Context) []ConfiguredFeatureGap {
		return []ConfiguredFeatureGap{{"Discord", "discord hint", "discord"}, {"Google Chat", "chat hint", "google_chat"},
			{"IRC", "irc hint", "irc"}, {"MCP servers", "mcp hint", "mcp"}}
	})
	var calls []SyncVenvOptions
	peSwap(t, &clientSyncVenv, func(_ context.Context, opts SyncVenvOptions) error {
		calls = append(calls, opts)
		return nil
	})
	var out bytes.Buffer
	if err := InstallConfiguredFeaturesMissingDeps(withSyncOutput(context.Background(), &out), root); err != nil {
		t.Fatal(err)
	}
	extras := []string{"discord", "google-chat", "mcp"}
	want := []SyncVenvOptions{{Extras: extras, Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true, Automatic: extras}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %+v", calls)
	}
	text := out.String()
	if !strings.Contains(text, "IRC: irc hint") || strings.Contains(text, "Discord") || strings.Contains(text, "MCP servers") {
		t.Fatalf("output %q", text)
	}
}
