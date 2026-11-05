package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const ouHelperEnv = "ICEBATH_TEST_OU_TAKEOVER"

// Takeover child behaviour, active only when re-executed by ouUseTestTakeover
func TestOUTakeoverHelper(t *testing.T) {
	if os.Getenv(ouHelperEnv) == "" {
		return
	}
	os.Exit(ouTakeoverChild())
}

// Checks the request a takeover child receives, records the run and acknowledges as the test asked
func ouTakeoverChild() int {
	contextPath, resultPath := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	data, err := os.ReadFile(contextPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 90
	}
	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 91
	}
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return 92
	}
	if _, ok := os.LookupEnv("PYTHONPATH"); ok {
		return fail("PYTHONPATH reached the takeover")
	}
	if _, ok := os.LookupEnv("UV_INDEX_URL"); ok {
		return fail("UV_INDEX_URL reached the takeover")
	}
	if request["home"] != os.Getenv("ICEBATH_HOME") {
		return fail("home %v env %q", request["home"], os.Getenv("ICEBATH_HOME"))
	}
	if os.Getenv("ICEBATH_UPDATE_HANDOFF_PID") != os.Getenv(ouHelperEnv+"_PARENT") {
		return fail("handoff pid %q", os.Getenv("ICEBATH_UPDATE_HANDOFF_PID"))
	}
	home := request["home"].(string)
	_, cleanup := request["stopped_serves"]
	runs, err := os.OpenFile(filepath.Join(home, "runs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fail("%v", err)
	}
	defer runs.Close()
	ack, status := os.Getenv(ouHelperEnv+"_ACK"), os.Getenv(ouHelperEnv+"_STATUS")
	if cleanup {
		var keys []string
		for key := range request {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"argv", "home", "root", "stopped_serves"}) {
			return fail("cleanup keys %v", keys)
		}
		serves := request["stopped_serves"].(map[string]any)
		if serves["pending"] != true || serves["entries"].([]any)[0].(map[string]any)["profile"] != "work profile" {
			return fail("stopped serves %v", serves)
		}
		runs.WriteString("cleanup\n")
		ack, status = os.Getenv(ouHelperEnv+"_CLEANUP_ACK"), os.Getenv(ouHelperEnv+"_CLEANUP_STATUS")
	} else {
		want := map[string]any{"restart_update": false, "desktop": nil, "pre_update_snapshot_id": nil,
			"pre_update_version": nil, "gateway_mode": true, "assume_yes": false, "windows_resume": nil}
		for key, value := range want {
			if got, ok := request[key]; !ok || got != value {
				return fail("request %s %v want %v", key, got, value)
			}
		}
		receipt, _ := request["receipt"].(map[string]any)
		plan, _ := request["plan"].(map[string]any)
		if receipt["update_id"] != "old-correlation" || plan["install_method"] != "git" {
			return fail("receipt %v plan %v", request["receipt"], request["plan"])
		}
		runs.WriteString("child\n")
	}
	if ack != "" {
		if err := os.WriteFile(resultPath, []byte(ack), 0o644); err != nil {
			return fail("%v", err)
		}
	}
	code, _ := strconv.Atoi(status)
	return code
}

// Points the takeover spawn at this test binary and resets the once per process takeover memo
func ouUseTestTakeover(t *testing.T, env map[string]string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "isolated home 日本 café")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("PYTHONPATH", "/not/the/new/source")
	t.Setenv("UV_INDEX_URL", "http://127.0.0.1:9/simple")
	t.Setenv("ICEBATH_UPDATE_HANDOFF_PID", "")
	os.Unsetenv("ICEBATH_UPDATE_HANDOFF_PID")
	t.Setenv(ouHelperEnv, "1")
	t.Setenv(ouHelperEnv+"_PARENT", strconv.Itoa(os.Getpid()))
	for key, value := range env {
		t.Setenv(ouHelperEnv+"_"+key, value)
	}
	previousArgv, previousArgs := takeoverArgv, os.Args
	takeoverArgv = func() ([]string, error) {
		return []string{previousArgs[0], "-test.run=^TestOUTakeoverHelper$", "-test.count=1"}, nil
	}
	os.Args = []string{previousArgs[0], "update", "--gateway"}
	takeoverMemo.Lock()
	takeoverMemo.set, takeoverMemo.code = false, 0
	takeoverMemo.Unlock()
	t.Cleanup(func() {
		takeoverArgv, os.Args = previousArgv, previousArgs
		takeoverMemo.Lock()
		takeoverMemo.set, takeoverMemo.code = false, 0
		takeoverMemo.Unlock()
		SetUpdateReceiptHooks(UpdateReceiptHooks{})
		SetUpdateLockHooks(UpdateLockHooks{})
	})
	return home
}

// Exit code a returned SystemExit carries
func ouExitCode(t *testing.T, err error) int {
	t.Helper()
	var exit *SystemExit
	if !errors.As(err, &exit) {
		t.Fatalf("want SystemExit, got %v", err)
	}
	return exit.Code
}

func TestOUTakeoverWaitsPropagatesStatusAndNeverReentersOldCode(t *testing.T) {
	for _, status := range []int{0, 7} {
		for _, encoding := range []string{"utf-8", "utf-8-sig"} {
			t.Run(fmt.Sprintf("%d-%s", status, encoding), func(t *testing.T) {
				ack := `{"resume_handled": true, "receipt_handled": true}`
				if encoding == "utf-8-sig" {
					ack = "\xef\xbb\xbf" + ack
				}
				home := ouUseTestTakeover(t, map[string]string{"STATUS": strconv.Itoa(status), "ACK": ack})
				cleared := 0
				SetUpdateReceiptHooks(UpdateReceiptHooks{
					Current: func() map[string]any {
						return map[string]any{"update_id": "old-correlation", "outcome": "running", "plan": map[string]any{"install_method": "git"}}
					},
					Clear: func() { cleared++ },
				})
				var stdout, stderr bytes.Buffer
				ctx := WithCLIStderr(withSyncOutput(context.Background(), &stdout), &stderr)
				first := stopForRelaunch(ctx, false)
				second := stopForRelaunch(ctx, false)
				if got := ouExitCode(t, first); got != status {
					t.Fatalf("first status %d want %d, stderr %s", got, status, stderr.String())
				}
				if got := ouExitCode(t, second); got != status {
					t.Fatalf("second status %d want %d", got, status)
				}
				runs, err := os.ReadFile(filepath.Join(home, "runs"))
				if err != nil || string(runs) != "child\n" {
					t.Fatalf("runs %q %v, stderr %s", runs, err, stderr.String())
				}
				if cleared != 1 {
					t.Fatalf("receipt cleared %d times", cleared)
				}
				if !strings.Contains(stdout.String(), "-> Handing off to the new updater (package manager) for the rest of this update...") {
					t.Fatalf("stdout %q", stdout.String())
				}
				if strings.Contains(stderr.String(), "run `icebath` again") || strings.Contains(stderr.String(), "failed") {
					t.Fatalf("stderr %q", stderr.String())
				}
			})
		}
	}
}

func TestOUAtexitRecoversOnlyStoppedServesAfterCachedUpdate(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		for _, cleanupStatus := range []int{0, 9} {
			t.Run(fmt.Sprintf("%v-%d", acknowledged, cleanupStatus), func(t *testing.T) {
				home := ouUseTestTakeover(t, map[string]string{"STATUS": "7", "ACK": "{}",
					"CLEANUP_STATUS": strconv.Itoa(cleanupStatus), "CLEANUP_ACK": fmt.Sprintf(`{"serves_handled": %v}`, acknowledged)})
				SetUpdateReceiptHooks(UpdateReceiptHooks{Current: func() map[string]any {
					return map[string]any{"update_id": "old-correlation", "plan": map[string]any{"install_method": "git"}}
				}})
				var stdout, stderr bytes.Buffer
				ctx := WithCLIStderr(withSyncOutput(context.Background(), &stdout), &stderr)
				token := NewOrderedObject()
				token.Set("pending", true)
				entry := NewOrderedObject()
				for _, kv := range [][2]any{{"purpose", "serve"}, {"profile", "work profile"}, {"host", "127.0.0.1"}, {"port", 8119}} {
					entry.Set(kv[0].(string), kv[1])
				}
				token.Set("entries", []any{entry})
				if got := ouExitCode(t, stopForRelaunch(ctx, false)); got != 7 {
					t.Fatalf("update status %d, stderr %s", got, stderr.String())
				}
				if err := RelaunchStoppedServes(ctx, token); err != nil {
					t.Fatal(err)
				}
				if pending, _ := token.Get("pending"); pending == false {
					if err := RelaunchStoppedServes(ctx, token); err != nil {
						t.Fatal(err)
					}
				}
				runs, _ := os.ReadFile(filepath.Join(home, "runs"))
				if string(runs) != "child\ncleanup\n" {
					t.Fatalf("runs %q, stderr %s", runs, stderr.String())
				}
				if pending, _ := token.Get("pending"); pending != !acknowledged {
					t.Fatalf("pending %v acknowledged %v", pending, acknowledged)
				}
				if got, want := strings.Contains(stderr.String(), "restart the affected"), !acknowledged || cleanupStatus != 0; got != want {
					t.Fatalf("stderr %q", stderr.String())
				}
			})
		}
	}
}

func TestOUIncompleteHandoffRecordsStopWithoutChild(t *testing.T) {
	home := ouUseTestTakeover(t, map[string]string{"STATUS": "0"})
	var reasons []string
	SetUpdateReceiptHooks(UpdateReceiptHooks{RecordStopReason: func(reason string) { reasons = append(reasons, reason) }})
	var stderr bytes.Buffer
	ctx := WithCLIStderr(withSyncOutput(context.Background(), &bytes.Buffer{}), &stderr)
	if got := ouExitCode(t, stopForRelaunch(ctx, true)); got != 1 {
		t.Fatalf("status %d", got)
	}
	if !reflect.DeepEqual(reasons, []string{"old_version_handoff"}) {
		t.Fatalf("reasons %v", reasons)
	}
	if want := "You're updating from an older version of icebath. To complete this update, run `icebath update` again.\n"; stderr.String() != want {
		t.Fatalf("stderr %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a child ran: %v", err)
	}
}

func TestOUInvalidAcknowledgementFailsTheTakeover(t *testing.T) {
	cases := map[string]string{
		"[1]":   "Update takeover failed: invalid update takeover acknowledgement\n",
		"{oops": "Update takeover failed: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)\n",
	}
	for ack, want := range cases {
		t.Run(ack, func(t *testing.T) {
			ouUseTestTakeover(t, map[string]string{"STATUS": "0", "ACK": ack})
			SetUpdateReceiptHooks(UpdateReceiptHooks{Current: func() map[string]any {
				return map[string]any{"update_id": "old-correlation", "plan": map[string]any{"install_method": "git"}}
			}})
			var stderr bytes.Buffer
			ctx := WithCLIStderr(withSyncOutput(context.Background(), &bytes.Buffer{}), &stderr)
			if got := ouExitCode(t, stopForRelaunch(ctx, false)); got != 1 {
				t.Fatalf("status %d", got)
			}
			if stderr.String() != want {
				t.Fatalf("stderr %q want %q", stderr.String(), want)
			}
			if got := ouExitCode(t, stopForRelaunch(ctx, false)); got != 1 {
				t.Fatalf("memo status %d", got)
			}
		})
	}
}
