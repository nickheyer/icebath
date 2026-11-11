package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/provider"
)

// KanbanOwnerWaitSeconds is the longest the owner may stay silent between answers before the worker gives up
const KanbanOwnerWaitSeconds = 180.0

var (
	kanbanTransientOwnerStates  = map[string]bool{"draining": true, "starting": true}
	kanbanTransientRPCReasons   = map[string]bool{"runtime_draining": true, "runtime_retired": true}
	kanbanRestartControlReasons = map[string]bool{"invalid_control_peer": true, "incomplete_control_response": true, "invalid_control_response": true}
)

const kanbanWorkerReceiptPollDelay = 200 * time.Millisecond

// kanbanClaim is what the owner has said about this attempt, carried across connections
type kanbanClaim struct {
	answered    time.Time
	sessionID   any
	admissionID any
}

// Seconds since the owner's last answer
func (c *kanbanClaim) silentFor() float64 { return time.Now().Sub(c.answered).Seconds() }

// kanbanWorkerParams are the dispatcher's attempt coordinates the owner admits
type kanbanWorkerParams struct {
	board, taskID, claimLock string
	runID                    int64
}

// Python KeyError of a missing mapping key
func kanbanKeyError(key string) error {
	return &kanbanPyError{"KeyError", pyrepr.Repr(key)}
}

// kanbanPyError is a Python exception class this client raises without a dedicated Go type
type kanbanPyError struct{ Class, Msg string }

func (e *kanbanPyError) Error() string      { return e.Msg }
func (e *kanbanPyError) PyTypeName() string { return e.Class }

// Whether an error is one the reconnect loop handles, a gateway client, websocket, OS or timeout failure
func kanbanWorkerLoopError(err error) bool {
	var gce *GatewayClientError
	var closeErr *websocket.CloseError
	var netErr net.Error
	return errors.As(err, &gce) || errors.As(err, &closeErr) || errors.Is(err, websocket.ErrBadHandshake) ||
		errors.Is(err, websocket.ErrCloseSent) || errors.As(err, &netErr) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) || pypath.IsOSError(err)
}

// False for the owner's own final answer, true for a restarting owner or a lost transport
func kanbanRestartable(err error) bool {
	var rpcErr *GatewayRPCError
	if errors.As(err, &rpcErr) {
		return kanbanTransientRPCReasons[rpcErr.Error()]
	}
	var unavailable *GatewayUnavailableError
	if errors.As(err, &unavailable) {
		return kanbanTransientOwnerStates[unavailable.State] || kanbanRestartControlReasons[unavailable.ReasonCode]
	}
	return true
}

// Submits the claim to its profile owner, reconnecting while the owner keeps answering, and returns the attempt exit code
func runKanbanWorkerClient(ctx context.Context, params kanbanWorkerParams, boardDB string) (int, error) {
	if os.Getenv("ICEBATH_TUI_GATEWAY_URL") != "" {
		return 0, &GatewayClientError{Msg: "Kanban requires the assigned local profile owner"}
	}
	claim := &kanbanClaim{answered: time.Now()}
	failures := 0
	for {
		seen := claim.answered
		receipt, err := kanbanWorkerAttempt(ctx, params, boardDB, claim)
		if err == nil {
			return kanbanWorkerExitCode(receipt, params, boardDB)
		}
		if !kanbanWorkerLoopError(err) || !kanbanRestartable(err) {
			return 0, err
		}
		if claim.silentFor() >= KanbanOwnerWaitSeconds {
			return 0, &GatewayClientError{Msg: fmt.Sprintf("Kanban owner silent for %.0fs: %s", claim.silentFor(), kanbanSilentDetail(err))}
		}
		if !claim.answered.Equal(seen) {
			failures = 1
		} else {
			failures++
		}
		delay := provider.JitteredBackoff(failures, provider.BackoffOptions{BaseDelay: time.Second, MaxDelay: 8 * time.Second, JitterRatio: 1.0})
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// One connection to the owner outside any restart wait, its events drained while the receipt settles
func kanbanWorkerAttempt(ctx context.Context, params kanbanWorkerParams, boardDB string, claim *kanbanClaim) (map[string]any, error) {
	client, err := ConnectGateway(OutsideRestartWait(ctx))
	if err != nil {
		return nil, err
	}
	defer client.Close()
	drainCtx, stopDrain := context.WithCancel(ctx)
	defer stopDrain()
	go func() {
		for {
			if _, err := client.Events.Get(drainCtx); err != nil {
				return
			}
		}
	}()
	return kanbanAwaitReceipt(ctx, client, params, boardDB, claim)
}

// Mapping value of key, a KeyError when absent
func kanbanItem(m map[string]any, key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, kanbanKeyError(key)
	}
	return v, nil
}

// Submits the claim or re-reads its admission and polls until the receipt settles, recording each answer
func kanbanAwaitReceipt(ctx context.Context, client *GatewayClient, params kanbanWorkerParams, boardDB string, claim *kanbanClaim) (map[string]any, error) {
	var receipt map[string]any
	if claim.admissionID == nil {
		db, err := pypath.Resolve(boardDB)
		if err != nil {
			return nil, err
		}
		budget := time.Duration(kanban.AdmissionAnswerBudget() * float64(time.Second))
		raw, err := client.RPCTimeout(ctx, "kanban.run", map[string]any{
			"board": params.board, "task_id": params.taskID, "run_id": params.runID, "claim_lock": params.claimLock, "db": db,
		}, budget)
		if err != nil {
			return nil, err
		}
		accepted, ok := pyjson.Plain(raw).(map[string]any)
		if !ok {
			return nil, &kanbanPyError{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(raw))}
		}
		rv, err := kanbanItem(accepted, "receipt")
		if err != nil {
			return nil, err
		}
		if receipt, ok = rv.(map[string]any); !ok {
			return nil, &kanbanPyError{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(rv))}
		}
		sessionID, err := kanbanItem(accepted, "session_id")
		if err != nil {
			return nil, err
		}
		admissionID, err := kanbanItem(receipt, "admission_id")
		if err != nil {
			return nil, err
		}
		claim.sessionID, claim.admissionID = sessionID, admissionID
		fmt.Fprintf(os.Stderr, "Session: %s\n", pyrepr.Str(claim.sessionID))
	} else {
		var err error
		if receipt, err = kanbanReceipt(ctx, client, claim); err != nil {
			return nil, err
		}
	}
	claim.answered = time.Now()
	for {
		status, err := kanbanItem(receipt, "status")
		if err != nil {
			return nil, err
		}
		if status == "terminal" || status == "unknown" {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(kanbanWorkerReceiptPollDelay):
		}
		if receipt, err = kanbanReceipt(ctx, client, claim); err != nil {
			return nil, err
		}
		claim.answered = time.Now()
	}
}

// Exit code of a settled receipt from the dispatcher's exact attempt, printing the attempt's reply
func kanbanWorkerExitCode(receipt map[string]any, params kanbanWorkerParams, boardDB string) (int, error) {
	if receipt["status"] == "unknown" {
		return 0, &GatewayClientError{Msg: "Kanban execution unknown; retained by owner"}
	}
	result, err := kanbanWorkerResult(boardDB, params)
	if err != nil {
		return 0, err
	}
	if out, _ := result.Get("last_output"); pyrepr.Truthy(out) {
		fmt.Fprintln(os.Stdout, pyrepr.Str(out))
	}
	code, ok := result.Get("exit_code")
	if !ok {
		return 1, nil
	}
	n, err := pyint.Of(code)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// The worker_result event of exactly this attempt read only, empty unless its claim lock matches
func kanbanWorkerResult(path string, params kanbanWorkerParams) (*pyjson.Object, error) {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return nil, err
	}
	uri, err := pypath.AsURI(resolved)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var payload sql.NullString
	err = db.QueryRow("SELECT payload FROM task_events WHERE run_id=? AND task_id=? AND kind='worker_result' ORDER BY id DESC LIMIT 1",
		params.runID, params.taskID).Scan(&payload)
	result := pyjson.NewObject()
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		decoded, err := pyjson.Loads(payload.String)
		if err != nil {
			return nil, err
		}
		obj, ok := decoded.(*pyjson.Object)
		if !ok {
			return nil, &kanbanPyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
		}
		result = obj
	}
	if lock, _ := result.Get("claim_lock"); lock == params.claimLock {
		return result, nil
	}
	return pyjson.NewObject(), nil
}

// The owner's receipt of this attempt's admission
func kanbanReceipt(ctx context.Context, client *GatewayClient, claim *kanbanClaim) (map[string]any, error) {
	return client.RPC(ctx, "prompt.receipt", map[string]any{"session_id": claim.sessionID, "admission_id": claim.admissionID})
}

// Runs the worker client and exits when argv carries its entry word, always writing the exit trailer
func DispatchKanbanWorkerClientEntry(ctx context.Context) {
	if len(os.Args) < 2 || os.Args[1] != kanban.KanbanWorkerClientEntry {
		return
	}
	rc, err := kanbanWorkerClientSubmit(ctx)
	if err != nil {
		rc = 1
	}
	fmt.Fprintf(os.Stderr, "\n%s%d\n", kanban.WorkerExitTrailer, rc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Traceback (most recent call last):\n%s: %s\n", pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err))
	}
	os.Exit(rc)
}

// Reads the attempt from the environment, clears task identity and submits, a refusal printed for bad input
func kanbanWorkerClientSubmit(ctx context.Context) (int, error) {
	params, boardDB, err := kanbanWorkerParamsFromEnv()
	if err == nil {
		for _, kv := range os.Environ() {
			key, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(key, "ICEBATH_KANBAN_") && key != "ICEBATH_KANBAN_HOME" && key != "ICEBATH_KANBAN_BUSY_TIMEOUT_MS" {
				_ = os.Unsetenv(key)
			}
		}
		for _, key := range []string{"ICEBATH_SESSION_SOURCE", "ICEBATH_TENANT", "TERMINAL_CWD"} {
			_ = os.Unsetenv(key)
		}
		var rc int
		rc, err = runKanbanWorkerClient(ctx, params, boardDB)
		if err == nil {
			return rc, nil
		}
	}
	var pyErr *kanbanPyError
	if (errors.As(err, &pyErr) && (pyErr.Class == "KeyError" || pyErr.Class == "ValueError")) || pypath.IsOSError(err) {
		text := pyrepr.ExceptionMessage(err)
		if pypath.IsOSError(err) {
			text = pypath.OSErrorText(err)
		}
		fmt.Fprintf(os.Stderr, "Kanban owner refused: %s\n", text)
		return 1, nil
	}
	return 0, err
}

// Attempt coordinates and board database from the dispatcher's environment
func kanbanWorkerParamsFromEnv() (kanbanWorkerParams, string, error) {
	get := func(key string) (string, error) {
		v, ok := os.LookupEnv(key)
		if !ok {
			return "", kanbanKeyError(key)
		}
		return v, nil
	}
	var p kanbanWorkerParams
	var err error
	if p.board, err = get("ICEBATH_KANBAN_BOARD"); err != nil {
		return p, "", err
	}
	if p.taskID, err = get("ICEBATH_KANBAN_TASK"); err != nil {
		return p, "", err
	}
	rawRun, err := get("ICEBATH_KANBAN_RUN_ID")
	if err != nil {
		return p, "", err
	}
	n, ok := pyint.Parse(rawRun)
	if !ok || !n.IsInt64() {
		return p, "", &kanbanPyError{"ValueError", "invalid literal for int() with base 10: " + pyrepr.Repr(rawRun)}
	}
	p.runID = n.Int64()
	if p.claimLock, err = get("ICEBATH_KANBAN_CLAIM_LOCK"); err != nil {
		return p, "", err
	}
	boardDB, err := get("ICEBATH_KANBAN_DB")
	return p, boardDB, err
}
