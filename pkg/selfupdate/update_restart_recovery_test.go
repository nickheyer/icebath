package selfupdate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Child stdout of a recovery run that verified or relaunched exactly the named profiles
func rrSuccess(verified, attempted []string) *recoveryCompleted {
	if verified == nil {
		verified = []string{}
	}
	if attempted == nil {
		attempted = []string{}
	}
	raw, _ := json.Marshal(map[string]any{"verified": verified, "relaunch_attempted": attempted, "failed": []string{}})
	return &recoveryCompleted{Stdout: string(raw)}
}

// Plan runtime of one profile under a supervisor, a gateway unless kind says otherwise
func rrRuntime(profile, supervisor, kind string) *RuntimeRecord {
	pid := 1234
	if kind == "" {
		kind = "gateway"
	}
	return &RuntimeRecord{Kind: kind, Profile: profile, Supervisor: supervisor, PID: &pid}
}

// One fresh recovery child the capture seam saw, its argv, env and stdin payload
type rrCall struct {
	argv    []string
	env     []string
	payload map[string]any
}

// Replaces the fresh child capture with result, recording each child it was handed
func rrFakeCapture(t *testing.T, result func() *recoveryCompleted) *[]rrCall {
	t.Helper()
	calls := &[]rrCall{}
	old := abortRecoveryCapture
	t.Cleanup(func() { abortRecoveryCapture = old })
	abortRecoveryCapture = func(child *exec.Cmd, _ time.Duration) (*recoveryCompleted, error) {
		raw, _ := io.ReadAll(child.Stdin)
		payload := map[string]any{}
		_ = json.Unmarshal(raw, &payload)
		*calls = append(*calls, rrCall{argv: slices.Clone(child.Args), env: slices.Clone(child.Env), payload: payload})
		return result(), nil
	}
	return calls
}

// Recovery outcome of the abort pass over runtimes with the given already restarted profiles
func rrRecover(t *testing.T, runtimes []*RuntimeRecord, skip map[string]bool) map[string]any {
	t.Helper()
	result, err := recoverGatewayRestartAfterAbort(context.Background(), io.Discard, &UpdatePlan{Runtimes: runtimes}, false, skip, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// String list of a result bucket
func rrList(t *testing.T, result map[string]any, key string) []string {
	t.Helper()
	list, ok := result[key].([]string)
	if !ok {
		t.Fatalf("%s is %T", key, result[key])
	}
	return list
}

// Skipped rows of a result as profile, kind and reason
func rrSkipped(result map[string]any) [][3]string {
	var out [][3]string
	for _, row := range result["skipped"].([]any) {
		obj := row.(*pyjson.Object)
		out = append(out, [3]string{obj.GetOr("profile", "").(string), obj.GetOr("kind", "").(string), obj.GetOr("reason", "").(string)})
	}
	return out
}

func TestAbortRecoveryHandsManagedProfilesToAFreshProcess(t *testing.T) {
	calls := rrFakeCapture(t, func() *recoveryCompleted { return rrSuccess([]string{"coder", "default"}, nil) })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", ""), rrRuntime("coder", "launchd", ""),
		rrRuntime("manual-box", "manual", ""), rrRuntime("desktop", "desktop", "serve")}, nil)
	if !slices.Equal(rrList(t, result, "requested"), []string{"coder", "default"}) || !slices.Equal(rrList(t, result, "verified"), []string{"coder", "default"}) ||
		len(rrList(t, result, "relaunch_attempted")) != 0 || len(rrList(t, result, "failed")) != 0 {
		t.Fatalf("%v", result)
	}
	skipped := map[[2]string]bool{}
	for _, row := range rrSkipped(result) {
		skipped[[2]string{row[0], row[1]}] = true
		if row[2] == "" {
			t.Fatalf("skip without reason %v", row)
		}
	}
	if len(skipped) != 2 || !skipped[[2]string{"manual-box", "gateway"}] || !skipped[[2]string{"desktop", "serve"}] {
		t.Fatalf("%v", skipped)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d calls", len(*calls))
	}
	call := (*calls)[0]
	exe, _ := runningExecutable()
	if call.argv[0] != exe || !slices.Equal(call.argv[1:3], []string{UpdateRestartRecoveryEntry, "--stdin"}) {
		t.Fatalf("%v", call.argv)
	}
	profiles, _ := json.Marshal(call.payload["profiles"])
	supervisors, _ := json.Marshal(call.payload["supervisors"])
	if string(profiles) != `["coder","default"]` || string(supervisors) != `{"coder":"launchd","default":"systemd"}` {
		t.Fatalf("%s %s", profiles, supervisors)
	}
	serve, _ := call.payload["serve_units"].(map[string]any)
	if _, ok := serve["recover"]; !ok || len(serve) != 2 {
		t.Fatalf("%v", serve)
	}
	if _, ok := serve["skip"]; !ok {
		t.Fatalf("%v", serve)
	}
	if !slices.Contains(call.env, restartRecoveryEnv+"=1") {
		t.Fatalf("%v", call.env)
	}
}

func TestAbortRecoveryDoesNotClaimSuccessWhenFreshProcessFails(t *testing.T) {
	rrFakeCapture(t, func() *recoveryCompleted { return &recoveryCompleted{ReturnCode: 1} })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", "")}, nil)
	if !slices.Equal(rrList(t, result, "requested"), []string{"default"}) || len(rrList(t, result, "verified")) != 0 ||
		len(rrList(t, result, "relaunch_attempted")) != 0 || !slices.Equal(rrList(t, result, "failed"), []string{"default"}) {
		t.Fatalf("%v", result)
	}
}

func TestAbortRecoveryReportsUnverifiedRelaunchConservatively(t *testing.T) {
	rrFakeCapture(t, func() *recoveryCompleted { return rrSuccess(nil, []string{"default"}) })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", "")}, nil)
	if len(rrList(t, result, "verified")) != 0 || !slices.Equal(rrList(t, result, "relaunch_attempted"), []string{"default"}) || len(rrList(t, result, "failed")) != 0 {
		t.Fatalf("%v", result)
	}
}

func TestAbortRecoverySkipsProfilesAlreadyRestartedByThePhase(t *testing.T) {
	calls := rrFakeCapture(t, func() *recoveryCompleted { return rrSuccess([]string{"coder"}, nil) })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", ""), rrRuntime("coder", "systemd", "")}, map[string]bool{"default": true})
	if !slices.Equal(rrList(t, result, "requested"), []string{"coder"}) || !slices.Equal(rrList(t, result, "verified"), []string{"coder"}) || len(rrList(t, result, "failed")) != 0 {
		t.Fatalf("%v", result)
	}
	profiles, _ := json.Marshal((*calls)[0].payload["profiles"])
	if string(profiles) != `["coder"]` {
		t.Fatalf("%s", profiles)
	}
}

func TestAbortRecoveryRejectsPartialJsonSuccess(t *testing.T) {
	rrFakeCapture(t, func() *recoveryCompleted { return rrSuccess([]string{"default"}, nil) })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", ""), rrRuntime("coder", "systemd", "")}, nil)
	if !slices.Equal(rrList(t, result, "requested"), []string{"coder", "default"}) || len(rrList(t, result, "verified")) != 0 ||
		!slices.Equal(rrList(t, result, "failed"), []string{"coder", "default"}) {
		t.Fatalf("%v", result)
	}
}

func TestAbortRecoveryRejectsMalformedJsonSuccess(t *testing.T) {
	rrFakeCapture(t, func() *recoveryCompleted { return &recoveryCompleted{Stdout: "not-json"} })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", "")}, nil)
	if !slices.Equal(rrList(t, result, "requested"), []string{"default"}) || len(rrList(t, result, "verified")) != 0 ||
		!slices.Equal(rrList(t, result, "failed"), []string{"default"}) {
		t.Fatalf("%v", result)
	}
}

func TestAbortRecoveryDoesNotRestartManualOnlyFleet(t *testing.T) {
	calls := rrFakeCapture(t, func() *recoveryCompleted { return rrSuccess(nil, nil) })
	old := abortServeRecovery
	t.Cleanup(func() { abortServeRecovery = old })
	abortServeRecovery = func() bool { return false }
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("manual-box", "manual", "")}, nil)
	if len(rrList(t, result, "requested")) != 0 || len(rrList(t, result, "verified")) != 0 || len(rrList(t, result, "failed")) != 0 {
		t.Fatalf("%v", result)
	}
	if rows := rrSkipped(result); len(rows) != 1 || rows[0][0] != "manual-box" {
		t.Fatalf("%v", rows)
	}
	if len(*calls) != 0 {
		t.Fatalf("%v", *calls)
	}
}

func TestAbortRecoveryRecordsServeRuntimesAsSkippedWithReason(t *testing.T) {
	rrFakeCapture(t, func() *recoveryCompleted { return rrSuccess([]string{"default"}, nil) })
	result := rrRecover(t, []*RuntimeRecord{rrRuntime("default", "systemd", ""), rrRuntime("default", "desktop", "serve"), rrRuntime("ops", "manual-serve", "dashboard")}, nil)
	byKind := map[string][3]string{}
	for _, row := range rrSkipped(result) {
		byKind[row[1]] = row
	}
	if len(byKind) != 2 || !strings.Contains(byKind["serve"][2], "desktop app") || byKind["dashboard"][0] != "ops" || !strings.Contains(byKind["dashboard"][2], "relaunch") {
		t.Fatalf("%v", byKind)
	}
}

func TestServiceMatchingIsExactForOverlappingProfileNames(t *testing.T) {
	for _, c := range []struct {
		profile, service string
		want             bool
	}{
		{"foo", "icebath-gateway-foo.service", true},
		{"foo", "icebath-gateway-foobar.service", false},
		{"default", "ai.icebath.gateway", true},
		{"default", "ai.icebath.gateway-foo", false},
		{"default", "gui/501/ai.icebath.gateway", true},
		{"foo", "user/icebath-gateway-foo.service", true},
	} {
		if got := gatewayServiceMatchesProfile(c.profile, c.service); got != c.want {
			t.Fatalf("%s %s: %v", c.profile, c.service, got)
		}
	}
}

// One recovery command the fake runner saw with the options it was started with
type rrRun struct {
	argv []string
	opts recoveryRunOptions
}

// Replaces the recovery command runner, recording every command
func rrFakeRun(t *testing.T, answer func(argv []string) *recoveryCompleted) *[]rrRun {
	t.Helper()
	runs := &[]rrRun{}
	old := recoveryRun
	t.Cleanup(func() { recoveryRun = old })
	recoveryRun = func(argv []string, _ time.Duration, o recoveryRunOptions) *recoveryCompleted {
		*runs = append(*runs, rrRun{slices.Clone(argv), o})
		return answer(argv)
	}
	return runs
}

// PATH holding only an executable named systemctl when present, so the lookup finds it or nothing
func rrSystemctlPath(t *testing.T, present bool) {
	t.Helper()
	dir := t.TempDir()
	if present {
		if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", t.TempDir())
}

// Bucket lists and covered map of a child result in a comparable form
func rrBuckets(t *testing.T, result *pyjson.Object) string {
	t.Helper()
	text, err := pyjson.Dumps(result, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestRecoveryChildRestartsEachProfileWithAFreshMain(t *testing.T) {
	rrSystemctlPath(t, false)
	t.Setenv("_ICEBATH_GATEWAY", "1")
	runs := rrFakeRun(t, func([]string) *recoveryCompleted { return &recoveryCompleted{} })
	result, err := restartProfiles(context.Background(), []string{"default", "coder"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rrBuckets(t, result); got != `{"covered": {}, "failed": [], "relaunch_attempted": ["coder", "default"], "verified": []}` {
		t.Fatal(got)
	}
	exe, _ := runningExecutable()
	want := [][]string{{exe, "-p", "coder", "gateway", "restart", "--interrupt", "--detach"}, {exe, "-p", "default", "gateway", "restart", "--interrupt", "--detach"}}
	if len(*runs) != 2 {
		t.Fatalf("%v", *runs)
	}
	for i, run := range *runs {
		if !slices.Equal(run.argv, want[i]) || !run.opts.DevNull || !run.opts.Detached {
			t.Fatalf("%v", run)
		}
		if !slices.Contains(run.opts.Env, restartRecoveryEnv+"=1") {
			t.Fatalf("%v", run.opts.Env)
		}
		for _, kv := range run.opts.Env {
			if strings.HasPrefix(kv, "_ICEBATH_GATEWAY=") {
				t.Fatalf("%v", run.opts.Env)
			}
		}
	}
}

func TestRecoveryChildVerifiesSystemdProfilesViaIsActive(t *testing.T) {
	rrSystemctlPath(t, true)
	runs := rrFakeRun(t, func(argv []string) *recoveryCompleted {
		if strings.HasSuffix(argv[0], "systemctl") {
			if argv[len(argv)-1] == "icebath-gateway.service" {
				return &recoveryCompleted{Stdout: "active"}
			}
			return &recoveryCompleted{ReturnCode: 3, Stdout: "inactive"}
		}
		return &recoveryCompleted{}
	})
	result, err := restartProfiles(context.Background(), []string{"default", "coder"}, map[string]string{"default": "systemd", "coder": "launchd"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rrBuckets(t, result); got != `{"covered": {}, "failed": [], "relaunch_attempted": ["coder"], "verified": ["default"]}` {
		t.Fatal(got)
	}
	for _, run := range *runs {
		if strings.HasSuffix(run.argv[0], "systemctl") && strings.Contains(run.argv[len(run.argv)-1], "coder") {
			t.Fatalf("launchd profile probed with systemctl %v", run.argv)
		}
	}
}

func TestRecoveryChildTreatsMissingSystemctlAsUnverified(t *testing.T) {
	rrSystemctlPath(t, false)
	rrFakeRun(t, func([]string) *recoveryCompleted { return &recoveryCompleted{} })
	result, err := restartProfiles(context.Background(), []string{"default"}, map[string]string{"default": "systemd"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rrBuckets(t, result); got != `{"covered": {}, "failed": [], "relaunch_attempted": ["default"], "verified": []}` {
		t.Fatal(got)
	}
}

func TestRecoveryChildReportsFailedProfileWithoutLosingSuccesses(t *testing.T) {
	rrSystemctlPath(t, false)
	outcomes := []int{1, 0}
	rrFakeRun(t, func([]string) *recoveryCompleted {
		code := outcomes[0]
		outcomes = outcomes[1:]
		return &recoveryCompleted{ReturnCode: code}
	})
	result, err := restartProfiles(context.Background(), []string{"coder", "default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rrBuckets(t, result); got != `{"covered": {}, "failed": ["coder"], "relaunch_attempted": ["default"], "verified": []}` {
		t.Fatal(got)
	}
}

func TestRecoveryPayloadRejectsPathLikeProfileIds(t *testing.T) {
	if _, _, _, _, err := parseRecoveryPayload(`{"profiles": ["../other"]}`); err == nil || !strings.Contains(err.Error(), "invalid profile") {
		t.Fatalf("path-like profile id must be rejected: %v", err)
	}
}

func TestRecoveryPayloadRejectsMalformedSupervisorsMap(t *testing.T) {
	if _, _, _, _, err := parseRecoveryPayload(`{"profiles": ["default"], "supervisors": {"default": "sys/temd"}}`); err == nil || !strings.Contains(err.Error(), "supervisors") {
		t.Fatalf("malformed supervisors map must be rejected: %v", err)
	}
}

const rrE2ELedgerEnv = "ICEBATH_RECOVERY_E2E_LEDGER"

// Runs the recovery entry in this re-executed test binary with its gateway restarts and systemctl answered and recorded
func rrE2EChild(args []string) int {
	ledger := os.Getenv(rrE2ELedgerEnv)
	recoveryRun = func(argv []string, _ time.Duration, o recoveryRunOptions) *recoveryCompleted {
		if strings.HasSuffix(argv[0], "systemctl") {
			if argv[len(argv)-1] == "icebath-gateway.service" {
				return &recoveryCompleted{Stdout: "active"}
			}
			return &recoveryCompleted{ReturnCode: 3, Stdout: "inactive"}
		}
		raw, _ := json.Marshal(map[string]any{"argv": argv, "env": o.Env})
		f, err := os.OpenFile(ledger, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return nil
		}
		defer f.Close()
		f.Write(append(raw, '\n'))
		return &recoveryCompleted{}
	}
	return UpdateRestartRecoveryMain(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
}

func TestRecoveryModuleEndToEndInARealFreshProcess(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "grandchild_calls.jsonl")
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], UpdateRestartRecoveryEntry, "--stdin")
	child.Env = append(os.Environ(), rrE2ELedgerEnv+"="+ledger, "PATH="+dir, "_ICEBATH_GATEWAY=1", "ICEBATH_GATEWAY_LOCK_DIR="+t.TempDir())
	child.Stdin = strings.NewReader(`{"profiles": ["default", "coder"], "supervisors": {"default": "systemd", "coder": "launchd"}}`)
	out, err := child.Output()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var got any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	normalized, _ := json.Marshal(got)
	if string(normalized) != `{"covered":{},"failed":[],"relaunch_attempted":["coder"],"serve_units":{"failed":[],"verified":[]},"verified":["default"]}` {
		t.Fatal(string(normalized))
	}
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var call struct {
			Argv []string `json:"argv"`
			Env  []string `json:"env"`
		}
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, call.Argv[slices.Index(call.Argv, "-p")+1])
		if !slices.Equal(call.Argv[len(call.Argv)-4:], []string{"gateway", "restart", "--interrupt", "--detach"}) {
			t.Fatalf("%v", call.Argv)
		}
		for _, kv := range call.Env {
			if strings.HasPrefix(kv, "_ICEBATH_GATEWAY=") {
				t.Fatalf("gateway marker leaked to the grandchild %v", call.Env)
			}
		}
	}
	if !slices.Equal(profiles, []string{"coder", "default"}) {
		t.Fatalf("%v", profiles)
	}
}
