package skills

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Bundles runner over a temp bundles dir writing to a buffer, its line reader failing the test unless replaced
func bundlesCLIEnv(t *testing.T) (*BundlesCLI, *bytes.Buffer, string) {
	t.Helper()
	s, bundles, _ := bundlesEnv(t)
	t.Setenv("COLUMNS", "400")
	var out bytes.Buffer
	b := NewBundlesCLI(s, &out)
	b.LineInput = func(prompt string) (string, error) {
		t.Fatalf("unexpected prompt %q", prompt)
		return "", nil
	}
	return b, &out, bundles
}

// Runs one bundles command, returning its status and the output it printed with line wraps removed
func runBundles(t *testing.T, b *BundlesCLI, out *bytes.Buffer, a BundlesArgs) (int, string) {
	t.Helper()
	out.Reset()
	code, err := b.BundlesCommand(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return code, strings.ReplaceAll(out.String(), "\n", "|")
}

func TestBundlesCLICreateRefusesOverwrite(t *testing.T) {
	b, out, bundles := bundlesCLIEnv(t)
	if code, _ := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "dup", Skill: []string{"s1"}}); code != 0 {
		t.Fatal(code)
	}
	code, got := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "dup", Skill: []string{"s2"}})
	want := "Bundle already exists at " + filepath.Join(bundles, "dup.yaml") + "|Pass --force to overwrite.|"
	if code != 1 || got != want {
		t.Fatalf("%d %q", code, got)
	}
	if code, _ := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "dup", Skill: []string{"s2"}, Force: true}); code != 0 {
		t.Fatal(code)
	}
	if info, _, _ := b.Scope.GetBundle("dup"); info["skills"].([]any)[0] != "s2" {
		t.Fatal(info)
	}
}

func TestBundlesCLICreateRequiresSkills(t *testing.T) {
	b, out, bundles := bundlesCLIEnv(t)
	for _, stop := range []error{cliui.ErrEOF, cliui.ErrInterrupt} {
		var prompts []string
		b.LineInput = func(prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return "", stop
		}
		code, got := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "empty"})
		if code != 1 || got != "No skills passed via --skill. Enter one skill name per line.|Submit an empty line to finish.||Cancelled.|" || len(prompts) != 1 || prompts[0] != "skill> " {
			t.Fatalf("%v %d %q %q", stop, code, got, prompts)
		}
	}
	b.LineInput = func(string) (string, error) { return "  ", nil }
	if code, got := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "empty"}); code != 1 || !strings.HasSuffix(got, "|A bundle must reference at least one skill.|") {
		t.Fatalf("%d %q", code, got)
	}
	if _, err := os.Stat(filepath.Join(bundles, "empty.yaml")); err == nil {
		t.Fatal("refused bundle was written")
	}
}

func TestBundlesCLICreateReadsSkillsInteractively(t *testing.T) {
	b, out, bundles := bundlesCLIEnv(t)
	lines := []string{" alpha ", "beta", ""}
	b.LineInput = func(string) (string, error) {
		line := lines[0]
		lines = lines[1:]
		return line, nil
	}
	code, got := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "My Pack", Description: "Both", Instruction: "Go"})
	want := "No skills passed via --skill. Enter one skill name per line.|Submit an empty line to finish.|Created bundle: " +
		filepath.Join(bundles, "my-pack.yaml") + "|  Invoke with: /my-pack  (loads 2 skills)|"
	if code != 0 || got != want {
		t.Fatalf("%d %q", code, got)
	}
	raw, _ := os.ReadFile(filepath.Join(bundles, "my-pack.yaml"))
	if string(raw) != "name: My Pack\nskills:\n  - alpha\n  - beta\ndescription: Both\ninstruction: Go\n" {
		t.Fatalf("%q", raw)
	}
}

func TestBundlesCLICreateReportsValueErrors(t *testing.T) {
	b, out, _ := bundlesCLIEnv(t)
	if code, got := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "!!!", Skill: []string{"a"}}); code != 1 || got != "Bundle name '!!!' normalizes to an empty slug|" {
		t.Fatalf("%d %q", code, got)
	}
	if code, got := runBundles(t, b, out, BundlesArgs{Action: "create", Name: "x", Skill: []string{" "}}); code != 1 || got != "Bundle must reference at least one skill|" {
		t.Fatalf("%d %q", code, got)
	}
}

func TestBundlesCLIListShowDeleteAndReload(t *testing.T) {
	b, out, bundles := bundlesCLIEnv(t)
	code, got := runBundles(t, b, out, BundlesArgs{})
	if code != 0 || got != "No bundles installed yet. Create one with:|  icebath bundles create <name> --skill skill1 --skill skill2|Bundles directory: "+bundles+"|" {
		t.Fatalf("%d %q", code, got)
	}
	makeBundleYAML(t, bundles, "zeta", []string{"a", "b"}, "Zeta pack", "Run them in order")
	makeBundleYAML(t, bundles, "alpha", []string{"c"}, "", "")
	code, got = runBundles(t, b, out, BundlesArgs{Action: "list"})
	if code != 0 || !strings.Contains(got, "Skill Bundles (2)") || !strings.HasSuffix(got, "||Bundles directory: "+bundles+"|") ||
		strings.Index(got, "/alpha") > strings.Index(got, "/zeta") || !strings.Contains(got, "Load 1 skills as a bundle") {
		t.Fatalf("%d %q", code, got)
	}
	for _, row := range []string{"Command", "Name", "Skills", "Description", "Zeta pack"} {
		if !strings.Contains(got, row) {
			t.Fatalf("table lacks %q in %q", row, got)
		}
	}
	code, got = runBundles(t, b, out, BundlesArgs{Action: "show", Name: "Zeta"})
	want := "/zeta  zeta|  Zeta pack|  File: " + filepath.Join(bundles, "zeta.yaml") + "|  Skills (2):|    - a|    - b|  Instruction:|    Run them in order|"
	if code != 0 || got != want {
		t.Fatalf("%d %q", code, got)
	}
	if code, got = runBundles(t, b, out, BundlesArgs{Action: "show", Name: "nope"}); code != 1 || got != "Bundle 'nope' not found.|" {
		t.Fatalf("%d %q", code, got)
	}
	if code, got = runBundles(t, b, out, BundlesArgs{Action: "reload"}); code != 0 || got != "No changes. 2 bundle(s) loaded.|" {
		t.Fatalf("%d %q", code, got)
	}
	if code, got = runBundles(t, b, out, BundlesArgs{Action: "delete", Name: "alpha"}); code != 0 || got != "Deleted bundle: "+filepath.Join(bundles, "alpha.yaml")+"|" {
		t.Fatalf("%d %q", code, got)
	}
	if code, got = runBundles(t, b, out, BundlesArgs{Action: "delete", Name: "alpha"}); code != 1 || got != "No bundle at "+filepath.Join(bundles, "alpha.yaml")+"|" {
		t.Fatalf("%d %q", code, got)
	}
	makeBundleYAML(t, bundles, "fresh", []string{"x"}, "Fresh one", "")
	os.Remove(filepath.Join(bundles, "zeta.yaml"))
	code, got = runBundles(t, b, out, BundlesArgs{Action: "reload"})
	if code != 0 || got != "Added (1):|  + fresh — Fresh one|Removed (1):|  - zeta|Total bundles now: 1|" {
		t.Fatalf("%d %q", code, got)
	}
}
