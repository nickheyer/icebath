//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const relaunchExeSuffix = ""

func TestResolveHermesBinResolvesRelativeArgv0(t *testing.T) {
	dir := t.TempDir()
	fake := writeExecutable(t, filepath.Join(dir, "icebath"), "#!/bin/sh\n")
	setRelaunchArgv(t, "icebath")
	t.Chdir(dir)
	t.Setenv("PATH", t.TempDir())
	want, err := filepath.EvalSymlinks(fake)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resolveHermesBin()
	if resolved, _ := filepath.EvalSymlinks(got); !ok || resolved != want {
		t.Fatalf("resolved %q %v, want %q", got, ok, want)
	}
}

func TestRelaunchCallsExecvp(t *testing.T) {
	bin := resolvedRelaunchBin(t)
	type call struct {
		path string
		argv []string
	}
	var calls []call
	saved := relaunchExecvp
	relaunchExecvp = func(path string, argv []string) error {
		calls = append(calls, call{path, argv})
		return nil
	}
	t.Cleanup(func() { relaunchExecvp = saved })
	relaunch([]string{"--resume", "abc"}, true)
	if len(calls) != 1 || calls[0].path != bin || !slices.Equal(calls[0].argv, []string{bin, "--resume", "abc"}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestResolveHermesBinWindowsPyGuardPosixPythonLauncherFallsThroughToPath(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, filepath.Join(dir, "icebath"), "#!/usr/bin/env python3\n")
	wrapper := writeExecutable(t, filepath.Join(dir, "bin", "icebath"), "#!/usr/bin/env bash\n")
	setRelaunchArgv(t, script, "chat")
	t.Setenv("PATH", filepath.Dir(wrapper))
	if got, ok := resolveHermesBin(); !ok || got != wrapper {
		t.Fatalf("resolved %q %v, want %q", got, ok, wrapper)
	}
}

func TestResolveHermesBinWindowsPyGuardPosixPythonLauncherOnPathFallsBackToCurrentPython(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, filepath.Join(dir, "icebath"), "#!/usr/bin/env python3\n")
	setRelaunchArgv(t, script, "chat")
	t.Setenv("PATH", dir)
	if got, ok := resolveHermesBin(); ok {
		t.Fatalf("a python launcher on PATH resolved to %q", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(filepath.Dir(exe), "python")
	pinned := writeExecutable(t, filepath.Join(dir, "pinned", "icebath"), "#!"+interpreter+"\n")
	setRelaunchArgv(t, pinned, "chat")
	if got, ok := resolveHermesBin(); !ok || got != pinned {
		t.Fatalf("pinned launcher resolved to %q %v", got, ok)
	}
}
