package app

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

func TestTheOpenedSessionViewFollowsTheCLISessionAndUnbindsWithoutAConnection(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Cleanup(func() { bindGatewayView(nil, nil) })
	c := &CLI{sessionID: "first"}
	connection := &gatewaycli.GatewayConnection{}
	view := bindGatewaySession(c, connection)
	if view == nil || currentGatewayView() != view || view.connection != connection || !view.dirty {
		t.Fatalf("bound view = %+v", view)
	}
	if got := view.sessionID(); got != "first" {
		t.Fatalf("session = %q", got)
	}
	c.uiMu.Lock()
	c.sessionID = "second"
	c.uiMu.Unlock()
	if got := view.sessionID(); got != "second" {
		t.Fatalf("session after /new = %q", got)
	}
	if bindGatewaySession(c, nil) != nil || currentGatewayView() != nil {
		t.Fatal("a session with no connection kept a view")
	}
}
