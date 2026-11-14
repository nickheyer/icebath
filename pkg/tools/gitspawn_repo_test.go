package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Runs git under a config free environment failing the test on error
func cleanGit(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// Repo arming fsmonitor, hooks and attribute scoped diff drivers that each leave marker.<sink> when fired
func makeMaliciousRepo(t *testing.T, tmp string) (string, string) {
	t.Helper()
	repo := filepath.Join(tmp, "poc")
	cleanGit(t, "init", "-q", repo)
	writeFileT(t, filepath.Join(repo, "README"), "hi\n")
	ident := []string{"-c", "user.email=a@b", "-c", "user.name=a"}
	cleanGit(t, append(append([]string{"-C", repo}, ident...), "add", ".")...)
	cleanGit(t, append(append([]string{"-C", repo}, ident...), "commit", "-qm", "init")...)
	marker := filepath.Join(tmp, "MARKER")
	hooks := filepath.Join(repo, "evil-hooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	markerShell := filepath.ToSlash(marker)
	for _, name := range []string{"post-checkout", "reference-transaction"} {
		hook := filepath.Join(hooks, name)
		writeFileT(t, hook, "#!/bin/sh\ntouch '"+markerShell+".hook'\n")
		if err := os.Chmod(hook, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, kv := range [][2]string{
		{"core.fsmonitor", "touch '" + markerShell + ".fsmonitor'"},
		{"core.hooksPath", filepath.ToSlash(hooks)},
		{"diff.evil.command", "touch '" + markerShell + ".extdiff'"},
		{"diff.evil.textconv", "touch '" + markerShell + ".textconv'; cat"},
	} {
		cleanGit(t, "-C", repo, "config", kv[0], kv[1])
	}
	writeFileT(t, filepath.Join(repo, ".gitattributes"), "* diff=evil\n")
	writeFileT(t, filepath.Join(repo, "README"), "changed\n")
	return repo, marker
}

// Sinks that fired since the last call, each marker removed once reported
func firedSinks(marker string) []string {
	var out []string
	for _, sink := range []string{"fsmonitor", "hook", "extdiff", "textconv", "ssh"} {
		p := marker + "." + sink
		if _, err := os.Stat(p); err == nil {
			out = append(out, sink)
			os.Remove(p)
		}
	}
	return out
}

func writeFileT(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
