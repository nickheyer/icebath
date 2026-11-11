//go:build unix

package selfupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

func TestDisarmNeverWritesThroughAPlantedRestoreAlias(t *testing.T) {
	root := uoFresh(t)
	host := uoHost(t, "ORIGINAL")
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	ugWrite(t, sentinel, "PRECIOUS")
	uoArm(t, root, strings.Repeat("a", 40))
	name := filepath.Base(host)
	for _, alias := range []string{filepath.Join(filepath.Dir(host), name+".restore"),
		filepath.Join(filepath.Dir(host), fmt.Sprintf(".%s.%d.restore", name, os.Getpid()))} {
		if err := os.Symlink(sentinel, alias); err != nil {
			t.Fatal(err)
		}
	}

	uoDisarm(t)

	if got := ugRead(t, sentinel); got != "PRECIOUS" {
		t.Fatalf("sentinel = %q", got)
	}
	if isSymlink(host) || ugRead(t, host) != "ORIGINAL" {
		t.Fatalf("host record = %q, symlink %v", ugRead(t, host), isSymlink(host))
	}
}

func TestDisarmRestoresThroughAnUnpredictableTempAndLeavesEverySibling(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "hardlink", "none"} {
		t.Run(kind, func(t *testing.T) {
			root := uoFresh(t)
			tail := CompletionPendingPath(context.Background(), root)
			ugWrite(t, tail, "OLD-TAIL")
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			ugWrite(t, sentinel, "PRECIOUS")
			uoArm(t, root, strings.Repeat("a", 40))
			planted := filepath.Join(filepath.Dir(tail), fmt.Sprintf(".%s.%d.restore", filepath.Base(tail), os.Getpid()))
			switch kind {
			case "regular":
				ugWrite(t, planted, "USER-FILE")
			case "symlink":
				if err := os.Symlink(sentinel, planted); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(sentinel, planted); err != nil {
					t.Fatal(err)
				}
			}

			uoDisarm(t)

			if isSymlink(tail) || ugRead(t, tail) != "OLD-TAIL" {
				t.Fatalf("tail = %q, symlink %v", ugRead(t, tail), isSymlink(tail))
			}
			if got := ugRead(t, sentinel); got != "PRECIOUS" {
				t.Fatalf("sentinel = %q", got)
			}
			switch kind {
			case "regular":
				if got := ugRead(t, planted); got != "USER-FILE" {
					t.Fatalf("planted = %q", got)
				}
			case "symlink", "hardlink":
				if !lexists(planted) || ugRead(t, planted) != "PRECIOUS" {
					t.Fatal("the planted alias was touched")
				}
			}
			entries, err := os.ReadDir(filepath.Dir(tail))
			if err != nil {
				t.Fatal(err)
			}
			var restores []string
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".restore") {
					restores = append(restores, entry.Name())
				}
			}
			var want []string
			if kind != "none" {
				want = []string{filepath.Base(planted)}
			}
			if !slices.Equal(restores, want) {
				t.Fatalf("restore siblings = %v, want %v", restores, want)
			}
		})
	}
}

func TestAPythonBumpIsJudgedByAnInterpreterTheTargetAdmits(t *testing.T) {
	for _, verdict := range []string{"null", `["icebath_constants.py", "SyntaxError: judged by the target"]`} {
		t.Run(verdict, func(t *testing.T) {
			tmp := t.TempDir()
			targetPython := filepath.Join(tmp, "python3.99")
			if err := os.WriteFile(targetPython, []byte("#!/bin/sh\ncat > /dev/null\necho '"+verdict+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			uoPatchAdmittedPython(t, targetPython)
			repo := filepath.Join(tmp, "repo")
			if err := os.Mkdir(repo, 0o755); err != nil {
				t.Fatal(err)
			}
			newer, _ := uoPythonBumpRepo(t, repo, "def newer_syntax(:\n")
			judged, err := targetSyntaxError(context.Background(), &bytes.Buffer{}, []string{"git"}, repo, newer, []string{"icebath_constants.py"})
			if err != nil {
				t.Fatal(err)
			}
			if verdict == "null" {
				if judged != nil {
					t.Fatalf("judged = %+v, want admitted", judged)
				}
				return
			}
			want := syntaxFault{Path: "icebath_constants.py", Error: "SyntaxError: judged by the target (judged by " + targetPython + ")"}
			if judged == nil || *judged != want {
				t.Fatalf("judged = %+v, want %+v", judged, want)
			}
		})
	}
}

func TestTheTreeMoveMarkerRecordsTheAbsoluteGitTheRepairReruns(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(tmp, "store", "git", "cmd", "git")
	if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'git version 2.99.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	ctx := context.Background()
	marker, err := armTreeMove(ctx, []string{"git", "-c", "gc.autoDetach=false"}, tmp,
		treeMove{Pre: strings.Repeat("a", 40), Target: strings.Repeat("b", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if want, err := pm.InterruptedPullMarkerPath(tmp); err != nil || marker != want {
		t.Fatalf("marker = %q, want %q (%v)", marker, want, err)
	}
	fields, err := readMarkerFields(marker)
	if err != nil {
		t.Fatal(err)
	}
	if fields["git"] != fake {
		t.Fatalf("git = %q, want %q", fields["git"], fake)
	}
	t.Setenv("PATH", "")
	if got := gitExecutable(ctx, fields["git"]); got != fake {
		t.Fatalf("the repair resolved %q, want the recorded %q", got, fake)
	}
	gone := filepath.Join(tmp, "gone")
	if got := gitExecutable(ctx, gone); got == gone {
		t.Fatal("the repair trusted a recorded git that is not there")
	}
}
