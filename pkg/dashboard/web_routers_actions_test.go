package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Empty action registries over a log dir, the module attributes the hermes cases patch, restored after the case
func isolateActionRegistries(t *testing.T, logDir string) {
	t.Helper()
	actionMu.Lock()
	procs, results, commands, ids, starts := actionProcs, actionResults, actionCommands, actionIDs, actionLogStarts
	actionProcs, actionResults, actionCommands = map[string]actionProc{}, map[string]*pyjson.Object{}, map[string][]string{}
	actionIDs, actionLogStarts = map[string]string{}, map[string]actionLogStart{}
	actionMu.Unlock()
	savedDir := actionLogDir
	actionLogDir = func() string { return logDir }
	t.Cleanup(func() {
		actionMu.Lock()
		actionProcs, actionResults, actionCommands, actionIDs, actionLogStarts = procs, results, commands, ids, starts
		actionMu.Unlock()
		actionLogDir = savedDir
	})
}

// Popen replaced by a handle of the pid that never exits
func fakeActionPopen(t *testing.T, pid int) {
	saved := actionPopen
	actionPopen = func([]string, string, map[string]string, *os.File, *syscall.SysProcAttr) (actionProc, error) {
		return &fakeActionProc{pid: pid}, nil
	}
	t.Cleanup(func() { actionPopen = saved })
}

func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Drops the live process registry, and the action ids when asked, the way a dashboard restart loses them
func clearActionRegistries(ids bool) {
	actionMu.Lock()
	defer actionMu.Unlock()
	clear(actionProcs)
	if ids {
		clear(actionIDs)
	}
}

// Writes a begun and finalized update receipt of the profile home stamped with the action id
func writeUpdateReceipt(t *testing.T, profileHome, actionID, outcome string, debt func(ctx context.Context)) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", profileHome)
	t.Setenv("ICEBATH_ACTION_ID", actionID)
	ctx := updaterecord.UpdateReceiptScope(context.Background())
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	if debt != nil {
		debt(ctx)
	}
	updaterecord.FinalizeUpdateReceipt(ctx, outcome, nil, "")
	os.Unsetenv("ICEBATH_ACTION_ID")
}

func updateStatus(t *testing.T, c sessionClient) map[string]any {
	t.Helper()
	r := c.get("/api/actions/hermes-update/status?lines=2000")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	return jsonObject(t, r)
}

func receiptOf(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	m, ok := data["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt %v", data["receipt"])
	}
	return m
}

func TestUpdateStatusRecoversCompletedResultAfterDashboardRestart(t *testing.T) {
	c := newSessionClient(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "coder"))
	actionID := strings.Repeat("c", 32)
	writeTestFile(t, filepath.Join(tmp, "hermes-update.log"), "=== hermes-update started 2026-08-17 11:19:34 "+actionID+" ===\npulling updates...\n")
	writeTestFile(t, filepath.Join(root, "logs", "update.log"), "=== icebath update started 2026-08-17T11:19:35 ===\n"+
		"✓ Update complete!\n=== icebath-update completed "+actionID+" ===\n")
	isolateActionRegistries(t, tmp)

	data := updateStatus(t, c)
	if data["running"] != false || !pyrepr.Equal(data["exit_code"], int64(0)) || data["action_id"] != actionID {
		t.Fatalf("data %v", data)
	}
	if !slices.Contains(data["lines"].([]any), any("=== icebath-update completed "+actionID+" ===")) {
		t.Fatalf("lines %v", data["lines"])
	}
}

func TestUpdateStatusIgnoresAnotherProfilesCompletionInTheSharedRootLog(t *testing.T) {
	c := newSessionClient(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "b"))
	isolateActionRegistries(t, filepath.Join(tmp, "b-logs"))
	fakeActionPopen(t, 7)
	bID := strings.Repeat("b", 32)
	if _, err := spawnHermesAction(context.Background(), []string{"update"}, "hermes-update", map[string]string{"ICEBATH_ACTION_ID": bID}); err != nil {
		t.Fatal(err)
	}
	clearActionRegistries(false)

	statusAfterRootCompletion := func(actionID string) map[string]any {
		writeTestFile(t, filepath.Join(root, "logs", "update.log"), "=== icebath update started 2026-08-17T11:19:35 ===\n"+
			"=== icebath-update completed "+actionID+" ===\n")
		return updateStatus(t, c)
	}
	foreign := statusAfterRootCompletion(strings.Repeat("a", 32))
	if _, has := foreign["action_id"]; foreign["exit_code"] != nil || has {
		t.Fatalf("foreign %v", foreign)
	}
	own := statusAfterRootCompletion(bID)
	if !pyrepr.Equal(own["exit_code"], int64(0)) || own["action_id"] != bID {
		t.Fatalf("own %v", own)
	}
}

func TestRestartedDashboardKeepsItsActionIdentityPastTheLogTail(t *testing.T) {
	for _, tc := range []struct{ logShape, bOutcome string }{
		{"short", "success"}, {"long", "success"}, {"rotated", "success"}, {"long", "failed"}, {"long", ""},
	} {
		t.Run(tc.logShape+"-"+tc.bOutcome, func(t *testing.T) {
			c := newSessionClient(t)
			tmp := t.TempDir()
			root := filepath.Join(tmp, "root")
			bID, aID := strings.Repeat("b", 32), strings.Repeat("a", 32)
			if err := os.MkdirAll(filepath.Join(root, "logs"), 0o755); err != nil {
				t.Fatal(err)
			}
			bLogs := filepath.Join(tmp, "b-logs")
			isolateActionRegistries(t, bLogs)
			fakeActionPopen(t, 7)
			t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "b"))
			if _, err := spawnHermesAction(context.Background(), []string{"update"}, "hermes-update", map[string]string{"ICEBATH_ACTION_ID": bID}); err != nil {
				t.Fatal(err)
			}
			for _, w := range []struct{ profile, id, outcome string }{{"b", bID, tc.bOutcome}, {"a", aID, "success"}} {
				if w.outcome != "" {
					writeUpdateReceipt(t, filepath.Join(root, "profiles", w.profile), w.id, w.outcome, nil)
				}
			}
			t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "b"))
			log := filepath.Join(bLogs, "hermes-update.log")
			switch tc.logShape {
			case "long":
				f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(strings.Repeat("build output\n", 2001)); err != nil {
					t.Fatal(err)
				}
				f.Close()
			case "rotated":
				if err := os.Remove(log); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bOutcome == "success" {
				writeTestFile(t, filepath.Join(root, "logs", "update.log"), "=== icebath-update completed "+bID+" ===\n")
			}
			clearActionRegistries(true)

			data := updateStatus(t, c)
			if data["running"] != false {
				t.Fatalf("running %v", data["running"])
			}
			wantExit, wantAction := any(nil), any(nil)
			if tc.bOutcome == "success" {
				wantExit, wantAction = int64(0), bID
			}
			if !pyrepr.Equal(data["exit_code"], wantExit) || !pyrepr.Equal(data["action_id"], wantAction) {
				t.Fatalf("exit %v action %v", data["exit_code"], data["action_id"])
			}
			wantReceipt := aID
			if tc.bOutcome != "" {
				wantReceipt = bID
			}
			if got := receiptOf(t, data)["action_id"]; got != wantReceipt {
				t.Fatalf("receipt action %v", got)
			}
		})
	}
}

func TestRootLatestReceiptOfAnotherProfileNeverCertifiesThisAction(t *testing.T) {
	for _, bOutcome := range []string{"", "failed", "success"} {
		t.Run("b-"+bOutcome, func(t *testing.T) {
			c := newSessionClient(t)
			tmp := t.TempDir()
			root := filepath.Join(tmp, "root")
			if err := os.MkdirAll(filepath.Join(root, "logs"), 0o755); err != nil {
				t.Fatal(err)
			}
			bID, aID := strings.Repeat("b", 32), strings.Repeat("a", 32)
			for _, w := range []struct{ profile, id, outcome string }{{"b", bID, bOutcome}, {"a", aID, "success"}} {
				if w.outcome != "" {
					writeUpdateReceipt(t, filepath.Join(root, "profiles", w.profile), w.id, w.outcome, nil)
				}
			}
			t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "b"))
			if got := updaterecord.ReadLatestReceipt(context.Background())["action_id"]; got != aID {
				t.Fatalf("latest receipt action %v", got)
			}
			writeTestFile(t, filepath.Join(tmp, "hermes-update.log"), "=== hermes-update started 2026-08-17 11:19:34 "+bID+" ===\n")
			writeTestFile(t, filepath.Join(root, "logs", "update.log"), "=== icebath-update completed "+aID+" ===\n")
			isolateActionRegistries(t, tmp)

			data := updateStatus(t, c)
			wantExit := any(nil)
			if bOutcome == "success" {
				wantExit = int64(0)
			}
			if !pyrepr.Equal(data["exit_code"], wantExit) {
				t.Fatalf("exit %v", data["exit_code"])
			}
			if _, has := data["action_id"]; has {
				t.Fatalf("action_id %v", data["action_id"])
			}
			receipt := receiptOf(t, data)
			wantID, wantOutcome := aID, "success"
			if bOutcome != "" {
				wantID, wantOutcome = bID, bOutcome
			}
			if receipt["action_id"] != wantID || receipt["outcome"] != wantOutcome {
				t.Fatalf("receipt %v", receipt)
			}
		})
	}
}

func TestStatusReceiptCarriesOwedStepsOfACommittedUpdate(t *testing.T) {
	for _, debt := range []string{"none", "followups", "user_action"} {
		t.Run(debt, func(t *testing.T) {
			c := newSessionClient(t)
			tmp := t.TempDir()
			root, actionID := filepath.Join(tmp, "root"), strings.Repeat("c", 32)
			profileHome := filepath.Join(root, "profiles", "coder")
			writeUpdateReceipt(t, profileHome, actionID, "success", func(ctx context.Context) {
				switch debt {
				case "followups":
					updaterecord.RecordFollowup(ctx, "dependencies", "synthetic selected-interpreter sync failed", "the next launch or `icebath update` retries it")
					updaterecord.RecordFollowup(ctx, "config_migration", "disk full", "the next launch or `icebath update` retries it")
				case "user_action":
					updaterecord.RecordUserAction(ctx, "autostash", "re-apply the parked stash")
				}
			})
			t.Setenv("ICEBATH_HOME", profileHome)
			writeTestFile(t, filepath.Join(tmp, "hermes-update.log"), "=== hermes-update started 2026-08-17 11:19:34 "+actionID+" ===\n")
			isolateActionRegistries(t, tmp)

			data := updateStatus(t, c)
			receipt := receiptOf(t, data)
			var steps []any
			for _, f := range receipt["followups"].([]any) {
				steps = append(steps, f.(map[string]any)["step"])
			}
			wantSteps := []any(nil)
			if debt == "followups" {
				wantSteps = []any{"dependencies", "config_migration"}
			}
			if !slices.Equal(steps, wantSteps) {
				t.Fatalf("followups %v", steps)
			}
			wantUser := any(nil)
			if debt == "user_action" {
				wantUser = map[string]any{"step": "autostash", "reason": "re-apply the parked stash"}
			}
			if !reflect.DeepEqual(receipt["user_action"], wantUser) {
				t.Fatalf("user_action %v", receipt["user_action"])
			}
			wantExit := int64(0)
			if debt == "user_action" {
				wantExit = 1
			}
			if !pyrepr.Equal(data["exit_code"], wantExit) {
				t.Fatalf("exit %v", data["exit_code"])
			}
		})
	}
}

const receiptActionID = "dddddddddddddddddddddddddddddddd"

// Writes latest.json of the test home receipt store and returns the receipt
func writeLatestReceipt(t *testing.T, outcome string) *pyjson.Object {
	t.Helper()
	receipt := pyjson.ObjectOf(
		"schema", int64(1),
		"started_at", "2026-08-23T07:00:00+00:00",
		"finished_at", "2026-08-23T07:03:20+00:00",
		"argv", []any{"icebath", "update"},
		"pid", int64(12345),
		"outcome", outcome,
		"action_id", receiptActionID,
		"pre_update", pyjson.ObjectOf("sha", strings.Repeat("a", 40), "version", "0.20.4"),
		"post_update", pyjson.ObjectOf("sha", strings.Repeat("b", 40), "version", "0.20.5"),
		"steps", []any{pyjson.ObjectOf("name", "pre_update_backup", "ok", true, "detail", "", "at", "")},
		"skips", []any{},
		"gateway_restart", pyjson.NewObject(),
		"fleet", []any{pyjson.ObjectOf("profile", "default", "pid", int64(1), "code_sha", strings.Repeat("b", 40), "state", "current")},
	)
	raw, err := pyjson.Dumps(receipt)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "logs", "update_receipts", "latest.json"), raw)
	return receipt
}

func TestReceiptEndpointReturnsFullReceiptAndSummary(t *testing.T) {
	c := newSessionClient(t)
	receipt := writeLatestReceipt(t, "success")
	r := c.get("/api/icebath/update/receipt")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	full := data["receipt"].(map[string]any)
	steps, _ := receipt.Get("steps")
	if full["outcome"] != "success" || !pyrepr.Equal(full["steps"], pyjson.Plain(steps)) {
		t.Fatalf("receipt %v", full)
	}
	summary := data["summary"].(map[string]any)
	if summary["outcome"] != "success" || summary["pre_sha"] != strings.Repeat("a", 40) || summary["post_sha"] != strings.Repeat("b", 40) ||
		summary["post_version"] != "0.20.5" || !pyrepr.Equal(summary["fleet_states"], []any{"current"}) {
		t.Fatalf("summary %v", summary)
	}
}

func TestReceiptEndpoint404WhenNoReceipt(t *testing.T) {
	c := newSessionClient(t)
	if r := c.get("/api/icebath/update/receipt"); r.Status != 404 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

// Action log of the receipt cases naming the receipt's action, with the registries empty
func clearReceiptCaseRegistries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "actions")
	writeTestFile(t, filepath.Join(dir, "hermes-update.log"), "=== hermes-update started 2026-08-23 07:00:00 "+receiptActionID+" ===\n")
	isolateActionRegistries(t, dir)
}

func TestStatusAttachesReceiptSummary(t *testing.T) {
	c := newSessionClient(t)
	writeLatestReceipt(t, "success")
	clearReceiptCaseRegistries(t)
	r := c.get("/api/actions/hermes-update/status")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := receiptOf(t, jsonObject(t, r))["outcome"]; got != "success" {
		t.Fatalf("outcome %v", got)
	}
}

func TestFinishedReceiptIsTheOutcomeWhenMarkerAndMemoryGone(t *testing.T) {
	c := newSessionClient(t)
	writeLatestReceipt(t, "success")
	clearReceiptCaseRegistries(t)
	data := jsonObject(t, c.get("/api/actions/hermes-update/status"))
	if data["running"] != false || !pyrepr.Equal(data["exit_code"], int64(0)) {
		t.Fatalf("data %v", data)
	}
}

func TestPartialReceiptMapsToExit1(t *testing.T) {
	c := newSessionClient(t)
	writeLatestReceipt(t, "partial")
	clearReceiptCaseRegistries(t)
	if got := jsonObject(t, c.get("/api/actions/hermes-update/status"))["exit_code"]; !pyrepr.Equal(got, int64(1)) {
		t.Fatalf("exit %v", got)
	}
}

func TestRunningReceiptProvesNothing(t *testing.T) {
	c := newSessionClient(t)
	writeLatestReceipt(t, "running")
	clearReceiptCaseRegistries(t)
	if got := jsonObject(t, c.get("/api/actions/hermes-update/status"))["exit_code"]; got != nil {
		t.Fatalf("exit %v", got)
	}
}

func TestNonUpdateActionsUntouched(t *testing.T) {
	c := newSessionClient(t)
	writeLatestReceipt(t, "success")
	clearReceiptCaseRegistries(t)
	if _, has := jsonObject(t, c.get("/api/actions/gateway-restart/status"))["receipt"]; has {
		t.Fatal("receipt attached to a non update action")
	}
}

// Commit build install root whose stamp names the build, with the dashboard project root and version cache on it
func commitBuildRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "app")
	writeTestFile(t, filepath.Join(root, "install-stamp.json"), `{"source": "commit-build", "distribution": "desktop-app", `+
		`"payload": "bundled", "updateMechanism": "external", "commit": "`+strings.Repeat("a", 40)+`", `+
		`"displayVersion": "1.2.3+gabcdef12", "baseVersion": "1.2.3", "channel": "canary"}`)
	t.Cleanup(pm.PatchRepoRoot(root))
	os.Unsetenv("ICEBATH_INSTALL_ROOT")
	updaterecord.ResetVersionInfoCache()
	t.Cleanup(updaterecord.ResetVersionInfoCache)
	setProjectRoot(t, root)
	return root
}

// Points the web server PROJECT_ROOT at a dir until the case ends
func setProjectRoot(t *testing.T, root string) {
	webStateMu.Lock()
	savedRoot := projectRoot
	projectRoot = root
	webStateMu.Unlock()
	t.Cleanup(func() {
		webStateMu.Lock()
		projectRoot = savedRoot
		webStateMu.Unlock()
	})
}

// Git checkout project root the install method probe reports as git
func gitProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	setProjectRoot(t, root)
}

// Source check replaced by a fixed status, recording the home and force of each call
func fakeUpdateCheck(t *testing.T, status func() *pyjson.Object) *[]string {
	seen := []string{}
	saved := UpdateCLI.CheckForUpdates
	UpdateCLI.CheckForUpdates = func(ctx context.Context, force bool) (*pyjson.Object, error) {
		seen = append(seen, home.Dir(ctx)+" "+fmt.Sprint(force))
		return status(), nil
	}
	t.Cleanup(func() { UpdateCLI.CheckForUpdates = saved })
	return &seen
}

func TestGitInstallReportsBehindCount(t *testing.T) {
	c := newSessionClient(t)
	t.Cleanup(hostinfo.PinContainer(false))
	gitProjectRoot(t)
	fakeUpdateCheck(t, func() *pyjson.Object { return pyjson.ObjectOf("behind", int64(5), "commits", []any{}) })

	r := c.get("/api/icebath/update/check")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	body := jsonObject(t, r)
	for _, k := range []string{"install_method", "current_version", "behind", "update_available", "can_apply", "update_command", "message"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("missing %s in %v", k, body)
		}
	}
	if body["install_method"] != "git" || !pyrepr.Equal(body["behind"], int64(5)) || body["update_available"] != true || body["can_apply"] != true {
		t.Fatalf("body %v", body)
	}
}

func TestReleaseMessageNamesOnlyAKnownVersion(t *testing.T) {
	for _, held := range []bool{true, false} {
		for _, version := range []any{nil, "", "1.2.3"} {
			t.Run(fmt.Sprint(held, version), func(t *testing.T) {
				c := newSessionClient(t)
				t.Cleanup(hostinfo.PinContainer(false))
				gitProjectRoot(t)
				fakeUpdateCheck(t, func() *pyjson.Object {
					behind, heldValue := int64(-1), any(nil)
					if held {
						behind, heldValue = 0, "downgrade"
					}
					return pyjson.ObjectOf("behind", behind, "held", heldValue, "rollback", !held, "sourceVersion", version,
						"channel", "preview", "commits", []any{})
				})
				r := c.get("/api/icebath/update/check")
				if r.Status != 200 {
					t.Fatalf("status %d %s", r.Status, r.Body)
				}
				body := jsonObject(t, r)
				release := "preview release"
				if v, _ := version.(string); v != "" {
					release += " v" + v
				}
				prefix := "This build is newer than " + release + "."
				if held {
					prefix = "This build is newer than " + release + ","
				}
				message, _ := body["message"].(string)
				if !strings.HasPrefix(message, prefix) || body["update_available"] == held || !pyrepr.Equal(body["source_version"], version) {
					t.Fatalf("body %v", body)
				}
				if held && body["held"] != "downgrade" {
					t.Fatalf("held %v", body["held"])
				}
				if !held && !strings.Contains(message, "Updating moves it back") {
					t.Fatalf("message %q", message)
				}
			})
		}
	}
}

func TestManagedRuntimeDashboardIsNotApplyable(t *testing.T) {
	c := newSessionClient(t)
	t.Cleanup(hostinfo.PinContainer(true))
	gitProjectRoot(t)
	isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
	if !dashboardLocalUpdateManagedExternally(context.Background()) {
		t.Fatal("dashboard not managed externally")
	}
	body := jsonObject(t, c.get("/api/icebath/update/check"))
	if body["install_method"] != "managed-runtime" || body["can_apply"] != false || body["update_available"] != false || body["behind"] != nil {
		t.Fatalf("body %v", body)
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "managed outside this dashboard") || body["update_command"] != "" {
		t.Fatalf("body %v", body)
	}
	refused := jsonObject(t, c.send("POST", "/api/icebath/update", nil))
	if refused["ok"] != false || refused["error"] != "dashboard_update_managed_externally" || refused["update_command"] != "" {
		t.Fatalf("refused %v", refused)
	}
}

func TestForcedUpdateCheckRunsInTheNamedProfilesScope(t *testing.T) {
	c := newSessionClient(t)
	launch := os.Getenv("ICEBATH_HOME")
	beta := filepath.Join(launch, "profiles", "worker_beta")
	for _, h := range []string{launch, beta} {
		writeTestFile(t, filepath.Join(h, "memories", "MEMORY.md"), "memory of "+filepath.Base(h)+"\n")
		writeTestFile(t, filepath.Join(h, "memories", "USER.md"), "user of "+filepath.Base(h)+"\n")
		writeTestFile(t, filepath.Join(h, "webhook_subscriptions.json"), `{"alerts": {"secret": "s", "events": []}}`)
		writeTestFile(t, filepath.Join(h, "config.yaml"), "hooks:\n  pre_tool_call:\n  - command: /bin/true\nproxy:\n  label: "+filepath.Base(h)+"\n")
		writeTestFile(t, filepath.Join(h, ".update_check"), "cached\n")
	}
	writeTestFile(t, filepath.Join(beta, ".env"), "BETA=1\n")
	t.Cleanup(hostinfo.PinContainer(false))
	gitProjectRoot(t)
	seen := fakeUpdateCheck(t, func() *pyjson.Object { return pyjson.ObjectOf("behind", int64(0), "commits", []any{}) })

	r := c.get("/api/icebath/update/check?force=true&profile=worker_beta")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if !pyrepr.Equal(jsonObject(t, r)["behind"], int64(0)) {
		t.Fatalf("body %s", r.Body)
	}
	if !slices.Equal(*seen, []string{beta + " true"}) {
		t.Fatalf("seen %v", *seen)
	}
}

func TestCommitBackendUpdateRoutesRefuseBeforeChecksOrSpawns(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprint(managed), func(t *testing.T) {
			c := newSessionClient(t)
			t.Setenv("ICEBATH_INSTALL_ROOT", "")
			commitBuildRoot(t)
			isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
			t.Cleanup(hostinfo.PinContainer(managed))
			if got := dashboardLocalUpdateManagedExternally(context.Background()); got != managed {
				t.Fatalf("managed externally %v, want %v", got, managed)
			}
			saved := actionPopen
			actionPopen = func([]string, string, map[string]string, *os.File, *syscall.SysProcAttr) (actionProc, error) {
				t.Error("no updater process")
				return nil, errors.New("no updater process")
			}
			savedCheck := UpdateCLI.CheckForUpdates
			UpdateCLI.CheckForUpdates = func(context.Context, bool) (*pyjson.Object, error) {
				t.Error("no update check")
				return nil, errors.New("no update check")
			}
			t.Cleanup(func() { actionPopen, UpdateCLI.CheckForUpdates = saved, savedCheck })
			cache := filepath.Join(os.Getenv("ICEBATH_HOME"), ".update_check")
			writeTestFile(t, cache, "source checkout cache")

			r := c.get("/api/icebath/update/check?force=true")
			if r.Status != 200 {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			data := jsonObject(t, r)
			if data["message"] != selfupdate.CommitBuildUpdateMessage || data["can_apply"] != false || data["current_version"] != "1.2.3+gabcdef12" {
				t.Fatalf("check %v", data)
			}
			if got := jsonObject(t, c.send("POST", "/api/icebath/update", nil))["message"]; got != selfupdate.CommitBuildUpdateMessage {
				t.Fatalf("update message %v", got)
			}
			if raw, err := os.ReadFile(cache); err != nil || string(raw) != "source checkout cache" {
				t.Fatalf("cache %q %v", raw, err)
			}
		})
	}
}

// Project root whose code stamp names the install method, with the container probe pinned outside a container
func installMethodRoot(t *testing.T, method string) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".install_method"), method+"\n")
	setProjectRoot(t, root)
	t.Cleanup(hostinfo.PinContainer(false))
}

// Popen failing the case when an action would spawn
func forbidActionSpawn(t *testing.T, why string) {
	saved := actionPopen
	actionPopen = func([]string, string, map[string]string, *os.File, *syscall.SysProcAttr) (actionProc, error) {
		t.Error(why)
		return nil, errors.New(why)
	}
	t.Cleanup(func() { actionPopen = saved })
}

func TestGatewayDrainBadAction400(t *testing.T) {
	c := newSessionClient(t)
	if r := c.send("POST", "/api/gateway/drain", map[string]any{"action": "explode"}); r.Status != 400 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
}

func TestUpdateHermesReturnsDockerGuidanceWithoutSpawning(t *testing.T) {
	c := newSessionClient(t)
	installMethodRoot(t, "docker")
	isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
	forbidActionSpawn(t, "docker update guard should not spawn icebath update")

	r := c.send("POST", "/api/icebath/update", nil)
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["ok"] != false || data["name"] != "hermes-update" || data["pid"] != nil || data["error"] != "docker_update_unsupported" {
		t.Fatalf("data %v", data)
	}
	status := c.get("/api/actions/hermes-update/status")
	if status.Status != 200 {
		t.Fatalf("status %d %s", status.Status, status.Body)
	}
	sd := jsonObject(t, status)
	if sd["running"] != false || !pyrepr.Equal(sd["exit_code"], int64(1)) || sd["pid"] != nil {
		t.Fatalf("status %v", sd)
	}
}

func TestUpdateHermesReturnsAptGuidanceWithoutSpawning(t *testing.T) {
	c := newSessionClient(t)
	installMethodRoot(t, "apt")
	isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
	forbidActionSpawn(t, "APT-managed update guard should not spawn icebath update")

	r := c.send("POST", "/api/icebath/update", nil)
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	if data["ok"] != false || data["pid"] != nil || data["error"] != "apt_update_required" || !pyrepr.Truthy(data["update_command"]) {
		t.Fatalf("data %v", data)
	}
	check := c.get("/api/icebath/update/check")
	if check.Status != 200 {
		t.Fatalf("status %d %s", check.Status, check.Body)
	}
	cd := jsonObject(t, check)
	if cd["install_method"] != "apt" || cd["can_apply"] != false || cd["update_command"] != data["update_command"] {
		t.Fatalf("check %v", cd)
	}
}

func TestUpdateHermesReturnsPackageManagerGuidanceWithoutSpawning(t *testing.T) {
	for method, want := range map[string][2]string{
		"homebrew":   {"brew upgrade --cask icebath", "Icebath is managed by Homebrew; run `brew upgrade --cask icebath`."},
		"chocolatey": {"choco upgrade icebath", "Icebath is managed by Chocolatey; run `choco upgrade icebath`."},
		"dpkg":       {"sudo apt update && sudo apt install --only-upgrade icebath", "Icebath is managed by its Debian package; run `sudo apt update && sudo apt install --only-upgrade icebath`."},
	} {
		t.Run(method, func(t *testing.T) {
			c := newSessionClient(t)
			installMethodRoot(t, method)
			isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
			forbidActionSpawn(t, "a package manager install must not spawn icebath update")
			r := c.send("POST", "/api/icebath/update", nil)
			if r.Status != 200 {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			data := jsonObject(t, r)
			if data["ok"] != false || data["pid"] != nil || data["error"] != "update_not_in_place" || data["update_command"] != want[0] {
				t.Fatalf("data %v", data)
			}
			check := c.get("/api/icebath/update/check")
			cd := jsonObject(t, check)
			if cd["install_method"] != method || cd["can_apply"] != false || cd["update_command"] != want[0] || cd["message"] != want[1] {
				t.Fatalf("check %v", cd)
			}
		})
	}
}

func TestUpdateHermesSpawnsWithActionID(t *testing.T) {
	c := newSessionClient(t)
	installMethodRoot(t, "git")
	isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
	var calls [][]string
	var childIDs []string
	saved := actionPopen
	actionPopen = func(argv []string, _ string, env map[string]string, _ *os.File, _ *syscall.SysProcAttr) (actionProc, error) {
		calls = append(calls, argv)
		childIDs = append(childIDs, env["ICEBATH_ACTION_ID"])
		return &fakeActionProc{pid: 12345}, nil
	}
	t.Cleanup(func() { actionPopen = saved })

	r := c.send("POST", "/api/icebath/update", nil)
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	data := jsonObject(t, r)
	actionID, _ := data["action_id"].(string)
	if !actionIDRE.MatchString(actionID) {
		t.Fatalf("action id %q", actionID)
	}
	r.expect(t, 200, `{"ok":true,"pid":12345,"name":"hermes-update","action_id":"`+actionID+`"}`)
	if len(calls) != 1 || calls[0][len(calls[0])-1] != "update" || !slices.Equal(childIDs, []string{actionID}) {
		t.Fatalf("calls %v ids %v", calls, childIDs)
	}
	actionMu.Lock()
	recorded := actionIDs["hermes-update"]
	actionMu.Unlock()
	if recorded != actionID {
		t.Fatalf("recorded action id %q", recorded)
	}
}

func TestUpdateHermesReusesRunningAction(t *testing.T) {
	c := newSessionClient(t)
	installMethodRoot(t, "git")
	isolateActionRegistries(t, filepath.Join(t.TempDir(), "logs"))
	forbidActionSpawn(t, "must not spawn a duplicate update")
	actionMu.Lock()
	actionProcs["hermes-update"] = &fakeActionProc{pid: 24680}
	actionIDs["hermes-update"] = strings.Repeat("b", 32)
	actionMu.Unlock()

	r := c.send("POST", "/api/icebath/update", nil)
	r.expect(t, 200, `{"ok":true,"pid":24680,"name":"hermes-update","already_running":true,"action_id":"`+strings.Repeat("b", 32)+`"}`)
}
