package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Sandboxes the home and the host state dir and pins the checkout head and the live fleet for the test
type fleetHarness struct {
	t    *testing.T
	ctx  context.Context
	home string
	err  *bytes.Buffer
}

func newFleetHarness(t *testing.T) *fleetHarness {
	t.Helper()
	h := &fleetHarness{t: t, ctx: context.Background(), home: t.TempDir(), err: &bytes.Buffer{}}
	t.Setenv("ICEBATH_HOME", h.home)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", t.TempDir())
	savedSHA, savedFleet, savedLive, savedOut, savedErr := fleetReadCheckoutSHA, fleetCollectVersions, servePidAliveMatches, serveStdout, serveStderr
	savedRoot, savedInventory := fleetProjectRoot, gatewaylessInventory
	t.Cleanup(func() {
		fleetReadCheckoutSHA, fleetCollectVersions, servePidAliveMatches, serveStdout, serveStderr = savedSHA, savedFleet, savedLive, savedOut, savedErr
		fleetProjectRoot, gatewaylessInventory = savedRoot, savedInventory
	})
	fleetProjectRoot = func() string { return filepath.Join(h.home, "install-tree") }
	fleetCollectVersions = func() []*pyjson.Object { return nil }
	servePidAliveMatches = func(int64, *float64) (bool, bool) { return true, true }
	gatewaylessInventory = func(context.Context) *UpdatePlan { return NewUpdatePlan() }
	return h
}

// Pins the checkout head, an empty sha standing for one that cannot be resolved
func (h *fleetHarness) checkout(sha string) {
	fleetReadCheckoutSHA = func() (*string, error) {
		if sha == "" {
			return nil, nil
		}
		return &sha, nil
	}
}

// Pins the live fleet matrix to the given rows
func (h *fleetHarness) fleet(rows ...map[string]any) {
	fleetCollectVersions = func() []*pyjson.Object {
		out := make([]*pyjson.Object, len(rows))
		for i, row := range rows {
			obj := pyjson.NewObject()
			for key, value := range row {
				obj.Set(key, value)
			}
			out[i] = obj
		}
		return out
	}
}

// Writes the latest update receipt of the sandboxed home
func (h *fleetHarness) receipt(doc map[string]any) {
	h.t.Helper()
	dir := filepath.Join(h.home, "logs", "update_receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "latest.json"), raw, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// Arms the obligation, failing the test when it could not be armed
func (h *fleetHarness) arm(expectedSHA string, runtimes []any) {
	h.t.Helper()
	armed, err := writeFleetRestartPendingMarker(h.ctx, expectedSHA, runtimes, "")
	if err != nil || !armed {
		h.t.Fatalf("arm %v %v", armed, err)
	}
}

func (h *fleetHarness) armed() bool { return fleetRestartObligationArmed(h.ctx) }

// Runs the startup hint and returns what it wrote
func (h *fleetHarness) startupWarning() string {
	h.err.Reset()
	WarnPendingFleetRestartOnStartup(h.ctx, h.home, h.err)
	return h.err.String()
}

func (h *fleetHarness) pendingNeeded() bool {
	h.t.Helper()
	needed, err := pendingFleetRestartNeeded(h.ctx, nil, nil, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return needed
}

func gatewayRows(profiles ...string) []any {
	rows := make([]any, len(profiles))
	for i, profile := range profiles {
		rows[i] = map[string]any{"kind": "gateway", "profile": profile}
	}
	return rows
}

func currentRow(profile, sha string) map[string]any {
	return map[string]any{"profile": profile, "pid": int64(42), "code_sha": sha, "code_version": "0.21.0", "state": "current"}
}

var (
	shaE = strings.Repeat("e", 40)
	shaF = strings.Repeat("f", 40)
	sha7 = strings.Repeat("7", 40)
)

func TestObligationRoundTripIsHostScoped(t *testing.T) {
	h := newFleetHarness(t)
	path := updaterecord.HostObligationPath()
	if filepath.Dir(path) != updaterecord.HostStateDir() {
		t.Fatalf("obligation path %s outside the host state dir", path)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("obligation present before arming")
	}
	h.arm("abc123", nil)
	if !h.armed() {
		t.Fatal("obligation not armed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["expected_sha"] != "abc123" || record["pid"] == nil || record["started"] == nil {
		t.Fatalf("record %v", record)
	}
	if _, err := os.Stat(filepath.Join(h.home, "fleet_restart_pending")); err == nil {
		t.Fatal("legacy per home marker written")
	}
	clearFleetRestartPendingMarker(h.ctx)
	if h.armed() {
		t.Fatal("obligation still armed after clearing")
	}
}

func TestPendingNeededWhenMarkerExists(t *testing.T) {
	h := newFleetHarness(t)
	h.arm("", nil)
	if !h.pendingNeeded() {
		t.Fatal("armed marker not pending")
	}
	clearFleetRestartPendingMarker(h.ctx)
	if h.pendingNeeded() {
		t.Fatal("pending with nothing armed")
	}
}

func TestPendingNeededWhenUnfinishedReceiptRuntimeShaSkews(t *testing.T) {
	h := newFleetHarness(t)
	h.checkout(shaE)
	h.receipt(map[string]any{"exit_code": 1, "stop_reason": "KeyboardInterrupt: ", "outcome": "failed", "plan": map[string]any{"expected_sha": shaE,
		"runtimes": []any{map[string]any{"kind": "gateway", "profile": "default", "pid": 2111768, "supervisor": "systemd", "code_sha": sha7, "restart_via": "systemd"}}}})
	if !h.pendingNeeded() {
		t.Fatal("unfinished receipt with a skewed runtime not pending")
	}
}

func TestSuccessfulReceiptWithPreUpdatePlanShasDoesNotRetrigger(t *testing.T) {
	h := newFleetHarness(t)
	disk, old := strings.Repeat("n", 40), strings.Repeat("o", 40)
	h.checkout(disk)
	h.receipt(map[string]any{"exit_code": 0, "outcome": "success", "plan": map[string]any{"expected_sha": old,
		"runtimes": []any{map[string]any{"kind": "gateway", "profile": "default", "pid": 1, "code_sha": old}}},
		"fleet": []any{map[string]any{"profile": "default", "pid": 2, "code_sha": disk, "state": "current"}}, "gateway_restart": map[string]any{"incomplete": false}})
	if h.pendingNeeded() {
		t.Fatal("finished update retriggered from its plan")
	}
}

func TestSuccessfulCommandBoundaryReceiptWithoutFleetDoesNotRetrigger(t *testing.T) {
	h := newFleetHarness(t)
	disk, old := strings.Repeat("n", 40), strings.Repeat("o", 40)
	h.checkout(disk)
	h.receipt(map[string]any{"exit_code": 0, "outcome": "success", "stop_reason": "completed at command boundary", "plan": map[string]any{"expected_sha": old,
		"runtimes": []any{map[string]any{"kind": "gateway", "profile": "default", "pid": 1, "code_sha": old}}}, "fleet": []any{}, "gateway_restart": map[string]any{}})
	if h.pendingNeeded() {
		t.Fatal("a command boundary stop read as an interrupted update")
	}
}

func TestStopReasonOnlyMarksUnfinishedWhenNothingVouchesForSuccess(t *testing.T) {
	for _, c := range []struct {
		id         string
		receipt    map[string]any
		unfinished bool
	}{
		{"success-sys-exit-0", map[string]any{"outcome": "success", "exit_code": int64(0), "stop_reason": "sys.exit(0)"}, false},
		{"success-no-exit-code", map[string]any{"outcome": "success", "stop_reason": "KeyboardInterrupt: "}, false},
		{"exit-0-no-outcome", map[string]any{"exit_code": int64(0), "stop_reason": "sys.exit(0)"}, false},
		{"refused-stop-reason-only", map[string]any{"outcome": "refused", "stop_reason": "not_updatable_in_place"}, true},
		{"failed-interrupt", map[string]any{"outcome": "failed", "exit_code": int64(1), "stop_reason": "KeyboardInterrupt: "}, true},
	} {
		if got := receiptLooksUnfinished(c.receipt); got != c.unfinished {
			t.Errorf("%s unfinished %v", c.id, got)
		}
	}
}

func TestStaleFleetMatrixOnLatestReceiptIsPending(t *testing.T) {
	h := newFleetHarness(t)
	h.checkout(strings.Repeat("n", 40))
	h.receipt(map[string]any{"outcome": "partial", "exit_code": 1,
		"fleet": []any{map[string]any{"profile": "default", "pid": 9, "code_sha": strings.Repeat("s", 40), "state": "stale"}}})
	if !h.pendingNeeded() {
		t.Fatal("stale fleet matrix not pending")
	}
}

func TestStartupWarnPrintsWhenMarkerPresent(t *testing.T) {
	h := newFleetHarness(t)
	h.arm("", nil)
	warning := h.startupWarning()
	if !strings.Contains(warning, "did not restart running gateways") || !strings.Contains(warning, "icebath gateway restart") {
		t.Fatalf("warning %q", warning)
	}
	want := "⚠ A previous `icebath update` pulled new code but did not restart running gateways.\n" +
		"  Gateways may still be serving pre-update modules (mixed sys.modules).\n  Run `icebath update` or `icebath gateway restart`.\n"
	if warning != want {
		t.Fatalf("warning %q", warning)
	}
}

func TestStartupWarnSilentWhenNothingPending(t *testing.T) {
	h := newFleetHarness(t)
	out := &bytes.Buffer{}
	serveStdout = out
	if warning := h.startupWarning(); warning != "" || out.Len() != 0 {
		t.Fatalf("warning %q stdout %q", warning, out.String())
	}
}

func TestStartupWarnDischargedWhenFleetCurrent(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, gatewayRows("default"))
	h.checkout(shaE)
	h.fleet(currentRow("default", shaE))
	if warning := h.startupWarning(); warning != "" || h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestStartupWarnDischargedWhenMultiplexerCoversOwedProfiles(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, gatewayRows("default", "coder"))
	h.checkout(shaE)
	h.receipt(map[string]any{"outcome": "partial", "exit_code": 1, "plan": map[string]any{"runtimes": []any{
		map[string]any{"kind": "gateway", "profile": "default", "pid": 42}, map[string]any{"kind": "gateway", "profile": "coder", "pid": 43}}}})
	row := currentRow("default", shaE)
	row["served_profiles"] = []any{"default", "coder"}
	h.fleet(row)
	if warning := h.startupWarning(); warning != "" || h.armed() || h.pendingNeeded() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestStartupWarnDischargedWhenInventoryHoldsSupervisedServe(t *testing.T) {
	for _, supervisor := range []string{"desktop", "launchd"} {
		t.Run(supervisor, func(t *testing.T) {
			h := newFleetHarness(t)
			h.arm(shaE, []any{map[string]any{"kind": "gateway", "profile": "default", "pid": int64(42), "supervisor": "systemd"},
				map[string]any{"kind": "serve", "profile": "default", "pid": int64(6161), "supervisor": supervisor, "detail": map[string]any{"create_time": 1000.0}}})
			h.checkout(shaE)
			h.fleet(currentRow("default", shaE))
			if warning := h.startupWarning(); warning != "" || h.armed() {
				t.Fatalf("warning %q armed %v", warning, h.armed())
			}
		})
	}
}

func TestStartupWarnKeptWhenInventoryHoldsUnclassifiedServe(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, []any{map[string]any{"kind": "gateway", "profile": "default", "pid": int64(42), "supervisor": "systemd"},
		map[string]any{"kind": "serve", "profile": "default", "pid": int64(6161), "supervisor": "manual"}})
	h.checkout(shaE)
	h.fleet(currentRow("default", shaE))
	if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") || !h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestStartupWarnKeptWithoutPositiveEvidence(t *testing.T) {
	for _, c := range []struct {
		id, disk string
		fleet    []map[string]any
	}{
		{"stale-row", shaE, []map[string]any{{"profile": "default", "pid": int64(42), "code_sha": sha7, "code_version": "0.20.0", "state": "stale"}}},
		{"empty-probe", shaE, nil},
		{"unknown-identity", shaE, []map[string]any{{"profile": "default", "pid": int64(42), "code_sha": nil, "code_version": nil, "state": "unknown"}}},
		{"unknown-checkout", "", []map[string]any{{"profile": "default", "code_sha": shaE, "state": "current"}}},
		{"checkout-moved", shaF, []map[string]any{{"profile": "default", "pid": int64(42), "code_sha": shaE, "code_version": nil, "state": "current"}}},
	} {
		t.Run(c.id, func(t *testing.T) {
			h := newFleetHarness(t)
			saved := fleetCheckoutContains
			t.Cleanup(func() { fleetCheckoutContains = saved })
			fleetCheckoutContains = func(context.Context, string) (bool, error) { return false, nil }
			h.arm(shaE, gatewayRows("default"))
			h.checkout(c.disk)
			h.fleet(c.fleet...)
			if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") || !h.armed() {
				t.Fatalf("warning %q armed %v", warning, h.armed())
			}
		})
	}
}

// Real checkout holding a pulled commit and a carried one on top, made the install tree of the test
func checkoutWithCarriedCommit(t *testing.T, h *fleetHarness) (expected, head string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	for _, step := range [][2]string{{"a", "pulled"}, {"b", "carried hotfix"}} {
		if err := os.WriteFile(filepath.Join(repo, step[0]), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", step[0])
		git("commit", "-qm", step[1])
		if step[0] == "a" {
			expected = git("rev-parse", "HEAD")
		}
	}
	head = git("rev-parse", "HEAD")
	fleetProjectRoot = func() string { return repo }
	pinFleetIdentity(t, head, "git")
	h.checkout(head)
	return expected, head
}

func TestObligationDischargesWhenGatewayServesDescendantOfExpectedSha(t *testing.T) {
	h := newFleetHarness(t)
	expected, head := checkoutWithCarriedCommit(t, h)
	h.arm(expected, gatewayRows("default"))
	h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": head, "code_version": "0.21.4", "state": "current"})
	if h.pendingNeeded() {
		t.Fatal("fleet on a descendant of the pulled sha still pending")
	}
	if warning := h.startupWarning(); warning != "" || h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestObligationKeptWhenGatewayServesStaleCodeOnCarriedCheckout(t *testing.T) {
	h := newFleetHarness(t)
	expected, _ := checkoutWithCarriedCommit(t, h)
	h.arm(expected, gatewayRows("default"))
	h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": strings.Repeat("0", 40), "code_version": "0.21.3", "state": "stale"})
	if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") || !h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestStartupWarnKeptWhenReceiptOwedGatewayIsDown(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, gatewayRows("alpha", "beta"))
	h.checkout(shaE)
	old := strings.Repeat("o", 40)
	h.receipt(map[string]any{"outcome": "partial", "exit_code": 1, "plan": map[string]any{"runtimes": []any{
		map[string]any{"kind": "gateway", "profile": "alpha", "code_sha": old, "pid": 1}, map[string]any{"kind": "gateway", "profile": "beta", "code_sha": old, "pid": 1}}},
		"fleet": []any{map[string]any{"profile": "alpha", "pid": 42, "code_sha": shaE, "state": "current"},
			map[string]any{"profile": "beta", "pid": 43, "code_sha": nil, "state": "down"}}})
	h.fleet(currentRow("alpha", shaE))
	if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") || !h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

var completedRestart = map[string]any{"restarted_services": []any{"icebath-gateway"}, "relaunched_profiles": []any{}, "externally_supervised_profiles": []any{},
	"killed_pids": []any{}, "failed_units": []any{}, "incomplete": false, "phase_error": ""}

func TestStartupWarnSilentWhenFailedReceiptAlreadyRestartedFleet(t *testing.T) {
	h := newFleetHarness(t)
	pre, pulled, checkout := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	h.checkout(checkout)
	h.receipt(map[string]any{"outcome": "failed", "exit_code": 1, "stop_reason": "AttributeError: module 'icebath_cli.main_dashboard' has no attribute 'x'",
		"pre_update": map[string]any{"sha": pre}, "post_update": map[string]any{"sha": pulled}, "gateway_restart": completedRestart, "fleet": []any{},
		"plan": map[string]any{"runtimes": []any{map[string]any{"kind": "gateway", "profile": "default", "code_sha": pre, "pid": 1}}}})
	h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": pulled, "code_version": "0.21.3", "state": "stale"})
	if warning := h.startupWarning(); warning != "" {
		t.Fatalf("warning %q", warning)
	}
	if !h.pendingNeeded() {
		t.Fatal("update catch up no longer owes the fleet the checkout")
	}
}

func TestStartupWarnSilentWhenCompletedUpdateFleetRestartedOntoMovedCheckout(t *testing.T) {
	h := newFleetHarness(t)
	pre, pulled, checkout := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	h.checkout(checkout)
	h.receipt(map[string]any{"outcome": "success", "exit_code": 0, "pre_update": map[string]any{"sha": pre}, "post_update": map[string]any{"sha": pulled},
		"gateway_restart": completedRestart, "fleet": []any{map[string]any{"profile": "default", "pid": 7, "code_sha": pulled, "state": "current"}},
		"plan": map[string]any{"runtimes": []any{map[string]any{"kind": "gateway", "profile": "default", "code_sha": pre, "pid": 1}}}})
	h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": checkout, "code_version": "0.21.3", "state": "current"})
	if warning := h.startupWarning(); warning != "" {
		t.Fatalf("warning %q", warning)
	}
}

func TestStartupWarnDischargedWhenInventoryLessMarkerFleetCurrent(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, nil)
	if _, has := updaterecord.ReadHostObligation().Get("inventory"); has {
		t.Fatal("inventory recorded for a marker armed without one")
	}
	h.checkout(shaE)
	h.fleet(currentRow("default", shaE))
	if warning := h.startupWarning(); warning != "" || h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestStartupWarnKeptWhenInventoryLessMarkerFleetStale(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, nil)
	h.checkout(shaE)
	h.fleet(map[string]any{"profile": "default", "pid": int64(42), "code_sha": sha7, "code_version": "0.20.0", "state": "stale"})
	if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") || !h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestEmptyInventoryDoesNotArmMarker(t *testing.T) {
	h := newFleetHarness(t)
	h.arm(shaE, []any{})
	if h.armed() {
		t.Fatal("an explicit empty inventory armed the obligation")
	}
}

// Writes a host obligation record that names no sha
func writeSHALessObligation(t *testing.T) {
	t.Helper()
	path := updaterecord.HostObligationPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "started": 0.0, "pid": 424242, "armed_by_profile": "default", "expected_sha": ""})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStartupWarnDischargedWhenShaLessMarkerFleetCurrent(t *testing.T) {
	h := newFleetHarness(t)
	writeSHALessObligation(t)
	h.checkout(shaE)
	row := currentRow("default", shaE)
	row["code_version"] = "0.21.5"
	h.fleet(row)
	if warning := h.startupWarning(); warning != "" || h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestStartupWarnKeptWhenShaLessMarkerFleetIsAllExternal(t *testing.T) {
	h := newFleetHarness(t)
	writeSHALessObligation(t)
	h.checkout(shaE)
	h.fleet(map[string]any{"profile": "other", "pid": int64(43), "code_sha": shaF, "code_version": "0.21.5", "state": "external", "code_root": "/elsewhere/icebath-agent"})
	if warning := h.startupWarning(); !strings.Contains(warning, "did not restart running gateways") || !h.armed() {
		t.Fatalf("warning %q armed %v", warning, h.armed())
	}
}

func TestLaunchdServeRowNeverPessimizeGatewayCoverage(t *testing.T) {
	launchd := map[string]any{"kind": "serve", "profile": "work", "pid": int64(900), "supervisor": "launchd", "code_sha": nil, "code_version": nil,
		"restart_via": "launchd", "detail": map[string]any{}}
	receipt := map[string]any{"plan": map[string]any{"runtimes": []any{map[string]any{"kind": "gateway", "profile": "default"}, launchd}}, "fleet": []any{}}
	owed, known, err := receiptOwedGateways(receipt, []any{})
	if err != nil || !known || len(owed) != 1 || !owed[gatewayIdentity{"gateway", "default"}] {
		t.Fatalf("owed %v %v %v", owed, known, err)
	}
	if _, known, _ := receiptOwedGateways(map[string]any{"plan": map[string]any{"runtimes": []any{map[string]any{"kind": "serve", "supervisor": "manual"}}}}, nil); known {
		t.Fatal("an unclassified serve row left coverage verified")
	}
	if _, known, _ := receiptOwedGateways(map[string]any{"fleet": []any{map[string]any{"profile": "unknown"}}}, nil); known {
		t.Fatal("an unidentified fleet row left coverage verified")
	}
}

func TestLegacyMarkerFieldsAndGatewaylessDischarge(t *testing.T) {
	h := newFleetHarness(t)
	marker := fleetRestartPendingMarkerPath(h.ctx)
	inventory, _ := json.Marshal(map[string]any{"version": 1, "runtimes": []any{map[string]any{"kind": "gateway", "profile": "default"}}})
	if err := os.WriteFile(marker, []byte("started=0\npid=1\nexpected_sha="+shaE+"\ninventory="+string(inventory)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fields, armed := fleetObligationFields(h.ctx)
	if !armed || fields["expected_sha"] != shaE || fields["inventory"] != string(inventory) || !h.armed() {
		t.Fatalf("legacy fields %v %v", fields, armed)
	}
	h.checkout(shaE)
	h.fleet(currentRow("default", shaE))
	if warning := h.startupWarning(); warning != "" || h.armed() {
		t.Fatalf("legacy marker with a current fleet: warning %q armed %v", warning, h.armed())
	}
	if err := os.WriteFile(marker, []byte("expected_sha=a\nexpected_sha=b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, armed := fleetObligationFields(h.ctx); armed {
		t.Fatal("a marker with a repeated key read as well formed")
	}
	if !h.pendingNeeded() {
		t.Fatal("a malformed marker discharged")
	}
	if err := os.WriteFile(marker, []byte("expected_sha="+shaE+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.fleet()
	saved := fleetHostOwesNoRestart
	t.Cleanup(func() { fleetHostOwesNoRestart = saved })
	fleetHostOwesNoRestart = func(context.Context) bool { return false }
	if !h.pendingNeeded() || !h.armed() {
		t.Fatal("an inventory less marker discharged on a host that may still owe a gateway restart")
	}
	fleetHostOwesNoRestart = saved
	if h.pendingNeeded() || h.armed() {
		t.Fatal("an inventory less marker kept on a host with no gateway")
	}
}

func TestHostOwesNoGatewayRestartReadsProfileStatesAndTheInventory(t *testing.T) {
	h := newFleetHarness(t)
	savedHomes, savedStatus := fleetHomes, fleetReadStatus
	t.Cleanup(func() { fleetHomes, fleetReadStatus = savedHomes, savedStatus })
	states := map[string]any{"/p/a": "stopped", "/p/b": nil}
	fleetHomes = func() []profileHome { return []profileHome{{"a", "/p/a"}, {"b", "/p/b"}} }
	fleetReadStatus = func(path string) *pyjson.Object {
		state, has := states[filepath.Dir(path)]
		if !has || state == nil {
			return nil
		}
		record := pyjson.NewObject()
		record.Set("gateway_state", state)
		return record
	}
	if !hostOwesNoGatewayRestart(h.ctx) {
		t.Fatal("a host whose only gateway stopped cleanly still owes a restart")
	}
	states["/p/b"] = "running"
	if hostOwesNoGatewayRestart(h.ctx) {
		t.Fatal("a gateway that went away without a clean stop owes nothing")
	}
	states["/p/b"] = "startup_failed"
	pid := 6161
	gatewaylessInventory = func(context.Context) *UpdatePlan {
		plan := NewUpdatePlan()
		plan.Runtimes = append(plan.Runtimes, newRuntime("serve", "default", &pid, "desktop", nil, nil, nil))
		return plan
	}
	if !hostOwesNoGatewayRestart(h.ctx) {
		t.Fatal("a desktop owned serve kept the host owing a restart")
	}
	gatewaylessInventory = func(context.Context) *UpdatePlan {
		plan := NewUpdatePlan()
		plan.Runtimes = append(plan.Runtimes, newRuntime("gateway", "default", &pid, "manual", nil, nil, nil))
		return plan
	}
	if hostOwesNoGatewayRestart(h.ctx) {
		t.Fatal("a live gateway left the host owing nothing")
	}
}
