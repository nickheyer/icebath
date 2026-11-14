package selfupdate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Runs git with a fixed identity in a fixture repository and returns its stripped stdout
func ucGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
	cmd.Dir = cwd
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func TestStableUpdateFetchBringsTheReleaseTag(t *testing.T) {
	tmp := t.TempDir()
	origin, checkout := filepath.Join(tmp, "origin"), filepath.Join(tmp, "checkout")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	ucGit(t, origin, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(origin, "f"), []byte("1"), 0o644)
	ucGit(t, origin, "add", "f")
	ucGit(t, origin, "commit", "-qm", "one")
	ucGit(t, origin, "tag", "v0.22.2")
	ucGit(t, tmp, "clone", "-q", origin, checkout)
	os.WriteFile(filepath.Join(origin, "f"), []byte("2"), 0o644)
	ucGit(t, origin, "commit", "-qam", "two")
	ucGit(t, origin, "tag", "v0.22.3")
	release := ucGit(t, origin, "rev-parse", "HEAD")
	ucPatchRoot(t, checkout)
	savedChannel, savedResolve, savedConfig := checkRunChannel, checkResolveSourceTarget, checkReadableConfig
	t.Cleanup(func() {
		checkRunChannel, checkResolveSourceTarget, checkReadableConfig = savedChannel, savedResolve, savedConfig
	})
	checkRunChannel = func(context.Context, map[string]any) (string, error) { return "stable", nil }
	checkResolveSourceTarget = func(context.Context, string, []string, string, string, bool) (SourceTarget, error) {
		return SourceTarget{RequestedChannel: "stable", Channel: "stable", Repository: "NousResearch/hermes-agent", Commit: release, Version: "0.22.3"}, nil
	}
	checkReadableConfig = func(context.Context, string) (map[string]any, error) { return map[string]any{}, nil }
	request := map[string]any{"home": tmp}
	var out bytes.Buffer
	ctx := context.Background()
	target, err := selectApplyTarget(ctx, &out, map[string]any{"branch": nil}, "main", request, []string{"git"}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ucGit(t, checkout, "fetch", "--no-tags", "origin", target.Ref)
	if err := fetchReleaseTag(ctx, &out, []string{"git"}, checkout, request); err != nil {
		t.Fatal(err)
	}
	ucGit(t, checkout, "merge", "-q", "--ff-only", release)
	updaterecord.ResetVersionInfoCache()
	info := updaterecord.GitVersionInfo(checkout, true)
	if info.Commit == nil || *info.Commit != release || info.BaseVersion != "0.22.3" || info.DisplayVersion() != "0.22.3" {
		t.Fatalf("version info = %+v", info)
	}
}

func TestAnUnfetchableReleaseTagIsReportedNeverFatal(t *testing.T) {
	missing, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, gitCmd := range [][]string{{"git"}, {missing + "-missing-git"}} {
		t.Run(filepath.Base(gitCmd[0]), func(t *testing.T) {
			tmp := t.TempDir()
			ucGit(t, tmp, "init", "-q", "-b", "main")
			var out bytes.Buffer
			if err := fetchReleaseTag(context.Background(), &out, gitCmd, tmp, map[string]any{"release_tag": "v9.9.9"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Could not fetch the v9.9.9 tag") {
				t.Fatalf("output = %q", out.String())
			}
		})
	}
}
