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
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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
	}{{"script", false, true}, {"setup-app", false, true}, {"", false, true}, {"script", true, false}, {"homebrew", false, false}, {"docker", false, false}} {
		got, err := releaseBinaryInstall(context.Background(), releaseTree(t, tc.method, tc.git))
		if err != nil || got != tc.want {
			t.Fatalf("%+v got %v %v", tc, got, err)
		}
	}
	if got, err := releaseBinaryInstall(context.Background(), t.TempDir()); err != nil || got {
		t.Fatalf("a tree without the binary is not a release install %v %v", got, err)
	}
}

func TestUpdateViaReleaseHandsTheSwappedReleaseToTheCompletion(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root := releaseTree(t, "script", false)
	savedRoot, savedUpdate, savedComplete := fleetProjectRoot, releaseBinaryUpdate, zipCompleteSourceUpdate
	t.Cleanup(func() {
		fleetProjectRoot, releaseBinaryUpdate, zipCompleteSourceUpdate = savedRoot, savedUpdate, savedComplete
	})
	fleetProjectRoot = func() string { return root }
	var calls []string
	result, failure := install.BinaryUpdate{Tag: "v1.2.0", Commit: strings.Repeat("b", 40), Changed: true}, error(nil)
	releaseBinaryUpdate = func(_ context.Context, dir, repoURL, commit, branch string, _ io.Writer) (install.BinaryUpdate, error) {
		calls = append(calls, strings.Join([]string{dir, repoURL, commit, branch}, " "))
		return result, failure
	}
	var completed []map[string]any
	zipCompleteSourceUpdate = func(_ context.Context, _ io.Writer, request map[string]any) error {
		completed = append(completed, request)
		return nil
	}
	pinned := strings.Repeat("a", 40)
	request := map[string]any{}
	if err := updateViaRelease(context.Background(), &bytes.Buffer{}, applyTarget{Ref: pinned, ReleaseSHA: pinned}, request); err != nil {
		t.Fatal(err)
	}
	if calls[0] != root+" https://github.com/"+OfficialRepository+" "+pinned+" " {
		t.Fatalf("stable call %q", calls[0])
	}
	if request["apply_mode"] != "release" || request["expected_sha"] != result.Commit || request["completion_message"] != nil || len(completed) != 1 {
		t.Fatalf("request %v", request)
	}
	result.Changed = false
	request = map[string]any{}
	if err := updateViaRelease(context.Background(), &bytes.Buffer{}, applyTarget{Ref: "origin/main", Repository: "fork/icebath"}, request); err != nil {
		t.Fatal(err)
	}
	if calls[1] != root+" https://github.com/fork/icebath  main" || request["completion_message"] != "✓ Already up to date!" {
		t.Fatalf("branch call %q request %v", calls[1], request)
	}
	failure = errors.New("no published release of x follows branch main")
	var out bytes.Buffer
	err := updateViaRelease(context.Background(), &out, applyTarget{Ref: "origin/main"}, map[string]any{})
	var exit *pm.SystemExit
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(out.String(), "✗ Release update failed: no published release") || len(completed) != 2 {
		t.Fatalf("failure %v %q", err, out.String())
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

func TestCheckReleaseUpdateReportsTheSourceCheckVerdict(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := releaseSourceCheck
	t.Cleanup(func() { releaseSourceCheck = saved })
	var status *pyjson.Object
	var seen SourceCheckOptions
	releaseSourceCheck = func(_ context.Context, opts SourceCheckOptions) (*pyjson.Object, error) {
		seen = opts
		return status, nil
	}
	for _, tc := range []struct {
		status *pyjson.Object
		want   string
		code   int
	}{
		{pyjson.ObjectOf("behind", int64(0), "branch", "main"), "✓ Already up to date.\n", 0},
		{pyjson.ObjectOf("behind", int64(3), "channel", "stable"), "⚕ Update available: 3 commits behind stable.\n  Run 'icebath update' to install.\n", 0},
		{pyjson.ObjectOf("behind", int64(UpdateAvailableNoCount), "branch", "main"), "⚕ Update available (behind main).\n  Run 'icebath update' to install.\n", 0},
		{pyjson.ObjectOf("behind", nil, "reason", "network"), "✗ Could not check for updates: network\n", 1},
	} {
		status = tc.status
		var out bytes.Buffer
		err := checkReleaseUpdate(context.Background(), &out, "main", false, nil)
		var exit *pm.SystemExit
		if (tc.code == 0 && err != nil) || (tc.code != 0 && (!errors.As(err, &exit) || exit.Code != tc.code)) || out.String() != tc.want {
			t.Fatalf("status %v got %q %v", tc.status, out.String(), err)
		}
		if !seen.Force || seen.Branch != nil {
			t.Fatalf("options %+v", seen)
		}
	}
	status = pyjson.ObjectOf("behind", int64(0))
	if err := checkReleaseUpdate(context.Background(), &bytes.Buffer{}, "feature", true, nil); err != nil || seen.Branch == nil || *seen.Branch != "feature" {
		t.Fatalf("explicit branch %+v %v", seen, err)
	}
}
