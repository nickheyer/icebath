package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Canonical local id abbreviation the TUI exit hint prints
var localIDHint = regexp.MustCompile(`^local-[0-9a-f]{16}$`)

// Fields that make a launch bypass the profile config
var bypassFields = []string{"safe_mode", "ignore_user_config"}

// Refusal with the session's fresh revision fences attached so the client retries once without a re-read
type fencedStoreError struct {
	refusal              *state.RuntimeStoreError
	revision, generation int64
}

func (e *fencedStoreError) Error() string { return e.refusal.Error() }

func (e *fencedStoreError) Unwrap() error { return e.refusal }

func (e *fencedStoreError) RefusalRevision() (int64, int64, bool) {
	return e.revision, e.generation, true
}

// Hex of sixteen random bytes, the uuid4 hex a creation request id defaults to
func newRequestID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return hex.EncodeToString(raw[:])
}

// Session create verb, a titled create resolved, created and titled as one step per authority
func (c *AuthorityConnection) create(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	raw, titled := params.Get("title")
	if !titled {
		return c.createSession(ctx, params, "")
	}
	params.Delete("title")
	title, err := validateTitle(raw)
	if err != nil {
		return nil, err
	}
	lock := &c.authority.titledCreateLock
	if err := lock.Lock(ctx); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	return c.createSession(ctx, params, title)
}

// Resolves a titled session or stages, commits, titles and hides a new one, then resumes it
func (c *AuthorityConnection) createSession(ctx context.Context, params *pyjson.Object, title string) (any, error) {
	a := c.authority
	hidden, _ := params.Get("hidden")
	params.Delete("hidden")
	if hidden == nil {
		hidden = false
	}
	hide, isBool := hidden.(bool)
	if !isBool {
		field, detail := "hidden", "must be a boolean"
		return nil, state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	var ref *SessionRef
	if title != "" {
		found, err := resolveTitledSession(ctx, a, c.actor, title, true)
		if err != nil {
			return nil, err
		}
		ref = found
	}
	if ref == nil && title == "Bot Chat" {
		bypass := false
		for _, name := range bypassFields {
			if params.GetOr(name, nil) == true {
				bypass = true
			}
		}
		if !bypass {
			follows, err := stagedCreationFollows(ctx, a, c.actor, params.GetOr("request_id", nil))
			if err != nil {
				return nil, err
			}
			if _, set := params.Get("follow_profile_config"); follows && !set {
				params.Set("follow_profile_config", true)
			}
		}
	}
	if ref == nil {
		var skillsPrompt *string
		if _, named := params.Get("skills"); named {
			if _, set := params.Get("request_id"); !set {
				params.Set("request_id", newRequestID())
			}
			sid, err := state.LocalIdentity(a.ProfileID, c.actor.Subject, params.GetOr("request_id", nil))
			if err != nil {
				return nil, err
			}
			row, err := a.DB.GetSession(ctx, sid)
			if err != nil {
				return nil, err
			}
			if row == nil {
				plain, _ := pyjson.Plain(params).(map[string]any)
				if skillsPrompt, err = renderLaunchSkills(ctx, plain, sid); err != nil {
					return nil, err
				}
			}
		}
		desktopLocal := false
		if ws, ok := c.transport.(*tuigateway.WSTransport); ok && ws != nil {
			desktopLocal = ws.DesktopLocal
		}
		plain, _ := pyjson.Plain(params).(map[string]any)
		existing, staged, err := stageLocalSession(ctx, a, c.actor, plain, localCreateOptions{SkillsPrompt: skillsPrompt,
			ClientLocal: c.nativeOwner, DesktopLocal: desktopLocal})
		if err != nil {
			return nil, err
		}
		if staged != nil {
			receipt := *staged
			if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
				return a.DB.CommitLocalSession(taskCtx, a.Epoch, receipt)
			}, nil, false, nil); err != nil {
				return nil, err
			}
			sid, err := receiptField(receipt.Receipt, "session_id")
			if err != nil {
				return nil, err
			}
			restored, err := restoreLocalSession(ctx, a, pyStrOf(sid))
			if err != nil {
				return nil, err
			}
			existing = &restored
		}
		ref = existing
		if params.GetOr("accept_hooks", nil) == true {
			receipt, err := a.DB.LocalReceipt(ctx, ref.SessionID)
			if err != nil {
				return nil, err
			}
			if receipt == nil {
				return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
			}
			raw, err := receiptField(receipt, "policy")
			if err != nil {
				return nil, err
			}
			policy, err := restorePolicy(raw)
			if err != nil {
				return nil, err
			}
			if err := acceptLaunchHooks(ctx, policy); err != nil {
				return nil, err
			}
		}
		created := *ref
		if title != "" {
			if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
				return nil, titleNewSession(taskCtx, a, created, title)
			}, nil, false, nil); err != nil {
				return nil, err
			}
		}
		if hide {
			if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
				return a.DB.SetSessionHidden(taskCtx, created.SessionID, true)
			}, nil, false, nil); err != nil {
				return nil, err
			}
		}
	}
	return c.resume(ctx, *ref, pyjson.NewObject())
}

// Session info verb, the local session info plus its banner inventory
func (c *AuthorityConnection) info(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	if len(params.Keys) != 1 || params.Keys[0] != "session_id" {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := c.authority.Resolve(ctx, c.actor, ref); err != nil {
		return nil, err
	}
	return snapshotInfo(ctx, c.authority, ref)
}

// Session a resume names, latest or titled, a local id hint expanded and a lineage resolved to its owner
func (c *AuthorityConnection) resumeTarget(ctx context.Context, ref SessionRef, params *pyjson.Object) (SessionRef, error) {
	a := c.authority
	if latest, ok := params.Get("latest"); ok {
		return resolveLatestSession(ctx, a, c.actor, latest, params.GetOr("workspace", nil))
	}
	if title, ok := params.Get("title"); ok {
		found, err := resolveTitledSession(ctx, a, c.actor, title, false)
		if err != nil {
			return SessionRef{}, err
		}
		return *found, nil
	}
	if localIDHint.MatchString(ref.SessionID) {
		resolved, err := a.DB.ResolveSessionID(ref.SessionID)
		if err != nil {
			return SessionRef{}, err
		}
		if resolved != "" {
			ref = SessionRef{ProfileID: ref.ProfileID, SessionID: resolved}
		}
	}
	if err := refreshSettled(ctx, a, ref.SessionID); err != nil {
		return SessionRef{}, err
	}
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil || row == nil || len(row.Keys) == 0 {
		return ref, err
	}
	source, _ := row.GetOr("source", nil).(string)
	local := source == string(PlatformLocal)
	for _, legacy := range state.LegacyLocalSources() {
		if source == legacy {
			local = true
		}
	}
	if !local {
		if local, err = adoptsClientHistory(ctx, a, c.actor, row); err != nil {
			return SessionRef{}, err
		}
	}
	if !local {
		return messagingOwnerRef(ctx, a, ref)
	}
	tip, err := a.DB.GetCompressionTip(ctx, ref.SessionID)
	if err != nil {
		return SessionRef{}, err
	}
	if tip == "" {
		tip = ref.SessionID
	}
	return resolveLocalTarget(ctx, a, c.actor, tip)
}

// Session resume verb, the attach snapshot with transcript, pending rows, open prompts and local info
func (c *AuthorityConnection) resume(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	a := c.authority
	c.mu.Lock()
	cursor, hasCursor := c.replayCursors[ref.SessionID]
	delete(c.replayCursors, ref.SessionID)
	c.mu.Unlock()
	requested := ref.SessionID
	ref, err := c.resumeTarget(ctx, ref, params)
	if err != nil {
		return nil, err
	}
	if editor := params.GetOr("editor", nil); editor != nil {
		if err := a.Authorize(ctx, c.actor, ref, "session:control"); err != nil {
			return nil, err
		}
		if err := resumeEditorMCP(ctx, a, ref, editor); err != nil {
			return nil, err
		}
	}
	if apiKey, ok := params.Get("api_key"); ok {
		if err := a.Authorize(ctx, c.actor, ref, "session:control"); err != nil {
			return nil, err
		}
		if err := rebindLaunchKey(ctx, a, ref, apiKey); err != nil {
			return nil, err
		}
	}
	if err := a.Authorize(ctx, c.actor, ref, "session:read"); err != nil {
		return nil, err
	}
	mounted, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{key: ref.SessionID}
	}
	if c.actor.Capabilities["session:control"] && mounted.Source != nil && mounted.Source.Platform == PlatformLocal {
		c.mu.Lock()
		_, attached := c.subscriptions[ref.SessionID]
		c.mu.Unlock()
		refreshResumedPolicy(ctx, a, ref, attached)
	}
	omit := params.GetOr("omit_messages", nil) == true
	var replayFrom *ReplayCursor
	if hasCursor && ref.SessionID == requested {
		replayFrom = &cursor
	}
	snapshot, err := a.Attach(ctx, c.actor, ref, !omit, replayFrom)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.subscriptions[ref.SessionID] = snapshot.SubscriptionID
	delete(c.handedBack, ref.SessionID)
	c.mu.Unlock()
	c.spawnNotice(ctx, ref)
	info := pyjson.ObjectOf("desktop_protocol", CanonicalGatewayProtocol)
	attachedLive, ok := a.live(snapshot.Handle.Ref.SessionID)
	if !ok {
		return nil, &pyKeyError{key: snapshot.Handle.Ref.SessionID}
	}
	if source := attachedLive.Source; source != nil && source.Platform == PlatformLocal {
		policy, err := policyForSource(ctx, a.Runner, source)
		if err != nil {
			return nil, err
		}
		refreshProfileHooks(ctx, policy)
		if info, err = snapshotInfo(ctx, a, snapshot.Handle.Ref); err != nil {
			return nil, err
		}
	}
	history := append([]map[string]any(nil), snapshot.History...)
	display := pyjson.NewObject()
	if pyTruthy(params.GetOr("display_projection", nil)) {
		shown := []map[string]any{}
		for _, row := range history {
			if !modelOnly(row) {
				shown = append(shown, row)
			}
		}
		rows, err := displayProjection(ctx, shown, a.ProfileID)
		if err != nil {
			return nil, err
		}
		display.Set("display_messages", rows)
	}
	transcript, err := resumeTranscript(ctx, a, ref, snapshot, history, omit)
	if err != nil {
		return nil, err
	}
	connection, err := pendingConnection(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	pending := make([]any, len(snapshot.Pending))
	for i, row := range snapshot.Pending {
		pending[i] = pendingAdmissionDict(row)
	}
	prompts := make([]any, len(snapshot.Prompts))
	for i, prompt := range snapshot.Prompts {
		prompts[i] = prompt
	}
	open := []any{}
	for _, request := range OpenServerRequests(attachedLive.EventStream, snapshot.Handle.Ref.SessionID) {
		open = append(open, request)
	}
	result := pyjson.ObjectOf("session_id", ref.SessionID, "stored_session_id", ref.SessionID)
	for _, part := range []*pyjson.Object{transcript, display} {
		for _, key := range part.Keys {
			result.Set(key, part.Vals[key])
		}
	}
	var connectionValue any
	if connection != nil {
		connectionValue = connection
	}
	for _, kv := range [][2]any{{"running", snapshot.Handle.ExecutionState == "running"},
		{"authority_epoch", snapshot.Handle.AuthorityEpoch}, {"replay_epoch", snapshot.ReplayEpoch},
		{"last_sequence", snapshot.LastSequence}, {"subscription_id", snapshot.SubscriptionID},
		{"revision", snapshot.Handle.Revision}, {"execution_generation", snapshot.Handle.ExecutionGeneration},
		{"pending", pending}, {"prompts", prompts}, {"info", info}, {"pending_connection", connectionValue},
		{"open_requests", open}} {
		result.Set(kv[0].(string), kv[1])
	}
	return result, nil
}

// Prompt submit verb, a typed voice stop or its retry answered without admission, else the queued admission
func (c *AuthorityConnection) submit(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	stopped, err := c.voiceStopReplay(ctx, ref, params)
	if err != nil {
		return nil, err
	}
	if !stopped {
		if stopped, err = c.typedVoiceStop(ctx, ref, params); err != nil {
			return nil, err
		}
	}
	if stopped {
		c.recordVoiceStop(ref, params)
		return pyjson.ObjectOf("voice_stopped", true), nil
	}
	request, err := c.submission(ctx, ref, params)
	if err != nil {
		return nil, err
	}
	receipt, err := c.authority.Submit(ctx, c.actor, request, SubmitOptions{QueueWhileRestarting: c.operator})
	if err != nil {
		return nil, err
	}
	return admissionReceiptDict(receipt), nil
}

// Session mutate verb, guard refusals answered as busy and fence conflicts carrying the fresh fences
func (c *AuthorityConnection) mutate(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	a := c.authority
	result, err := mutateSession(ctx, a, c.actor, ref, params)
	if err != nil {
		if state.IsMutationGuardRefusal(err) {
			busy := state.BusyRefusal(err)
			busy.Cause = err
			return nil, busy
		}
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) && (refusal.Reason == "revision_conflict" || refusal.Reason == "stale_generation") {
			row, readErr := a.DB.GetSession(ctx, ref.SessionID)
			if readErr != nil {
				return nil, readErr
			}
			if row != nil {
				revision, generation := rowInt(row, "runtime_revision"), rowInt(row, "runtime_generation")
				if revision != nil && generation != nil {
					return nil, &fencedStoreError{refusal: refusal, revision: *revision, generation: *generation}
				}
			}
		}
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	if child, ok := result.GetOr("branched_session_id", nil).(string); ok && child != "" {
		c.mu.Lock()
		_, attached := c.subscriptions[child]
		c.mu.Unlock()
		if !attached {
			if _, err := c.resume(ctx, SessionRef{ProfileID: ref.ProfileID, SessionID: child}, pyjson.NewObject()); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
