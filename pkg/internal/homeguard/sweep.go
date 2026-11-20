package homeguard

import (
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Name prefix of the scratch roots, followed by the pid namespace and the pid of the owning test binary
const scratchPrefix = "ibt-"

// Variable naming the paths a copy of the test binary removes once the binary that started it is gone
const sweepEnv = "HOMEGUARD_SWEEP"

// Gives this test binary one scratch root for the temp files it and its children make and has it removed when the binary ends
func ownScratch() {
	base := resolve(os.TempDir())
	reapStale(base)
	root := filepath.Join(base, scratchPrefix+pidNamespace()+"-"+strconv.Itoa(os.Getpid()))
	removeTree(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return
	}
	setTemp(root)
	paths := []string{root}
	if guard.home != guard.original {
		paths = append(paths, guard.home)
	}
	sweepAtExit(paths)
}

// Points the temp dir variables of the platform and the temp dir of the testing package at root
func setTemp(root string) {
	for _, name := range []string{"TMPDIR", "TMP", "TEMP", "GOTMPDIR"} {
		os.Setenv(name, root)
	}
}

// Write end of the pipe whose closing tells the sweeper that this test binary ended, held here so that it stays open for the life of the process
var sweepSignal *os.File

// Starts a detached copy of this binary that removes paths when this process ends, a kill included
func sweepAtExit(paths []string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), sweepEnv+"="+strings.Join(paths, string(os.PathListSeparator)))
	cmd.Stdin = r
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return
	}
	r.Close()
	sweepSignal = w
}

// Waits for the binary that started this copy to end, then removes the paths it named
func sweep(paths string) {
	io.Copy(io.Discard, os.Stdin)
	for _, path := range filepath.SplitList(paths) {
		if owned(path) {
			removeTree(path)
		}
	}
}

// Whether path is a private home or a scratch root, the only directories a sweep may remove
func owned(path string) bool {
	name := filepath.Base(path)
	return filepath.IsAbs(path) && (strings.HasPrefix(name, privatePrefix) || strings.HasPrefix(name, scratchPrefix))
}

// Removes a tree, opening directories a test left without write permission
func removeTree(path string) {
	if os.RemoveAll(path) == nil {
		return
	}
	filepath.WalkDir(path, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	os.RemoveAll(path)
}
