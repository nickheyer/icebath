//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Writes an executable shell script, making parent directories
func makeResolverExe(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPathHitComesFirstAndKnownDirsAreStillRecorded(t *testing.T) {
	tmp := t.TempDir()
	onPath := makeResolverExe(t, filepath.Join(tmp, "pathbin", "tool"))
	inKnown := makeResolverExe(t, filepath.Join(tmp, "known", "tool"))
	res := locateCommand("tool", &commandLookupContext{Path: strPtr(filepath.Dir(onPath))}, []string{filepath.Dir(inKnown)})
	if res.Kind != "path_executable" || !slices.Equal(res.Command(), []string{onPath}) {
		t.Fatalf("%+v", res)
	}
	if len(res.Candidates) != 2 || !res.Candidates[0].Present || !res.Candidates[1].Present {
		t.Fatalf("%+v", res.Candidates)
	}
	var present []string
	for _, c := range res.PresentCandidates() {
		present = append(present, c.Value)
	}
	if !slices.Equal(present, []string{onPath, inKnown}) {
		t.Fatalf("present %v", present)
	}
}

func TestKnownDirHitWhenPathMisses(t *testing.T) {
	inKnown := makeResolverExe(t, filepath.Join(t.TempDir(), "known", "tool"))
	res := locateCommand("tool", &commandLookupContext{Path: strPtr("")}, []string{filepath.Dir(inKnown)})
	if res.Kind != "known_path" || res.Source() != "known_dir:"+filepath.Dir(inKnown) || res.Candidates[0].Present {
		t.Fatalf("%+v", res)
	}
}

func TestExplicitPathBypassesSearch(t *testing.T) {
	tmp := t.TempDir()
	exe := makeResolverExe(t, filepath.Join(tmp, "bin", "tool"))
	res := locateCommand(exe, &commandLookupContext{Path: strPtr("")}, nil)
	if res.Kind != "explicit_path" || !slices.Equal(res.Command(), []string{exe}) {
		t.Fatalf("%+v", res)
	}
	missing := locateCommand(filepath.Join(tmp, "nope", "tool"), &commandLookupContext{Path: strPtr("")}, nil)
	if missing.Kind != "missing" || missing.Candidates[0].Source != "explicit" {
		t.Fatalf("%+v", missing)
	}
}

func TestLocateNeverSearchesTheWorkingDirectory(t *testing.T) {
	tmp := t.TempDir()
	makeResolverExe(t, filepath.Join(tmp, "tool"))
	t.Chdir(tmp)
	if res := locateCommand("tool", &commandLookupContext{Path: strPtr("")}, nil); res.Kind != "missing" {
		t.Fatalf("%+v", res)
	}
	relative := locateCommand("./tool", &commandLookupContext{Path: strPtr("")}, nil)
	if relative.Kind != "missing" || relative.Candidates[0].Present {
		t.Fatalf("%+v", relative)
	}
}

func TestKnownDirExpandsHomeAndEnv(t *testing.T) {
	tmp := t.TempDir()
	exe := makeResolverExe(t, filepath.Join(tmp, "home", ".local", "bin", "tool"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("TOOLROOT", filepath.Join(tmp, "home", ".local"))
	for _, spec := range []string{"~/.local/bin", "$TOOLROOT/bin"} {
		res := locateCommand("tool", &commandLookupContext{Path: strPtr("")}, []string{spec})
		if !slices.Equal(res.Command(), []string{exe}) {
			t.Fatalf("%s %+v", spec, res)
		}
	}
}
