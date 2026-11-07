package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Environment double exposing only the backend name and the detected remote home
type fakePathEnv struct {
	Environment
	envType  string
	home     string
	detected bool
}

func (f *fakePathEnv) EnvType() string                { return f.envType }
func (f *fakePathEnv) RemoteHome() (string, bool)     { return f.home, f.detected }
func (f *fakePathEnv) SandboxHome() string            { return "" }
func (f *fakePathEnv) Cleanup(forceRemove bool) error { return nil }

func installFakePathEnv(t *testing.T, taskID string, e Environment) {
	t.Helper()
	key, err := resolveContainerTaskID(nil, taskID)
	if err != nil {
		t.Fatal(err)
	}
	termPool.Lock()
	termPool.active[key] = e
	termPool.Unlock()
	t.Cleanup(func() {
		termPool.Lock()
		delete(termPool.active, key)
		termPool.Unlock()
	})
}

func TestFilePathsPosixNormpath(t *testing.T) {
	for in, want := range map[string]string{"": ".", "/": "/", "//": "//", "///a//b": "/a/b", "a/./b/../c": "a/c", "../a/..": "..", "/../x": "/x", "a/b/../../..": "..", "//x/../y": "//y"} {
		if got := posixNormpath(in); got != want {
			t.Errorf("posixNormpath(%q) = %q want %q", in, got, want)
		}
	}
	if posixJoin("/a", "b") != "/a/b" || posixJoin("/a/", "b") != "/a/b" || posixJoin("/a", "/b") != "/b" || posixJoin("", "b") != "b" {
		t.Fatal("posixJoin")
	}
}

func TestFilePathsExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ICEBATH_HOME", "")
	t.Setenv("TERMINAL_HOME_MODE", "real")
	if got := expandTilde("~/x/../y"); got != home+"/x/../y" {
		t.Fatalf("expandTilde = %q", got)
	}
	if got := expandTilde("~"); got != home {
		t.Fatalf("expandTilde(~) = %q", got)
	}
	if got := expandTilde("~no_such_user_zz/x"); got != "~no_such_user_zz/x" {
		t.Fatalf("unknown user must stay, got %q", got)
	}
	if got := expandTilde("a~b"); got != "a~b" {
		t.Fatalf("inner tilde %q", got)
	}
}

func TestFilePathsSentinels(t *testing.T) {
	for raw, want := range map[any]string{"auto": "", ".": "", "  cwd ": "", "rel/dir": "", "/abs/dir": "/abs/dir", nil: ""} {
		if got := sentinelFreeAbsCwd(nil, raw); got != want {
			t.Errorf("sentinelFreeAbsCwd(%v) = %q want %q", raw, got, want)
		}
	}
	if !sshPathEscapesHome("~/../x") || !sshPathEscapesHome("~/..") || sshPathEscapesHome("~/a") || sshPathEscapesHome("~") || sshPathEscapesHome("/a/..") {
		t.Fatal("sshPathEscapesHome")
	}
}

func TestFilePathsHostResolution(t *testing.T) {
	t.Setenv("TERMINAL_CWD", "")
	launch := t.TempDir()
	env := &Env{Cwd: launch}
	got, err := resolvePathForTaskID(env, "paths-host-a", "a/../b.txt")
	if err != nil || got != filepath.Join(resolveNonStrict(launch), "b.txt") {
		t.Fatalf("relative to launch dir: %q %v", got, err)
	}
	root := t.TempDir()
	RecordSessionCwd(env, "paths-host-b", root)
	t.Cleanup(func() { ClearSessionCwd(env, "paths-host-b") })
	got, err = resolvePathForTaskID(env, "paths-host-b", "sub/file.go")
	if err != nil || got != filepath.Join(resolveNonStrict(root), "sub", "file.go") {
		t.Fatalf("relative to session cwd: %q %v", got, err)
	}
	wr, err := authoritativeWorkspaceRootFor(env, "paths-host-b")
	if err != nil || wr != root {
		t.Fatalf("workspace root %q %v", wr, err)
	}
	outside, _ := resolvePathForTaskID(env, "paths-host-b", "../elsewhere.go")
	warn := pathResolutionWarningFor(env, "paths-host-b", "../elsewhere.go", outside)
	if !strings.HasPrefix(warn, "Relative path '../elsewhere.go' resolved to '"+outside+"', which is OUTSIDE the active workspace ('"+resolveNonStrict(root)+"').") {
		t.Fatalf("warning %q", warn)
	}
	if pathResolutionWarningFor(env, "paths-host-b", "/abs/x", "/abs/x") != "" || pathResolutionWarningFor(env, "paths-host-b", "sub/file.go", got) != "" {
		t.Fatal("absolute or inside paths never warn")
	}
	if pathResolutionWarningFor(env, "paths-host-a", "../x", "/somewhere") != "" {
		t.Fatal("no workspace root means no warning")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "target"), link); err != nil {
		t.Fatal(err)
	}
	entry, err := resolveEntryForTaskID(env, "paths-host-b", "link")
	if err != nil || entry != filepath.Join(resolveNonStrict(root), "link") {
		t.Fatalf("entry keeps the final symlink: %q %v", entry, err)
	}
}

func TestFilePathsContainerResolution(t *testing.T) {
	installFakePathEnv(t, "paths-docker", &fakePathEnv{envType: "docker"})
	if !usesContainerPaths(nil, "paths-docker") || containerBackendForTask(nil, "paths-docker") != "docker" {
		t.Fatal("docker task must use container paths")
	}
	RecordSessionCwd(nil, "paths-docker", "/workspace/app")
	t.Cleanup(func() { ClearSessionCwd(nil, "paths-docker") })
	got, err := resolvePathForTaskID(nil, "paths-docker", "src/../main.go")
	if err != nil || got != "/workspace/app/main.go" {
		t.Fatalf("container path %q %v", got, err)
	}
	got, err = resolveEntryForTaskID(nil, "paths-docker", "dir/name")
	if err != nil || got != "/workspace/app/dir/name" {
		t.Fatalf("container entry %q %v", got, err)
	}
	if w := pathResolutionWarningFor(nil, "paths-docker", "../../etc/x", "/etc/x"); !strings.Contains(w, "OUTSIDE the active workspace ('/workspace/app')") {
		t.Fatalf("container warning %q", w)
	}
}

func TestFilePathsSSHResolution(t *testing.T) {
	t.Setenv("TERMINAL_CWD", "")
	installFakePathEnv(t, "paths-ssh", &fakePathEnv{envType: "ssh", home: "/home/remote", detected: true})
	RecordSessionCwd(nil, "paths-ssh", "~/proj")
	t.Cleanup(func() { ClearSessionCwd(nil, "paths-ssh") })
	got, err := resolvePathForTaskID(nil, "paths-ssh", "src/../a.go")
	if err != nil || got != "/home/remote/proj/a.go" {
		t.Fatalf("ssh path %q %v", got, err)
	}
	base, err := resolveBaseDirFor(nil, "paths-ssh", nil)
	if err != nil || base != "/home/remote/proj" {
		t.Fatalf("ssh base %q %v", base, err)
	}
	if w := pathResolutionWarningFor(nil, "paths-ssh", "../../out", "/home/out"); !strings.Contains(w, "('/home/remote/proj')") {
		t.Fatalf("ssh warning %q", w)
	}
	if pathResolutionWarningFor(nil, "paths-ssh", "~/x", "/home/remote/x") != "" {
		t.Fatal("tilde paths never warn on ssh")
	}
	installFakePathEnv(t, "paths-ssh-guess", &fakePathEnv{envType: "ssh", home: "/home/guess", detected: false})
	RecordSessionCwd(nil, "paths-ssh-guess", "~/w")
	t.Cleanup(func() { ClearSessionCwd(nil, "paths-ssh-guess") })
	got, err = resolvePathForTaskID(nil, "paths-ssh-guess", "a/./b/../c")
	if err != nil || got != "~/w/a/c" {
		t.Fatalf("guessed home keeps tilde: %q %v", got, err)
	}
	got, err = resolveSSHPath(nil, "paths-ssh-guess", "~other/x/../y")
	if err != nil || got != "~other/y" {
		t.Fatalf("tilde user path %q %v", got, err)
	}
}

func TestFilePathsResolutionFailureKinds(t *testing.T) {
	_, err := os.Stat(filepath.Join(t.TempDir(), "missing"))
	if !isPathResolutionFailure(err) {
		t.Fatal("filesystem errors are resolution failures")
	}
	if isPathResolutionFailure(&TerminalPolicyUnavailable{Reason: "x"}) {
		t.Fatal("policy refusals must propagate")
	}
}

// Raw task id overrides surface their cwd and an unregistered task has none
func TestRegisteredTaskCwdOverrideKeysByRawTaskID(t *testing.T) {
	env, _ := newFileToolTestEnv(t)
	workspace := t.TempDir()
	if err := RegisterTaskEnvOverrides(env, "acp-session-7", map[string]any{"cwd": workspace}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ClearTaskEnvOverrides(env, "acp-session-7") })
	if got := RegisteredTaskCwdOverride(env, "acp-session-7"); got != workspace {
		t.Fatalf("override = %q, want %q", got, workspace)
	}
	if got := RegisteredTaskCwdOverride(env, "never-registered"); got != "" {
		t.Fatalf("unregistered override = %q", got)
	}
}
