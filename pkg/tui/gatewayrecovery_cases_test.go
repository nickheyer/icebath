package tui

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPlanGatewayRecovery(t *testing.T) {
	t.Run("recovers the live session and records the attempt", func(t *testing.T) {
		if got := planGatewayRecovery("sess-1", "", nil, 1000); !reflect.DeepEqual(got, recoveryPlan{Attempts: []float64{1000}, Recover: true, Sid: "sess-1"}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("does not recover when there is no session to resume", func(t *testing.T) {
		if got := planGatewayRecovery("", "", nil, 1000); !reflect.DeepEqual(got, recoveryPlan{Attempts: []float64{}}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("keeps retrying the recovery target through a startup crash-loop, bounded by the budget", func(t *testing.T) {
		plan := planGatewayRecovery("sess-1", "", nil, 0)
		if !plan.Recover || plan.Sid != "sess-1" {
			t.Fatalf("first %#v", plan)
		}
		attempts := plan.Attempts
		for i := 1; i < gatewayRecoveryLimit; i++ {
			plan = planGatewayRecovery("", "sess-1", attempts, float64(i))
			if !plan.Recover || plan.Sid != "sess-1" {
				t.Fatalf("attempt %d %#v", i, plan)
			}
			attempts = plan.Attempts
		}
		plan = planGatewayRecovery("", "sess-1", attempts, gatewayRecoveryLimit)
		if plan.Recover || plan.Sid != "sess-1" {
			t.Fatalf("over budget %#v", plan)
		}
	})
	t.Run("prunes attempts older than the window so recovery re-arms", func(t *testing.T) {
		plan := planGatewayRecovery("sess-1", "", []float64{0, 1, 2}, gatewayRecoveryWindowMs+100)
		if !reflect.DeepEqual(plan.Attempts, []float64{gatewayRecoveryWindowMs + 100}) || !plan.Recover {
			t.Fatalf("plan %#v", plan)
		}
	})
}

func TestReportGatewayStartTimeout(t *testing.T) {
	t.Run("shows a terminal bootstrap verdict and its remedy, never \"still waiting\"", func(t *testing.T) {
		c := newTurnController(newTUIStore())
		var statuses []string
		c.reportGatewayStartTimeout("Command failed: python3 gateway_bootstrap.py --start\n"+
			"gateway unavailable: bootstrap_dependencies\n"+
			"dependency environment unavailable: no dependency environment is committed for this install\n"+
			"gateway unavailable: run `icebath pm repair`, then restart the TUI\n", func(s string) { statuses = append(statuses, s) })
		var messages []string
		for _, a := range c.store.turn.Activity {
			messages = append(messages, a.Text)
		}
		if !slices.Contains(messages, "gateway unavailable: run `icebath pm repair`, then restart the TUI") {
			t.Fatalf("messages %q", messages)
		}
		if slices.ContainsFunc(messages, func(m string) bool { return strings.Contains(m, "Still waiting") }) ||
			slices.ContainsFunc(statuses, func(s string) bool { return strings.Contains(s, "still starting") }) {
			t.Fatalf("messages %q statuses %q", messages, statuses)
		}
	})
}
