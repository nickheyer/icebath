package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestNativeLaunchdAmbiguousDomainsCannotStart(t *testing.T) {
	var calls [][]string
	swapSeam(t, &serviceProbeRun, func(ctx context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		if argv[0] != "launchctl" {
			t.Fatal(argv)
		}
		calls = append(calls, argv)
		return pysubprocess.RunWith(ctx, []string{"/bin/sh", "-c", "echo 'state = not running'"}, o)
	})
	h, _ := filepath.EvalSymlinks(t.TempDir())
	_, err := DiscoverExistingGatewayService(context.Background(), h, pythread.Monotonic()+5)
	var start *RuntimeStartError
	if !errors.As(err, &start) || start.Reason != "service_scope_conflict" {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][1] != "print" || calls[1][1] != "print" {
		t.Fatal(calls)
	}
}
