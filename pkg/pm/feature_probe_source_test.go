//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunCapturedTimeoutCarriesStderrSoFar(t *testing.T) {
	isolateHome(t)
	code := "import sys, time; sys.stderr.write('partial'); sys.stderr.flush(); time.sleep(30)"
	_, err := runCaptured(context.Background(), []string{peHostPython(t), "-c", code}, t.TempDir(), environValue(), 2*time.Second)
	var timeout *uvTimeoutError
	if !errors.As(err, &timeout) || string(timeout.Stderr) != "partial" {
		t.Fatalf("timeout gave %v", err)
	}
}
