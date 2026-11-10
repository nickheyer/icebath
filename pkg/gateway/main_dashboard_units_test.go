package gateway

import (
	"reflect"
	"testing"
)

func TestSystemdRestartCommandsFollowTheScope(t *testing.T) {
	user, system := "systemctl --user restart icebath-serve.service", "sudo systemctl restart icebath-serve.service"
	for _, c := range []struct {
		scope any
		want  []string
	}{{"user", []string{user}}, {"system", []string{system}}, {nil, []string{user, system}}, {"", []string{user, system}}, {"other", []string{user, system}},
		{int64(3), []string{user, system}}, {[]any{}, []string{user, system}}} {
		got, err := SystemdRestartCommands("icebath-serve.service", c.scope)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("scope %v gave %v %v want %v", c.scope, got, err, c.want)
		}
	}
	if _, err := SystemdRestartCommands("u", []any{"x"}); err == nil {
		t.Error("unhashable scope accepted")
	}
	if got := FormatRestartCommands([]string{user, system}); got != "`"+user+"` or `"+system+"`" {
		t.Errorf("formatted %q", got)
	}
}
