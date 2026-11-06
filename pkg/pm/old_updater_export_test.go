package pm

import (
	"context"
	"errors"
	"testing"
)

func TestStopForRelaunchReturnsTheMemoizedTakeoverStatus(t *testing.T) {
	takeoverMemo.Lock()
	takeoverMemo.set, takeoverMemo.code = true, 7
	takeoverMemo.Unlock()
	t.Cleanup(func() {
		takeoverMemo.Lock()
		takeoverMemo.set, takeoverMemo.code = false, 0
		takeoverMemo.Unlock()
	})
	err := StopForRelaunch(context.Background())
	var exit *SystemExit
	if !errors.As(err, &exit) || exit.Code != 7 {
		t.Fatalf("StopForRelaunch returned %v, want SystemExit 7", err)
	}
}
