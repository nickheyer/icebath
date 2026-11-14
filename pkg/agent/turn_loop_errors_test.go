package agent

import (
	"errors"
	"testing"
)

func panicsInLoopTest() {
	var rows []int
	_ = rows[3]
}

func TestRecoveredOuterLoopErrorNamesTheUnwoundModules(t *testing.T) {
	var got *OuterLoopError
	func() {
		defer func() { got = RecoveredOuterLoopError(recover()) }()
		panicsInLoopTest()
	}()
	if got == nil || !got.Modules["turn_loop_errors_test"] {
		t.Fatalf("modules: %v", got)
	}
	var target *OuterLoopError
	if !errors.As(error(got), &target) || got.Error() != "runtime error: index out of range [3] with length 0" {
		t.Fatalf("error: %q", got.Error())
	}
	plain := RecoveredOuterLoopError("boom")
	if plain.Error() != "boom" || plain.Stack == "" {
		t.Fatalf("non error panic value: %q", plain.Error())
	}
}
