package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

func TestNativeLaunchdAmbiguousDomainsCannotStart(t *testing.T) {
	var calls [][]string
	swapSeam(t, &serviceProbeRun, func(ctx context.Context, argv []string, o executil.Options) (*executil.Completed, error) {
		if argv[0] != "launchctl" {
			t.Fatal(argv)
		}
		calls = append(calls, argv)
		return executil.Run(ctx, []string{"/bin/sh", "-c", "echo 'state = not running'"}, o)
	})
	h, _ := filepath.EvalSymlinks(t.TempDir())
	_, err := DiscoverExistingGatewayService(context.Background(), h, timeutil.Monotonic()+5)
	var start *RuntimeStartError
	if !errors.As(err, &start) || start.Reason != "service_scope_conflict" {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][1] != "print" || calls[1][1] != "print" {
		t.Fatal(calls)
	}
}
