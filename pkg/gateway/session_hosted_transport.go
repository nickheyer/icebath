package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// State meta prefix of a session's durable hosted transport binding
const hostedTransportBinding = "gateway.hosted.transport.v1:"

// Capabilities a hosted owner principal holds on its target
var hostedOwnerCaps = map[string]bool{"session:create": true, "session:read": true, "session:submit": true, "session:control": true, "session:approve": true}

// Refusal reasons an owner exchange forwards unchanged, every other one reading as runtime_draining
var ownerRequestReasons = map[string]bool{"permission_denied": true, "profile_mismatch": true, "invalid_params": true, "unknown_execution": true,
	"stale_generation": true, "admission_conflict": true, "response_too_large": true, "request_too_large": true}

// Authenticated private socket exchange with the owner of home, no owner start or storage fallback
func ownerRequest(ctx context.Context, ownerHome, verb string, params map[string]any, timeout time.Duration) (any, error) {
	resolved, err := pypath.Resolve(ownerHome)
	if err != nil {
		return nil, err
	}
	if pypath.PathStr(ownerHome) != resolved {
		return nil, runtimeRefusal("permission_denied")
	}
	deadline := time.Now().Add(timeout)
	discovered, err := discoverGatewayEndpoint(ctx, ownerHome, timeout)
	if err != nil {
		return nil, err
	}
	if discovered.State != "ready" || discovered.Endpoint == nil {
		return nil, runtimeRefusal("runtime_draining")
	}
	if home.Key(discovered.Endpoint.ProfileID) != home.Key(ownerHome) {
		return nil, runtimeRefusal("profile_mismatch")
	}
	sent := make(map[string]any, len(params)+1)
	for k, v := range params {
		sent[k] = v
	}
	sent["profile_id"] = discovered.Endpoint.ProfileID
	controlHome := ControlHomeFor(ownerHome, discovered.Endpoint)
	if time.Until(deadline) <= 0 {
		return nil, runtimeRefusal("runtime_draining")
	}
	text, err := pyjson.Dumps(pyjson.ObjectOf("protocol", 1, "id", 1, "verb", verb, "params", sent))
	if err != nil {
		return nil, err
	}
	request := []byte(text + "\n")
	if len(request) > 65536 {
		return nil, runtimeRefusal("request_too_large")
	}
	raw, err := ownerControlExchange(controlHome, request, deadline)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, runtimeRefusal("runtime_draining")
	}
	response, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		return nil, err
	}
	if ok, _ := response.Get("ok"); ok != true {
		problem := pyrepr.Str(response.GetOr("error", ""))
		parts := strings.Split(problem, ": ")
		reason := parts[len(parts)-1]
		if !ownerRequestReasons[reason] {
			reason = "runtime_draining"
		}
		return nil, runtimeRefusal(reason)
	}
	if protocol, _ := response.Get("protocol"); !pyjson.Equal(protocol, 1) {
		return nil, runtimeRefusal("runtime_draining")
	}
	if id, _ := response.Get("id"); !pyjson.Equal(id, 1) {
		return nil, runtimeRefusal("runtime_draining")
	}
	return pyItem(response, "result")
}

// Source owner attestation of one operation on a bound target, refusing a result naming no owner or another target
func hostedAttest(ctx context.Context, binding *pyjson.Object, operation string, params map[string]any) (*pyjson.Object, error) {
	sourceHome, err := pyItem(binding, "source_home")
	if err != nil {
		return nil, err
	}
	selector, err := pyItem(binding, "selector")
	if err != nil {
		return nil, err
	}
	targetHome, err := pyItem(binding, "target_home")
	if err != nil {
		return nil, err
	}
	sent := map[string]any{"_target_home": targetHome}
	for k, v := range params {
		sent[k] = v
	}
	result, err := ownerRequest(ctx, pyrepr.Str(sourceHome), "hosted-attest", map[string]any{"selector": selector, "operation": operation, "params": sent}, 30*time.Second)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil, err
		}
		drained := state.NewRuntimeStoreError("runtime_draining", nil, nil)
		drained.Cause = err
		return nil, drained
	}
	attested, ok := pyjson.AsObject(result)
	if !ok {
		return nil, runtimeRefusal("permission_denied")
	}
	if owner, _ := attested.Get("owner"); owner == nil || owner == "" {
		return nil, runtimeRefusal("permission_denied")
	} else if _, isText := owner.(string); !isText {
		return nil, runtimeRefusal("permission_denied")
	}
	if attestedHome, _ := attested.Get("target_home"); !pyjson.Equal(attestedHome, targetHome) {
		return nil, runtimeRefusal("permission_denied")
	}
	return attested, nil
}

// Hosted owner principal namespaced by its source profile so equal owner strings never alias
func hostedPrincipal(a *SessionAuthority, binding *pyjson.Object) (Principal, error) {
	sourceHome, err := pyItem(binding, "source_home")
	if err != nil {
		return Principal{}, err
	}
	owner, err := pyItem(binding, "owner")
	if err != nil {
		return Principal{}, err
	}
	identity, err := pyjson.Dumps([]any{sourceHome, owner}, pyjson.Separators(",", ":"))
	if err != nil {
		return Principal{}, err
	}
	sum := sha256.Sum256([]byte(identity))
	return Principal{Subject: "hosted-owner:" + hex.EncodeToString(sum[:]), ProfileID: a.ProfileID, Capabilities: hostedOwnerCaps,
		TransportID: "hosted-owner-transport"}, nil
}

// Reauthorizes a durable target binding at its source owner before claim, false only for a non transport session
func checkRemoteHostedAdmission(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (bool, error) {
	ctx = ownerScope(ctx, a, false)
	stored, found, err := a.DB.GetMeta(ctx, hostedTransportBinding+ref.SessionID)
	if err != nil || !found {
		return false, err
	}
	attested, err := attestRemoteHostedRow(ctx, a, ref, row, stored)
	if err != nil {
		refusal := state.NewRuntimeStoreError("permission_denied", nil, nil)
		refusal.Cause = err
		return false, refusal
	}
	attachments, err := pyItem(attested, "attachments")
	if err != nil {
		return false, err
	}
	digests, _ := attested.Get("attachment_digests")
	if err := verifyAttestedDocuments(ctx, attachments, digests); err != nil {
		return false, err
	}
	return true, nil
}

// Attested source result of a hosted row whose owner, target and input still match its durable binding
func attestRemoteHostedRow(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, stored string) (*pyjson.Object, error) {
	binding, err := pyjson.LoadsDict(stored)
	if err != nil {
		return nil, err
	}
	requestID := rowString(row, "request_id")
	if len(requestID) < 7 {
		return nil, &PyError{Kind: "ValueError", Msg: "binding mismatch"}
	}
	decoded, err := pyjson.Loads(requestID[7:])
	if err != nil {
		return nil, err
	}
	pair, ok := decoded.([]any)
	if !ok || len(pair) != 2 {
		return nil, &PyError{Kind: "ValueError", Msg: "not enough values to unpack"}
	}
	principal, err := hostedPrincipal(a, binding)
	if err != nil {
		return nil, err
	}
	targetHome, _ := binding.Get("target_home")
	if !strings.HasPrefix(requestID, "hosted:") || !SameOwner(rowValue(row, "principal_id"), principal.Subject) ||
		ref.ProfileID != a.ProfileID || !pyjson.Equal(targetHome, a.ProfileID) {
		return nil, &PyError{Kind: "ValueError", Msg: "binding mismatch"}
	}
	attested, err := hostedAttest(ctx, binding, "execute", map[string]any{"task": pair[0], "execution_generation": pair[1]})
	if err != nil {
		return nil, err
	}
	owner, _ := binding.Get("owner")
	if attestedOwner, err := pyItem(attested, "owner"); err != nil {
		return nil, err
	} else if !pyjson.Equal(attestedOwner, owner) {
		return nil, &PyError{Kind: "ValueError", Msg: "owner changed"}
	}
	prompt, err := pyItem(attested, "prompt")
	if err != nil {
		return nil, err
	}
	attachments, err := pyItem(attested, "attachments")
	if err != nil {
		return nil, err
	}
	digests, _ := attested.Get("attachment_digests")
	text, _ := prompt.(string)
	expected, err := attestedSubmissionPayload(ctx, text, attachments, digests)
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(rowPayload(row), expected) {
		return nil, &PyError{Kind: "ValueError", Msg: "input changed"}
	}
	return attested, nil
}
