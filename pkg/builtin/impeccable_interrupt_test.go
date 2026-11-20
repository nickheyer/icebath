//go:build !windows

package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Runs the real sh launcher against a download that stalls mid body and signals its process group
func TestImpeccableLauncherRemovesPartialDownloadWhenInterrupted(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tree, err := Materialize()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("#!/bin/sh\necho verified-engine\n")
	sum := sha256.Sum256(payload)
	osName, arch := "linux", "x64"
	if runtime.GOOS == "darwin" {
		osName = "darwin"
	}
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	asset := "impeccable-" + osName + "-" + arch
	for _, c := range []struct {
		sig    syscall.Signal
		status int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		t.Run(c.sig.String(), func(t *testing.T) {
			root := t.TempDir()
			scripts, home, cache, tools := filepath.Join(root, "skill scripts"), filepath.Join(root, "home"), filepath.Join(root, "cache"), filepath.Join(root, "tools")
			for _, d := range []string{scripts, home, tools} {
				os.MkdirAll(d, 0o755)
			}
			os.WriteFile(filepath.Join(scripts, "VERSION"), []byte("0.0.0-test\n"), 0o644)
			launcher := filepath.Join(scripts, "impeccable")
			os.WriteFile(launcher, must(os.ReadFile(tree.Payload("impeccable/skill/scripts/impeccable"))), 0o755)
			os.WriteFile(filepath.Join(scripts, "engine-checksums.txt"), []byte("# test manifest\n\n0.0.0-test "+asset+" "+hex.EncodeToString(sum[:])+"\n"), 0o644)
			var requests atomic.Int32
			stalled, done := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(200)
				w.Write(payload[:8])
				w.(http.Flusher).Flush()
				close(stalled)
				select {
				case <-r.Context().Done():
				case <-done:
				}
			}))
			defer server.Close()
			defer close(done)
			cmd := exec.Command("/bin/sh", launcher)
			cmd.Dir = root
			cmd.Env = []string{"PATH=" + tools + ":/usr/bin:/bin", "HOME=" + home, "USERPROFILE=" + home, "TEMP=" + root, "TMP=" + root,
				"IMPECCABLE_HOME=" + cache, "IMPECCABLE_DOWNLOAD_BASE=" + server.URL}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-stalled:
			case <-time.After(20 * time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatal("launcher never reached the download")
			}
			syscall.Kill(-cmd.Process.Pid, c.sig)
			err := cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signaled() || exit.ExitCode() != c.status {
				t.Fatalf("launcher exit %v want status %d", err, c.status)
			}
			if requests.Load() != 1 {
				t.Fatalf("%d requests", requests.Load())
			}
			cacheDir := filepath.Join(cache, "bin", "0.0.0-test")
			if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) != 0 {
				t.Fatal(fmt.Sprint("no staging file left behind ", entries))
			}
		})
	}
}
