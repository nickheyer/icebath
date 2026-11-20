//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Tree holding an icebath binary stamped with method, a git dir added when git is set
func releaseTree(t *testing.T, method string, git bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(install.InstallBinaryPath(root), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if method != "" {
		if err := install.WriteInstallMethod(root, method); err != nil {
			t.Fatal(err)
		}
	}
	if git {
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReleaseBinaryInstallFollowsTheStamp(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, tc := range []struct {
		method string
		git    bool
		want   bool
	}{{"script", false, true}, {"setup-app", false, true}, {"", false, false}, {"script", true, false}, {"homebrew", false, false}, {"docker", false, false}} {
		got, err := releaseBinaryInstall(context.Background(), releaseTree(t, tc.method, tc.git))
		if err != nil || got != tc.want {
			t.Fatalf("%+v got %v %v", tc, got, err)
		}
	}
	if got, err := releaseBinaryInstall(context.Background(), t.TempDir()); err != nil || got {
		t.Fatalf("a tree without the binary is not a release install %v %v", got, err)
	}
}

func TestRebuildSourceCheckoutRunsOnlyInAnIcebathCheckout(t *testing.T) {
	saved := checkoutBinaryBuild
	t.Cleanup(func() { checkoutBinaryBuild = saved })
	var built []string
	failure := error(nil)
	checkoutBinaryBuild = func(_ context.Context, _ io.Writer, root string) error {
		built = append(built, root)
		return failure
	}
	plain := t.TempDir()
	if err := rebuildSourceCheckout(context.Background(), &bytes.Buffer{}, plain); err != nil || len(built) != 0 {
		t.Fatalf("a binary tree is not rebuilt %v %v", err, built)
	}
	checkout := t.TempDir()
	os.MkdirAll(filepath.Join(checkout, "cmd", "icebath"), 0o755)
	os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/nickheyer/icebath\n\ngo 1.27\n"), 0o644)
	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "cmd", "icebath"), 0o755)
	os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/other\n"), 0o644)
	if err := rebuildSourceCheckout(context.Background(), &bytes.Buffer{}, other); err != nil || len(built) != 0 {
		t.Fatalf("another module is not rebuilt %v %v", err, built)
	}
	if err := rebuildSourceCheckout(context.Background(), &bytes.Buffer{}, checkout); err != nil || len(built) != 1 || built[0] != checkout {
		t.Fatalf("checkout rebuild %v %v", err, built)
	}
	failure = errors.New("go build ./cmd/icebath: exit status 1")
	var out bytes.Buffer
	err := rebuildSourceCheckout(context.Background(), &out, checkout)
	var exit *pm.SystemExit
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(out.String(), "✗ Rebuilding icebath failed (the code update itself succeeded)") {
		t.Fatalf("failure %v %q", err, out.String())
	}
}

func TestReleaseRevisionFeedsTheSourceCheckOnlyForReleaseInstalls(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := releaseCheckoutSHA
	t.Cleanup(func() { releaseCheckoutSHA = saved })
	sha := strings.Repeat("c", 40)
	releaseCheckoutSHA = func() (*string, error) { return &sha, nil }
	if got := releaseRevision(context.Background(), releaseTree(t, "script", false)); got != sha {
		t.Fatalf("script revision %q", got)
	}
	for _, tree := range []string{releaseTree(t, "homebrew", false), releaseTree(t, "script", true), t.TempDir()} {
		if got := releaseRevision(context.Background(), tree); got != "" {
			t.Fatalf("%s revision %q", tree, got)
		}
	}
	short := "abc"
	releaseCheckoutSHA = func() (*string, error) { return &short, nil }
	if got := releaseRevision(context.Background(), releaseTree(t, "script", false)); got != "" {
		t.Fatalf("partial revision %q", got)
	}
}
