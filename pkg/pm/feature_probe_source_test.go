package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestFeatureProbeSourceSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte(featureProbeSource))
	if got := hex.EncodeToString(sum[:]); got != "7fd550c3c986d0402369e2e7f95b969d9dcd829f38b2acfc02ef1d3fdb8effa7" {
		t.Fatalf("sha256 %s differs from the hermes installed_extras probe", got)
	}
}

func TestFeatureProbeDetailFollowsSubprocessErrors(t *testing.T) {
	argv := []string{"python", "-c", "probe"}
	called := fmt.Errorf("Command '%s' returned non-zero exit status 1.", pyrepr.Repr(argv))
	if got := featureProbeDetail(uvResult{Code: 1, Stderr: "Traceback\nRuntimeError: no site\n"}, called); got != "Traceback\nRuntimeError: no site" {
		t.Fatalf("stderr detail %q", got)
	}
	if got := featureProbeDetail(uvResult{Code: 1, Stderr: " \n"}, called); got != "" {
		t.Fatalf("whitespace stderr detail %q", got)
	}
	if got := featureProbeDetail(uvResult{Code: 1}, called); got != "Command '['python', '-c', 'probe']' returned non-zero exit status 1." {
		t.Fatalf("empty stderr detail %q", got)
	}
	silent := &uvTimeoutError{Argv: argv, Timeout: 60 * time.Second}
	if got := featureProbeDetail(uvResult{}, silent); got != "Command '['python', '-c', 'probe']' timed out after 60 seconds" {
		t.Fatalf("silent timeout detail %q", got)
	}
	want := "b'partial'"
	if runtime.GOOS == "windows" {
		want = "partial"
	}
	if got := featureProbeDetail(uvResult{}, &uvTimeoutError{Argv: argv, Timeout: 60 * time.Second, Stderr: []byte(" partial\n")}); got != want {
		t.Fatalf("timeout with stderr detail %q, want %q", got, want)
	}
}

func TestRunCapturedTimeoutCarriesStderrSoFar(t *testing.T) {
	isolateHome(t)
	code := "import sys, time; sys.stderr.write('partial'); sys.stderr.flush(); time.sleep(30)"
	_, err := runCaptured(context.Background(), []string{peHostPython(t), "-c", code}, t.TempDir(), pyEnviron(), 2*time.Second)
	var timeout *uvTimeoutError
	if !errors.As(err, &timeout) || string(timeout.Stderr) != "partial" {
		t.Fatalf("timeout gave %v", err)
	}
}
