package plugins

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestCmdUpdateOfAMissingPluginExitsOne(t *testing.T) {
	ctx, _ := listWorld(t)
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdUpdate(ctx, "nonexistent-plugin", true, false, &out); !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestClearPluginBytecodeRemovesNestedCaches(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "myplugin")
	top, nested := filepath.Join(plugin, "__pycache__"), filepath.Join(plugin, "sub", "__pycache__")
	c6Write(t, filepath.Join(top, "a.pyc"), "stale")
	c6Write(t, filepath.Join(nested, "b.pyc"), "stale")
	if removed := clearPluginBytecode(plugin); removed != 2 {
		t.Fatalf("%d", removed)
	}
	for _, d := range []string{top, nested} {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s survived", d)
		}
	}
}

func TestSubdirInstallStaysUpdatable(t *testing.T) {
	_, h := pubWorld(t)
	ctx := home.WithOverride(context.Background(), h)
	repo := filepath.Join(t.TempDir(), "monorepo")
	c6Write(t, filepath.Join(repo, "README.md"), "# Monorepo docs\n")
	c6Write(t, filepath.Join(repo, "tests", "test_x.py"), "def test_x():\n    pass\n")
	c6Write(t, filepath.Join(repo, "my-plugin", "plugin.yaml"), "name: my-plugin\nmanifest_version: 1\ndescription: A subdir plugin\n")
	c6Write(t, filepath.Join(repo, "my-plugin", "__init__.py"), "# plugin entry\n")
	c6Git(t, repo, "init", "-q")
	c6Commit(t, repo, "init")
	target, _, _, err := installPluginCore(ctx, "file://"+repo+"#my-plugin", installCoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		t.Fatal("subdir install carries a .git")
	}
	c6Write(t, filepath.Join(repo, "my-plugin", "__init__.py"), "VERSION = 2\n")
	newSha := c6Commit(t, repo, "v2")
	pinned, notGit := func(map[string]any) string { return "pinned" }, func() string { return "not git" }
	output, err := pullPluginUpdate(ctx, h, target, pinned, notGit, nil, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(target, "__init__.py"))
	metadata, _ := readInstallMetadata(h)
	record, _ := metadata["my-plugin"].(map[string]any)
	if !strings.Contains(string(raw), "VERSION = 2") || record["revision"] != newSha || strings.Contains(output, "Already up to date") {
		t.Fatalf("%q %v %q", raw, record, output)
	}
	if again, err := pullPluginUpdate(ctx, h, target, pinned, notGit, nil, false, false, nil); err != nil || !strings.Contains(again, "Already up to date") {
		t.Fatalf("%q %v", again, err)
	}
}

type updateCall struct {
	name        string
	interactive bool
}

// Four check rows, two movable, a pip row and a pin, with the single plugin update recorded
func updateFleet(t *testing.T) (context.Context, *[]updateCall) {
	ctx, h := listWorld(t)
	yes, no := true, false
	rows := []*CheckResult{
		{Name: "cat", Class: "catalog", Current: strings.Repeat("a", 40), Latest: strings.Repeat("b", 40), UpdateAvailable: &yes},
		{Name: "gitty", Class: "git", Current: strings.Repeat("c", 40), Latest: strings.Repeat("d", 40), UpdateAvailable: &yes},
		{Name: "pypkg", Class: "pip", Current: "1.0", Latest: "1.1", UpdateAvailable: &yes},
		{Name: "pinned", Class: "git", Current: strings.Repeat("e", 40), UpdateAvailable: &no, Reason: "pinned @ eeee"},
	}
	var calls []updateCall
	savedChecks, savedTarget, savedOne, savedTTY, savedInput := updateAllChecks, updateAllTarget, updateAllOne, isTTY, inputLine
	t.Cleanup(func() {
		updateAllChecks, updateAllTarget, updateAllOne, isTTY, inputLine = savedChecks, savedTarget, savedOne, savedTTY, savedInput
	})
	updateAllChecks = func(context.Context, string) []*CheckResult { return rows }
	updateAllTarget = func(_ context.Context, name string) (string, error) { return filepath.Join(h, name), nil }
	updateAllOne = func(_ context.Context, name, _ string, _ *Console, interactive, _ bool) (bool, error) {
		calls = append(calls, updateCall{name, interactive})
		if name == "cat" {
			return false, errors.New("TimeoutError: catalog pin refused")
		}
		return true, nil
	}
	return ctx, &calls
}

func TestNoTerminalRefusesWithoutYesAndUpdatesOnlyMovableRowsWithIt(t *testing.T) {
	ctx, calls := updateFleet(t)
	isTTY = func() bool { return false }
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdUpdateAll(ctx, false, &out); !errors.As(err, &exit) || exit.Code != 1 || len(*calls) != 0 || !strings.Contains(out.String(), "--yes") {
		t.Fatalf("%v %v %q", err, *calls, out.String())
	}
	out.Reset()
	if err := CmdUpdateAll(ctx, true, &out); !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("%v", err)
	}
	if !slices.Equal(*calls, []updateCall{{"cat", false}, {"gitty", false}}) {
		t.Fatalf("%v", *calls)
	}
	text := out.String()
	_, summary, _ := strings.Cut(text, "Summary")
	if !strings.Contains(summary, "catalog pin refused") || !strings.Contains(text, "skipped pypkg") || !strings.Contains(text, "skipped pinned") {
		t.Fatalf("%q", text)
	}
}

func TestTerminalAsksOnceAndCancelMovesNothing(t *testing.T) {
	ctx, calls := updateFleet(t)
	isTTY = func() bool { return true }
	var prompts []string
	inputLine = func(prompt string) (string, error) { prompts = append(prompts, prompt); return "n", nil }
	var out bytes.Buffer
	if err := CmdUpdateAll(ctx, false, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(prompts, []string{"\nUpdate all 2? [Y/n]: "}) || len(*calls) != 0 || !strings.Contains(out.String(), "Cancelled. Nothing was updated.") {
		t.Fatalf("%v %v %q", prompts, *calls, out.String())
	}
}

func TestPlanUpdateAllSplitsEveryRowKind(t *testing.T) {
	yes, no := true, false
	plan := PlanUpdateAll([]*CheckResult{
		{Name: "broken", Class: "git", NeedsFixing: "update_url mismatch"},
		{Name: "unknown", Class: "git"},
		{Name: "current", Class: "git", UpdateAvailable: &no},
		{Name: "movable", Class: "git", UpdateAvailable: &yes},
	})
	if len(plan.Movable) != 1 || plan.Movable[0].Name != "movable" || plan.Current != 1 || len(plan.Skipped) != 2 ||
		plan.Skipped[0].Reason != "needs fixing: update_url mismatch" || plan.Skipped[1].Reason != "update status unknown" {
		t.Fatalf("%+v", plan)
	}
	if got := shortReason(strings.Repeat("x", 250) + "\nsecond"); len(got) != 200 || !strings.HasSuffix(got, "...") {
		t.Fatalf("%q", got)
	}
}
