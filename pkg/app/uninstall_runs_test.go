package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/spf13/cobra"
)

func TestDryRunPrintsPlanWithoutMutating(t *testing.T) {
	tmp := t.TempDir()
	projectRoot := filepath.Join(tmp, "icebath-agent")
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755)
	uninstallWrite(t, filepath.Join(h, "config.yaml"), "model: {}\n")
	t.Setenv("ICEBATH_HOME", h)
	swap(t, &uninstallProjectRoot, func() string { return projectRoot })
	swap(t, &uninstallIsDefaultHome, func(string) bool { return false })
	swap(t, &uninstallDiscoverProfiles, func(context.Context, io.Writer) []manage.ProfileInfo { return nil })
	called := false
	swap(t, &uninstallPerform, func(context.Context, io.Writer, string, string, bool, bool, []manage.ProfileInfo) { called = true })
	var out bytes.Buffer
	if code, err := RunUninstall(context.Background(), map[string]any{"dry_run": true, "yes": true, "full": true}, &out); code != 0 || err != nil {
		t.Fatalf("exit %d %v", code, err)
	}
	text := out.String()
	if called || !strings.Contains(text, "Dry run") || !strings.Contains(text, projectRoot) || !strings.Contains(text, h) {
		t.Fatalf("called %v output %q", called, text)
	}
	if !uninstallExists(projectRoot) || !uninstallExists(h) {
		t.Fatal("dry run removed files")
	}
}

func TestDryRunListsNamedProfiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	swap(t, &uninstallIsDefaultHome, func(string) bool { return true })
	swap(t, &uninstallDiscoverProfiles, func(context.Context, io.Writer) []manage.ProfileInfo {
		return []manage.ProfileInfo{{Name: "work", Path: filepath.Join(tmp, "profiles", "work")}}
	})
	var out bytes.Buffer
	printUninstallDryRun(context.Background(), &out, tmp, filepath.Join(tmp, ".icebath"), true)
	if !strings.Contains(out.String(), "Named profiles") || !strings.Contains(out.String(), "work") {
		t.Fatalf("output %q", out.String())
	}
}

func TestBuildUninstallParserAcceptsDryRun(t *testing.T) {
	c := commands.NewCLI()
	var ns map[string]any
	c.Bind("uninstall", func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	})
	if err := c.Execute([]string{"uninstall", "--dry-run", "--full"}); err != nil {
		t.Fatal(err)
	}
	if ns["dry_run"] != true || ns["full"] != true {
		t.Fatalf("namespace %v", ns)
	}
}

// Fake checkout root, a git checkout or a sealed tree stamped with its steward
func uninstallFakeProjectRoot(t *testing.T, git bool, distribution string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath-agent")
	os.MkdirAll(root, 0o755)
	if git {
		os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	}
	if distribution != "" {
		uninstallWrite(t, filepath.Join(root, "install-stamp.json"), fmt.Sprintf(`{"distribution": %q, "updateMechanism": "external"}`, distribution))
	}
	swap(t, &uninstallProjectRoot, func() string { return root })
	return root
}

func TestGitCheckoutAllowsCodeRemoval(t *testing.T) {
	uninstallFakeProjectRoot(t, true, "")
	if _, refused := codeRemovalRefusal(); refused {
		t.Fatal("git checkout refused")
	}
}

func TestSealedNixTreeRefusesCodeRemoval(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "nix")
	refusal, refused := codeRemovalRefusal()
	if !refused || !strings.Contains(refusal, "Nix") {
		t.Fatalf("refusal %q", refusal)
	}
}

func TestStamplessSealedTreeStillRefuses(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "")
	if _, refused := codeRemovalRefusal(); !refused {
		t.Fatal("stampless tree allowed")
	}
}

func TestRunUninstallExitsOnSealedTree(t *testing.T) {
	uninstallFakeProjectRoot(t, false, "nix")
	var out bytes.Buffer
	code, err := RunUninstall(context.Background(), map[string]any{"yes": true, "full": true, "dry_run": false}, &out)
	if err != nil || code != 1 || !strings.Contains(out.String(), "Nix") {
		t.Fatalf("exit %d %v %q", code, err, out.String())
	}
}
