package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner seams producer ingress admits through, authz_mixin _owning_profile and run_adapters _canonicalize and _admit_primary_source
type ProducerScopeRunner interface {
	AuthorityRunner
	SessionAuthority() *SessionAuthority
	OwningProfile(adapter PlatformAdapter, platform Platform) (registered bool, profile string)
	Canonicalize(ctx context.Context, source *SessionSource, transportProfile, primaryHome string) *RoutingIdentity
	ResolveProfileHomeForSource(ctx context.Context, source *SessionSource) (string, error)
	AdmitPrimarySource(ctx context.Context, source *SessionSource, defaultHome string) (string, bool)
}

// Runner running the producer admission plugin hooks, run_inbound_hooks _hm_pre_gateway_dispatch_hook and run_inbound_consumer run_post_admission_hook
type ProducerHooksRunner interface {
	HMPreGatewayDispatchHook(ctx context.Context, event *MessageEvent, source *SessionSource) *MessageEvent
	RunPostAdmissionHook(ctx context.Context, event *MessageEvent, source *SessionSource, sessionKey string) (bool, *string, error)
}

// Plugin hook that may consume a delivery after its admission is prepared
const PostAdmissionHook = "post_gateway_admission"

// Runner bound to the adapter's message handler, the bound method owner of hermes
func handlerOwner(adapter PlatformAdapter) any {
	b := adapter.Base()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messageHandlerOwner
}

// Consumer reply a post admission plugin handed back for the caller's own reply leg
func (e *MessageEvent) ConsumerReply() *string { return e.consumerReply }

// Whether admission resolved the delivery to an already committed provider receipt
func (e *MessageEvent) WebhookDuplicate() bool { return e.webhookDuplicate }

// Authority that admits a producer delivery and the context carrying its callback and runtime scope
func ProducerScope(ctx context.Context, adapter PlatformAdapter, event *MessageEvent) (context.Context, *SessionAuthority, error) {
	owner := handlerOwner(adapter)
	holder, ok := owner.(launchAuthorityRunner)
	if !ok || holder == nil || holder.SessionAuthority() == nil {
		return nil, nil, runtimeRefusal("not_found")
	}
	runner, ok := owner.(ProducerScopeRunner)
	if !ok {
		return nil, nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_owning_profile'", owner)}
	}
	registered, profile := runner.OwningProfile(adapter, adapter.Base().Platform)
	var profileKey *string
	if profile != "" {
		profileKey = &profile
	}
	home := transportHomeOf(runner, profileKey)
	if !registered || home == nil {
		return nil, nil, runtimeRefusal("profile_mismatch")
	}
	source := event.Source
	var runtimeHome string
	if profile != "" {
		runner.Canonicalize(ctx, source, profile, "")
		runtimeHome = *home
		if source.Profile != nil && *source.Profile != "" {
			resolved, err := runner.ResolveProfileHomeForSource(ctx, source)
			if err != nil {
				return nil, nil, err
			}
			runtimeHome = resolved
		}
	} else {
		admitted, ok := runner.AdmitPrimarySource(ctx, source, *home)
		if !ok {
			rejected := ""
			if source.rejectedRouteProfile != nil {
				rejected = *source.rejectedRouteProfile
			}
			if err := refuseParked(runner, rejected); err != nil {
				return nil, nil, err
			}
			source.ProfileRouteRejected = true
			admitted = *home
		}
		runtimeHome = admitted
	}
	source.authorizationProfileHome = *home
	authority, err := authorityForHome(runner, runtimeHome)
	if err != nil {
		return nil, nil, err
	}
	if authority == nil || authority.Retiring {
		routed := profile
		if source.Profile != nil && *source.Profile != "" {
			routed = *source.Profile
		}
		if err := refuseParked(runner, routed); err != nil {
			return nil, nil, err
		}
	}
	if authority == nil {
		return nil, nil, runtimeRefusal("profile_mismatch")
	}
	scoped, err := NativeCallback(ctx, runner, event, home, profileKey)
	if err != nil {
		return nil, nil, err
	}
	return ProfileRuntimeScope(scoped, runtimeHome, true), authority, nil
}

// Retryable parked profile refusal for a delivery routed to a parked profile, nil otherwise
func refuseParked(runner any, profile string) error {
	if profileParked(runner, profile) {
		return NewParkedProfileRefusal(profile)
	}
	return nil
}

// The 503 for a webhook whose profile is not served right now, nil when the delivery may proceed
func ParkedProfileRefusalResponse(ctx context.Context, runner any, profile string, routeConfig map[string]any,
	admissionSource func() (*SessionSource, error)) (*WebResponse, error) {
	var message string
	notServed, err := prefixNotServed(runner, profile)
	if err != nil {
		return nil, err
	}
	if notServed {
		message = fmt.Sprintf("Profile %s is not being served; retry this delivery", profile)
	} else {
		refusal, err := coalescedRouteRefusal(runner, profile, routeConfig, admissionSource)
		if err != nil {
			return nil, err
		}
		if refusal == "" {
			return nil, nil
		}
		if refusal == "refused" {
			return WebJSONResponse(map[string]any{"error": "Admission unavailable; retry this delivery"}, http.StatusServiceUnavailable, nil)
		}
		message = "The routed profile is not being served right now; retry this delivery"
	}
	return WebJSONResponse(map[string]any{"error": message}, ParkedRouteRefusedStatus, ParkedRouteRefusedHeaders)
}

// Parked, or on disk with no serving authority here, for a /p/profile prefix
func prefixNotServed(runner any, profile string) (bool, error) {
	if profileParked(runner, profile) {
		return true, nil
	}
	if profile == "" || registryOf(runner) == nil {
		return false, nil
	}
	dir, err := profiles.GetProfileDir(profile)
	if err != nil {
		return false, err
	}
	authority, err := authorityForHome(runner, dir)
	if err != nil {
		return false, err
	}
	return authority == nil || authority.Retiring, nil
}

// How admission would refuse a coalesced bare route delivery profile_routes sends away, parked, refused or empty
func coalescedRouteRefusal(runner any, profile string, routeConfig map[string]any, admissionSource func() (*SessionSource, error)) (string, error) {
	if profile != "" {
		return "", nil
	}
	switch routeConfig["coalesce"].(type) {
	case map[string]any, *pyjson.Object:
	default:
		return "", nil
	}
	source, err := admissionSource()
	if err != nil {
		return "", err
	}
	if source.ProfileRouteRejected {
		if rejected := source.rejectedRouteProfile; rejected != nil && *rejected != "" && profileParked(runner, *rejected) {
			return "parked", nil
		}
		return "refused", nil
	}
	if source.Profile == nil || *source.Profile == "" {
		return "", nil
	}
	routed := *source.Profile
	if profileParked(runner, routed) {
		return "parked", nil
	}
	notServed, err := prefixNotServed(runner, routed)
	if err != nil || !notServed {
		return "", err
	}
	return "refused", nil
}

// Commits a producer delivery, nil when a plugin skipped or consumed it
func AdmitProducer(ctx context.Context, adapter PlatformAdapter, event *MessageEvent) (*AdmissionReceipt, error) {
	scoped, authority, err := ProducerScope(ctx, adapter, event)
	if err != nil {
		return nil, err
	}
	webhook := adapter.Base().Platform == PlatformWebhook
	var hooks []string
	for _, name := range []string{"pre_gateway_dispatch", PostAdmissionHook} {
		if plugins.HasHook(scoped, name) {
			hooks = append(hooks, name)
		}
	}
	if !webhook && len(hooks) == 0 {
		return admitProducerNative(scoped, authority, event)
	}
	payload, principal, err := preparedDelivery(scoped, authority, event)
	if err != nil {
		return nil, err
	}
	writeCtx, writeStarted := withNativeWriteFlag(scoped)
	var receipt *AdmissionReceipt
	requestID := ""
	if event.MessageID != nil {
		requestID = *event.MessageID
	}
	err = deliveryTurn(writeCtx, authority.ProfileID, principal, requestID, func(turnCtx context.Context) error {
		rows, err := committedRows(turnCtx, authority, principal, requestID)
		if err != nil {
			return err
		}
		receipt, err = admitOnce(turnCtx, authority, event, webhook, hooks, payload, rows)
		return err
	})
	if err != nil {
		if !(errors.Is(err, context.Canceled) && writeStarted()) {
			if releaseErr := releasePreparation(context.WithoutCancel(scoped), authority, payload); releaseErr != nil {
				return nil, releaseErr
			}
		}
		return nil, err
	}
	return receipt, nil
}

// Runs the retry check, the plugin hooks and the commit of one delivery
func admitOnce(ctx context.Context, authority *SessionAuthority, event *MessageEvent, webhook bool, hooks []string,
	payload *pyjson.Object, rows []*pyjson.Object) (*AdmissionReceipt, error) {
	if webhook {
		prior, err := webhookRetry(ctx, authority, event, payload, rows)
		if err != nil || prior != nil {
			return prior, err
		}
	}
	admitted := event
	if len(hooks) > 0 && (webhook || len(rows) == 0) {
		runner, ok := authority.Runner.(ProducerHooksRunner)
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_hm_pre_gateway_dispatch_hook'", authority.Runner)}
		}
		if slices.Contains(hooks, "pre_gateway_dispatch") {
			rewritten := runner.HMPreGatewayDispatchHook(ctx, event, event.Source)
			if rewritten == nil {
				return nil, releasePreparation(ctx, authority, payload)
			}
			if rewritten != event {
				fingerprint, err := ingressFingerprint(payload, event.RawMessage)
				if err != nil {
					return nil, err
				}
				rewritten.ingressSHA256 = fingerprint
			}
			admitted = rewritten
		}
		if slices.Contains(hooks, PostAdmissionHook) {
			key, err := runnerSessionKeyForSource(ctx, authority.Runner, admitted.Source)
			if err != nil {
				return nil, err
			}
			consumed, reply, err := runner.RunPostAdmissionHook(ctx, admitted, admitted.Source, key)
			if err != nil {
				return nil, err
			}
			event.consumerReply = reply
			if consumed {
				return nil, releasePreparation(ctx, authority, payload)
			}
		}
	}
	return admitProducerNative(ctx, authority, admitted)
}

// Admits a producer delivery whose adapter lifecycle the executor runs once the row starts
func admitProducerNative(ctx context.Context, authority *SessionAuthority, event *MessageEvent) (*AdmissionReceipt, error) {
	event.producerDelivery = true
	receipt, err := authority.AdmitNative(ctx, event)
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

var (
	deliveriesMu sync.Mutex
	deliveries   = map[string]*keyedTurnEntry{}
)

// Runs body as the one turn of a provider delivery id, unlocked when the delivery carries no id
func deliveryTurn(ctx context.Context, profileID, principal, requestID string, body func(ctx context.Context) error) error {
	if requestID == "" {
		return body(ctx)
	}
	key, err := pyjson.Dumps([]any{profileID, principal, requestID})
	if err != nil {
		return err
	}
	return keyedTurn(ctx, &deliveriesMu, deliveries, key, nil, body)
}

// Releases the media a skipped, consumed, refused or cancelled delivery copied into the retained store
func releasePreparation(ctx context.Context, authority *SessionAuthority, payload *pyjson.Object) error {
	native, err := nativeEnvelope(payload)
	if err != nil {
		return err
	}
	media, _ := native.Get("media")
	references, _ := media.([]any)
	if len(references) == 0 {
		return nil
	}
	_, err = releaseUnheldMedia(ctx, authority.DB, references, false)
	return err
}

// Validated payload of the delivery and its messaging principal, a refused sender raising here
func preparedDelivery(ctx context.Context, authority *SessionAuthority, event *MessageEvent) (*pyjson.Object, string, error) {
	leaseCtx, scope, release := captureLease(ctx, authority)
	payload, err := prepareNative(leaseCtx, authority.Runner, event)
	if err != nil {
		if abandonErr := abandonCapture(leaseCtx, scope); abandonErr != nil {
			release()
			return nil, "", abandonErr
		}
		release()
		return nil, "", err
	}
	release()
	source, err := restoreNativeSource(ctx, payload, nil)
	if err != nil {
		return nil, "", err
	}
	opt := func(v *string) any {
		if v == nil {
			return nil
		}
		return *v
	}
	identity, err := pyjson.Dumps([]any{opt(source.Profile), string(source.Platform), source.ChatID, opt(source.ThreadID),
		opt(source.UserID)}, pyjson.Separators(",", ":"))
	if err != nil {
		return nil, "", err
	}
	return payload, "messaging:" + identity, nil
}

// Admission rows of this delivery's identity in the FIFO
func committedRows(ctx context.Context, authority *SessionAuthority, principal, requestID string) ([]*pyjson.Object, error) {
	return authority.DB.ReadAll(ctx, "SELECT admission_id FROM session_admissions WHERE principal_id=? AND request_id=?",
		principal, requestID)
}

// Receipt of an already committed identical webhook delivery, nil when none is committed
func webhookRetry(ctx context.Context, authority *SessionAuthority, event *MessageEvent, payload *pyjson.Object, rows []*pyjson.Object) (*AdmissionReceipt, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	row, err := authority.DB.GetSessionAdmission(ctx, rowString(rows[0], "admission_id"))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
	}
	stored, _ := row.Get("payload")
	storedPayload, _ := stored.(*pyjson.Object)
	if storedPayload == nil {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(stored) + "' object is not subscriptable"}
	}
	retained, err := nativeEnvelope(storedPayload)
	if err != nil {
		return nil, err
	}
	var same bool
	if prior, ok := retained.Get("ingress_sha256"); ok {
		fingerprint, err := ingressFingerprint(payload, event.RawMessage)
		if err != nil {
			return nil, err
		}
		same = pyjson.Equal(prior, fingerprint)
	} else {
		_, hasDelivery := retained.Get("webhook_delivery")
		_, hasRoute := retained.Get("webhook_route")
		if !hasDelivery && !hasRoute {
			native, err := nativeEnvelope(payload)
			if err != nil {
				return nil, err
			}
			for _, key := range []string{"webhook_delivery", "webhook_route"} {
				if _, ok := native.Get(key); !ok {
					return nil, &pyKeyError{key}
				}
				native.Delete(key)
			}
			if _, ok := retained.Get("provenance"); !ok {
				native.Delete("provenance")
			}
		}
		same = pyjson.Equal(stored, payload)
	}
	if len(rows) != 1 || !same {
		return nil, runtimeRefusal("admission_conflict")
	}
	event.webhookDuplicate = true
	receipt := authority.receiptOf(row)
	if _, err := FinalizeWebhook(ctx, authority, receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// Ends only the settled admitted generation of a webhook session, atomically with the epoch fence
func FinalizeWebhook(ctx context.Context, authority *SessionAuthority, receipt AdmissionReceipt) (bool, error) {
	if receipt.Status != "terminal" || receipt.ExecutionGeneration == nil {
		return false, nil
	}
	sid := receipt.Ref.SessionID
	generation := *receipt.ExecutionGeneration
	var changed int64
	err := authority.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		n, err := authority.DB.EndAndBump(ctx, conn, `
            UPDATE sessions SET ended_at=?, end_reason=?
            WHERE id=? AND ended_at IS NULL AND runtime_generation=?
              AND EXISTS (SELECT 1 FROM runtime_epoch WHERE singleton=1 AND epoch=?)
              AND EXISTS (SELECT 1 FROM session_admissions WHERE admission_id=?
                AND target_session_id=? AND status='terminal' AND generation=? AND owner_epoch=?)
              AND NOT EXISTS (SELECT 1 FROM session_admissions WHERE target_session_id=? AND status!='terminal')
            `, []any{float64(time.Now().UnixNano()) / 1e9, "webhook_complete", sid, generation, authority.Epoch,
			receipt.AdmissionID, sid, generation, receipt.AuthorityEpoch, sid}, sid, "webhook_complete")
		changed = n
		return err
	})
	return changed != 0, err
}

// Closes settled original webhook sessions whose current route authorization still holds, per session outcome
func RecoverWebhookFinalizations(ctx context.Context, authority *SessionAuthority) (map[string]string, error) {
	rows, err := authority.DB.ReadAll(ctx, `
        SELECT a.admission_id FROM sessions AS s
        JOIN session_admissions AS a ON a.target_session_id=s.id
        WHERE s.source='webhook' AND s.ended_at IS NULL
          AND a.status='terminal' AND a.generation IS NOT NULL
          AND json_extract(a.payload_json, '$.native_text_v1.source.platform')='webhook'
          AND json_extract(a.payload_json, '$.native_text_v1.automation') IS NULL
        `)
	if err != nil {
		return nil, err
	}
	results := map[string]string{}
	for _, candidate := range rows {
		row, err := authority.DB.GetSessionAdmission(ctx, rowString(candidate, "admission_id"))
		if err != nil {
			return nil, err
		}
		if row == nil {
			continue
		}
		sid := rowString(row, "target_session_id")
		outcome, err := recoverWebhookFinalization(ctx, authority, row, sid)
		var keyErr *pyKeyError
		var refusal *state.RuntimeStoreError
		switch {
		case errors.As(err, &keyErr) || errors.As(err, &refusal):
			results[sid] = "unavailable"
		case err != nil:
			return nil, err
		default:
			results[sid] = outcome
		}
	}
	return results, nil
}

// Finalizes one settled webhook admission after its route authorization passes
func recoverWebhookFinalization(ctx context.Context, authority *SessionAuthority, row *pyjson.Object, sid string) (string, error) {
	stored, ok := row.Get("payload")
	if !ok {
		return "", &pyKeyError{"payload"}
	}
	payload, _ := stored.(*pyjson.Object)
	if payload == nil {
		return "", &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(stored) + "' object is not subscriptable"}
	}
	envelope, err := nativeEnvelope(payload)
	if err != nil {
		return "", err
	}
	route, ok := envelope.Get("route")
	if !ok {
		return "", &pyKeyError{"route"}
	}
	routeKey, _ := route.(string)
	entry := authority.Runner.SessionStore().LookupBySessionKey(ctx, routeKey)
	if entry == nil {
		return "", runtimeRefusal("admission_conflict")
	}
	owner, err := authority.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return "", err
	}
	if owner != sid {
		return "", runtimeRefusal("admission_conflict")
	}
	var adapter PlatformAdapter
	if entry.Origin != nil {
		if adapter, err = authority.Runner.AdapterForSource(ctx, entry.Origin); err != nil {
			return "", err
		}
	}
	target, err := authority.PhysicalTarget(ctx, SessionRef{authority.ProfileID, sid})
	if err != nil {
		return "", err
	}
	if _, _, err := checkNativeRoute(ctx, authority.Runner, payload, target, entry.Origin, adapter); err != nil {
		return "", err
	}
	finalized, err := FinalizeWebhook(ctx, authority, authority.receiptOf(row))
	if err != nil {
		return "", err
	}
	if finalized {
		return "finalized", nil
	}
	return "unchanged", nil
}
