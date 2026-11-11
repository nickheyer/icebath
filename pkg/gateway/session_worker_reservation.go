package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Launcher hops allowed between the owner's handle and the real worker interpreter
const maxLauncherHops = 3

// Verified pid and birth of the interpreter behind a live direct child, proven by the owner from the hello frame
func verifyWorkerIdentity(process *workerProcess, hello *pyjson.Object) (int, float64, error) {
	if process == nil || process.poll() {
		return 0, 0, runtimeRefusal("worker_not_live")
	}
	kind, _ := hello.Get("type")
	rawPID, _ := hello.Get("pid")
	rawBirth, _ := hello.Get("birth")
	rawAncestors, _ := hello.Get("ancestors")
	pidValue, pidIsInt := pyrepr.Int(rawPID)
	_, birthIsFloat := rawBirth.(float64)
	_, birthIsInt := pyrepr.Int(rawBirth)
	ancestors, ancestorsList := rawAncestors.([]any)
	valid := hello != nil && kind == "hello" && pidIsInt && (birthIsFloat || birthIsInt) && ancestorsList
	for _, ancestor := range ancestors {
		if _, isInt := pyrepr.Int(ancestor); !isInt {
			valid = false
		}
	}
	if !valid {
		return 0, 0, runtimeRefusal("invalid_worker_frame")
	}
	if pidValue.Sign() <= 0 || !pidValue.IsInt64() || pidValue.Int64() == int64(os.Getpid()) {
		return 0, 0, runtimeRefusal("permission_denied")
	}
	pid := int(pidValue.Int64())
	claimed, _ := rawBirth.(float64)
	if birthIsInt {
		n, _ := pyrepr.Int(rawBirth)
		claimed, _ = new(big.Float).SetInt(n).Float64()
	}
	ppid, ok := proc.PPID(process.Pid)
	if !ok {
		return 0, 0, runtimeRefusal("worker_not_live")
	}
	if ppid != os.Getpid() || proc.Zombie(process.Pid) {
		return 0, 0, runtimeRefusal("permission_denied")
	}
	birth, err := proc.WorkerBirth(pid)
	if err != nil {
		return 0, 0, runtimeRefusal("worker_not_live")
	}
	if birth != claimed || !proc.Exists(pid) || proc.Zombie(pid) {
		return 0, 0, runtimeRefusal("permission_denied")
	}
	parents, err := proc.Parents(pid)
	if err != nil {
		return 0, 0, runtimeRefusal("worker_not_live")
	}
	chain := parents[:min(len(parents), maxLauncherHops)]
	if pid != process.Pid && !slices.Contains(chain, process.Pid) {
		return 0, 0, runtimeRefusal("permission_denied")
	}
	return pid, birth, nil
}

// URL safe base64 text of n random bytes without padding
func workerSecret(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Private bootstrap scope reserving a started admission for the owner's verified live child
func reserveAdmissionWorker(ctx context.Context, a *SessionAuthority, admissionID string, process *workerProcess, principalID any, hello *pyjson.Object, workerSubject string) (*pyjson.Object, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	pid, birth, err := verifyWorkerIdentity(process, hello)
	if err != nil {
		return nil, err
	}
	secret := workerSecret(32)
	var scope *pyjson.Object
	err = a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		scope = nil
		if err := state.RuntimeEpoch(ctx, conn, a.Epoch); err != nil {
			return err
		}
		admission, err := state.RuntimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		status, _ := admission.Get("status")
		ownerEpoch, _ := admission.Get("owner_epoch")
		if status != "started" || !pyrepr.Equal(ownerEpoch, a.Epoch) {
			return runtimeRefusal("producer_not_started")
		}
		storedPrincipal, _ := admission.Get("principal_id")
		if !SameOwner(storedPrincipal, principalID) {
			return runtimeRefusal("permission_denied")
		}
		owner := pyStrOf(admission.GetOr("target_session_id", nil))
		generation, _ := admission.Get("generation")
		session, err := state.RuntimeSession(ctx, conn, owner)
		if err != nil {
			return err
		}
		if !pyrepr.Equal(session["runtime_generation"], generation) {
			return runtimeRefusal("stale_generation")
		}
		sid, err := state.LocalPhysicalTarget(ctx, conn, owner)
		if err != nil {
			return err
		}
		executionID := "admission-worker:" + admissionID
		if row, err := state.ConnRow(ctx, conn, "SELECT 1 FROM worker_executions WHERE execution_id=?", executionID); err != nil {
			return err
		} else if row != nil {
			return runtimeRefusal("admission_conflict")
		}
		if row, err := state.ConnRow(ctx, conn, "SELECT 1 FROM worker_executions WHERE session_id=? AND status!='terminal'", sid); err != nil {
			return err
		} else if row != nil {
			return runtimeRefusal("stale_generation")
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=? WHERE id=?", generation, sid); err != nil {
			return err
		}
		if sid != owner {
			if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET lineage_json=json_insert(lineage_json,'$[#]',?) WHERE admission_id=?", sid, admissionID); err != nil {
				return err
			}
		}
		claimant := principalID
		if workerSubject != "" {
			claimant = workerSubject
		}
		claim, err := state.AdmissionFingerprint(sid, map[string]any{"profile_id": a.ProfileID, "session_id": sid,
			"execution_id": executionID, "generation": generation, "pid": pid, "birth": birth, "secret": secret, "principal": claimant})
		if err != nil {
			return err
		}
		digest, err := state.SecretDigest(claim)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO worker_executions(execution_id,session_id,kind,owner_epoch,generation,status,adoption_digest) "+
			"VALUES(?,?,'compute',?,?,'registered',?)", executionID, sid, a.Epoch, generation, digest); err != nil {
			return err
		}
		scope = pyjson.ObjectOf("profile_id", a.ProfileID, "session_id", sid, "execution_id", executionID, "generation", generation,
			"pid", pid, "birth", birth, "secret", secret, "epoch", a.Epoch)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scope, nil
}

// Revokes a lost worker's private assignment and pauses its FIFO at unknown, with no inference replay
func loseAdmissionWorker(ctx context.Context, a *SessionAuthority, row, scope *pyjson.Object) error {
	return a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		epoch, _ := scope.Get("epoch")
		generation, _ := scope.Get("generation")
		executionID := pyStrOf(scope.GetOr("execution_id", nil))
		epochValue, _ := pyrepr.Int(epoch)
		if epochValue == nil {
			return &PyError{Kind: "TypeError", Msg: "epoch must be an int"}
		}
		if err := state.RuntimeEpoch(ctx, conn, epochValue.Int64()); err != nil {
			return err
		}
		current, err := state.RuntimeAdmission(ctx, conn, rowString(row, "admission_id"))
		if err != nil {
			return err
		}
		status, _ := current.Get("status")
		ownerEpoch, _ := current.Get("owner_epoch")
		currentGeneration, _ := current.Get("generation")
		if status != "started" || !pyrepr.Equal(ownerEpoch, epoch) || !pyrepr.Equal(currentGeneration, generation) {
			return runtimeRefusal("stale_generation")
		}
		worker, err := state.ConnRow(ctx, conn, "SELECT * FROM worker_executions WHERE execution_id=?", executionID)
		if err != nil {
			return err
		}
		if worker == nil || !pyrepr.Equal(worker["generation"], generation) || !pyrepr.Equal(worker["owner_epoch"], epoch) {
			return runtimeRefusal("stale_generation")
		}
		if _, err := conn.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE execution_id=?", executionID); err != nil {
			return err
		}
		if err := state.CompactTerminalReceipts(ctx, conn, executionID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='unknown' WHERE admission_id=?", rowString(row, "admission_id")); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", current.GetOr("target_session_id", nil))
		return err
	})
}
