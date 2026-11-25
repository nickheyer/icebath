//go:build !icebath_core && !icebath_noextras

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
	runs, err := os.OpenFile(filepath.Join(home, "runs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fail("%v", err)
	}
	defer runs.Close()
	ack, status := os.Getenv(ouHelperEnv+"_ACK"), os.Getenv(ouHelperEnv+"_STATUS")
	want := map[string]any{"restart_update": false, "pre_update_snapshot_id": nil,
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
	if ack != "" {
		if err := os.WriteFile(resultPath, []byte(ack), 0o644); err != nil {
			return fail("%v", err)
		}
	}
	code, _ := strconv.Atoi(status)
	return code
}

// Points the takeover spawn at this test binary
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
	previousArgv := takeoverArgv
	takeoverArgv = func() ([]string, error) {
		return []string{os.Args[0], "-test.run=^TestOUTakeoverHelper$", "-test.count=1"}, nil
	}
	t.Cleanup(func() {
		takeoverArgv = previousArgv
		SetUpdateReceiptHooks(UpdateReceiptHooks{})
		SetUpdateLockHooks(UpdateLockHooks{})
	})
	return home
}

// Post swap hand off file of a shipped updater whose payload carries the fields the takeover child checks
func ouHandoff(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handoff.json")
	payload := `{"pre_update_snapshot_id": null, "pre_update_version": null, "gateway_mode": true, ` +
		`"plan": {"install_method": "git"}, "receipt": {"update_id": "old-correlation", "outcome": "running", "plan": {"install_method": "git"}}}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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

func TestOUTakeoverWaitsAndPropagatesStatus(t *testing.T) {
	for _, status := range []int{0, 7} {
		for _, encoding := range []string{"utf-8", "utf-8-sig"} {
			t.Run(fmt.Sprintf("%d-%s", status, encoding), func(t *testing.T) {
				ack := `{"resume_handled": true, "receipt_handled": true}`
				if encoding == "utf-8-sig" {
					ack = "\xef\xbb\xbf" + ack
				}
				home := ouUseTestTakeover(t, map[string]string{"STATUS": strconv.Itoa(status), "ACK": ack})
				handoff := ouHandoff(t)
				var stdout bytes.Buffer
				got, err := ContinueLegacyPostSwap(withSyncOutput(context.Background(), &stdout), handoff, []string{"--gateway"})
				if err != nil || got != status {
					t.Fatalf("status %d %v want %d", got, err, status)
				}
				runs, err := os.ReadFile(filepath.Join(home, "runs"))
				if err != nil || string(runs) != "child\n" {
					t.Fatalf("runs %q %v", runs, err)
				}
				if _, err := os.Stat(handoff); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the hand off file was left behind: %v", err)
				}
				if !strings.Contains(stdout.String(), "-> Handing off to the new updater (package manager) for the rest of this update...") {
					t.Fatalf("stdout %q", stdout.String())
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
	if got := ouExitCode(t, StopForRelaunchIncomplete(ctx)); got != 1 {
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
		"[1]":   "invalid update takeover acknowledgement",
		"{oops": "invalid update takeover acknowledgement: json: invalid character 'o' looking for beginning of object key string at line 1, column 2",
	}
	for ack, want := range cases {
		t.Run(ack, func(t *testing.T) {
			ouUseTestTakeover(t, map[string]string{"STATUS": "0", "ACK": ack})
			_, err := ContinueLegacyPostSwap(withSyncOutput(context.Background(), &bytes.Buffer{}), ouHandoff(t), []string{"--gateway"})
			if err == nil || err.Error() != want {
				t.Fatalf("error %v want %q", err, want)
			}
		})
	}
}
