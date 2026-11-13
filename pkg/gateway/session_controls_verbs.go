package gateway

import (
	"context"
	"errors"
	"maps"
	"math"
	"math/big"
	"slices"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Generation a turn never carries, standing for a None or non integer generation param
const absentGeneration = math.MinInt64

// Execution generation of a param compared the way Python's equality compares it with an int
func generationParam(value any) int64 {
	switch v := value.(type) {
	case bool:
		if v {
			return 1
		}
		return 0
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<63 {
			return int64(v)
		}
		return absentGeneration
	}
	if n, ok := pyrepr.Int(value); ok && n.IsInt64() {
		return n.Int64()
	}
	return absentGeneration
}

// Dataclass dict of a session handle
func sessionHandleDict(h SessionHandle) *pyjson.Object {
	return pyjson.ObjectOf("ref", pyjson.ObjectOf("profile_id", h.Ref.ProfileID, "session_id", h.Ref.SessionID),
		"instance_id", h.InstanceID, "authority_epoch", h.AuthorityEpoch, "revision", h.Revision,
		"execution_generation", h.ExecutionGeneration, "execution_state", h.ExecutionState)
}

// Runtime store refusal an error carries, nil for any other error
func runtimeStoreErrorOf(err error) *state.RuntimeStoreError {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return refusal
	}
	return nil
}

// Admission id param, empty for a missing or non text one
func admissionIDParam(params *pyjson.Object) string {
	id, _ := params.GetOr("admission_id", nil).(string)
	return id
}

// Liveness verb in the dispatch handler shape
func (c *AuthorityConnection) pingVerb(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	plain, _ := pyjson.Plain(params).(map[string]any)
	return c.ping(plain)
}

// Ends one subscription of this connection, a viewer closing the conversation when it names an end reason
func (c *AuthorityConnection) detach(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	named := map[string]bool{}
	for _, key := range params.Keys {
		if key != "end_reason" {
			named[key] = true
		}
	}
	if len(named) != 2 || !named["session_id"] || !named["subscription_id"] {
		return nil, runtimeRefusal("invalid_params")
	}
	sessionID, sessionText := params.GetOr("session_id", nil).(string)
	subscriptionID, subscriptionText := params.GetOr("subscription_id", nil).(string)
	rawReason := params.GetOr("end_reason", nil)
	endReason, reasonText := rawReason.(string)
	if !sessionText || sessionID == "" || !subscriptionText || subscriptionID == "" ||
		(rawReason != nil && !(reasonText && endReasonPattern.MatchString(endReason))) {
		return nil, runtimeRefusal("invalid_params")
	}
	result := pyjson.ObjectOf("session_id", sessionID, "subscription_id", subscriptionID, "detached", false)
	c.mu.Lock()
	current := c.subscriptions[sessionID]
	c.mu.Unlock()
	if current != subscriptionID {
		return result, nil
	}
	if err := c.authority.Detach(ctx, c.actor, subscriptionID, endReason, false); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.subscriptions[sessionID] == subscriptionID {
		delete(c.subscriptions, sessionID)
	}
	if rawReason != nil {
		delete(c.handedBack, sessionID)
	} else {
		c.handedBack[sessionID] = true
	}
	c.mu.Unlock()
	result.Set("detached", true)
	return result, nil
}

// Replay window of the session's event stream after a cursor, with the server requests still open
func (c *AuthorityConnection) eventsSince(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := c.authority.Authorize(ctx, c.actor, ref, "session:read"); err != nil {
		return nil, err
	}
	sequence := params.GetOr("last_sequence", params.GetOr("last_seen", int64(0)))
	rawEpoch := params.GetOr("replay_epoch", nil)
	epoch, epochText := rawEpoch.(string)
	n, isInt := strictInt(sequence)
	if !isInt || n.Sign() < 0 || (rawEpoch != nil && !epochText) {
		return nil, runtimeRefusal("invalid_params")
	}
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return nil, err
	}
	stream := live.EventStream
	after := int(n.Int64())
	if !n.IsInt64() {
		after = math.MaxInt
	}
	replay := stream.Since(epoch, after)
	if required, _ := replay.Get("snapshot_required"); required != true {
		highest := after
		events, _ := replay.GetOr("events", nil).([]any)
		for i, raw := range events {
			frame, _ := raw.(*pyjson.Object)
			seq, _ := pyrepr.Int(frame.GetOr("seq", nil))
			if seq != nil && (i == 0 || int(seq.Int64()) > highest) {
				highest = int(seq.Int64())
			}
		}
		replayEpoch, _ := replay.GetOr("replay_epoch", "").(string)
		c.mu.Lock()
		c.replayCursors[ref.SessionID] = ReplayCursor{Epoch: replayEpoch, Sequence: int64(highest)}
		c.mu.Unlock()
	}
	out := replay.Copy()
	open := []any{}
	for _, request := range OpenServerRequests(stream, ref.SessionID) {
		open = append(open, request)
	}
	out.Set("open_requests", open)
	return out, nil
}

// Params a prompt.submit shaped request may carry
var submissionFields = []string{"session_id", "text", "submission_id", "input_id", "queued", "attachments", "finite",
	"unattended", "surface", "voice_context", "interrupted", "voice_turn", "display_kind", "title_preview",
	"turn_author", "api_text", "model_override", "clarify_batch"}

// Admission a prompt.submit shaped request asks for, prompt.replace_queued too
func (c *AuthorityConnection) submission(ctx context.Context, ref SessionRef, params *pyjson.Object) (Submission, error) {
	c.mu.Lock()
	_, subscribed := c.subscriptions[ref.SessionID]
	c.mu.Unlock()
	if !subscribed {
		return Submission{}, runtimeRefusal("permission_denied")
	}
	if paramsOutside(params, submissionFields...) {
		return Submission{}, runtimeRefusal("invalid_params")
	}
	requestID := params.GetOr("submission_id", nil)
	if !pyTruthy(requestID) {
		requestID = params.GetOr("input_id", nil)
	}
	id, isText := requestID.(string)
	if !isText || id == "" {
		return Submission{}, runtimeRefusal("invalid_params")
	}
	plain, _ := pyjson.Plain(params).(map[string]any)
	finite, err := admitFiniteFor(ctx, plain, &finiteSubmitter{connection: c, ref: ref})
	if err != nil {
		return Submission{}, err
	}
	payload := map[string]any{"text": params.GetOr("text", nil)}
	for _, key := range finite.Keys {
		payload[key] = finite.Vals[key]
	}
	maps.Copy(payload, submitSurfaceFields(plain))
	maps.Copy(payload, SubmitDisplayFields(plain))
	maps.Copy(payload, submitCLITurnFields(plain))
	if value, ok := plain["attachments"]; ok {
		payload["attachments"] = value
	}
	return Submission{RequestID: id, Ref: ref, Payload: payload, Intent: "queue"}, nil
}

// Receipt of one admission, with the committed structured result of a terminal one when asked
func (c *AuthorityConnection) receipt(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	receipt, err := c.authority.Receipt(ctx, c.actor, ref, admissionIDParam(params))
	if err != nil {
		return nil, err
	}
	out := admissionReceiptDict(receipt)
	if params.GetOr("include_result", nil) == true && receipt.Status == AdmissionTerminal {
		saved, err := admissionResult(ctx, c.authority.DB, receipt.AdmissionID)
		if err != nil {
			return nil, err
		}
		if record, ok := saved.(*pyjson.Object); ok {
			out.Set("result", record.GetOr("result", nil))
			usage := record.GetOr("usage", nil)
			if !pyTruthy(usage) {
				usage = pyjson.NewObject()
			}
			out.Set("usage", usage)
		}
	}
	return out, nil
}

// Withdraws a queued admission, stopping the exact claim when the drain already started it
func (c *AuthorityConnection) cancel(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	a := c.authority
	admissionID := admissionIDParam(params)
	receipt, err := a.Receipt(ctx, c.actor, ref, admissionID)
	if err != nil {
		return nil, err
	}
	fresh := func() (any, error) {
		latest, err := a.Receipt(ctx, c.actor, ref, admissionID)
		if err != nil {
			return nil, err
		}
		return admissionReceiptDict(latest), nil
	}
	if receipt.Status == "started" {
		row, err := a.DB.GetSessionAdmission(ctx, admissionID)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
		}
		if status, _ := row.GetOr("status", nil).(string); status == "started" {
			var generation any
			if receipt.ExecutionGeneration != nil {
				generation = *receipt.ExecutionGeneration
			}
			if _, err := c.interrupt(ctx, ref, pyjson.ObjectOf("execution_generation", generation)); err != nil {
				if refusal := runtimeStoreErrorOf(err); refusal == nil || refusal.Reason != "stale_generation" {
					return nil, err
				}
			}
			return fresh()
		}
	}
	if receipt.Status != "queued" {
		cancelled, err := a.CancelQueued(ctx, c.actor, ref, admissionID)
		if err != nil {
			return nil, err
		}
		return admissionReceiptDict(cancelled), nil
	}
	row, err := a.CancelOrClaimed(ctx, c.actor, ref, admissionID)
	if err != nil {
		return nil, err
	}
	if row != nil {
		if _, err := c.interrupt(ctx, ref, pyjson.ObjectOf("execution_generation", row.GetOr("generation", nil))); err != nil {
			return nil, err
		}
	}
	return fresh()
}

// Operator acknowledgement that an unknown turn will not finish
func (c *AuthorityConnection) resolveUnknown(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if len(params.Keys) != 3 || paramsOutside(params, "session_id", "admission_id", "execution_generation") {
		return nil, runtimeRefusal("invalid_params")
	}
	receipt, err := c.authority.ResolveUnknown(ctx, c.actor, ref, admissionIDParam(params),
		generationParam(params.GetOr("execution_generation", nil)))
	if err != nil {
		return nil, err
	}
	return admissionReceiptDict(receipt), nil
}

// Stops the running turn of a generation, a managed worker's turn first
func (c *AuthorityConnection) interrupt(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	generation := params.GetOr("execution_generation", nil)
	handled, err := interruptManaged(ctx, c.authority, c.actor, ref, generation)
	if err != nil {
		return nil, err
	}
	var handle SessionHandle
	if handled {
		handle, err = c.authority.handle(ctx, ref)
	} else {
		handle, err = c.authority.Interrupt(ctx, c.actor, ref, generationParam(generation))
	}
	if err != nil {
		return nil, err
	}
	return sessionHandleDict(handle), nil
}

// Answers one approval prompt of a generation
func (c *AuthorityConnection) respond(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if len(params.Keys) != 4 || paramsOutside(params, "session_id", "execution_generation", "prompt_id", "choice") {
		return nil, runtimeRefusal("invalid_params")
	}
	return c.authority.Respond(ctx, c.actor, ref, params.GetOr("execution_generation", nil), params.GetOr("prompt_id", nil),
		pyjson.ObjectOf("choice", params.GetOr("choice", nil)), "approval")
}

// Answers one clarify prompt of a generation
func (c *AuthorityConnection) respondClarify(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if len(params.Keys) != 4 || paramsOutside(params, "session_id", "execution_generation", "prompt_id", "answer") {
		return nil, runtimeRefusal("invalid_params")
	}
	return c.authority.Respond(ctx, c.actor, ref, params.GetOr("execution_generation", nil), params.GetOr("prompt_id", nil),
		pyjson.ObjectOf("answer", params.GetOr("answer", nil)), "clarify")
}

// Tears the connection down, orphaning its subscriptions and arming the close of sessions it kept in the background
func (c *AuthorityConnection) Close(ctx context.Context) error {
	c.mu.Lock()
	siblings := make([]*AuthorityConnection, 0, len(c.siblings))
	for _, sibling := range c.siblings {
		siblings = append(siblings, sibling)
	}
	c.mu.Unlock()
	for _, sibling := range siblings {
		if err := sibling.Close(ctx); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.siblings = map[string]*AuthorityConnection{}
	c.mu.Unlock()
	forgetOneshots(c)
	a := c.authority
	scoped := ownerScope(ctx, a, false)
	c.mu.Lock()
	type held struct {
		sessionID    string
		subscription any
	}
	subscriptions := make([]held, 0, len(c.subscriptions))
	for sessionID, subscription := range c.subscriptions {
		subscriptions = append(subscriptions, held{sessionID, subscription})
	}
	handedBack := make([]string, 0, len(c.handedBack))
	for sessionID := range c.handedBack {
		handedBack = append(handedBack, sessionID)
	}
	c.mu.Unlock()
	for _, entry := range subscriptions {
		live, ok := a.live(entry.sessionID)
		subscription, _ := entry.subscription.(string)
		if !ok {
			continue
		}
		a.mu.Lock()
		_, subscribed := live.Subscribers[subscription]
		a.mu.Unlock()
		if subscribed {
			if err := a.Detach(scoped, c.actor, subscription, "", true); err != nil {
				return err
			}
		}
	}
	for _, sessionID := range handedBack {
		if err := armDetachedOrphanClose(scoped, a, sessionID); err != nil {
			return err
		}
	}
	unbindNativeTransport(a, c.actor)
	c.mu.Lock()
	c.subscriptions = map[string]any{}
	c.mu.Unlock()
	a.mu.Lock()
	delete(a.Events, c.actor.TransportID)
	a.mu.Unlock()
	return nil
}

// Numeric sort key of a started_at figure, zero for a missing one
func startedAtKey(value any) float64 {
	if !pyTruthy(value) {
		return 0
	}
	f, _ := pyint.Float(value)
	return f
}

// Live picker feed, the stored one, or one title's identity, over the sessions the actor may read
func (c *AuthorityConnection) listSessions(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if !c.actor.Capabilities["session:read"] {
		return nil, runtimeRefusal("permission_denied")
	}
	limit, isInt := strictInt(params.GetOr("limit", int64(200)))
	if paramsOutside(params, "limit", "title", "include_hidden", "scope") || !isInt || limit.Cmp(big.NewInt(1)) < 0 ||
		limit.Cmp(big.NewInt(200)) > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	scope := params.GetOr("scope", "live")
	switch scope.(type) {
	case *pyjson.Object, []any:
		return nil, &PyError{Kind: "TypeError", Msg: "unhashable type: '" + pyTypeName(scope) + "'"}
	}
	_, hasTitle := params.Get("title")
	_, hasScope := params.Get("scope")
	if (scope != "live" && scope != "stored") || (hasTitle && hasScope) {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	if scope == "stored" {
		return storedPickerSessions(ctx, a, c.actor, int(limit.Int64()), pyTruthy(params.GetOr("include_hidden", false)))
	}
	if hasTitle {
		title := params.GetOr("title", nil)
		found, err := resolveTitledSession(ctx, a, c.actor, title, true)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return pyjson.ObjectOf("sessions", []any{}, "scope", "stored"), nil
		}
		row, err := a.DB.GetSession(ctx, found.SessionID)
		if err != nil {
			return nil, err
		}
		if row == nil {
			row = pyjson.NewObject()
		}
		byTitle, _, err := a.DB.ResolveSessionByTitle(ctx, title.(string))
		if err != nil {
			return nil, err
		}
		if byTitle == "" {
			byTitle = found.SessionID
		}
		root, err := a.DB.GetSession(ctx, byTitle)
		if err != nil {
			return nil, err
		}
		if root == nil {
			root = row
		}
		or := func(value, fallback any) any {
			if pyTruthy(value) {
				return value
			}
			return fallback
		}
		_, running := a.live(found.SessionID)
		entry := pyjson.ObjectOf("session_id", found.SessionID, "id", or(root.GetOr("id", nil), found.SessionID),
			"resolved_id", found.SessionID, "title", or(row.GetOr("title", nil), ""), "root_title", or(root.GetOr("title", nil), ""),
			"source", row.GetOr("source", nil), "started_at", row.GetOr("started_at", nil),
			"message_count", row.GetOr("message_count", int64(0)), "running", running)
		return pyjson.ObjectOf("sessions", []any{entry}, "scope", "stored"), nil
	}
	entries, err := liveEntries(ctx, a, c.actor)
	if err != nil {
		return nil, err
	}
	sessions := []*pyjson.Object{}
	for _, entry := range entries {
		row := entry.row
		source, _ := row["source"].(string)
		hidden := !pyTruthy(params.GetOr("include_hidden", nil)) && (pyTruthy(row["hidden"]) || pyTruthy(row["archived"]))
		if slices.Contains(InternalListingSources, source) || entry.closed || hidden {
			continue
		}
		title := row["title"]
		if !pyTruthy(title) {
			title = ""
		}
		count, hasCount := row["message_count"]
		if !hasCount {
			count = int64(0)
		}
		sessions = append(sessions, pyjson.ObjectOf("session_id", entry.sessionID, "id", entry.sessionID, "title", title,
			"source", row["source"], "started_at", row["started_at"], "message_count", count, "model", row["model"],
			"running", entry.handle.ExecutionState == "running"))
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return startedAtKey(sessions[i].GetOr("started_at", nil)) > startedAtKey(sessions[j].GetOr("started_at", nil))
	})
	out := []any{}
	for i, session := range sessions {
		if i >= int(limit.Int64()) {
			break
		}
		out = append(out, session)
	}
	return pyjson.ObjectOf("sessions", out, "scope", "live"), nil
}

// Owner identity, capabilities, creation parameters and the owning profile's skin
func (c *AuthorityConnection) describe(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if _, err := c.pingVerb(ctx, ref, params); err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(Surfaces))
	for name := range Surfaces {
		sources = append(sources, name)
	}
	sort.Strings(sources)
	parameters := []string{"title"}
	for name := range CreateFields {
		if name != "title" {
			parameters = append(parameters, name)
		}
	}
	sort.Strings(parameters)
	a := c.authority
	return pyjson.ObjectOf("instance_id", a.InstanceID, "profile_id", a.ProfileID, "authority_epoch", a.Epoch,
		"capabilities", []any{"durable-admission-v1", "event-replay-v1", "local-cli-create-v1", "acp-editor-policy-v1",
			"acp-session-mcp-v1", "clarify-batch-v1"},
		"session_create", pyjson.ObjectOf("sources", stringsAny(sources), "parameters", stringsAny(parameters)),
		"skin", resolvedSkin(ctx)), nil
}

// Strings as a JSON list value
func stringsAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
