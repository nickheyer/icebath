package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Liveness answer of the tri state a case names, alive, unknown or dead
func pinManualLiveness(alive string) {
	servePidAliveMatches = func(int64, *float64) (bool, bool) { return alive == "alive", alive != "unknown" }
}

func manualServeRuntime() *RuntimeRecord {
	pid := 900
	return &RuntimeRecord{Kind: "serve", Profile: "work", PID: &pid, Supervisor: "manual-serve", RestartVia: "respawn-argv",
		Detail: pyjson.ObjectOf("create_time", 1000.0)}
}

func manualInventory(runtimes ...*RuntimeRecord) func(context.Context) *UpdatePlan {
	return func(context.Context) *UpdatePlan {
		plan := NewUpdatePlan()
		plan.Runtimes = append(plan.Runtimes, runtimes...)
		return plan
	}
}

func latestReceiptBytes(t *testing.T, h *fleetHarness) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.home, "logs", "update_receipts", "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHistoricalManualObligationDoesNotBlockHealthyGateway(t *testing.T) {
	for _, alive := range []string{"alive", "unknown", "dead"} {
		for _, historicalFleet := range []string{"stale", "current", "empty"} {
			for _, marker := range []bool{true, false} {
				for _, gatewayPresent := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s-%s-marker=%v-gateway=%v", alive, historicalFleet, marker, gatewayPresent), func(t *testing.T) {
						h := newFleetHarness(t)
						runtimes := []any{manualServeRow("serve", 1000.0), map[string]any{"kind": "gateway", "profile": "default"}}
						fleet := []any{map[string]any{"profile": "default", "state": "stale", "code_sha": "old"}}
						switch historicalFleet {
						case "current":
							fleet = []any{map[string]any{"profile": "default", "state": "current", "code_sha": "new"}}
						case "empty":
							fleet = []any{}
						}
						if !gatewayPresent {
							runtimes, fleet = []any{manualServeRow("serve", 1000.0)}, []any{}
						}
						h.receipt(map[string]any{"outcome": "partial", "plan": map[string]any{"runtimes": runtimes}, "fleet": fleet})
						before := latestReceiptBytes(t, h)
						h.checkout("new")
						pinManualLiveness(alive)
						if gatewayPresent {
							h.fleet(map[string]any{"profile": "default", "state": "current", "code_sha": "new"})
						}
						gatewaylessInventory = manualInventory()
						if alive != "dead" {
							gatewaylessInventory = manualInventory(manualServeRuntime())
						}
						if marker {
							h.arm("new", nil)
						}
						if h.pendingNeeded() {
							t.Fatal("a historical manual obligation kept the gateway restart pending")
						}
						warning := h.startupWarning()
						if strings.Contains(warning, "serve [work] pid 900") != (alive != "dead") || strings.Contains(warning, "icebath gateway restart") {
							t.Fatalf("warning = %q", warning)
						}
						if !bytes.Equal(before, latestReceiptBytes(t, h)) {
							t.Fatal("the historical receipt changed")
						}
					})
				}
			}
		}
	}
}

func TestStampedManualOnlyHistoryHasNoGatewayObligation(t *testing.T) {
	for _, marker := range []bool{true, false} {
		t.Run(fmt.Sprintf("marker=%v", marker), func(t *testing.T) {
			h := newFleetHarness(t)
			row := manualServeRow("serve", 1000.0)
			row["code_sha"] = "old"
			h.receipt(map[string]any{"outcome": "partial", "plan": map[string]any{"runtimes": []any{row}}, "fleet": []any{}})
			pinManualLiveness("alive")
			h.checkout("new")
			runtime := manualServeRuntime()
			old := "old"
			runtime.CodeSHA = &old
			gatewaylessInventory = manualInventory(runtime)
			if marker {
				h.arm("new", nil)
			}
			if h.pendingNeeded() {
				t.Fatal("a manual only history left a gateway obligation")
			}
			warning := h.startupWarning()
			if !strings.Contains(warning, "serve [work] pid 900") || strings.Contains(warning, "icebath gateway restart") {
				t.Fatalf("warning = %q", warning)
			}
		})
	}
}

func TestHistoricalRetentionIsIndependentOfPlanOrder(t *testing.T) {
	unsupportedRows := []any{map[string]any{"kind": "serve"}, map[string]any{"kind": "gateway", "profile": "unknown"}, nil}
	for _, manualFirst := range []bool{true, false} {
		for i, unsupported := range unsupportedRows {
			t.Run(fmt.Sprintf("manual-first=%v-unsupported%d", manualFirst, i), func(t *testing.T) {
				h := newFleetHarness(t)
				ctx := updaterecord.UpdateReceiptScope(h.ctx)
				rows := []any{manualServeRow("serve", 1000.0), unsupported}
				if !manualFirst {
					rows = []any{unsupported, manualServeRow("serve", 1000.0)}
				}
				receipt := map[string]any{"outcome": "partial",
					"plan":  map[string]any{"runtimes": append([]any{map[string]any{"kind": "gateway", "profile": "default"}}, rows...)},
					"fleet": []any{map[string]any{"profile": "default", "state": "current", "code_sha": "new"}}}
				h.receipt(receipt)
				pinManualLiveness("alive")
				h.checkout("new")
				pendingManual, err := RetainReceiptManualServes(h.ctx, receipt)
				if err != nil {
					t.Fatal(err)
				}
				if owed, known, err := receiptOwedGateways(receipt, pendingManual); err != nil || known {
					t.Fatalf("owed = %v known = %v err = %v", owed, known, err)
				}
				filed, _ := filepath.Glob(filepath.Join(h.home, "serve_restart_pending", "*.json"))
				if len(filed) == 0 {
					t.Fatal("no manual serve reminder was filed")
				}
				updaterecord.BeginUpdateReceipt(ctx, nil, "")
				updaterecord.FinalizeUpdateReceipt(ctx, "success", []any{}, "")
				if warning := h.startupWarning(); !strings.Contains(warning, "serve [work] pid 900") {
					t.Fatalf("warning = %q", warning)
				}
			})
		}
	}
}

func TestUnsavedManualRestartWarningSurvivesARunningAndAKilledUpdate(t *testing.T) {
	h := newFleetHarness(t)
	h.receipt(map[string]any{"outcome": "partial", "plan": map[string]any{"runtimes": []any{manualServeRow("serve", 1000.0)}}})
	if err := os.WriteFile(filepath.Join(h.home, "serve_restart_pending"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinManualLiveness("alive")
	warned := func(stage string) {
		t.Helper()
		if warning := h.startupWarning(); !strings.Contains(warning, "serve [work] pid 900") {
			t.Fatalf("%s: warning = %q", stage, warning)
		}
	}

	updaterecord.BeginUpdateReceipt(updaterecord.UpdateReceiptScope(h.ctx), nil, "")
	var running map[string]any
	if err := json.Unmarshal(latestReceiptBytes(t, h), &running); err != nil {
		t.Fatal(err)
	}
	if _, planned := running["plan"]; running["outcome"] != "running" || planned {
		t.Fatalf("running receipt = %v", running)
	}
	warned("while the update runs")
	warned("after the update was killed")

	next := updaterecord.UpdateReceiptScope(h.ctx)
	updaterecord.BeginUpdateReceipt(next, nil, "")
	updaterecord.FinalizeUpdateReceipt(next, "success", []any{}, "")
	warned("after the next run")
}
