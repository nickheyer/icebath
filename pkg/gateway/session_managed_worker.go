package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Managed worker supervision windows, the hello wait, the stop acknowledgement and the loss note hold up
const (
	managedHelloSeconds   = 60.0
	managedStopAckSeconds = 30.0
	lossRecordWaitSeconds = 10.0
)

// Child bootstrap frame of a reserved managed turn with the per turn facts riding the hydrated request
func managedBootstrap(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, policy *LocalSessionPolicy, scope *pyjson.Object, text *string) (*pyjson.Object, error) {
	decoded, err := pyjson.Loads(policy.TerminalJSON)
	if err != nil {
		return nil, err
	}
	terminal, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(decoded) + "' object does not support item assignment"}
	}
	if policy.ConfigSecretRef != nil && *policy.ConfigSecretRef != "" {
		recovered, err := recoverConfigSecrets(ctx, a, policy)
		if err != nil {
			return nil, err
		}
		for i, path := range recovered.paths {
			if path[0] == nil {
				terminal.Set(pyStrOf(path[1]), recovered.values[i])
			}
		}
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	payload := rowPayload(row)
	yolo, err := sessionYolo(ctx, a, live.Route, policy)
	if err != nil {
		return nil, err
	}
	turn := pyjson.ObjectOf("finite", payload.GetOr("finite", false), "unattended", payload.GetOr("unattended", nil) == true, "yolo", yolo)
	for _, key := range []string{"surface_v1", "display_v1"} {
		if value := payload.GetOr(key, nil); pyrepr.Truthy(value) {
			turn.Set(key, value)
		}
	}
	requestValue, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	request, isDict := requestValue.(*pyjson.Object)
	if !isDict {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(requestValue) + "' object is not iterable"}
	}
	request = request.Copy()
	request.Set("turn_v1", turn)
	construct, err := constructInputs(ctx, a, policy, live.Source.ChatID)
	if err != nil {
		return nil, err
	}
	request.Set("construct_v1", construct)
	config, err := workerConfig(ctx, a, policy)
	if err != nil {
		return nil, err
	}
	hydrated := *policy
	if hydrated.ConfigJSON, err = pyjson.Dumps(config); err != nil {
		return nil, err
	}
	if hydrated.RequestJSON, err = pyjson.Dumps(request); err != nil {
		return nil, err
	}
	if hydrated.TerminalJSON, err = pyjson.Dumps(terminal); err != nil {
		return nil, err
	}
	hydrated.CredentialRef, hydrated.ConfigSecretRef = nil, nil
	apiKey, err := routeLaunchKey(ctx, a, policy, config)
	if err != nil {
		return nil, err
	}
	turnText := any(nil)
	if text != nil {
		turnText = *text
	} else if value, ok := payload.Get("text"); ok {
		turnText = value
	} else {
		return nil, &pyKeyError{"text"}
	}
	frame := pyjson.ObjectOf("version", 1, "home", a.ProfileID, "scope", scope, "policy", hydrated.asDict(),
		"api_key", optStrAny(apiKey), "text", turnText, "route", live.Route)
	if attachments, ok := payload.Get("attachments_v1"); ok {
		frame.Set("attachments_v1", attachments)
	}
	frame.Set("user_id", optStrAny(live.Source.UserID))
	frame.Set("chat_id", live.Source.ChatID)
	frame.Set("turn_author", rowTurnAuthor(policy, row))
	frame.Set("safe_mode", policy.SafeMode)
	frame.Set("ignore_user_config", policy.IgnoreUserConfig)
	return frame, nil
}

// Frozen session config the child binds, its command allowlist read from the live profile file for ordinary sessions
func workerConfig(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy) (map[string]any, error) {
	config, err := policy.config(ctx, a, true)
	if err != nil {
		return nil, err
	}
	if policy.IgnoreUserConfig || !filepath.IsAbs(a.ProfileID) {
		return config, nil
	}
	live := loadGatewayConfig(ctx, filepath.Join(a.ProfileID, "config.yaml"))
	delete(config, "command_allowlist")
	if allowlist, ok := live["command_allowlist"]; ok && allowlist != nil {
		config["command_allowlist"] = allowlist
	}
	return config, nil
}

// Route's yolo bypass as the in process turn arms it, the launch flag seeded then the persisted copy
func sessionYolo(ctx context.Context, a *SessionAuthority, route string, policy *LocalSessionPolicy) (bool, error) {
	var persisted *bool
	if store := a.Runner.SessionStore(); store != nil {
		if entry := store.LookupBySessionKey(ctx, route); entry != nil {
			persisted = entry.Yolo
		}
	}
	tools.RestoreGatewayYolo(ctx, route, persisted, policy.Yolo())
	return tools.IsSessionYoloEnabled(route), nil
}

// Child side of turn_v1 binding the finite flags, the route's yolo and the committed surface and display
func WorkerTurnScope(ctx context.Context, frame *pyjson.Object) (context.Context, error) {
	invalid := &PyError{Kind: "ValueError", Msg: "invalid_managed_worker_bootstrap"}
	policy, _ := frame.GetOr("policy", nil).(*pyjson.Object)
	text, _ := policy.GetOr("request_json", nil).(string)
	if text == "" {
		text = "{}"
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	request, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(decoded) + "' object has no attribute 'get'"}
	}
	rawTurn := request.GetOr("turn_v1", nil)
	if rawTurn == nil {
		return ctx, nil
	}
	turn, isDict := rawTurn.(*pyjson.Object)
	if !isDict {
		return nil, invalid
	}
	flags := 0
	for _, key := range turn.Keys {
		if key == "surface_v1" || key == "display_v1" {
			continue
		}
		if key != "finite" && key != "unattended" && key != "yolo" {
			return nil, invalid
		}
		if _, isBool := turn.Vals[key].(bool); !isBool {
			return nil, invalid
		}
		flags++
	}
	if flags != 3 {
		return nil, invalid
	}
	finite, unattended, yolo := turn.Vals["finite"].(bool), turn.Vals["unattended"].(bool), turn.Vals["yolo"].(bool)
	if unattended && !finite {
		return nil, invalid
	}
	var surface any
	if committed, present := turn.Get("surface_v1"); present {
		restored, err := restoreSurface(committed)
		if err != nil {
			if isStoreRefusal(err) {
				return nil, invalid
			}
			return nil, err
		}
		surface = restored
	}
	var display map[string]any
	if committed, present := turn.Get("display_v1"); present {
		restored, err := RestoreDisplay(committed)
		if err != nil {
			if isStoreRefusal(err) {
				return nil, invalid
			}
			return nil, err
		}
		display = restored
	}
	route := pyStrOf(frame.GetOr("route", nil))
	if yolo {
		tools.EnableSessionYolo(ctx, route)
	}
	scope, _ := frame.GetOr("scope", nil).(*pyjson.Object)
	executionID := pyStrOf(scope.GetOr("execution_id", nil))
	ctx = finiteTurnScope(ctx, finite, unattended)
	ctx = surfaceTurnScope(ctx, surface)
	return DisplayTurnScope(ctx, executionID, display), nil
}

// Whether an error is a runtime store refusal
func isStoreRefusal(err error) bool {
	var refusal *state.RuntimeStoreError
	return errors.As(err, &refusal)
}

// Managed worker of the session as the owner registered it, nil when none runs
func managedWorkerOf(a *SessionAuthority, sessionID string) *managedWorker {
	a.mu.Lock()
	defer a.mu.Unlock()
	worker, _ := a.managedWorkers[sessionID].(*managedWorker)
	return worker
}

// Stops the session's managed worker for an authorized actor at the running generation, false when none runs
func interruptManaged(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, generation any) (bool, error) {
	worker := managedWorkerOf(a, ref.SessionID)
	if worker == nil {
		return false, nil
	}
	if err := a.Authorize(ctx, actor, ref, "session:control"); err != nil {
		return false, err
	}
	handle, err := a.handle(ctx, SessionRef{a.ProfileID, ref.SessionID})
	if err != nil {
		return false, err
	}
	if !pyrepr.Equal(handle.ExecutionGeneration, generation) || handle.ExecutionState != "running" {
		return false, runtimeRefusal("stale_generation")
	}
	if err := worker.control(pyjson.ObjectOf("type", "stop")); err != nil {
		return false, err
	}
	return true, nil
}

// Approval fields a worker's approval frame may carry
var workerApprovalFields = map[string]bool{"request_id": true, "command": true, "description": true, "allow_session": true,
	"allow_permanent": true, "smart_denied": true, "edit": true}

// Projects a worker's approval, clarify or prompt settled frame onto the session's controls, false for any other frame
func promptFrame(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, worker *managedWorker, frame *pyjson.Object) (bool, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return false, &pyKeyError{ref.SessionID}
	}
	controls := live.pendingControls()
	kind, _ := frame.Get("type")
	generation := rowGeneration(row)
	if kind == "prompt_settled" && frameKeysAre(frame, "type", "prompt_id") {
		if promptID, isText := frame.Vals["prompt_id"].(string); isText {
			saved, held := controls.pending[promptID]
			controls.drop(promptID)
			if held {
				savedKind, _ := saved.prompt.Get("kind")
				live.EventStream.Publish(ref.SessionID, pyjson.ObjectOf("prompt_id", promptID, "execution_generation", generation),
					pyrepr.Str(savedKind)+".settled", nil)
			}
		}
		return true, nil
	}
	if kind != "approval" && kind != "clarify" {
		return false, nil
	}
	if len(controls.RemoteResponders) >= 16 {
		return false, runtimeRefusal("worker_control_backpressure")
	}
	var promptID any
	var data *pyjson.Object
	if kind == "approval" {
		data, _ = frame.GetOr("data", nil).(*pyjson.Object)
		if !frameKeysAre(frame, "type", "data") || data == nil {
			return false, runtimeRefusal("invalid_worker_frame")
		}
		for _, key := range data.Keys {
			if !workerApprovalFields[key] {
				return false, runtimeRefusal("invalid_worker_frame")
			}
		}
		promptID = data.GetOr("request_id", nil)
	} else {
		if !frameKeysAre(frame, "type", "prompt_id", "question", "choices", "multi_select") {
			return false, runtimeRefusal("invalid_worker_frame")
		}
		_, questionText := frame.Vals["question"].(string)
		choices, choicesList := frame.Vals["choices"].([]any)
		_, multiBool := frame.Vals["multi_select"].(bool)
		if !questionText || !choicesList || !multiBool {
			return false, runtimeRefusal("invalid_worker_frame")
		}
		for _, choice := range choices {
			if _, isText := choice.(string); !isText {
				return false, runtimeRefusal("invalid_worker_frame")
			}
		}
		promptID = frame.Vals["prompt_id"]
	}
	id, isText := promptID.(string)
	if _, pending := controls.pending[id]; !isText || id == "" || pending {
		return false, runtimeRefusal("invalid_worker_frame")
	}
	controls.RemoteResponders[id] = worker.respond
	if kind == "approval" {
		plain, _ := pyjson.Plain(data).(map[string]any)
		return true, a.RegisterApproval(ctx, ref.SessionID, generation, live.Route, plain, nil)
	}
	var choices []string
	for _, choice := range frame.Vals["choices"].([]any) {
		choices = append(choices, choice.(string))
	}
	entry := &tools.ClarifyEntry{ClarifyID: id, Question: frame.Vals["question"].(string), Choices: choices, MultiSelect: frame.Vals["multi_select"].(bool)}
	return true, a.RegisterClarify(ctx, ref.SessionID, generation, entry, nil)
}

// Child env of the owning profile under multiplex, its home and secrets over a scrubbed base, nil to inherit
func workerEnv(ctx context.Context, a *SessionAuthority) ([]string, error) {
	profileHome := a.ProfileID
	if !filepath.IsAbs(profileHome) {
		return nil, nil
	}
	routed := tools.IsRoutedHome(profileHome)
	if !routed && !provider.IsMultiplexActive(ctx) {
		return nil, nil
	}
	env, err := tools.BuildSubprocessEnv(ctx, nil, tools.SubprocessEnvOptions{
		Base: tools.StripLaunchProfileEnv(nil, tools.EnvironMap(), profileHome), InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		return nil, err
	}
	secrets := provider.BuildProfileSecretScope(profileHome)
	if routed {
		tools.ScrubCredentials(env, false)
	} else {
		merged := launchEnvOnlyCredentials(profileHome, env)
		for key, value := range secrets {
			merged[key] = value
		}
		secrets = merged
	}
	for key, value := range secrets {
		env[key] = value
	}
	env[home.EnvHome] = profileHome
	home.ApplySubprocessHomeEnv(ctx, env)
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out, nil
}

// Launch profile credentials from its frozen launch env that the scrub removed, credential names only
func launchEnvOnlyCredentials(profileHome string, env map[string]string) map[string]string {
	missing := map[string]string{}
	for key, value := range provider.LaunchSecretScope(profileHome) {
		if _, present := env[key]; !present && !tools.IsIcebathInternalSecret(key) {
			missing[key] = value
		}
	}
	plain := map[string]string{}
	for key, value := range missing {
		plain[key] = value
	}
	tools.ScrubCredentials(plain, false)
	out := map[string]string{}
	for key, value := range missing {
		if _, kept := plain[key]; !kept {
			out[key] = value
		}
	}
	return out
}

// Records an interrupted result for a turn stopped before its child received a bootstrap
func interruptedBeforeBootstrap(a *SessionAuthority, row *pyjson.Object, cause *string) string {
	result := pyjson.ObjectOf("final_response", "", "interrupted", true)
	if cause != nil && *cause != "" {
		result.Set("interrupt_message", *cause)
	}
	a.mu.Lock()
	a.PendingResults[rowString(row, "admission_id")] = pyjson.ObjectOf("result", result, "usage", pyjson.NewObject())
	a.mu.Unlock()
	return ""
}

// Restart or shutdown cause latched for this generation before its worker existed, consumed once
func latchedStopCause(a *SessionAuthority, sessionID string, generation any) *string {
	a.mu.Lock()
	defer a.mu.Unlock()
	latched, ok := a.pendingStopCauses[sessionID]
	delete(a.pendingStopCauses, sessionID)
	if ok && pyrepr.Equal(latched.generation, generation) {
		reason := latched.reason
		return &reason
	}
	return nil
}

// Accepted result carrying the stop cause the owner recorded when the turn was interrupted
func withStopCause(accepted *pyjson.Object, worker *managedWorker) *pyjson.Object {
	cause := worker.cause()
	if pyrepr.Truthy(accepted.GetOr("interrupted", nil)) && cause != nil && *cause != "" {
		out := accepted.Copy()
		out.Set("interrupt_message", *cause)
		return out
	}
	return accepted
}

// Session cumulative usage a managed turn completes with, summed over its compression path, empty on a read failure
func settledSessionUsage(ctx context.Context, a *SessionAuthority, sessionID string, accepted *pyjson.Object, executionID string) *pyjson.Object {
	db := a.DB
	rows, err := func() ([]*pyjson.Object, error) {
		tip, err := executionTip(ctx, db, executionID)
		if err != nil {
			return nil, err
		}
		var path []string
		if tip != "" && tip != sessionID {
			if path, err = continuationPath(ctx, db, sessionID, tip); err != nil {
				return nil, err
			}
		}
		if path == nil {
			path = []string{sessionID}
		}
		ancestors, err := compressionAncestors(ctx, db, sessionID)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		var out []*pyjson.Object
		for _, sid := range append(ancestors, path...) {
			if seen[sid] {
				continue
			}
			seen[sid] = true
			row, err := db.GetSession(ctx, sid)
			if err != nil {
				return nil, err
			}
			if row == nil {
				row = pyjson.NewObject()
			}
			out = append(out, row)
		}
		return out, nil
	}()
	if err != nil {
		return pyjson.NewObject()
	}
	sum := func(key string) int64 {
		total := int64(0)
		for _, row := range rows {
			if value := row.GetOr(key, nil); pyrepr.Truthy(value) {
				n, _ := pyint.Int64(value)
				total += n
			}
		}
		return total
	}
	counts := pyjson.NewObject()
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
		counts.Set(key, sum(key))
	}
	prompt := counts.Vals["input_tokens"].(int64) + counts.Vals["cache_read_tokens"].(int64) + counts.Vals["cache_write_tokens"].(int64)
	output := counts.Vals["output_tokens"].(int64)
	model := accepted.GetOr("model", nil)
	if !pyrepr.Truthy(model) {
		model = rows[len(rows)-1].GetOr("model", nil)
	}
	modelText := ""
	if pyrepr.Truthy(model) {
		modelText = pyrepr.Str(model)
	}
	usage := counts.Copy()
	usage.Set("prompt_tokens", prompt)
	usage.Set("completion_tokens", output)
	usage.Set("total_tokens", prompt+output)
	usage.Set("api_calls", sum("api_call_count"))
	usage.Set("model", modelText)
	return pyjson.ObjectOf("session_usage", usage)
}

// Session this worker execution last wrote to as the owner's ledger records it, empty when unknown
func executionTip(ctx context.Context, db *state.SessionDB, executionID string) (string, error) {
	if executionID == "" {
		return "", nil
	}
	var tip string
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		row, err := state.ConnRow(ctx, conn, "SELECT session_id FROM worker_executions WHERE execution_id=?", executionID)
		if err != nil || row == nil {
			return err
		}
		tip = pyStrOf(row["session_id"])
		return nil
	})
	return tip, err
}

// Ids from start to tip when tip reaches start through compression continuations only, else nil
func continuationPath(ctx context.Context, db *state.SessionDB, start, tip string) ([]string, error) {
	path, current := []string{tip}, tip
	for current != start && len(path) <= 200 {
		row, err := db.GetSession(ctx, current)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, nil
		}
		child, err := db.IsCompressionChildRow(ctx, row)
		if err != nil {
			return nil, err
		}
		if !child {
			return nil, nil
		}
		current = pyStrOf(row.GetOr("parent_session_id", nil))
		path = append(path, current)
	}
	if current != start {
		return nil, nil
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, nil
}

// Root first ids of the sessions this one continues by compression, itself excluded
func compressionAncestors(ctx context.Context, db *state.SessionDB, sessionID string) ([]string, error) {
	var ancestors []string
	current, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for current != nil && len(ancestors) < 200 {
		child, err := db.IsCompressionChildRow(ctx, current)
		if err != nil {
			return nil, err
		}
		if !child {
			break
		}
		parent := pyStrOf(current.GetOr("parent_session_id", nil))
		if parent == sessionID || slices.Contains(ancestors, parent) {
			break
		}
		ancestors = append(ancestors, parent)
		if current, err = db.GetSession(ctx, parent); err != nil {
			return nil, err
		}
	}
	for i, j := 0, len(ancestors)-1; i < j; i, j = i+1, j-1 {
		ancestors[i], ancestors[j] = ancestors[j], ancestors[i]
	}
	return ancestors, nil
}

// Marks the generation as spawning so a gateway stop before the worker registers latches for it
func markSpawning(a *SessionAuthority, ref SessionRef, row *pyjson.Object) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.managedSpawning == nil {
		a.managedSpawning = map[string]any{}
	}
	a.managedSpawning[ref.SessionID] = row.GetOr("generation", nil)
}

// Forgets the spawning mark of a session
func unmarkSpawning(a *SessionAuthority, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.managedSpawning, sessionID)
}

// Consumes a stop latched for this generation before any worker existed, recording the interrupted result
func stoppedBeforeSpawn(a *SessionAuthority, ref SessionRef, row *pyjson.Object) bool {
	generation := row.GetOr("generation", nil)
	a.mu.Lock()
	stop, latched := a.pendingStops[ref.SessionID]
	if generation == nil || !latched || !pyrepr.Equal(stop, generation) {
		a.mu.Unlock()
		return false
	}
	delete(a.pendingStops, ref.SessionID)
	delete(a.managedSpawning, ref.SessionID)
	a.mu.Unlock()
	interruptedBeforeBootstrap(a, row, latchedStopCause(a, ref.SessionID, generation))
	return true
}

// Tracked board note of a lost card attempt, nil for a stop, a stale generation or no card
func recordLostAttempt(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, err error, reason string, scope *pyjson.Object) *trackedTask {
	if policy.KanbanJSON == nil || *policy.KanbanJSON == "" || isCancellation(err) ||
		reason == "managed_worker_stopped" || reason == "stale_generation" {
		return nil
	}
	var pid any
	if scope != nil {
		pid = scope.GetOr("pid", nil)
	}
	caller := context.WithoutCancel(ctx)
	return trackMutation(ctx, a, func(context.Context) (any, error) {
		recordWorkerLoss(caller, policy, reason, pid, scope != nil)
		return nil, nil
	})
}

// Settles a lost worker's admission unknown once its board note landed or its wait ran out, even when cancelled
func settleLost(ctx context.Context, a *SessionAuthority, row, scope *pyjson.Object, recording *trackedTask) error {
	var cancelled error
	if recording != nil {
		timer := time.NewTimer(time.Duration(lossRecordWaitSeconds * float64(time.Second)))
		select {
		case <-recording.done:
		case <-timer.C:
		case <-ctx.Done():
			cancelled = ctx.Err()
		}
		timer.Stop()
	}
	caller := context.WithoutCancel(ctx)
	if _, err := trackedWrite(caller, a, func(context.Context) (any, error) {
		return nil, loseAdmissionWorker(caller, a, row, scope)
	}, nil, false, nil); err != nil {
		return err
	}
	return cancelled
}

// Spawns the turn's child and registers it for stops, nil when the spawn was cancelled with its interrupted result recorded
func spawnRegisteredWorker(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (*managedWorker, *workerProcess, error) {
	markSpawning(a, ref, row)
	generation := row.GetOr("generation", nil)
	logPath := workerLogPath(a.ProfileID)
	process, err := func() (*workerProcess, error) {
		env, err := workerEnv(ctx, a)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cwd := home.InstallRoot()
		return acquireProcess(ctx, func() (*workerProcess, error) { return spawnManagedWorker(env, cwd, logPath != "") })
	}()
	if err != nil {
		unmarkSpawning(a, ref.SessionID)
		if isCancellation(err) {
			interruptedBeforeBootstrap(a, row, latchedStopCause(a, ref.SessionID, generation))
			return nil, nil, nil
		}
		return nil, nil, err
	}
	worker := newManagedWorker(process)
	if logPath != "" {
		worker.drainStderr(context.WithoutCancel(ctx), logPath)
	}
	a.mu.Lock()
	if a.managedWorkers == nil {
		a.managedWorkers = map[string]managedWorkerHandle{}
	}
	a.managedWorkers[ref.SessionID] = worker
	delete(a.managedSpawning, ref.SessionID)
	a.mu.Unlock()
	worker.setCause(latchedStopCause(a, ref.SessionID, generation))
	return worker, process, nil
}

// Python class name or wire reason of a managed worker failure for the loss log
func lossReason(err error) string {
	var refusal *state.RuntimeStoreError
	switch {
	case errors.As(err, &refusal):
		return refusal.Reason
	case isCancellation(err):
		return "CancelledError"
	}
	if kind := pyErrorKind(err); kind != "" {
		return kind
	}
	var decode *pyjson.DecodeError
	if errors.As(err, &decode) {
		return "JSONDecodeError"
	}
	return pypath.OSErrorClass(err)
}

// Refuses a turn whose folder is gone before any worker exists, else runs it in a managed worker
func admitManaged(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, policy *LocalSessionPolicy) (any, error) {
	if !workspaceReady(policy) {
		result := workspaceUnavailableResult(ctx, policy)
		a.mu.Lock()
		a.PendingResults[rowString(row, "admission_id")] = pyjson.ObjectOf("result", result, "usage", pyjson.NewObject())
		a.mu.Unlock()
		return result.GetOr("final_response", nil), nil
	}
	return executeManaged(ctx, a, ref, row, policy)
}

// Runs an admitted turn in a reserved and supervised worker child, unknown when the worker is lost
func executeManaged(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, policy *LocalSessionPolicy) (any, error) {
	admissionID := rowString(row, "admission_id")
	markSpawning(a, ref, row)
	text, refusal := managedTurnText(ctx, a, row, policy)
	if refusal != nil {
		unmarkSpawning(a, ref.SessionID)
		a.mu.Lock()
		a.PendingResults[admissionID] = pyjson.ObjectOf("result", refusedReferencesResult(row, *refusal), "usage", pyjson.NewObject())
		a.mu.Unlock()
		return *refusal, nil
	}
	if stoppedBeforeSpawn(a, ref, row) {
		return "", nil
	}
	worker, process, err := spawnRegisteredWorker(ctx, a, ref, row)
	if err != nil {
		return nil, err
	}
	if worker == nil {
		return "", nil
	}
	a.AdoptAgent(ref.SessionID, rowGeneration(row), worker)
	var scope *pyjson.Object
	reply, err := superviseManaged(ctx, a, ref, row, policy, worker, process, text, &scope)
	if err != nil {
		reply, err = loseManaged(ctx, a, ref, row, policy, worker, scope, err)
	}
	a.mu.Lock()
	delete(a.managedWorkers, ref.SessionID)
	a.mu.Unlock()
	if closeErr := worker.close(); closeErr != nil {
		return nil, closeErr
	}
	return reply, err
}

// Hello, reservation, bootstrap and the frame loop of one managed turn, the reply once the worker finished
func superviseManaged(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, policy *LocalSessionPolicy,
	worker *managedWorker, process *workerProcess, text *string, scopeOut **pyjson.Object) (any, error) {
	admissionID := rowString(row, "admission_id")
	generation := rowGeneration(row)
	helloTimeout := managedHelloSeconds
	hello, err := worker.nextFrame(ctx, &helloTimeout, 0)
	if err != nil {
		return nil, err
	}
	if worker.stopped() {
		return nil, runtimeRefusal("managed_worker_stopped")
	}
	reserved, err := trackedWrite(ctx, a, func(context.Context) (any, error) {
		return reserveAdmissionWorker(context.WithoutCancel(ctx), a, admissionID, process, row.GetOr("principal_id", nil), hello, ownerWorkerSubject())
	}, nil, false, nil)
	if err != nil {
		return nil, err
	}
	scope := reserved.(*pyjson.Object)
	*scopeOut = scope
	pid, _ := pyint.Int64(scope.Vals["pid"])
	birth, _ := scope.Vals["birth"].(float64)
	worker.mu.Lock()
	worker.identity = &workerIdentity{pid: int(pid), birth: birth}
	worker.mu.Unlock()
	frame, err := managedBootstrap(ctx, a, ref, row, policy, scope, text)
	if err != nil {
		return nil, err
	}
	worker.mu.Lock()
	for _, value := range []any{frame.GetOr("api_key", nil), scope.GetOr("secret", nil)} {
		if secret, isText := value.(string); isText && secret != "" {
			worker.secrets = append(worker.secrets, secret)
		}
	}
	worker.mu.Unlock()
	if err := worker.send(frame); err != nil {
		return nil, err
	}
	worker.startWriter()
	var accepted, usage *pyjson.Object
	for {
		frame, err := worker.nextFrame(ctx, nil, managedStopAckSeconds)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		executing, running := a.executing[ref.SessionID]
		a.mu.Unlock()
		if !running || executing != generation {
			return nil, runtimeRefusal("stale_generation")
		}
		live, ok := a.live(ref.SessionID)
		if !ok {
			return nil, &pyKeyError{ref.SessionID}
		}
		live.EventStream.Lock.Lock()
		handled, err := promptFrame(ctx, a, ref, row, worker, frame)
		live.EventStream.Lock.Unlock()
		if err != nil {
			return nil, err
		}
		if handled {
			continue
		}
		if frameKeysAre(frame, "type", "reason") && frame.Vals["type"] == "error" && frame.Vals["reason"] == "managed_worker_failed" {
			return nil, runtimeRefusal("managed_worker_failed")
		}
		kind, _ := frame.Get("type")
		if kind == "ready" && frameKeysAre(frame, "type", "pid") && pyrepr.Equal(frame.Vals["pid"], scope.Vals["pid"]) {
			continue
		}
		if delta, isText := frame.GetOr("text", nil).(string); kind == "delta" && frameKeysAre(frame, "type", "text") && isText {
			if _, err := a.PublishExecution(ref.SessionID, generation, "message.delta", pyjson.ObjectOf("text", delta)); err != nil {
				return nil, err
			}
			continue
		}
		published, err := publishWorkerToolEvent(ctx, a, ref.SessionID, generation, frame)
		if err != nil {
			return nil, err
		}
		if published {
			continue
		}
		if kind == "result" && frameKeysAre(frame, "type", "result") && accepted == nil {
			accepted, usage, err = AcceptResult(frame.Vals["result"])
			if err != nil {
				if pyErrorKind(err) == "ValueError" {
					return nil, runtimeRefusal("invalid_worker_result")
				}
				return nil, err
			}
			live.pendingControls().snapshot(ref.SessionID, nil)
			if err := worker.send(pyjson.ObjectOf("type", "finish")); err != nil {
				return nil, err
			}
			continue
		}
		if isTypeFrame(frame, "finished") && accepted != nil {
			process.wait(10)
			accepted = withStopCause(accepted, worker)
			result := accepted.Copy()
			result.Set("local_viewer", true)
			settled := pyjson.ObjectOf("result", result, "usage", usage)
			settled.Update(settledSessionUsage(ctx, a, pyStrOf(scope.Vals["session_id"]), accepted, pyStrOf(scope.Vals["execution_id"])))
			a.mu.Lock()
			a.PendingResults[admissionID] = settled
			a.mu.Unlock()
			return accepted.GetOr("final_response", nil), nil
		}
		return nil, runtimeRefusal("invalid_worker_frame")
	}
}

// Settles a lost worker, interrupted when nothing was bootstrapped, otherwise unknown with the FIFO paused behind it
func loseManaged(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, policy *LocalSessionPolicy,
	worker *managedWorker, scope *pyjson.Object, cause error) (any, error) {
	reason := lossReason(cause)
	slog.Warn(fmt.Sprintf("Managed worker lost: %s", reason))
	if scope == nil {
		if isCancellation(cause) || isRuntimeRefusal(cause, "managed_worker_stopped") {
			return interruptedBeforeBootstrap(a, row, worker.cause()), nil
		}
		recordLostAttempt(ctx, a, policy, cause, reason, scope)
		return nil, cause
	}
	if err := settleLost(ctx, a, row, scope, recordLostAttempt(ctx, a, policy, cause, reason, scope)); err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	admissionID := rowString(row, "admission_id")
	live.EventStream.Lock.Lock()
	live.pendingControls().snapshot(ref.SessionID, nil)
	err := a.publishPending(context.WithoutCancel(ctx), ref)
	if err == nil {
		live.EventStream.Publish(ref.SessionID, pyjson.ObjectOf("text", "Worker execution is unknown.",
			"content", "Worker execution is unknown.", "admission_id", admissionID, "outcome", "unknown", "status", "error"), "message.complete", nil)
	}
	live.EventStream.Lock.Unlock()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	waiter := a.waiters[admissionID]
	delete(a.waiters, admissionID)
	a.mu.Unlock()
	if waiter != nil && !waiter.isDone() {
		waiter.setResult("Worker execution is unknown.")
	}
	return nil, &ManagedExecutionUnknown{Message: "managed_worker_unknown"}
}
