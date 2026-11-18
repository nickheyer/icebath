package selfupdate

import (
	"strings"
	"testing"
)

// A receipt of an earlier release naming a supervised dashboard leaves the gateways that serve the pulled code owing nothing
func TestSupervisedServeRowNeverVetoesGatewayDischarge(t *testing.T) {
	for _, supervisor := range []string{"desktop", "launchd", "systemd", "manual-serve"} {
		t.Run(supervisor, func(t *testing.T) {
			h := newFleetHarness(t)
			h.receipt(map[string]any{
				"outcome":         "failed",
				"post_update":     map[string]any{"sha": "pulled"},
				"gateway_restart": map[string]any{"restarted_services": []any{"icebath-gateway"}, "incomplete": false},
				"plan": map[string]any{"runtimes": []any{
					map[string]any{"kind": "gateway", "profile": "default", "code_sha": "old", "supervisor": "manual"},
					map[string]any{"kind": "dashboard", "profile": "default", "code_sha": nil, "supervisor": supervisor, "restart_via": "respawn-argv"},
				}},
			})
			h.checkout("new")
			h.fleet(map[string]any{"profile": "default", "state": "current", "code_sha": "new"})
			owes, err := updateOwesFleetRestart(h.ctx, nil)
			if err != nil || owes {
				t.Fatalf("owes = %v err = %v", owes, err)
			}
			if h.pendingNeeded() {
				t.Fatal("a supervised dashboard row kept the gateway restart pending")
			}
		})
	}
}

// An armed inventory holding a supervised serve row settles once every gateway serves the pulled code
func TestStartupWarnDischargedWhenInventoryHoldsSupervisedServe(t *testing.T) {
	for _, supervisor := range []string{"desktop", "launchd"} {
		t.Run(supervisor, func(t *testing.T) {
			h := newFleetHarness(t)
			disk := strings.Repeat("e", 40)
			h.arm(disk, []any{
				map[string]any{"kind": "gateway", "profile": "default", "pid": 42, "supervisor": "systemd"},
				map[string]any{"kind": "serve", "profile": "default", "pid": 6161, "supervisor": supervisor, "detail": map[string]any{"create_time": 1000.0}},
			})
			h.checkout(disk)
			h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": disk, "code_version": "0.21.0", "state": "current"})
			if warning := h.startupWarning(); warning != "" {
				t.Fatalf("warning = %q", warning)
			}
			if h.armed() {
				t.Fatal("the obligation stayed armed")
			}
		})
	}
}

// A serve row no supervisor owns stays evidence against settling the armed inventory
func TestStartupWarnKeptWhenInventoryHoldsUnclassifiedServe(t *testing.T) {
	h := newFleetHarness(t)
	disk := strings.Repeat("e", 40)
	h.arm(disk, []any{
		map[string]any{"kind": "gateway", "profile": "default", "pid": 42, "supervisor": "systemd"},
		map[string]any{"kind": "serve", "profile": "default", "pid": 6161, "supervisor": "manual"},
	})
	h.checkout(disk)
	h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": disk, "code_version": "0.21.0", "state": "current"})
	if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") {
		t.Fatalf("warning = %q", warning)
	}
	if !h.armed() {
		t.Fatal("an unclassified serve row let the obligation settle")
	}
}
