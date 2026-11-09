package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

const maxQuietNotifyRounds = 8

// KanbanWorkerExitTrailer prefixes the exit code line a Kanban worker writes to its log
const KanbanWorkerExitTrailer = "[kanban-worker-exit] rc="

// TurnReportFileEnv names the spawner's turn report path a quiet child pops at startup
const TurnReportFileEnv = "ICEBATH_QUIET_TURN_REPORT_FILE"

// ReportedTurnExitGraceSeconds is how long a reported child may keep lingering before its spawner books it
const ReportedTurnExitGraceSeconds = 2.0

// singleQueryExit unwinds a one shot turn to Main with its process exit code
type singleQueryExit struct{ code int }

// Ends a one shot turn with the exit code, a Kanban worker first writing the exit trailer to its log
func exitSingleQuery(code int) {
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		fmt.Fprintf(stderrWriter(), "\n%s%d\n", KanbanWorkerExitTrailer, code)
	}
	panic(singleQueryExit{code: code})
}

// Reads and removes the spawner's turn report path so subprocesses started during the turn do not inherit it
func takeTurnReportPath() string {
	path := os.Getenv(TurnReportFileEnv)
	_ = os.Unsetenv(TurnReportFileEnv)
	return path
}

// Atomically records pid, exit code, error and reply at the path, nothing without a path, never failing
func writeTurnReport(path string, exitCode int, errText, reply string) {
	if path == "" {
		return
	}
	record := map[string]any{"pid": os.Getpid(), "exit_code": exitCode, "error": errText, "reply": reply}
	if err := config.AtomicJSONWrite(path, record); err != nil {
		return
	}
	_ = os.Chmod(path, 0o600)
}

// Returns the child's turn report, nil while absent, unreadable or written by another process
func readTurnReport(path string, pid int) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var record map[string]any
	if json.Unmarshal(raw, &record) != nil || record == nil {
		return nil
	}
	if got, ok := record["pid"].(float64); !ok || int(got) != pid {
		return nil
	}
	return record
}

// ReportedTurn is the booked outcome of one quiet delivery child
type ReportedTurn struct {
	Args       []string
	ReturnCode int
	Stdout     string
	Stderr     string
}

// TurnTimeoutError reports a delivery child whose turn never ended within the timeout
type TurnTimeoutError struct {
	Args    []string
	Timeout float64
}

func (e *TurnTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %g seconds", strings.Join(e.Args, " "), e.Timeout)
}

// Runs one quiet delivery child, booking it from its exit or from its turn report while it lingers
func runReportedTurn(ctx context.Context, argv []string, env map[string]string, reportPath string, timeout float64, exitGrace *float64, cwd string) (ReportedTurn, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Dir = cwd
	cmd.Env = make([]string, 0, len(env)+1)
	for k, v := range env {
		if k != TurnReportFileEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, TurnReportFileEnv+"="+reportPath)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if err := cmd.Start(); err != nil {
		return ReportedTurn{Args: argv}, err
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	var report map[string]any
	finished := func() ReportedTurn {
		code := 0
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		return ReportedTurn{Args: argv, ReturnCode: code, Stdout: strings.ToValidUTF8(outBuf.String(), "�"), Stderr: strings.ToValidUTF8(errBuf.String(), "�")}
	}
	for {
		wait := 250 * time.Millisecond
		if report != nil && exitGrace != nil {
			wait = time.Duration(*exitGrace * float64(time.Second))
		}
		select {
		case <-done:
			return finished(), nil
		case <-time.After(wait):
		}
		if report != nil && exitGrace != nil {
			break
		}
		if r := readTurnReport(reportPath, cmd.Process.Pid); r != nil {
			report = r
		}
		if !time.Now().Before(deadline) {
			if report != nil {
				break
			}
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			report = readTurnReport(reportPath, cmd.Process.Pid)
			if report != nil {
				break
			}
			return ReportedTurn{Args: argv}, &TurnTimeoutError{Args: argv, Timeout: timeout}
		}
	}
	code, _ := report["exit_code"].(float64)
	reply, _ := report["reply"].(string)
	errText, _ := report["error"].(string)
	return ReportedTurn{Args: argv, ReturnCode: int(code), Stdout: reply, Stderr: errText}, nil
}

// Binds the approval and session key to this quiet session until the returned release runs
func bindQuietSessionKey(env *tools.Env, sessionID string) func() {
	if sessionID == "" {
		sessionID = "default"
	}
	tools.SetCurrentSessionKey(env, sessionID)
	return func() { tools.ResetCurrentSessionKey(env) }
}

// Reports whether every drained event is an automatic diagnostic the CLI policy suppresses
func diagnosticOnlyWakeMuted(ctx context.Context, events []map[string]any) bool {
	if len(events) == 0 {
		return false
	}
	for _, e := range events {
		if e != nil && !agent.DiagnosticProcessEvent(e) {
			return false
		}
	}
	return !config.WarningNotificationsEnabled(ctx, "cli", nil)
}

// Returns the one linger budget a quiet run shares between the drain loop and the finalize pass
func quietNotifyLingerSeconds(env *tools.Env) float64 {
	return tools.OneshotCompletionWaitSeconds(env)
}

// Lingers for notify_on_complete work and runs owned completion texts as follow up turns under one budget
func continueQuietNotifyCompletions(ctx context.Context, env *tools.Env, sessionID string, runTurn func(string) map[string]any,
	owns func(map[string]any) bool, maxRounds int, lingerBudget *float64) map[string]any {
	var last map[string]any
	registry := tools.DefaultProcessRegistry()
	budget := quietNotifyLingerSeconds(env)
	if lingerBudget != nil {
		budget = *lingerBudget
	}
	deadline := time.Now().Add(time.Duration(max(budget, 0) * float64(time.Second)))
	for range maxRounds {
		remaining := max(time.Until(deadline).Seconds(), 0)
		wait := registry.WaitForPendingCompletions(ctx, env, "", &remaining, oneshotPendingCompletionsPollIntervalSec)
		var drained []tools.DrainedNotification
		for _, item := range registry.DrainNotifications(env, sessionID, owns, false) {
			claimID, ok, err := tools.ClaimEventDelivery(env, item.Event, "cli-quiet")
			if err != nil || !ok {
				continue
			}
			_ = tools.CompleteEventDelivery(env, item.Event, claimID)
			drained = append(drained, item)
		}
		var texts []string
		var textEvents []map[string]any
		for _, item := range drained {
			if item.Text != "" {
				texts = append(texts, item.Text)
				textEvents = append(textEvents, item.Event)
			}
		}
		if len(texts) > 0 {
			follow := runTurn(strings.Join(texts, "\n\n"))
			if !diagnosticOnlyWakeMuted(ctx, textEvents) {
				last = follow
			}
		}
		if len(wait.TimedOut) > 0 {
			break
		}
		if len(texts) == 0 {
			return last
		}
	}
	return last
}
