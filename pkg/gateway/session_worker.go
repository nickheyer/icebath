package gateway

import (
	"context"
	"crypto/subtle"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Assignment keys every worker request carries
var workerScopeKeys = []string{"profile_id", "session_id", "execution_id", "generation", "pid", "birth", "secret"}

// Whether params hold exactly the worker scope plus the extra keys
func workerParamsAre(params *jsonutil.Object, extra ...string) bool {
	return frameKeysAre(params, append(append([]string(nil), workerScopeKeys...), extra...)...)
}

// Int value of a param that is a Python int and not a bool
func strictInt(value any) (int64, bool) {
	if _, isBool := value.(bool); isBool {
		return 0, false
	}
	return anyutil.Int(value)
}

// Number of a param that is a Python int or float and not a bool
func strictNumber(value any) (float64, bool) {
	if f, isFloat := value.(float64); isFloat {
		return f, true
	}
	if n, isInt := strictInt(value); isInt {
		return float64(n), true
	}
	return 0, false
}

// Producer claim of a live same user worker process, the durable adoption digest's preimage
func workerClaim(ctx context.Context, c *AuthorityConnection, params *jsonutil.Object) (string, error) {
	a := c.authority
	if !anyutil.Equal(params.ValueOr("profile_id", nil), a.ProfileID) || c.actor.ProfileID != a.ProfileID {
		return "", runtimeRefusal("profile_mismatch")
	}
	if len(c.actor.Capabilities) == 0 {
		return "", runtimeRefusal("permission_denied")
	}
	pidValue, pidInt := strictInt(params.ValueOr("pid", nil))
	birth, birthNumber := strictNumber(params.ValueOr("birth", nil))
	secret, secretText := params.ValueOr("secret", nil).(string)
	if !pidInt || pidValue <= 0 || pidValue == int64(os.Getpid()) || !birthNumber ||
		!secretText || utf8.RuneCountInString(secret) < 16 || utf8.RuneCountInString(secret) > 256 {
		return "", runtimeRefusal("invalid_params")
	}
	pid := int(pidValue)
	started, err := proc.WorkerBirth(pid)
	if err != nil {
		return "", runtimeRefusal("worker_not_live")
	}
	owner, err := proc.Username(pid)
	if err != nil {
		return "", runtimeRefusal("worker_not_live")
	}
	self, err := proc.Username(os.Getpid())
	if err != nil {
		return "", runtimeRefusal("worker_not_live")
	}
	if started != birth || !proc.Exists(pid) || proc.Zombie(pid) || owner != self {
		return "", runtimeRefusal("permission_denied")
	}
	origin, err := a.DB.WorkerClaimOrigin(ctx, params)
	if err != nil {
		return "", err
	}
	payload := map[string]any{}
	for _, key := range workerScopeKeys {
		payload[key] = params.Value(key)
	}
	payload["session_id"], payload["principal"] = origin, c.actor.Subject
	return state.AdmissionFingerprint(anyutil.String(origin), payload)
}

// Verifies the claim against the stored assignment of the execution the worker names
func verifyWorkerClaim(ctx context.Context, c *AuthorityConnection, params *jsonutil.Object, claim string) error {
	a := c.authority
	target, err := a.DB.WorkerRetryTarget(ctx, params)
	if err != nil {
		return err
	}
	return a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		row, err := state.ConnRow(ctx, conn, "SELECT * FROM worker_executions WHERE execution_id=?", params.ValueOr("execution_id", nil))
		if err != nil {
			return err
		}
		if row == nil {
			return runtimeRefusal("not_found")
		}
		if !anyutil.Equal(row["session_id"], target) {
			return runtimeRefusal("permission_denied")
		}
		if _, isInt := strictInt(params.ValueOr("generation", nil)); !isInt || !anyutil.Equal(row["generation"], params.Value("generation")) {
			return runtimeRefusal("stale_generation")
		}
		digest, err := state.SecretDigest(claim)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(anyutil.String(row["adoption_digest"])), []byte(digest)) != 1 {
			return runtimeRefusal("permission_denied")
		}
		return nil
	})
}

// Ledger int of a param, a value no stored generation or epoch ever holds when it is not an int
func workerLedgerInt(value any) int64 {
	if n, isInt := strictInt(value); isInt {
		return n
	}
	return -1
}

// Text param of the ledger, the invalid params refusal of anything else
func ledgerText(value any) (string, error) {
	text, isText := value.(string)
	if !isText {
		return "", runtimeRefusal("invalid_params")
	}
	return text, nil
}

// Execution scoped worker operation, register, adopt or persist, on the authenticated authority
func workerRequest(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, operation string) (*jsonutil.Object, error) {
	a := c.authority
	if err := requireServing(a); err != nil {
		return nil, err
	}
	if named, isText := params.ValueOr("operation", nil).(string); operation == "persist" && isText {
		if strings.HasPrefix(named, "delegation.") {
			return workerDelegationRequest(ctx, c, ref, params)
		}
		if named == "child.reserve" {
			return workerChildRequest(ctx, c, ref, params)
		}
	}
	extra := map[string][]string{"register": {"kind"}, "adopt": nil, "persist": {"epoch", "sequence", "operation", "payload"}}[operation]
	if !workerParamsAre(params, extra...) {
		return nil, runtimeRefusal("invalid_params")
	}
	claim, err := workerClaim(ctx, c, params)
	if err != nil {
		return nil, err
	}
	if operation == "register" {
		if err := a.requireAdmissionOpen(false); err != nil {
			return nil, err
		}
	}
	executionID, err := ledgerText(params.Value("execution_id"))
	if err != nil {
		return nil, err
	}
	sessionID, err := ledgerText(params.Value("session_id"))
	if err != nil {
		return nil, err
	}
	generation := workerLedgerInt(params.Value("generation"))
	if operation == "register" {
		if err := a.Authorize(ctx, c.actor, ref, "worker:adopt"); err != nil {
			return nil, err
		}
		if params.Value("kind") != "compute" {
			return nil, runtimeRefusal("unsupported_producer")
		}
		return a.DB.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: a.Epoch, ExecutionID: executionID,
			SessionID: sessionID, Generation: generation, Kind: "compute", AdoptionSecret: claim, RequireIdle: true})
	}
	if !c.actor.Capabilities["worker:adopt"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := verifyWorkerClaim(ctx, c, params, claim); err != nil {
		return nil, err
	}
	if operation == "adopt" {
		return a.DB.AdoptWorkerExecution(ctx, a.Epoch, executionID, sessionID, generation, claim)
	}
	named, _ := params.Value("operation").(string)
	result, err := a.DB.MutateWorkerExecution(ctx, state.WorkerReceipt{Epoch: workerLedgerInt(params.Value("epoch")), ExecutionID: executionID,
		SessionID: sessionID, Generation: generation, Sequence: max(workerLedgerInt(params.Value("sequence")), 0), Operation: named,
		Payload: params.Value("payload")})
	if err != nil {
		return nil, err
	}
	if named == "execution.finish" {
		if err := a.wakeAfterWorker(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	return result, nil
}
