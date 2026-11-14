package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const scopedDeadPID = 1<<22 - 7

func scopedManual() map[string]any {
	return map[string]any{"kind": "serve", "profile": "work", "pid": 900, "supervisor": "manual-serve", "restart_via": "respawn-argv",
		"code_sha": "old", "detail": map[string]any{"create_time": 1000.0}}
}

func scopedCurrent(overrides ...any) map[string]any {
	row := map[string]any{"profile": "alpha", "state": "current", "code_sha": "new"}
	for i := 0; i+1 < len(overrides); i += 2 {
		row[overrides[i].(string)] = overrides[i+1]
	}
	return row
}

func scopedGateway(profile string) map[string]any {
	return map[string]any{"kind": "gateway", "profile": profile, "code_sha": "old"}
}

// Writes the runtime status of a gateway whose pid is gone, an empty state omitting the field
func writeDeadGatewayState(t *testing.T, dir, state string, stated bool) {
	t.Helper()
	record := map[string]any{"pid": scopedDeadPID, "code_sha": "old"}
	if stated {
		record["gateway_state"] = state
	}
	raw, _ := json.Marshal(record)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gateway_state.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Seeds the latest receipt, the checkout at new, the live fleet, a dead gateway state and the marker when given
func scopedSeed(t *testing.T, old map[string]any, marker *string, live *[]map[string]any) (*fleetHarness, string) {
	t.Helper()
	h := newFleetHarness(t)
	h.receipt(old)
	h.checkout("new")
	fleetCollectVersions = func() []*pyjson.Object {
		rows := make([]*pyjson.Object, len(*live))
		for i, row := range *live {
			rows[i] = fleetObject()
			for _, key := range []string{"profile", "pid", "state", "code_sha"} {
				if value, present := row[key]; present {
					rows[i].Set(key, value)
				}
			}
		}
		return rows
	}
	writeDeadGatewayState(t, h.home, "running", true)
	if marker != nil {
		h.arm(*marker, nil)
	}
	return h, filepath.Join(h.home, "logs", "update_receipts", "latest.json")
}

func hostObligationExists() bool {
	_, err := os.Stat(updaterecord.HostObligationPath())
	return err == nil
}

func TestScopedReconciliationMatrix(t *testing.T) {
	marker := func(sha string) *string { return &sha }
	failed := func(runtimes ...any) map[string]any {
		return map[string]any{"outcome": "failed", "plan": map[string]any{"runtimes": runtimes}}
	}
	stale := scopedCurrent("state", "stale", "code_sha", "old")
	cases := []struct {
		name    string
		old     map[string]any
		marker  *string
		live    []map[string]any
		pending bool
	}{
		{"receipt-successor", failed(scopedGateway("alpha")), nil, []map[string]any{scopedCurrent()}, false},
		{"marker-external-restart", map[string]any{}, marker("new"), []map[string]any{scopedCurrent()}, false},
		{"missing-sibling", failed(scopedGateway("alpha"), scopedGateway("beta")), marker("new"), []map[string]any{scopedCurrent()}, true},
		{"stale-successor", failed(scopedGateway("alpha")), nil, []map[string]any{stale}, true},
		{"unknown-successor", failed(scopedGateway("alpha")), nil, []map[string]any{scopedCurrent("state", "unknown")}, true},
		{"marker-no-sha", map[string]any{}, marker(""), []map[string]any{scopedCurrent()}, false},
		{"marker-no-sha-stale", map[string]any{}, marker(""), []map[string]any{stale}, true},
		{"checkout-moved", map[string]any{}, marker("old"), []map[string]any{scopedCurrent()}, true},
		{"marker-empty-no-receipt", map[string]any{}, marker("new"), []map[string]any{}, true},
		{"markerless-stamped-manual", map[string]any{"outcome": "partial", "plan": map[string]any{"runtimes": []any{scopedManual()}}}, nil, []map[string]any{}, false},
		{"old-manual-new-marker", map[string]any{"outcome": "success", "post_update": map[string]any{"sha": "old"},
			"plan": map[string]any{"runtimes": []any{scopedManual()}}, "fleet": []any{}}, marker("new"), []map[string]any{}, true},
		{"same-sha-not-ownership", map[string]any{"outcome": "success", "post_update": map[string]any{"sha": "new"},
			"plan": map[string]any{"runtimes": []any{scopedManual()}}, "fleet": []any{}}, marker("new"), []map[string]any{}, true},
		{"mixed-receipt-successors", map[string]any{"outcome": "partial", "plan": map[string]any{"runtimes": []any{scopedGateway("alpha"), scopedManual()}},
			"fleet": []any{stale}}, nil, []map[string]any{scopedCurrent()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := append([]map[string]any{}, tc.live...)
			h, target := scopedSeed(t, tc.old, tc.marker, &live)
			switch tc.name {
			case "missing-sibling":
				h.arm(*tc.marker, []any{scopedGateway("alpha"), scopedGateway("beta")})
			case "marker-external-restart", "checkout-moved":
				h.arm(*tc.marker, []any{scopedGateway("alpha")})
			}
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			unchanged := func() {
				t.Helper()
				if after, err := os.ReadFile(target); err != nil || !bytes.Equal(before, after) {
					t.Fatalf("the receipt changed: %v", err)
				}
			}
			if h.pendingNeeded() != tc.pending {
				t.Fatalf("pending = %v, want %v", !tc.pending, tc.pending)
			}
			if warned := strings.Contains(h.startupWarning(), "icebath gateway restart"); warned != tc.pending {
				t.Fatalf("warned = %v, want %v", warned, tc.pending)
			}
			unchanged()
			if hostObligationExists() != (tc.marker != nil && tc.pending) {
				t.Fatalf("host obligation exists = %v", hostObligationExists())
			}
			if tc.name == "missing-sibling" {
				live = append(live, scopedCurrent("profile", "beta"))
				if h.pendingNeeded() || hostObligationExists() {
					t.Fatalf("the sibling's successor left the obligation, exists = %v", hostObligationExists())
				}
				unchanged()
			}
		})
	}
}

func TestGatewaylessHostSettlesOnHostEvidence(t *testing.T) {
	record := func(kind, profile string, pid int, supervisor, restartVia string) *RuntimeRecord {
		return &RuntimeRecord{Kind: kind, Profile: profile, PID: &pid, Supervisor: supervisor, RestartVia: restartVia}
	}
	desktop := record("serve", "default", 901, "desktop", "desktop-respawn")
	desktopReceipt := map[string]any{"outcome": "failed", "plan": map[string]any{"runtimes": []any{
		map[string]any{"kind": "serve", "profile": "default", "supervisor": "desktop"}}}}
	type gatewayState struct {
		profile, state string
		stated         bool
	}
	cases := []struct {
		name     string
		receipt  map[string]any
		runtimes []*RuntimeRecord
		states   []gatewayState
		checkout string
		pending  bool
	}{
		{"desktop-only", map[string]any{}, []*RuntimeRecord{desktop}, nil, "new", false},
		{"nothing-running", map[string]any{}, nil, nil, "new", false},
		{"gateway-stopped-cleanly", map[string]any{}, []*RuntimeRecord{desktop}, []gatewayState{{"default", "stopped", true}}, "new", false},
		{"gateway-startup-failed", map[string]any{}, nil, []gatewayState{{"default", "startup_failed", true}}, "new", false},
		{"carried-local-commit", map[string]any{}, []*RuntimeRecord{desktop}, nil, "hotfix", false},
		{"manual-receipt-gatewayless-host", map[string]any{"outcome": "success", "plan": map[string]any{"runtimes": []any{scopedManual()}}}, nil, nil, "new", false},
		{"desktop-receipt-stopped-gateway", desktopReceipt, []*RuntimeRecord{desktop}, []gatewayState{{"default", "running", true}}, "new", true},
		{"named-profile-gateway-gone", map[string]any{}, []*RuntimeRecord{desktop}, []gatewayState{{"work", "running", true}}, "new", true},
		{"state-record-without-state", map[string]any{}, nil, []gatewayState{{"default", "", false}}, "new", true},
		{"unclassified-serve", map[string]any{}, []*RuntimeRecord{record("serve", "default", 902, "manual", "")}, nil, "new", true},
		{"manual-serve-without-identity", map[string]any{}, []*RuntimeRecord{record("serve", "work", 903, "manual-serve", "respawn-argv")}, nil, "new", true},
		{"gateway-runtime-without-fleet-row", map[string]any{}, []*RuntimeRecord{record("gateway", "default", 904, "manual", "")}, nil, "new", true},
		{"checkout-diverged", map[string]any{}, []*RuntimeRecord{desktop}, nil, "elsewhere", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := []map[string]any{}
			sha := "new"
			h, _ := scopedSeed(t, tc.receipt, &sha, &live)
			if err := os.Remove(filepath.Join(h.home, "gateway_state.json")); err != nil {
				t.Fatal(err)
			}
			for _, state := range tc.states {
				dir := profiles.DefaultHome()
				if state.profile != "default" {
					dir = filepath.Join(profiles.ProfilesRoot(), state.profile)
				}
				writeDeadGatewayState(t, dir, state.state, state.stated)
			}
			gatewaylessInventory = func(context.Context) *UpdatePlan {
				plan := NewUpdatePlan()
				plan.Runtimes = append(plan.Runtimes, tc.runtimes...)
				return plan
			}
			h.checkout(tc.checkout)
			vsSwap(t, &fleetCheckoutContains, func(context.Context, string) (bool, error) { return tc.checkout == "hotfix", nil })

			if h.pendingNeeded() != tc.pending {
				t.Fatalf("pending = %v, want %v", !tc.pending, tc.pending)
			}
			if hostObligationExists() != tc.pending {
				t.Fatalf("host obligation exists = %v", hostObligationExists())
			}
			if warned := strings.Contains(h.startupWarning(), "icebath gateway restart"); warned != tc.pending {
				t.Fatalf("warned = %v, want %v", warned, tc.pending)
			}
		})
	}
}

func TestGatewaylessProbeFailureKeepsMarker(t *testing.T) {
	live := []map[string]any{}
	sha := "new"
	h, _ := scopedSeed(t, map[string]any{}, &sha, &live)
	if err := os.Remove(filepath.Join(h.home, "gateway_state.json")); err != nil {
		t.Fatal(err)
	}
	gatewaylessInventory = func(context.Context) *UpdatePlan { panic(errors.New("process table unreadable")) }
	if !h.pendingNeeded() || !hostObligationExists() {
		t.Fatalf("a failed probe settled the marker, exists = %v", hostObligationExists())
	}
}

func TestLegacyMarkerDischargesOnLiveFleetEvidenceWithoutReceipt(t *testing.T) {
	for _, completedRestart := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "completed-restart"}[completedRestart], func(t *testing.T) {
			old := map[string]any{"outcome": "failed", "plan": map[string]any{"runtimes": []any{scopedGateway("alpha")}}}
			if completedRestart {
				old["post_update"] = map[string]any{"sha": "new"}
				old["gateway_restart"] = map[string]any{"incomplete": false}
			}
			live := []map[string]any{scopedCurrent()}
			sha := "new"
			h, target := scopedSeed(t, old, &sha, &live)
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if warning := h.startupWarning(); strings.Contains(warning, "icebath gateway restart") {
				t.Fatalf("warning = %q", warning)
			}
			if h.pendingNeeded() || hostObligationExists() {
				t.Fatalf("the marker stayed, exists = %v", hostObligationExists())
			}
			if after, err := os.ReadFile(target); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("the receipt changed: %v", err)
			}
		})
	}
}

func TestInventoryLessMarkerSettlesAfterOutOfBandPull(t *testing.T) {
	cases := []struct {
		name    string
		live    []map[string]any
		pending bool
	}{
		{"fleet-current", []map[string]any{scopedCurrent()}, false},
		{"fleet-stale", []map[string]any{scopedCurrent("state", "stale", "code_sha", "old")}, true},
		{"fleet-empty", []map[string]any{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := tc.live
			sha := "old"
			h, _ := scopedSeed(t, map[string]any{}, &sha, &live)
			if warned := strings.Contains(h.startupWarning(), "icebath gateway restart"); warned != tc.pending {
				t.Fatalf("warned = %v, want %v", warned, tc.pending)
			}
			if h.pendingNeeded() != tc.pending || hostObligationExists() != tc.pending {
				t.Fatalf("pending want %v, exists = %v", tc.pending, hostObligationExists())
			}
		})
	}
}

func TestMalformedObligationStaysPending(t *testing.T) {
	for i, suffix := range []string{"{", `{"version": 1, "inventory": `, "broken-line"} {
		t.Run(fmt.Sprintf("suffix%d", i), func(t *testing.T) {
			live := []map[string]any{scopedCurrent()}
			sha := "new"
			h, _ := scopedSeed(t, map[string]any{}, &sha, &live)
			marker := updaterecord.HostObligationPath()
			text, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, append(text, suffix...), 0o644); err != nil {
				t.Fatal(err)
			}
			if !h.pendingNeeded() || !hostObligationExists() {
				t.Fatalf("a malformed obligation was discharged, exists = %v", hostObligationExists())
			}
		})
	}
}

func TestMarkerReconciliationCollectsOneLiveSnapshot(t *testing.T) {
	live := []map[string]any{scopedCurrent()}
	sha := "new"
	h, _ := scopedSeed(t, map[string]any{}, &sha, &live)
	h.arm("new", []any{scopedGateway("alpha")})
	probes := 0
	fleetCollectVersions = func() []*pyjson.Object {
		probes++
		if probes == 1 {
			return []*pyjson.Object{fleetObject("profile", "alpha", "state", "current", "code_sha", "new")}
		}
		return []*pyjson.Object{}
	}
	if h.pendingNeeded() || probes != 1 {
		t.Fatalf("pending with %d probes", probes)
	}
}
