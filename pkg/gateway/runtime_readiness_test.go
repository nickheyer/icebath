package gateway

import (
	"errors"
	"testing"
)

func TestRuntimeCheckLeavesOtherFailuresUncoded(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	answer := ReadinessFailure(errors.New("No usable credentials found for openrouter."))
	if answer.Len() != 2 || answer.ValueOr("ok", nil) != false || answer.ValueOr("error", nil) != "No usable credentials found for openrouter." {
		t.Fatalf("answer %v", answer)
	}
}
