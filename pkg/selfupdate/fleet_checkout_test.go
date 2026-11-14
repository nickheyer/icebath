package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Reports the given code identity for the test
func pinFleetIdentity(t *testing.T, sha, source string) {
	t.Helper()
	saved := fleetCodeIdentity
	t.Cleanup(func() { fleetCodeIdentity = saved })
	fleetCodeIdentity = func(bool) (*pyjson.Object, error) {
		identity := pyjson.NewObject()
		identity.Set("sha", sha)
		identity.Set("short_sha", sha[:8])
		identity.Set("source", source)
		return identity, nil
	}
}

// Routes every custody git through fn for the test
func routeCustodyGit(t *testing.T, fn func(argv []string, opts ChildOptions) (*CompletedProcess, error)) {
	t.Helper()
	saved := runInCustody
	t.Cleanup(func() { runInCustody = saved })
	runInCustody = func(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*CompletedProcess, error) {
		return fn(argv, opts)
	}
}

func mustContain(t *testing.T, sha string) bool {
	t.Helper()
	contained, err := CheckoutContains(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	return contained
}

func TestBuildStampedImageContainsExactlyItsStamp(t *testing.T) {
	stamped := "b936546561aa0d2e6d0f7c3d1a9c5e8f2b4d6a70"
	pinFleetIdentity(t, stamped, "build-file")
	routeCustodyGit(t, func([]string, ChildOptions) (*CompletedProcess, error) {
		t.Fatal("git probe on a build-stamped image")
		return nil, nil
	})
	if !mustContain(t, stamped) || !mustContain(t, stamped[:12]) {
		t.Fatal("stamp or its short form not contained")
	}
	if mustContain(t, "0000000000aa0d2e6d0f7c3d1a9c5e8f2b4d6a70") {
		t.Fatal("foreign sha contained in a stamped image")
	}
}

func TestGitCheckoutStillWalksAncestry(t *testing.T) {
	pinFleetIdentity(t, strings.Repeat("deadbeef", 5), "git")
	var calls [][]string
	routeCustodyGit(t, func(argv []string, opts ChildOptions) (*CompletedProcess, error) {
		calls = append(calls, argv)
		return &CompletedProcess{Args: argv, ReturnCode: 0}, nil
	})
	wanted := strings.Repeat("cafebabe", 5)
	if !mustContain(t, wanted) {
		t.Fatal("ancestor not contained")
	}
	if len(calls) == 0 || calls[0][0] != "git" || updaterecord.GitSubcommand(calls[0][1:]) != "merge-base" {
		t.Fatalf("calls %v", calls)
	}
	if tail := calls[0][len(calls[0])-3:]; !reflect.DeepEqual(tail, []string{"--is-ancestor", wanted, "HEAD"}) {
		t.Fatalf("argv tail %v", tail)
	}
}

// Runs git with a fixed identity in cwd and returns its stripped stdout
func fleetGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestPartialCloneNeverDownloadsHistoryForAShaItLacks(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	fleetGit(t, origin, "init", "-q", "-b", "main")
	fleetGit(t, origin, "config", "uploadpack.allowFilter", "true")
	fleetGit(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	fleetGit(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	clone := filepath.Join(tmp, "clone")
	fleetGit(t, tmp, "clone", "-q", "--filter=blob:none", "file://"+origin, clone)
	fleetGit(t, origin, "commit", "-q", "--allow-empty", "-m", "newer")
	unseen := fleetGit(t, origin, "rev-parse", "HEAD")
	packs, _ := filepath.Glob(filepath.Join(clone, ".git", "objects", "pack", "*.pack"))
	pinFleetIdentity(t, fleetGit(t, clone, "rev-parse", "HEAD"), "git")
	saved := fleetProjectRoot
	t.Cleanup(func() { fleetProjectRoot = saved })
	fleetProjectRoot = func() string { return clone }
	if mustContain(t, unseen) {
		t.Fatal("a sha the clone lacks reported contained")
	}
	after, _ := filepath.Glob(filepath.Join(clone, ".git", "objects", "pack", "*.pack"))
	if !reflect.DeepEqual(after, packs) {
		t.Fatalf("packs grew from %v to %v", packs, after)
	}
}
