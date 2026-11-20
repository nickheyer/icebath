package mcp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestReissueRefusesUnknownMixedLiveAndSettledRows(t *testing.T) {
	isolateHome(t)
	a, b := tools.NewConnTarget("notion", "mcp", "install"), tools.NewConnTarget("linear", "mcp", "install")
	c := tools.NewConnTarget("blender", "plugin", "install")
	op := tools.NewConnectionOperation([]*tools.ConnTarget{a, b, c}, "s1", "", nil)
	ctx := context.Background()
	for _, c := range []struct {
		names []string
		want  string
	}{{[]string{"nope"}, tools.ConnUnknownTarget}, {[]string{"linear", "blender"}, tools.ConnMixedKinds},
		{[]string{"notion"}, tools.ConnLinkStillValid}} {
		if got, err := Reissue(ctx, op, c.names); err != nil || got != c.want {
			t.Fatalf("%v: %q %v", c.names, got, err)
		}
	}
	if _, err := op.Transition("blender", tools.TargetFailed, tools.ActorBackendWatcher, tools.TransitionFields{}); err != nil {
		t.Fatal(err)
	}
	op.Settle(tools.SettleContinue)
	if got, _ := Reissue(ctx, op, []string{"blender"}); got != tools.ConnSettled {
		t.Fatalf("settled %q", got)
	}
}
