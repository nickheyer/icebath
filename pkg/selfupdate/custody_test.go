package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

type cuEvent []any

// Kernel calls of the join launcher recorded against a fake leader whose job reports stray processes
type cuFakeJoin struct {
	events  *[]cuEvent
	left    int
	placed  bool
	rebind  bool
	stderr  bytes.Buffer
	handles map[string]uintptr
}

func (f *cuFakeJoin) rec(e ...any) { *f.events = append(*f.events, cuEvent(e)) }

func (f *cuFakeJoin) Assign(job, process uintptr) bool {
	f.rec("AssignProcessToJobObject", job, process)
	if job != 11 || process != 33 {
		return true
	}
	f.placed = f.rebind
	return f.rebind
}
func (f *cuFakeJoin) CurrentProcess() uintptr { f.rec("GetCurrentProcess"); return 7 }
func (f *cuFakeJoin) LastError() int          { return 0 }
func (f *cuFakeJoin) CreateJob() uintptr      { f.rec("CreateJobObjectW"); return 22 }
func (f *cuFakeJoin) SetKillOnClose(job uintptr) bool {
	f.rec("SetInformationJobObject", job)
	return true
}
func (f *cuFakeJoin) InJob(process, job uintptr) bool {
	f.rec("IsProcessInJob", process, job)
	return f.placed
}
func (f *cuFakeJoin) CloseHandle(handle uintptr)         { f.rec("CloseHandle", handle) }
func (f *cuFakeJoin) Resume(process uintptr) int         { f.rec("NtResumeProcess", process); return 0 }
func (f *cuFakeJoin) Terminate(job uintptr, code uint32) { f.rec("TerminateJobObject", job, code) }
func (f *cuFakeJoin) ActiveProcesses(job uintptr) (uint32, bool) {
	f.rec("QueryInformationJobObject", job)
	active := uint32(0)
	if f.left > 0 {
		active = 1
	}
	f.left--
	return active, true
}
func (f *cuFakeJoin) Stderr() io.Writer { return &f.stderr }
func (f *cuFakeJoin) StartSuspended(argv []string) (joinChild, error) {
	f.rec("Popen", strings.Join(argv, " "))
	return cuLeader{f}, nil
}

type cuLeader struct{ f *cuFakeJoin }

func (l cuLeader) Handle() uintptr { return 33 }
func (l cuLeader) Wait() int       { l.f.rec("leader exited"); return 3 }
func (l cuLeader) Kill()           { l.f.rec("kill") }

// Runs the real join launcher against the recording kernel and returns the ordered events
func cuRunJoinLauncher(t *testing.T, strayPolls int, escaped bool, rebind bool) []cuEvent {
	t.Helper()
	var events []cuEvent
	fake := &cuFakeJoin{events: &events, left: strayPolls, placed: !escaped, rebind: rebind}
	report := filepath.Join(t.TempDir(), "report.txt")
	code := joinJobMain([]string{"11", report, "npm", "run", "build"}, fake)
	events = append(events, cuEvent{"exit", code})
	return events
}

func cuIndex(t *testing.T, events []cuEvent, name string, args ...any) int {
	t.Helper()
	for i, e := range events {
		if e[0] != name || len(e) < 1+len(args) {
			continue
		}
		match := true
		for j, a := range args {
			if fmt.Sprint(e[1+j]) != fmt.Sprint(a) {
				match = false
			}
		}
		if match {
			return i
		}
	}
	t.Fatalf("no event %s %v in %v", name, args, events)
	return -1
}

func cuLast(events []cuEvent) string { return fmt.Sprint(events[len(events)-1]) }

func TestCustodyTheWindowsBuildLauncherReapsTheCommandTreeBeforeItReturns(t *testing.T) {
	events := cuRunJoinLauncher(t, 2, false, true)
	if cuIndex(t, events, "AssignProcessToJobObject", 22, 33) >= cuIndex(t, events, "NtResumeProcess") {
		t.Fatalf("the command ran before it was held in its own job: %v", events)
	}
	if cuIndex(t, events, "leader exited") >= cuIndex(t, events, "TerminateJobObject", 22) {
		t.Fatal(events)
	}
	var polls []int
	for i, e := range events {
		if e[0] == "TerminateJobObject" && e[1] != uintptr(22) {
			t.Fatalf("terminated the update job: %v", events)
		}
		if e[0] == "QueryInformationJobObject" {
			polls = append(polls, i)
		}
	}
	if len(polls) != 3 || polls[0] <= cuIndex(t, events, "TerminateJobObject", 22) {
		t.Fatalf("the launcher returned while a build descendant was still alive: %v", events)
	}
	if cuLast(events) != "[exit 3]" {
		t.Fatal(events)
	}
}

func TestCustodyABuildCommandThatStartsOutsideTheUpdateJobIsPutInItBeforeItRuns(t *testing.T) {
	events := cuRunJoinLauncher(t, 0, true, true)
	if cuIndex(t, events, "AssignProcessToJobObject", 11, 33) >= cuIndex(t, events, "NtResumeProcess") {
		t.Fatalf("the command ran before it was in the update job: %v", events)
	}
	if cuLast(events) != "[exit 3]" {
		t.Fatal(events)
	}
}

func TestCustodyABuildCommandTheUpdateJobWillNotTakeNeverRuns(t *testing.T) {
	events := cuRunJoinLauncher(t, 0, true, false)
	killed := false
	for _, e := range events {
		if e[0] == "NtResumeProcess" {
			t.Fatalf("the refused command ran: %v", events)
		}
		killed = killed || e[0] == "kill"
	}
	if !killed || cuLast(events) != fmt.Sprintf("[exit %d]", custodyRefusedExit) {
		t.Fatal(events)
	}
}

func TestCustodyTheWindowsBuildLauncherReturnsAtOnceWhenNothingWasLeft(t *testing.T) {
	events := cuRunJoinLauncher(t, 0, false, true)
	polls := 0
	for _, e := range events {
		if e[0] == "QueryInformationJobObject" {
			polls++
		}
	}
	if polls != 1 || cuLast(events) != "[exit 3]" {
		t.Fatal(events)
	}
}

func TestCustodyARefusedBuildJoinIsLoggedAndNotedInTheReceipt(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	cuNoServeHooks(t)
	logs := cuCaptureLogs(t)
	report := filepath.Join(t.TempDir(), "report.txt")
	note := custodyUnavailable + " (could not join the update job: 5); the command was not run"
	if err := os.WriteFile(report, []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := updaterecord.UpdateReceiptScope(context.Background())
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	if got := reportRefusedJoin(ctx, report, []string{"node", "scripts/build/tui.mjs"}); got == nil || *got != note {
		t.Fatalf("notice %v", got)
	}
	path := updaterecord.FinalizeUpdateReceipt(ctx, "success", nil, "")
	payload := cuReadReceipt(t, path)
	if !strings.Contains(logs.String(), note) || !strings.Contains(logs.String(), "tui.mjs") {
		t.Fatalf("warning not logged: %s", logs.String())
	}
	step := cuStep(payload, "update_custody")
	if step == nil || step["ok"] != false || !strings.Contains(fmt.Sprint(step["detail"]), note) {
		t.Fatalf("steps %v", payload["steps"])
	}
	if payload["outcome"] != "success" {
		t.Fatal(payload["outcome"])
	}
	if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the report file was left behind")
	}
	empty := filepath.Join(t.TempDir(), "joined.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := reportRefusedJoin(ctx, empty, []string{"node"}); got != nil {
		t.Fatalf("a joined child reported %q", *got)
	}
}

// Captures every slog record this test emits
func cuCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func cuReadReceipt(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Plain(loaded).(map[string]any)
}

// First receipt step with this name
func cuStep(payload map[string]any, name string) map[string]any {
	steps, _ := payload["steps"].([]any)
	for _, s := range steps {
		if step, ok := s.(map[string]any); ok && step["name"] == name {
			return step
		}
	}
	return nil
}

// Serve obligation hooks for a receipt that carries no plan and no manual serves
func cuNoServeHooks(t *testing.T) {
	updaterecord.SetServeObligationHooks(updaterecord.ServeObligationHooks{
		IsReminderRow: func(row any) bool {
			m, ok := row.(map[string]any)
			return ok && (m["kind"] == "serve" || m["kind"] == "dashboard") && m["supervisor"] == "manual-serve"
		},
		RetainReceiptManualServes: func(context.Context, map[string]any) []any { return []any{} },
	})
	t.Cleanup(installServeObligationHooks)
}
