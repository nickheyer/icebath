package gateway

import (
	"errors"
	"testing"
)

func TestRuntimeCheckLeavesOtherFailuresUncoded(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	answer := ReadinessFailure(errors.New("No usable credentials found for openrouter."))
	if len(answer.Keys) != 2 || answer.GetOr("ok", nil) != false || answer.GetOr("error", nil) != "No usable credentials found for openrouter." {
		t.Fatalf("answer %v", answer.Vals)
	}
}
