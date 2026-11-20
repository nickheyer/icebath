//go:build !windows

package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A compose verify refuses on live containers, a failed probe and a probe whose pipe outlives the deadline
func TestComposeProbeThroughRunVerify(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$FAKE_DOCKER\" in\n" +
		"running) printf 'proj-db-1\\n  \\nproj-web-1\\n' ;;\n" +
		"failed) echo first >&2; echo 'permission denied on docker.sock' >&2; exit 3 ;;\n" +
		"held) sleep 20 & exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	recipe := &Recipe{Name: "c", Kind: "compose", Build: []string{"docker compose build"}}
	for mode, want := range map[string]string{
		"running": "Refusing to run: this compose project already has running container(s): proj-db-1, proj-web-1. ",
		"failed":  "Refusing to run: docker compose ps failed (exit 3): permission denied on docker.sock. ",
		"held":    "Refusing to run: docker compose ps timed out after 15s; live containers cannot be ruled out. ",
	} {
		t.Setenv("FAKE_DOCKER", mode)
		started := time.Now()
		res, err := RunVerify(context.Background(), t.TempDir(), recipe, NewVerifyOptions())
		if err != nil {
			t.Fatalf("%s %v", mode, err)
		}
		if len(res.Phases) != 1 || res.Phases[0].Command != "docker compose build" || !strings.HasPrefix(res.Phases[0].OutputTail, want) {
			t.Fatalf("%s phases %#v", mode, res.Phases)
		}
		if took := time.Since(started); mode == "held" && (took < 14*time.Second || took > 19*time.Second) {
			t.Fatalf("held pipe probe returned after %s", took)
		}
	}
	t.Setenv("FAKE_DOCKER", "none")
	res, err := RunVerify(context.Background(), t.TempDir(), &Recipe{Name: "c", Kind: "compose", Build: []string{"true"}}, func() VerifyOptions {
		o := NewVerifyOptions()
		o.Phases = []string{"build"}
		return o
	}())
	if err != nil || len(res.Phases) != 1 || !res.OK() {
		t.Fatalf("idle compose project did not build %#v %v", res, err)
	}
}
