package pyctx

import (
	"context"
	"testing"
)

func TestRunningLoopMarker(t *testing.T) {
	if RunningLoop(context.Background()) {
		t.Fatal("a bare context reported a running loop")
	}
	if !RunningLoop(WithRunningLoop(context.Background())) {
		t.Fatal("a marked context did not report a running loop")
	}
}
