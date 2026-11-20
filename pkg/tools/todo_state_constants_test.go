package tools

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestTodoConstantsAreTheStateValues(t *testing.T) {
	for _, c := range []struct{ got, want any }{
		{MaxTodoResultChars, state.MaxTodoResultChars},
		{MaxTodoResultChars, 512_000},
		{TodoInjectionHeader, state.TodoInjectionHeader},
		{TodoInjectionHeader, "[Your active task list was preserved across context compression]"},
	} {
		if c.got != c.want {
			t.Fatalf("%v != %v", c.got, c.want)
		}
	}
}
