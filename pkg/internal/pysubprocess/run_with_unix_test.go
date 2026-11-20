//go:build !windows

package pysubprocess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunWithFeedsInputInDirAndReportsStatusLikeSubprocessRun(t *testing.T) {
	dir := t.TempDir()
	script := `cat; pwd >&2; printf "$MARK"; exit 3`
	res, err := RunWith(context.Background(), []string{"sh", "-c", script}, Options{
		Dir: dir, Env: append(os.Environ(), "MARK=m"), Input: []byte("in\n"), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if res.Returncode != 3 || string(res.Stdout) != "in\nm" || string(res.Stderr) != real+"\n" {
		t.Fatalf("got %d %q %q", res.Returncode, res.Stdout, res.Stderr)
	}
	_, err = RunWith(context.Background(), []string{"sh", "-c", "echo e >&2; exit 4"}, Options{Check: true})
	var cpe *CalledProcessError
	if !errors.As(err, &cpe) || cpe.Returncode != 4 || string(cpe.Stderr) != "e\n" {
		t.Fatalf("check: %v", err)
	}
	_, err = RunWith(context.Background(), []string{"sleep", "5"}, Options{Timeout: 200 * time.Millisecond})
	var te *TimeoutExpired
	if !errors.As(err, &te) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunWith(ctx, []string{"sleep", "5"}, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := RunWith(context.Background(), []string{filepath.Join(dir, "missing")}, Options{}); err == nil {
		t.Fatal("a missing program spawns")
	}
}
