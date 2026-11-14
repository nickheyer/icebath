package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestCanonicalSourceNeverPersistsHttpCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:token@example.com/owner/repo.git":   "https://example.com/owner/repo.git",
		"https://example.com/owner/repo.git?token=secret": "https://example.com/owner/repo.git",
	} {
		if got, err := canonicalSource(raw, ""); err != nil || got != want {
			t.Fatalf("%s = %q %v", raw, got, err)
		}
	}
}

func TestClonedOriginNeverPersistsHttpCredentials(t *testing.T) {
	c6RequireGit(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "https://user:secret@example.com/owner/repo.git?token=secret"
	c6Git(t, repo, "init", "-q")
	c6Git(t, repo, "remote", "add", "origin", source)
	if err := scrubClonedOrigin(context.Background(), repo, "git", source); err != nil {
		t.Fatal(err)
	}
	if got := c6Git(t, repo, "remote", "get-url", "origin"); got != "https://example.com/owner/repo.git" {
		t.Fatalf("origin %q", got)
	}
	if cfg := gitPullRead(t, filepath.Join(repo, ".git", "config")); strings.Contains(cfg, "secret") {
		t.Fatalf("config keeps the credential %q", cfg)
	}
}

func TestGitErrorsNeverEchoSourceCredentials(t *testing.T) {
	source := "https://user:secret@example.com/owner/repo.git?token=secret"
	result := &gitResult{Code: 1, Stderr: "fatal: unable to access '" + source + "': connection failed"}
	text, err := safeGitError(result, source)
	if err != nil || strings.Contains(text, "secret") || strings.Contains(text, "user:") || !strings.Contains(text, "https://example.com/owner/repo.git") {
		t.Fatalf("error %q %v", text, err)
	}
}

func TestMetadataIsProfileLocalAndReadFromDiskEachTime(t *testing.T) {
	_, homeA := pubWorld(t)
	if err := os.Remove(filepath.Join(homeA, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	source := "file://" + filepath.ToSlash(repo)
	ctxA := home.WithOverride(context.Background(), homeA)
	if _, _, _, err := installPluginCore(ctxA, source, installCoreOptions{Ref: old}); err != nil {
		t.Fatal(err)
	}
	homeB := filepath.Join(filepath.Dir(homeA), "profile-b")
	read := func(h string) map[string]any {
		t.Setenv("ICEBATH_HOME", h)
		metadata, err := readInstallMetadata(home.Dir(home.WithOverride(context.Background(), h)))
		if err != nil {
			t.Fatal(err)
		}
		return metadata
	}
	if demo, _ := read(homeA)["demo"].(map[string]any); demo["revision"] != old {
		t.Fatalf("profile a revision %v", demo)
	}
	if metadata := read(homeB); len(metadata) != 0 {
		t.Fatalf("profile b %v", metadata)
	}
	if demo, _ := read(homeA)["demo"].(map[string]any); demo["source"] != source {
		t.Fatalf("profile a source %v", demo)
	}
}

// Clone of the plugin repo whose HEAD answers newSHA the way a patched head revision does
func checkoutMismatchClone(t *testing.T, repo, newSHA string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	c6Git(t, filepath.Dir(clone), "clone", "-q", "file://"+filepath.ToSlash(repo), clone)
	real := runPluginGit
	t.Cleanup(func() { runPluginGit = real })
	runPluginGit = func(ctx context.Context, gitExe, target string, timeout time.Duration, authURL string, args ...string) (*gitResult, error) {
		if len(args) == 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
			return &gitResult{Stdout: newSHA + "\n"}, nil
		}
		return real(ctx, gitExe, target, timeout, authURL, args...)
	}
	return clone
}

func TestCheckoutMismatchIsRejected(t *testing.T) {
	repo, old, head := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	h := c6Home(t, nil)
	clone := checkoutMismatchClone(t, repo, head)
	err := checkoutExactRevision(home.WithOverride(context.Background(), h), h, clone, "git", old, "")
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "does not match requested") {
		t.Fatalf("mismatch %v", err)
	}
}

func TestCheckoutThatLandsOnAnotherCommitIsStillRejected(t *testing.T) {
	repo, old, head := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	c6Git(t, repo, "tag", "-a", "v1.0.2", old, "-m", "v1.0.2")
	tagObject := c6Git(t, repo, "rev-parse", "v1.0.2")
	h := c6Home(t, nil)
	clone := checkoutMismatchClone(t, repo, head)
	err := checkoutExactRevision(home.WithOverride(context.Background(), h), h, clone, "git", tagObject, "")
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "does not match requested") {
		t.Fatalf("mismatch %v", err)
	}
}
