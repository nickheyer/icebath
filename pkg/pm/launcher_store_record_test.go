package pm

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestLauncherStoreRecordRoundTripsThroughEachForm(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "owner's home", "tools")
	binary := filepath.Join(dir, "check out", installBinaryName())
	shell := filepath.Join(dir, "icebath-acp")
	if writeShell(shell, []string{binary, "acp"}, store) == "" {
		t.Fatal("shell launcher not written")
	}
	if got := mustRead(t, shell); got != "#!/bin/sh\n# icebath-store '"+filepath.Join(dir, "owner'\"'\"'s home", "tools")+"'\nexec '"+binary+"' acp \"$@\"\n" {
		t.Fatalf("shell body %q", got)
	}
	if launcherStore(shell) != store || launcherBinary(shell) != binary {
		t.Fatalf("shell reads store %q binary %q", launcherStore(shell), launcherBinary(shell))
	}
	cmd := filepath.Join(dir, "icebath.cmd")
	if writeCmd(cmd, binary, nil, store) == "" {
		t.Fatal("cmd launcher not written")
	}
	if got := mustRead(t, cmd); got != "@echo off\r\n@rem icebath-store \""+store+"\"\r\n\""+binary+"\" %*\r\n" {
		t.Fatalf("cmd body %q", got)
	}
	if launcherStore(cmd) != store || launcherBinary(cmd) != binary {
		t.Fatalf("cmd reads store %q binary %q", launcherStore(cmd), launcherBinary(cmd))
	}
	bare, bareCmd := filepath.Join(dir, "bare"), filepath.Join(dir, "bare.cmd")
	writeShell(bare, []string{binary}, "")
	writeCmd(bareCmd, binary, nil, "")
	if strings.Contains(mustRead(t, bare), storeRecordPrefix) || strings.Contains(mustRead(t, bareCmd), storeRecordPrefix) {
		t.Fatal("a launcher without a store wrote a record")
	}
	if launcherStore(bare) != "" || launcherStore(bareCmd) != "" || launcherBinary(bareCmd) != binary {
		t.Fatalf("recordless launchers read %q %q", launcherStore(bare), launcherStore(bareCmd))
	}
	if launcherStore(filepath.Join(dir, "missing")) != "" {
		t.Fatal("a missing launcher read a record")
	}
}

func TestLauncherBoundRootReadsOnlyALiveRecord(t *testing.T) {
	tmp := t.TempDir()
	root := sttMkdir(t, filepath.Join(tmp, "checkout"))
	owner := sttMkdir(t, filepath.Join(tmp, "owner"))
	local := sttMkdir(t, checkoutLauncherDir(root))
	binary := filepath.Join(owner, "tools", "icebath-entry", installBinaryName())
	writeFile(t, binary, []byte("binary"), 0o755)
	if mintLauncher("icebath", root, local, binary, "") == "" {
		t.Fatal("no launcher minted")
	}
	if got := launcherBoundRoot(root, []string{owner}); got != "" {
		t.Fatalf("a launcher without a record bound %q", got)
	}
	if mintLauncher("icebath", root, local, binary, filepath.Join(owner, "tools")) == "" {
		t.Fatal("no launcher minted")
	}
	if got := launcherBoundRoot(root, []string{owner}); got != owner {
		t.Fatalf("a live record bound %q", got)
	}
	if err := os.RemoveAll(filepath.Join(owner, "tools")); err != nil {
		t.Fatal(err)
	}
	if got := launcherBoundRoot(root, []string{owner}); got != "" {
		t.Fatalf("a dead record bound %q", got)
	}
}

func TestBorrowerPublicationLeavesTheOwnersRecord(t *testing.T) {
	tmp := resolvePath(isolateHome(t))
	t.Setenv(EnvRuntimeDir, "")
	os.Unsetenv(EnvRuntimeDir)
	root := fakeTestCheckout(t, filepath.Join(tmp, "checkout"))
	ownerHome, borrowerHome := filepath.Join(tmp, "owner"), filepath.Join(tmp, "borrower")
	sttMkdir(t, filepath.Join(ownerHome, "tools"))
	sttMkdir(t, filepath.Join(borrowerHome, "tools"))
	ctx := context.Background()
	t.Setenv(home.EnvHome, ownerHome)
	written, err := EnsureInstallLaunchers(ctx, root, checkoutLauncherDir(root))
	if err != nil || len(written) != len(windowsBinLaunchers) {
		t.Fatalf("owner published %v %v", written, err)
	}
	before := map[string]string{}
	for _, path := range written {
		if launcherStore(path) != filepath.Join(ownerHome, "tools") {
			t.Fatalf("%s records %q", path, launcherStore(path))
		}
		before[path] = mustRead(t, path)
	}
	t.Setenv(home.EnvHome, borrowerHome)
	again, err := EnsureInstallLaunchers(ctx, root, checkoutLauncherDir(root))
	if err != nil || !reflect.DeepEqual(again, written) {
		t.Fatalf("borrower published %v %v", again, err)
	}
	for path, body := range before {
		if mustRead(t, path) != body {
			t.Fatalf("the borrower rewrote %s", path)
		}
	}
	if err := os.RemoveAll(filepath.Join(ownerHome, "tools")); err != nil {
		t.Fatal(err)
	}
	healed, err := EnsureInstallLaunchers(ctx, root, checkoutLauncherDir(root))
	if err != nil || len(healed) != len(windowsBinLaunchers) {
		t.Fatalf("healing publication %v %v", healed, err)
	}
	for _, path := range healed {
		if launcherStore(path) != filepath.Join(borrowerHome, "tools") {
			t.Fatalf("%s still records %q after the owner's store died", path, launcherStore(path))
		}
	}
}

// A git checkout with a self update stamp and its own install binary, its root resolved
func fakeTestCheckout(t *testing.T, path string) string {
	t.Helper()
	sttMkdir(t, filepath.Join(path, ".git"))
	writeFile(t, filepath.Join(path, "install-stamp.json"), []byte(`{"updateMechanism": "self"}`), 0o644)
	writeFile(t, filepath.Join(path, installBinaryName()), []byte("fake install binary\n"), 0o755)
	return resolvePath(path)
}
