package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Encoded receipts per results read, under the private socket response line
const hostedResultsBudgetBytes = 384 * 1024

// Outcomes that never ran, so no result exists to recover
var hostedResultlessOutcomes = map[string]bool{"interrupted": true, "cancelled": true}

// Expiry of a hosted dispatch, ambiguous so it never cancels the admission or claims non admission
type HostedDispatchTimeoutError struct{}

func (e *HostedDispatchTimeoutError) Error() string { return "" }

// Check of one hosted operation against current room membership and durable task state
type HostedAuthorizer func(ctx context.Context, operation string, task *TaskIdentity, generation *int64) (bool, error)

// Check of a submit inside the FIFO writer's transaction, raising to refuse
type HostedWriteAuthorizer func(ctx context.Context, conn state.SQLiteConn, task TaskIdentity, generation int64) error

// Operation dispatch a producer subclass replaces
type hostedRPCCaller interface {
	call(ctx context.Context, operation string, params map[string]any) (any, error)
	history(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error)
}

// Private hosted room producer of one room member profile binding over the canonical local session FIFO
type HostedRoomAuthorityRPC struct {
	self                 hostedRPCCaller
	Authority            *SessionAuthority
	loop                 context.Context
	RoomID               string
	MemberID             string
	Profile              string
	Principal            Principal
	Authorizer           HostedAuthorizer
	AuthorizeWrite       HostedWriteAuthorizer
	Timeout              time.Duration
	callbacksMu          sync.Mutex
	callbacks            map[string]func(receipt map[string]any) error
	CreationID           string
	Ref                  SessionRef
	HostedAttachmentData *[]TransferredAttachment
}

// Producer for the binding whose canonical session identity derives from its room, member and profile
func NewHostedRoomAuthorityRPC(authority *SessionAuthority, loop context.Context, roomID, memberID, profile string, principal Principal,
	authorize HostedAuthorizer, authorizeWrite HostedWriteAuthorizer) (*HostedRoomAuthorityRPC, error) {
	r := &HostedRoomAuthorityRPC{}
	if err := r.init(authority, loop, roomID, memberID, profile, principal, authorize, authorizeWrite); err != nil {
		return nil, err
	}
	r.self = r
	return r, nil
}

func (r *HostedRoomAuthorityRPC) init(authority *SessionAuthority, loop context.Context, roomID, memberID, profile string, principal Principal,
	authorize HostedAuthorizer, authorizeWrite HostedWriteAuthorizer) error {
	r.Authority, r.loop, r.RoomID, r.MemberID, r.Profile = authority, loop, roomID, memberID, profile
	r.Principal, r.Authorizer, r.AuthorizeWrite, r.Timeout = principal, authorize, authorizeWrite, 30*time.Second
	r.callbacks = map[string]func(map[string]any) error{}
	binding, err := pyjson.Dumps([]any{roomID, memberID, profile}, pyjson.Separators(",", ":"))
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(binding))
	r.CreationID = "hosted:" + hex.EncodeToString(digest[:])
	sessionID, err := state.LocalIdentity(authority.ProfileID, principal.Subject, r.CreationID)
	if err != nil {
		return err
	}
	r.Ref = SessionRef{ProfileID: authority.ProfileID, SessionID: sessionID}
	return nil
}

// Every submit resolves event bound bytes for its member
func (r *HostedRoomAuthorityRPC) AcceptsAttachments() bool { return true }

func (r *HostedRoomAuthorityRPC) groupTitle() string { return "Group: " + r.RoomID }

// Runs the operation on the owner loop, refused from the loop itself and once the loop stopped
func (r *HostedRoomAuthorityRPC) call(ctx context.Context, operation string, params map[string]any) (any, error) {
	if onOwnerLoop(ctx) {
		return nil, runtimeRefusal("invalid_params")
	}
	if r.loop.Err() != nil {
		return nil, runtimeRefusal("runtime_draining")
	}
	type outcome struct {
		value any
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := r.dispatch(withOwnerLoop(r.loop), operation, params)
		done <- outcome{value, err}
	}()
	timer := time.NewTimer(r.Timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.value, result.err
	case <-timer.C:
		return nil, &HostedDispatchTimeoutError{}
	}
}

// Checks the bound profile, source, session, title and authorization before running the operation
func (r *HostedRoomAuthorityRPC) dispatch(ctx context.Context, operation string, params map[string]any) (any, error) {
	ctx = ownerScope(ctx, r.Authority, false)
	if value, has := params["profile"]; (has && !pyrepr.Equal(value, r.Profile)) || r.Principal.ProfileID != r.Authority.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	for key, want := range map[string]string{"source": RoomSessionSource, "session_id": r.Ref.SessionID, "title": r.groupTitle()} {
		if value, has := params[key]; has && !pyrepr.Equal(value, want) {
			return nil, runtimeRefusal("permission_denied")
		}
	}
	var task *TaskIdentity
	if identity, ok := params["task"].(TaskIdentity); ok {
		task = &identity
	}
	var generation *int64
	if value, ok := hostedStrictInt(params["execution_generation"]); ok {
		generation = &value
	}
	if operation == "submit" && (task == nil || task.RoomID != r.RoomID || generation == nil || *generation < 1) {
		return nil, runtimeRefusal("invalid_params")
	}
	allowed, err := r.Authorizer(ctx, operation, task, generation)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, runtimeRefusal("permission_denied")
	}
	switch operation {
	case "resolve_exact":
		return r.resolveExact(ctx, params)
	case "create":
		return r.create(ctx, params)
	case "resume":
		return r.resume(ctx)
	case "submit":
		return r.submit(ctx, params, *task, *generation)
	case "history":
		return r.historyRows(ctx)
	case "results":
		return r.results(ctx, params)
	case "info":
		return r.info(ctx)
	case "interrupt":
		return r.interrupt(ctx, params)
	case "discard":
		return r.discard(ctx, params)
	case "approve":
		return r.approve(ctx, params)
	}
	return nil, fmt.Errorf("'HostedRoomAuthorityRPC' object has no attribute '_%s'", operation)
}

func (r *HostedRoomAuthorityRPC) resolveExact(ctx context.Context, params map[string]any) (any, error) {
	row, err := r.Authority.DB.GetSession(ctx, r.Ref.SessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return r.resume(ctx)
}

// Creates the hidden room session under a frozen bot room policy with clarify disabled, then resumes it
func (r *HostedRoomAuthorityRPC) create(ctx context.Context, params map[string]any) (any, error) {
	existing, err := r.Authority.DB.GetSession(ctx, r.Ref.SessionID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return r.resume(ctx)
	}
	private := &RecoveredSecrets{}
	cfg := loadGatewayConfig(ctx, "")
	agentCfg := map[string]any{}
	switch section := cfg["agent"].(type) {
	case map[string]any:
		for key, value := range section {
			agentCfg[key] = value
		}
	case nil:
	default:
		if pyrepr.Truthy(section) {
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(section))
		}
	}
	disabled := map[string]bool{"clarify": true}
	for _, name := range tools.ParseConfigStringList(agentCfg["disabled_toolsets"]) {
		disabled[name] = true
	}
	disabledList := make([]any, 0, len(disabled))
	for _, name := range slices.Sorted(mapsKeys(disabled)) {
		disabledList = append(disabledList, name)
	}
	agentCfg["disabled_toolsets"] = disabledList
	merged := make(map[string]any, len(cfg)+1)
	for key, value := range cfg {
		merged[key] = value
	}
	merged["agent"] = agentCfg
	policy, err := buildPolicy(ctx, map[string]any{"source": "gui"}, merged, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		return nil, err
	}
	replaced := *policy
	replaced.Source, replaced.Platform = RoomSessionSource, RoomSessionSource
	if replaced.Model == nil || *replaced.Model == "" {
		model := resolveGatewayModel(ctx, policy.frozenConfig())
		replaced.Model = &model
	}
	toolsets := map[string]bool{RoomSessionSource: true}
	for _, name := range policy.Toolsets {
		toolsets[name] = true
	}
	replaced.Toolsets = slices.Sorted(mapsKeys(toolsets))
	_, staged, err := stageLocalSession(ctx, r.Authority, r.Principal, map[string]any{"request_id": r.CreationID},
		localCreateOptions{TrustedPolicy: &replaced, TrustedSecrets: private})
	if err != nil {
		return nil, err
	}
	if staged != nil {
		if _, err := trackedWrite(ctx, r.Authority, func(taskCtx context.Context) (any, error) {
			return r.Authority.DB.CommitLocalSession(taskCtx, r.Authority.Epoch, *staged)
		}, nil, false, nil); err != nil {
			return nil, err
		}
		sid, err := receiptField(staged.Receipt, "session_id")
		if err != nil {
			return nil, err
		}
		if _, err := restoreLocalSession(ctx, r.Authority, pyStrOf(sid)); err != nil {
			return nil, err
		}
	}
	if _, err := trackedWrite(ctx, r.Authority, func(taskCtx context.Context) (any, error) {
		return nil, r.Authority.DB.ExecuteWrite(taskCtx, func(conn state.SQLiteConn) error {
			if err := state.RuntimeEpoch(taskCtx, conn, r.Authority.Epoch); err != nil {
				return err
			}
			_, err := conn.ExecContext(taskCtx, "UPDATE sessions SET title=?, hidden=1 WHERE id=?", r.groupTitle(), r.Ref.SessionID)
			return err
		})
	}, nil, false, nil); err != nil {
		return nil, err
	}
	return r.resume(ctx)
}

func mapsKeys(m map[string]bool) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

func (r *HostedRoomAuthorityRPC) resume(ctx context.Context) (any, error) {
	if _, err := r.Authority.Resolve(ctx, r.Principal, r.Ref); err != nil {
		return nil, err
	}
	return map[string]any{"session_id": r.Ref.SessionID, "title": r.groupTitle()}, nil
}

// One hosted admission row with the task identity and hosted generation its request id names
type hostedAdmission struct {
	row        *pyjson.Object
	identity   TaskIdentity
	generation int64
}

// Task identity and hosted generation a hosted request id encodes, an error when it names none
func hostedRequestIdentity(requestID string) (TaskIdentity, int64, error) {
	if !strings.HasPrefix(requestID, "hosted:") {
		return TaskIdentity{}, 0, &DriverValidationError{"not a hosted admission"}
	}
	decoded, err := pyjson.Loads(requestID[7:])
	if err != nil {
		return TaskIdentity{}, 0, err
	}
	pair, ok := pyjson.Plain(decoded).([]any)
	if !ok || len(pair) != 2 {
		return TaskIdentity{}, 0, &DriverValidationError{"invalid hosted identity"}
	}
	identity, err := taskIdentityFromKwargs(pair[0])
	if err != nil {
		return TaskIdentity{}, 0, err
	}
	generation, isInt := hostedStrictInt(pair[1])
	if !isInt {
		return TaskIdentity{}, 0, &DriverValidationError{"invalid generation"}
	}
	return identity, generation, nil
}

// Identity built from exactly the four dataclass fields of a mapping
func taskIdentityFromKwargs(value any) (TaskIdentity, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return TaskIdentity{}, &DriverValidationError{"TaskIdentity() argument after ** must be a mapping"}
	}
	for key := range fields {
		if !slices.Contains([]string{"room_id", "task_id", "thread_id", "turn_id"}, key) {
			return TaskIdentity{}, &DriverValidationError{"TaskIdentity got an unexpected keyword argument '" + key + "'"}
		}
	}
	for _, key := range []string{"room_id", "task_id", "thread_id", "turn_id"} {
		if _, has := fields[key]; !has {
			return TaskIdentity{}, &DriverValidationError{"TaskIdentity missing required argument '" + key + "'"}
		}
	}
	return NewTaskIdentity(fields["room_id"], fields["task_id"], fields["thread_id"], fields["turn_id"])
}

// Hosted admissions of this producer's session owned by its principal, storage unavailable on a malformed one
func (r *HostedRoomAuthorityRPC) rows(ctx context.Context) ([]hostedAdmission, error) {
	if err := r.Authority.Authorize(ctx, r.Principal, r.Ref, "session:read"); err != nil {
		return nil, err
	}
	rows, err := state.ListSessionAdmissions(ctx, r.Authority.DB, r.Ref.SessionID, false)
	if err != nil {
		return nil, err
	}
	var out []hostedAdmission
	for _, row := range rows {
		principal, _ := row.Get("principal_id")
		requestID := rowString(row, "request_id")
		if !SameOwner(principal, r.Principal.Subject) || !strings.HasPrefix(requestID, "hosted:") {
			continue
		}
		identity, generation, err := hostedRequestIdentity(requestID)
		if err == nil && identity.RoomID != r.RoomID {
			err = &DriverValidationError{"invalid hosted identity"}
		}
		if err != nil {
			return nil, &state.RuntimeStoreError{Reason: "storage_unavailable", Cause: err}
		}
		out = append(out, hostedAdmission{row, identity, generation})
	}
	return out, nil
}

// Receipt of one terminal admission, handed once to the callback still waiting for it
func (r *HostedRoomAuthorityRPC) terminal(ctx context.Context, admission hostedAdmission) (map[string]any, error) {
	admissionID := rowString(admission.row, "admission_id")
	saved, err := admissionResult(ctx, r.Authority.DB, admissionID)
	if err != nil {
		return nil, err
	}
	outcome := rowString(admission.row, "outcome")
	savedMap, hasSaved := hostedMapping(pyjson.Plain(saved))
	if !hasSaved && !hostedResultlessOutcomes[outcome] {
		return nil, runtimeRefusal("storage_unavailable")
	}
	value := map[string]any{}
	if hasSaved {
		value, _ = hostedMapping(savedMap["result"])
	}
	status, known := map[string]string{"completed": "settled", "interrupted": "cancelled", "cancelled": "cancelled"}[outcome]
	if !known {
		status = "failed"
	}
	text, has := value["final_response"]
	if !has {
		text = ""
	}
	receipt := map[string]any{"status": status, "text": text, "message_id": admissionID, "settlement_id": admissionID,
		"task_id": admission.identity.TaskID, "execution_generation": admission.generation}
	r.callbacksMu.Lock()
	callback := r.callbacks[admissionID]
	delete(r.callbacks, admissionID)
	r.callbacksMu.Unlock()
	if callback != nil {
		if err := callback(receipt); err != nil {
			return nil, err
		}
	}
	return receipt, nil
}

// Admits one fenced member turn under a capture lease, its terminal receipt delivered through the callback
func (r *HostedRoomAuthorityRPC) submit(ctx context.Context, params map[string]any, task TaskIdentity, generation int64) (any, error) {
	requestJSON, err := pyjson.Dumps([]any{task.AsDict(), generation}, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	for _, admission := range rows {
		if rowString(admission.row, "status") == "unknown" {
			return nil, runtimeRefusal("unknown_execution")
		}
	}
	prompt, _ := params["prompt"].(string)
	leased, release := CaptureLease(ctx)
	payload, err := PrepareBeforeSubmit(func() (map[string]any, error) {
		return SubmissionPayload(leased, r, prompt, params["attachments"])
	})
	if err != nil {
		release()
		return nil, err
	}
	opts := SubmitOptions{}
	if authorizeWrite := r.AuthorizeWrite; authorizeWrite != nil {
		opts.AuthorizeWrite = func(writeCtx context.Context, conn state.SQLiteConn) error {
			return authorizeWrite(writeCtx, conn, task, generation)
		}
	}
	receipt, err := r.Authority.Submit(leased, r.Principal, Submission{RequestID: "hosted:" + requestJSON, Ref: r.Ref, Payload: payload, Intent: "queue"}, opts)
	release()
	if err != nil {
		return nil, err
	}
	onTerminal, _ := params["on_terminal"].(func(map[string]any) error)
	r.callbacksMu.Lock()
	r.callbacks[receipt.AdmissionID] = onTerminal
	r.callbacksMu.Unlock()
	if receipt.Status == AdmissionQueued || receipt.Status == AdmissionStarted {
		r.Authority.mu.Lock()
		waiter := r.Authority.waiters[receipt.AdmissionID]
		if waiter == nil {
			waiter = newAdmissionWaiter()
			r.Authority.waiters[receipt.AdmissionID] = waiter
		}
		r.Authority.mu.Unlock()
		go func() {
			<-waiter.done
			if err := r.completed(ctx, receipt.AdmissionID, waiter); err != nil {
				slog.Default().With("logger", "asyncio").Error("Exception in callback HostedRoomAuthorityRPC._completed", "exc_info", err)
			}
		}()
	}
	if receipt.Status == AdmissionTerminal {
		rows, err := r.rows(ctx)
		if err != nil {
			return nil, err
		}
		for _, admission := range rows {
			if rowString(admission.row, "admission_id") == receipt.AdmissionID {
				if _, err := r.terminal(ctx, admission); err != nil {
					return nil, err
				}
			}
		}
	}
	return pyjson.Plain(admissionReceiptDict(receipt)), nil
}

// Publishes the durable terminal receipt once the admission's waiter resolves without a refusal
func (r *HostedRoomAuthorityRPC) completed(ctx context.Context, admissionID string, waiter *admissionWaiter) error {
	r.callbacksMu.Lock()
	_, pending := r.callbacks[admissionID]
	r.callbacksMu.Unlock()
	if !pending || waiter.err != nil {
		return nil
	}
	allowed, err := r.Authorizer(ctx, "terminal", nil, nil)
	if err != nil {
		return err
	}
	if !allowed {
		r.callbacksMu.Lock()
		delete(r.callbacks, admissionID)
		r.callbacksMu.Unlock()
		return nil
	}
	rows, err := r.rows(ctx)
	if err != nil {
		return err
	}
	for _, admission := range rows {
		if rowString(admission.row, "admission_id") == admissionID && rowString(admission.row, "status") == AdmissionTerminal {
			if _, err := r.terminal(ctx, admission); err != nil {
				return err
			}
		}
	}
	return nil
}

// Transcript of the room session with each terminal receipt appended as an assistant row
func (r *HostedRoomAuthorityRPC) historyRows(ctx context.Context) (any, error) {
	if err := r.Authority.Authorize(ctx, r.Principal, r.Ref, "session:read"); err != nil {
		return nil, err
	}
	prepared, err := prepareHistory(ctx, r.Authority, r.Ref)
	if err != nil {
		return nil, err
	}
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	history, err := unlessDeleted(ctx, r.Authority, r.Ref, func() ([]map[string]any, error) {
		return localHistory(ctx, r.Authority, r.Ref, prepared)
	})
	if err != nil {
		return nil, err
	}
	for _, admission := range rows {
		if rowString(admission.row, "status") != AdmissionTerminal {
			continue
		}
		receipt, err := r.terminal(ctx, admission)
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(receipt)+2)
		for key, value := range receipt {
			row[key] = value
		}
		row["role"], row["content"] = "assistant", receipt["text"]
		history = append(history, row)
	}
	return history, nil
}

// Terminal receipts only, newest first within the byte budget, each text kept just over the driver bound
func (r *HostedRoomAuthorityRPC) results(ctx context.Context, params map[string]any) (any, error) {
	rawWanted, hasWanted := params["admission_ids"]
	var wanted map[string]bool
	if hasWanted && rawWanted != nil {
		list, ok := rawWanted.([]any)
		if !ok || len(list) > 256 {
			return nil, runtimeRefusal("invalid_params")
		}
		wanted = map[string]bool{}
		for _, item := range list {
			text, isText := item.(string)
			if !isText || text == "" {
				return nil, runtimeRefusal("invalid_params")
			}
			wanted[text] = true
		}
	}
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	receipts := []map[string]any{}
	used := 0
	for i := len(rows) - 1; i >= 0; i-- {
		admission := rows[i]
		if rowString(admission.row, "status") != AdmissionTerminal || (wanted != nil && !wanted[rowString(admission.row, "admission_id")]) {
			continue
		}
		receipt, err := r.terminal(ctx, admission)
		if err != nil {
			return nil, err
		}
		text, isText := receipt["text"].(string)
		if !isText {
			return nil, fmt.Errorf("'%s' object has no attribute 'encode'", pyrepr.TypeName(receipt["text"]))
		}
		if len(text) > MaxTerminalTextBytes {
			receipt["text"] = strings.ToValidUTF8(text[:MaxTerminalTextBytes+4], "")
		}
		encoded, err := pyjson.Dumps(receipt)
		if err != nil {
			return nil, err
		}
		used += len(encoded)
		if len(receipts) > 0 && used > hostedResultsBudgetBytes {
			break
		}
		receipts = append(receipts, receipt)
	}
	slices.Reverse(receipts)
	return receipts, nil
}

// Current attempt state of the session and the approval it waits on, publishing terminal receipts first
func (r *HostedRoomAuthorityRPC) info(ctx context.Context) (any, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	for _, admission := range rows {
		if rowString(admission.row, "status") == AdmissionTerminal {
			if _, err := r.terminal(ctx, admission); err != nil {
				return nil, err
			}
		}
	}
	var current *hostedAdmission
	for i := range rows {
		if status := rowString(rows[i].row, "status"); status == AdmissionStarted || status == "unknown" || status == AdmissionQueued {
			current = &rows[i]
			break
		}
	}
	result := map[string]any{"active": false, "task_id": nil, "execution_generation": nil, "status": "idle"}
	if current != nil {
		status := rowString(current.row, "status")
		result = map[string]any{"active": status != "unknown", "task_id": current.identity.TaskID,
			"execution_generation": current.generation, "status": status}
	}
	snapshot, err := r.Authority.Attach(ctx, r.Principal, r.Ref, false, nil)
	if err != nil {
		return nil, err
	}
	for _, prompt := range snapshot.Prompts {
		if !pyrepr.Equal(prompt["kind"], "approval") {
			continue
		}
		pending := make(map[string]any, len(prompt)+2)
		for key, value := range prompt {
			pending[key] = value
		}
		pending["request_id"] = prompt["prompt_id"]
		choices := []any{}
		list, _ := pyjson.Plain(prompt["choices"]).([]any)
		for _, choice := range list {
			if pyrepr.Equal(choice, "once") || pyrepr.Equal(choice, "deny") {
				choices = append(choices, choice)
			}
		}
		pending["choices"] = choices
		result["status"], result["pending_approval"] = "waiting_for_approval", pending
		break
	}
	return result, nil
}

// Interrupts the running attempt of the expected task, cancelling it first while still queued
func (r *HostedRoomAuthorityRPC) interrupt(ctx context.Context, params map[string]any) (any, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	var current *hostedAdmission
	for i := range rows {
		if status := rowString(rows[i].row, "status"); status == AdmissionStarted || status == "unknown" || status == AdmissionQueued {
			current = &rows[i]
			break
		}
	}
	if current == nil || !pyrepr.Equal(current.identity.TaskID, params["expected_task_id"]) {
		return nil, runtimeRefusal("stale_generation")
	}
	row := current.row
	switch rowString(row, "status") {
	case "unknown":
		return nil, runtimeRefusal("unknown_execution")
	case AdmissionQueued:
		if row, err = r.Authority.CancelOrClaimed(ctx, r.Principal, r.Ref, rowString(row, "admission_id")); err != nil {
			return nil, err
		}
	}
	if row != nil {
		raw, _ := row.Get("generation")
		generation, err := pyint.Int64(raw)
		if err != nil {
			return nil, err
		}
		if _, err := r.Authority.Interrupt(ctx, r.Principal, r.Ref, generation); err != nil {
			return nil, err
		}
	}
	return map[string]any{"interrupted": true, "status": "interrupted"}, nil
}

// Settles exactly one unknown or interrupted attempt of the expected task and hosted generation as cancelled
func (r *HostedRoomAuthorityRPC) discard(ctx context.Context, params map[string]any) (any, error) {
	generation, ok := hostedStrictInt(params["execution_generation"])
	if !ok || generation < 1 {
		return nil, runtimeRefusal("invalid_params")
	}
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	var matches []hostedAdmission
	for _, admission := range rows {
		status := rowString(admission.row, "status")
		if (status == "unknown" || (status == AdmissionTerminal && rowString(admission.row, "outcome") == "interrupted")) &&
			pyrepr.Equal(admission.identity.TaskID, params["expected_task_id"]) && admission.generation == generation {
			matches = append(matches, admission)
		}
	}
	if len(matches) != 1 {
		return nil, runtimeRefusal("stale_generation")
	}
	match := matches[0]
	if rowString(match.row, "status") == "unknown" {
		raw, _ := match.row.Get("generation")
		canonical, err := pyint.Int64(raw)
		if err != nil {
			return nil, err
		}
		if _, err := r.Authority.ResolveUnknown(ctx, r.Principal, r.Ref, rowString(match.row, "admission_id"), canonical); err != nil {
			return nil, err
		}
	}
	return map[string]any{"discarded": true, "status": "cancelled", "task_id": match.identity.TaskID, "execution_generation": generation}, nil
}

// Answers one pending approval prompt of the running generation with once or deny
func (r *HostedRoomAuthorityRPC) approve(ctx context.Context, params map[string]any) (any, error) {
	if !pyrepr.Equal(params["choice"], "once") && !pyrepr.Equal(params["choice"], "deny") {
		return nil, runtimeRefusal("invalid_params")
	}
	snapshot, err := r.Authority.Attach(ctx, r.Principal, r.Ref, false, nil)
	if err != nil {
		return nil, err
	}
	found := false
	for _, prompt := range snapshot.Prompts {
		if pyrepr.Equal(prompt["prompt_id"], params["request_id"]) && pyrepr.Equal(prompt["kind"], "approval") {
			found = true
			break
		}
	}
	if !found {
		return nil, runtimeRefusal("stale_generation")
	}
	return r.Authority.Respond(ctx, r.Principal, r.Ref, snapshot.Handle.ExecutionGeneration, params["request_id"],
		map[string]any{"choice": params["choice"]}, "approval")
}

// Plain mapping of a dispatch result, empty when the operation answered none
func hostedResultMap(value any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	if mapping, ok := hostedMapping(pyjson.Plain(value)); ok {
		return mapping, nil
	}
	return nil, fmt.Errorf("hosted operation returned %s, not a mapping", pyrepr.TypeName(value))
}

// Plain rows of a dispatch result list, non mappings refused
func hostedResultRows(value any, err error) ([]map[string]any, error) {
	if err != nil {
		return nil, err
	}
	switch rows := pyjson.Plain(value).(type) {
	case []map[string]any:
		return rows, nil
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, raw := range rows {
			row, ok := hostedMapping(raw)
			if !ok {
				return nil, fmt.Errorf("hosted operation returned a %s row", pyrepr.TypeName(raw))
			}
			out = append(out, row)
		}
		return out, nil
	}
	return nil, fmt.Errorf("hosted operation returned %s, not a list", pyrepr.TypeName(value))
}

func (r *HostedRoomAuthorityRPC) ResolveExact(ctx context.Context, profile, title, source string) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "resolve_exact", map[string]any{"profile": profile, "title": title, "source": source}))
}

func (r *HostedRoomAuthorityRPC) Create(ctx context.Context, profile, title, source string) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "create", map[string]any{"profile": profile, "title": title, "source": source}))
}

func (r *HostedRoomAuthorityRPC) Resume(ctx context.Context, profile, sessionID, source string) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "resume", map[string]any{"profile": profile, "session_id": sessionID, "source": source}))
}

// Submits a turn of this producer's own member, a task for another member refused as the wrong producer
func (r *HostedRoomAuthorityRPC) Submit(ctx context.Context, req HostedSubmit) (map[string]any, error) {
	if req.MemberID != nil && *req.MemberID != r.MemberID {
		return nil, runtimeRefusal("permission_denied")
	}
	var attachments any
	if req.Attachments != nil {
		attachments = req.Attachments
	}
	return hostedResultMap(r.self.call(ctx, "submit", map[string]any{"profile": req.Profile, "session_id": req.SessionID,
		"source": req.Source, "prompt": req.Prompt, "task": req.Task, "execution_generation": req.ExecutionGeneration,
		"on_terminal": req.OnTerminal, "attachments": attachments}))
}

func (r *HostedRoomAuthorityRPC) History(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error) {
	return r.self.history(ctx, profile, sessionID, source)
}

func (r *HostedRoomAuthorityRPC) history(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error) {
	return hostedResultRows(r.self.call(ctx, "history", map[string]any{"profile": profile, "session_id": sessionID, "source": source}))
}

// Terminal receipts of the session, narrowed to the given admission ids when any are named
func (r *HostedRoomAuthorityRPC) Results(ctx context.Context, profile, sessionID, source string, admissionIDs []any) ([]map[string]any, error) {
	var wanted any
	if admissionIDs != nil {
		wanted = admissionIDs
	}
	return hostedResultRows(r.self.call(ctx, "results", map[string]any{"profile": profile, "session_id": sessionID, "source": source,
		"admission_ids": wanted}))
}

func (r *HostedRoomAuthorityRPC) Info(ctx context.Context, profile, sessionID, source string) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "info", map[string]any{"profile": profile, "session_id": sessionID, "source": source}))
}

func (r *HostedRoomAuthorityRPC) Interrupt(ctx context.Context, profile, sessionID, source, expectedTaskID string) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "interrupt", map[string]any{"profile": profile, "session_id": sessionID, "source": source,
		"expected_task_id": expectedTaskID}))
}

// Settles the expected task's unknown attempt of the hosted generation as cancelled
func (r *HostedRoomAuthorityRPC) Discard(ctx context.Context, profile, sessionID, source, expectedTaskID string, executionGeneration int64) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "discard", map[string]any{"profile": profile, "session_id": sessionID, "source": source,
		"expected_task_id": expectedTaskID, "execution_generation": executionGeneration}))
}

func (r *HostedRoomAuthorityRPC) Approve(ctx context.Context, sessionID, requestID, choice string) (map[string]any, error) {
	return hostedResultMap(r.self.call(ctx, "approve", map[string]any{"session_id": sessionID, "request_id": requestID, "choice": choice}))
}
