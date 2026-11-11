package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// Hidden first argument that routes a binary to one restart safe cron payload
	ExternalWorkerEntry = "__icebath_cron_external_worker__"
	workerMarker        = "_ICEBATH_CRON_WORKER_BOOT"
)

var externalWorkerEntryDispatched atomic.Bool

// Places the worker command in a restart safe scope, swapped by tests like the patched process registry helper
var restartSafeChildArgv = tools.RestartSafeGatewayChildArgv

// Ledger row of one execution as the waiter reads it, swapped by tests like a patched get_execution
var getExecution = func(ctx context.Context, s *Store, executionID string) (map[string]any, error) {
	return openExecutionsLedger(s).Get(ctx, executionID)
}

// Blanket dead owner sweep the waiter falls back to, swapped by tests like a patched recover_interrupted_executions
var recoverInterruptedExecutions = func(ctx context.Context, s *Store) (int, error) {
	return openExecutionsLedger(s).RecoverInterrupted(ctx)
}

// Durable ownership transfer the worker wins before its acknowledgement, swapped by tests like a patched adopt_claimed_execution
var adoptClaimedExecution = func(ctx context.Context, s *Store, executionID string) (map[string]any, error) {
	return openExecutionsLedger(s).AdoptClaimed(ctx, executionID)
}

// Installs the persistent log handler of a home for a mode, assigned by the binary at startup
var SetupWorkerLogging func(h, mode string) error

// Registers the owning profile config hooks gateway startup registers, assigned by the binary at startup
var RegisterWorkerConfigHooks func(ctx context.Context, failureMessage string, level slog.Level)

// The waiter failed after the worker was spawned and the worker may own the execution
type externalWorkerPostHandoffError struct {
	msg   string
	cause error
}

func (e *externalWorkerPostHandoffError) Error() string { return e.msg }
func (e *externalWorkerPostHandoffError) Unwrap() error { return e.cause }

// Spawned worker the waiter watches, its exit observed by one reaping goroutine
type workerProcess interface {
	Wait(timeout time.Duration) (*int, bool)
	Poll() *int
}

type execWorker struct {
	done chan struct{}
	code int
}

// Starts reaping the worker at once so a worker left running after an early return never lingers as a zombie
func watchWorker(cmd *exec.Cmd) *execWorker {
	w := &execWorker{done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		w.code = processReturnCode(cmd.ProcessState)
		close(w.done)
	}()
	return w
}

// Exit status the way Popen reports it, a negative signal number for a killed worker
func processReturnCode(st *os.ProcessState) int {
	if st == nil {
		return -1
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return st.ExitCode()
}

func (w *execWorker) Wait(timeout time.Duration) (*int, bool) {
	select {
	case <-w.done:
		code := w.code
		return &code, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (w *execWorker) Poll() *int {
	select {
	case <-w.done:
		code := w.code
		return &code
	default:
		return nil
	}
}

// Runs one handed off payload and leaves through the binary's exit hooks when argv carries the entry
func DispatchExternalWorkerEntry(exit func(code int)) {
	externalWorkerEntryDispatched.Store(true)
	if len(os.Args) < 2 || os.Args[1] != ExternalWorkerEntry {
		return
	}
	_ = os.Unsetenv(workerMarker)
	fs := flag.NewFlagSet(ExternalWorkerEntry, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	payload := fs.String("external-worker-file", "", "")
	ack := fs.String("ack-file", "", "")
	if err := fs.Parse(os.Args[2:]); err != nil || *payload == "" || *ack == "" {
		fmt.Fprintln(os.Stderr, "usage: --external-worker-file EXTERNAL_WORKER_FILE --ack-file ACK_FILE")
		exit(2)
		return
	}
	if SetupWorkerLogging == nil {
		panic("cron.SetupWorkerLogging is not wired, the binary assigns it before dispatching the worker entry")
	}
	_ = SetupWorkerLogging(home.Dir(context.Background()), "cron")
	if runExternalWorkerPayload(context.Background(), *payload, *ack) {
		exit(0)
		return
	}
	exit(1)
}

// Hands the job to a restart safe worker under a managed gateway, false when the run stays in process
func (s *Store) launchExternalCronWorker(ctx context.Context, job *Job) (bool, error) {
	executionID := pyStrOrEmpty(strOrNil(job.ExecutionID))
	jobID := job.ID
	handoffDir := filepath.Join(s.home, "cron", "external-workers")
	payloadPath := filepath.Join(handoffDir, executionID+".json")
	ackPath := filepath.Join(handoffDir, executionID+".ready")
	stderrPath := filepath.Join(handoffDir, executionID+".stderr")
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	command := []string{exe, ExternalWorkerEntry, "--external-worker-file", payloadPath, "--ack-file", ackPath}
	multiplexActive := provider.IsMultiplexActive(ctx) || routedProfileFire(ctx, nil)
	requireScope := pyTruthy(s.cronConfigValue("require_restart_safe_scope", false))
	dispatch, err := restartSafeChildArgv(command, fmt.Sprintf("cron-%s-exec-%s", jobID, executionID), requireScope, false)
	if err != nil {
		return false, err
	}
	if dispatch.Mode == "in_process" {
		return false, nil
	}
	if !externalWorkerEntryDispatched.Load() {
		panic("cron external worker would re-execute a binary that never called cron.DispatchExternalWorkerEntry, call it first in main or TestMain")
	}
	pending, err := openExecutionsLedger(s).MarkHandoffPending(ctx, executionID)
	if err != nil {
		return false, err
	}
	if pending == nil {
		return false, errors.New("cron execution claim changed before external worker handoff")
	}
	if err := ensureCronDir(handoffDir); err != nil {
		return false, err
	}
	_ = os.Chmod(handoffDir, 0o700)
	if err := writeWorkerPayload(payloadPath, job, s.home, multiplexActive); err != nil {
		return false, err
	}
	scoped := s.installFireSecretScope(ctx)
	toolEnv := tools.ToolEnvFrom(scoped)
	workerEnv, err := tools.BuildSubprocessEnv(scoped, toolEnv, tools.SubprocessEnvOptions{
		ScrubSecrets: multiplexActive, InheritProfileHome: true, Extra: map[string]string{"ICEBATH_HOME": s.home},
	})
	if err != nil {
		return false, err
	}
	workerEnv = tools.SystemdUserBusEnv(tools.StripLaunchProfileEnv(toolEnv, workerEnv, s.home))
	for _, presence := range []string{"ICEBATH_INTERACTIVE", "ICEBATH_GATEWAY_SESSION", "ICEBATH_EXEC_ASK"} {
		delete(workerEnv, presence)
	}
	workerEnv[workerMarker] = "1"
	proc, pid, err := spawnExternalWorker(dispatch.Argv, filepath.Dir(exe), workerEnv, stderrPath)
	if err != nil {
		_ = os.Remove(payloadPath)
		_ = os.Remove(stderrPath)
		return false, err
	}
	key := s.inflightKey(jobID, "")
	inflight.Lock()
	inflightGuard.restartSafeWaiters[key] = true
	inflight.Unlock()
	waitOn := func(files ...string) (bool, error) {
		return s.waitForExternalCronWorker(ctx, proc, executionID, &jobID, files, &stderrPath)
	}
	deadline := time.Now().Add(time.Duration(HandoffAdoptionGraceSeconds * float64(time.Second)))
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(ackPath); statErr == nil {
			ack, readErr := readWorkerAck(ackPath)
			_ = os.Remove(ackPath)
			if readErr != nil {
				s.log.Error(fmt.Sprintf("Cron external worker %s published an unreadable acknowledgement; treating handoff as ownership-uncertain: %s", executionID, readErr))
				return waitOn(payloadPath, stderrPath)
			}
			var ackID any
			if ack != nil {
				ackID, _ = ack.Get("execution_id")
			}
			if ack == nil || ackID != executionID {
				s.log.Error(fmt.Sprintf("Cron external worker acknowledgement mismatch for %s; treating handoff as ownership-uncertain", executionID))
				return waitOn(payloadPath, stderrPath)
			}
			ackPID, _ := ack.Get("pid")
			s.log.Info(fmt.Sprintf("Cron job '%s' handed to restart-safe worker pid=%s execution=%s dispatch=%s", jobID, pyrepr.Str(ackPID), executionID, dispatch.Mode))
			if !pyTruthy(ackPID) {
				ackPID = pid
			}
			s.recordExternalCronWorker(jobID, ackPID, dispatch.Mode == "scoped")
			return waitOn(payloadPath, stderrPath)
		}
		if code := proc.Poll(); code != nil {
			inflight.Lock()
			delete(inflightGuard.restartSafeWaiters, key)
			inflight.Unlock()
			_ = os.Remove(payloadPath)
			tail := externalWorkerStderrTail(s, stderrPath)
			_ = os.Remove(stderrPath)
			if dispatch.Mode == "scoped" && tools.ScopedSpawnLostUserBus(workerEnv) {
				return false, fmt.Errorf("restart-safe systemd scope could not be created: the user D-Bus session at /run/user/%d/bus disappeared after the gateway started. On a system-level service install, run `sudo loginctl enable-linger <gateway-user>`; the next fire dispatches without scope isolation.", os.Getuid())
			}
			return false, fmt.Errorf("cron external worker exited before ownership acknowledgement (exit %d)%s", *code, tail)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.log.Warn(fmt.Sprintf("Cron external worker for job '%s' did not acknowledge within %.0fs; leaving the durable execution claim untouched", jobID, HandoffAdoptionGraceSeconds))
	return waitOn(payloadPath, ackPath, stderrPath)
}

// Writes the handoff payload exclusively with owner only access, removing it when the write fails
func writeWorkerPayload(path string, job *Job, profileHome string, multiplexActive bool) error {
	wire, err := claimedJobToWire(job)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"job": wire, "profile_home": profileHome, "multiplex_active": multiplexActive})
	if err != nil {
		return err
	}
	return writeExclusiveSynced(path, body)
}

// Creates the file exclusively at mode 0600, writes and fsyncs it, and removes it on any failure
func writeExclusiveSynced(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(body)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}

// Starts the detached worker with stderr captured to its file and stdin and stdout on the null device
func spawnExternalWorker(argv []string, dir string, env map[string]string, stderrPath string) (workerProcess, int, error) {
	stderrFile, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, 0, err
	}
	defer stderrFile.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = stderrFile
	cmd.SysProcAttr = cronScriptSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	return watchWorker(cmd), cmd.Process.Pid, nil
}

// Acknowledgement object the worker published, read as utf-8-sig, an error when it is not valid JSON
func readWorkerAck(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := pyjson.LoadsBytes(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil, err
	}
	obj, _ := v.(*pyjson.Object)
	return obj, nil
}

// Waits on the worker as the synchronous run contract needs, wrapping every failure as a post handoff error
func (s *Store) waitForExternalCronWorker(ctx context.Context, proc workerProcess, executionID string, jobID *string, handoffFiles []string, stderrPath *string) (bool, error) {
	defer func() {
		if jobID != nil {
			key := s.inflightKey(*jobID, "")
			inflight.Lock()
			delete(inflightGuard.restartSafeWaiters, key)
			delete(inflight.scopeIsolated, key)
			inflight.Unlock()
		}
		for _, stale := range handoffFiles {
			_ = os.Remove(stale)
		}
	}()
	ok, err := s.waitForExternalCronWorkerBody(ctx, proc, executionID, stderrPath)
	if err != nil {
		return false, &externalWorkerPostHandoffError{msg: err.Error(), cause: err}
	}
	return ok, nil
}

// Wakes on worker exit, true once the attempt is terminal, an error naming the exit when it died unterminalized
func (s *Store) waitForExternalCronWorkerBody(ctx context.Context, proc workerProcess, executionID string, stderrPath *string) (bool, error) {
	status := func(states map[string]bool) (bool, error) {
		current, err := getExecution(ctx, s, executionID)
		if err != nil {
			return false, err
		}
		return current != nil && states[pyStrOrEmpty(current["status"])], nil
	}
	for {
		returncode, exited := proc.Wait(time.Second)
		if !exited {
			terminal, err := status(executionTerminalStates)
			if err != nil {
				return false, err
			}
			if !terminal {
				continue
			}
			if returncode = proc.Poll(); returncode == nil {
				return true, nil
			}
		}
		if finished, err := status(FirerFinishedStates); err != nil || finished {
			return finished, err
		}
		reason := externalWorkerExitedReason(returncode)
		terminalized, err := openExecutionsLedger(s).TerminalizeDeadOwner(ctx, executionID, reason)
		if err != nil {
			return false, err
		}
		if !terminalized {
			if _, err := recoverInterruptedExecutions(ctx, s); err != nil {
				return false, err
			}
			if finished, err := status(FirerFinishedStates); err != nil || finished {
				return finished, err
			}
		}
		tail := ""
		if stderrPath != nil {
			tail = externalWorkerStderrTail(s, *stderrPath)
		}
		return false, errors.New(reason + tail)
	}
}

// Adopts the gateway dispatched execution, publishes the ready acknowledgement atomically, then runs the job once
func runExternalWorkerPayload(ctx context.Context, payloadPath, ackPath string) bool {
	job, profileHome, multiplexActive, executionID, err := loadWorkerPayload(payloadPath)
	_ = os.Remove(payloadPath)
	if err != nil {
		slog.Error(fmt.Sprintf("Cron external worker could not load payload %s: %s", payloadPath, err))
		return false
	}
	previousMultiplex := provider.IsMultiplexActive(context.Background())
	defer provider.SetMultiplexActive(previousMultiplex)
	ctx = home.WithOverride(ctx, profileHome)
	provider.SetMultiplexActive(multiplexActive)
	plugins.DiscoverPlugins(ctx, false)
	provider.HydrateProfileSecretSources(profileHome)
	ctx = provider.WithSecretScope(ctx, provider.BuildProfileSecretScope(profileHome), profileHome)
	if RegisterWorkerConfigHooks == nil {
		panic("cron.RegisterWorkerConfigHooks is not wired, the binary assigns it at startup")
	}
	RegisterWorkerConfigHooks(ctx, "Cron external worker: config hook registration failed", slog.LevelWarn)
	s := openCronStore(profileHome, time.Now)
	adopted, err := adoptClaimedExecution(ctx, s, executionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cron external worker could not adopt execution %s: %s\n", executionID, err)
		return false
	}
	if adopted == nil {
		s.log.Error(fmt.Sprintf("Cron external worker refused execution %s: durable ownership could not be established", executionID))
		return false
	}
	if err := publishWorkerAck(ackPath, executionID); err != nil {
		s.log.Error(fmt.Sprintf("Cron external worker could not publish ready acknowledgement for %s: %s", executionID, err))
		return false
	}
	old, had := os.LookupEnv(externalWorkerEnv)
	_ = os.Setenv(externalWorkerEnv, executionID)
	defer func() {
		if had {
			_ = os.Setenv(externalWorkerEnv, old)
		} else {
			_ = os.Unsetenv(externalWorkerEnv)
		}
	}()
	completed, err := runOneJobSeam(ctx, s, job, runOneJobOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cron external worker run of execution %s raised: %s\n", executionID, err)
		return false
	}
	_ = os.Remove(strings.TrimSuffix(ackPath, filepath.Ext(ackPath)) + ".stderr")
	return completed
}

// Job, resolved profile home, multiplex flag and execution id of a handoff payload file
func loadWorkerPayload(path string) (*Job, string, bool, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", false, "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &payload); err != nil {
		return nil, "", false, "", err
	}
	wire, ok := payload["job"].(map[string]any)
	if !ok {
		return nil, "", false, "", errors.New("KeyError: 'job'")
	}
	homeValue, ok := payload["profile_home"]
	if !ok {
		return nil, "", false, "", errors.New("KeyError: 'profile_home'")
	}
	execValue, ok := wire["execution_id"]
	if !ok {
		return nil, "", false, "", errors.New("KeyError: 'execution_id'")
	}
	job, err := claimedJobFromWire(wire)
	if err != nil {
		return nil, "", false, "", err
	}
	profileHome, err := pypath.Realpath(pyrepr.Str(homeValue), pypath.Lax)
	if err != nil {
		return nil, "", false, "", err
	}
	executionID := pyrepr.Str(execValue)
	job.ExecutionID = &executionID
	return job, profileHome, pyTruthy(payload["multiplex_active"]), executionID, nil
}

// Writes the acknowledgement to a temp sibling and renames it so readers see it absent or whole
func publishWorkerAck(ackPath, executionID string) error {
	if err := os.MkdirAll(filepath.Dir(ackPath), 0o777); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"pid": os.Getpid(), "execution_id": executionID})
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp%d", ackPath, os.Getpid())
	if err := writeExclusiveSynced(tmp, body); err != nil {
		return err
	}
	if err := os.Rename(tmp, ackPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
