package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// One gateway request answered as a plain map, the surface lock released while it waits
type gatewayRPC func(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error)

// Payload derived from the snapshot the CAS tuple is read from, nil meaning nothing to do
type mutationDerive func(snapshot map[string]any) (map[string]any, error)

// Retained identity of one prepared control, the payload JSON empty for a derived payload
type mutationKey struct {
	session, operation, payload string
	derived                     bool
}

// Request parameters of a retained mutation and its answer once one arrived
type mutationEntry struct {
	params    map[string]any
	result    map[string]any
	hasResult bool
}

// Prepared native controls retaining their identity across ambiguous replies
type PreparedMutations struct {
	pending map[mutationKey]*mutationEntry
}

// Empty retained mutation table
func NewPreparedMutations() *PreparedMutations {
	return &PreparedMutations{pending: map[mutationKey]*mutationEntry{}}
}

// Refusals that end a retained mutation, every other failure keeping its tuple for a retry
var mutationTerminalRefusals = map[string]bool{"invalid_params": true, "revision_conflict": true, "stale_generation": true,
	"session_busy": true, "unknown_execution": true, "permission_denied": true,
	"model_resolution_failed": true, "nothing_to_compress": true}

// Operations that carry no generation fence
var mutationMetadata = map[string]bool{"rename": true}

// Identity of a mutation, a derived payload keyed by operation alone
func mutationKeyOf(sessionID, operation string, payload any) (mutationKey, error) {
	if _, derived := payload.(mutationDerive); derived {
		return mutationKey{session: sessionID, operation: operation, derived: true}, nil
	}
	text, err := jsonutil.Encode(payload, jsonutil.SortKeys(), jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return mutationKey{}, err
	}
	return mutationKey{session: sessionID, operation: operation, payload: text}, nil
}

// Owner answer for this prepared mutation, re-sent with the confirmation token once a guarded target is confirmed
func (m *PreparedMutations) Apply(ctx context.Context, rpc gatewayRPC, sessionID, operation string, payload any,
	confirm func(ctx context.Context, refusal map[string]any) (bool, error)) (map[string]any, error) {
	key, err := mutationKeyOf(sessionID, operation, payload)
	if err != nil {
		return nil, err
	}
	m.Retire(sessionID, &key)
	if _, ok := m.pending[key]; !ok {
		snapshot, err := rpc(ctx, "session.resume", map[string]any{"session_id": sessionID}, 0)
		if err != nil {
			return nil, err
		}
		var body any
		if derive, ok := payload.(mutationDerive); ok {
			derived, err := derive(snapshot)
			if err != nil {
				return nil, err
			}
			if derived == nil {
				return map[string]any{"status": "nothing", "session_id": sessionID, "operation": operation}, nil
			}
			body = derived
		} else {
			if body, err = jsonutil.Decode(key.payload); err != nil {
				return nil, err
			}
		}
		params := map[string]any{"session_id": sessionID, "operation": operation, "payload": body,
			"request_id": uuidHex(), "expected_revision": requireKey(snapshot, "revision")}
		if !mutationMetadata[operation] {
			params["expected_generation"] = requireKey(snapshot, "execution_generation")
		}
		m.pending[key] = &mutationEntry{params: params}
	}
	entry := m.pending[key]
	for !entry.hasResult {
		budget := time.Duration(0)
		if operation == "compress" {
			budget = gateway.CompressRPCTimeout
		}
		result, err := rpc(ctx, "session.mutate", entry.params, budget)
		if err != nil {
			var refused *gateway.GatewayClientError
			if errors.As(err, &refused) && mutationTerminalRefusals[refused.Msg] {
				delete(m.pending, key)
			}
			return nil, err
		}
		entry.result, entry.hasResult = result, true
		refusal := result
		if !anyutil.Equal(refusal["status"], "confirmation_required") {
			break
		}
		entry.result, entry.hasResult = nil, false
		body := plainMap(entry.params["payload"])
		_, confirmed := body["confirm"]
		if confirm == nil || confirmed {
			delete(m.pending, key)
			return nil, &gateway.GatewayClientError{Msg: modelRefusalText(refusal, confirm != nil)}
		}
		accepted, err := confirm(ctx, refusal)
		if err != nil {
			delete(m.pending, key)
			return nil, err
		}
		if !accepted {
			delete(m.pending, key)
			return map[string]any{"status": "cancelled", "session_id": sessionID, "operation": operation}, nil
		}
		next := make(map[string]any, len(entry.params))
		for k, v := range entry.params {
			next[k] = v
		}
		withToken := make(map[string]any, len(body)+1)
		for k, v := range body {
			withToken[k] = v
		}
		withToken["confirm"] = requireKey(refusal, "confirm")
		next["request_id"], next["payload"] = uuidHex(), withToken
		entry.params = next
	}
	return entry.result, nil
}

// Drops the retained identity of this exact mutation
func (m *PreparedMutations) Acknowledge(sessionID, operation string, payload any) {
	if key, err := mutationKeyOf(sessionID, operation, payload); err == nil {
		delete(m.pending, key)
	}
}

// Retires every retained identity of the session except the kept one, so only the very next control replays
func (m *PreparedMutations) Retire(sessionID string, keep *mutationKey) {
	for key := range m.pending {
		if key.session == sessionID && (keep == nil || key != *keep) {
			delete(m.pending, key)
		}
	}
}

// Number of retained identities
func (m *PreparedMutations) Len() int { return len(m.pending) }

// Why a guarded model target was not applied, the owner guard text first
func modelRefusalText(refusal map[string]any, confirmed bool) string {
	reason := "this target needs a confirmation; run /model in an interactive session to confirm it."
	if confirmed {
		reason = "the session changed before the confirmation landed; run /model again."
	}
	return fmt.Sprintf("%s\n\nModel not switched: %s", anyutil.String(requireKey(refusal, "confirm_message")), reason)
}

// One prompt title for the owner guard warnings, the single warning title or the generic one
func confirmationTitle(refusal map[string]any) string {
	warnings, _ := refusal["warnings"].([]any)
	if len(warnings) == 1 {
		first, _ := warnings[0].(map[string]any)
		return anyutil.String(requireKey(first, "title"))
	}
	return "Model Selection Warning"
}

// Structured compress payload from the raw /compress arguments
func compressPayload(arg string) (*jsonutil.Object, error) {
	request := agent.ParseCompressArgs(arg)
	payload := jsonutil.NewObject()
	if request.Aggressive {
		payload.Set("aggressive", true)
	}
	if request.FocusTopic != nil && *request.FocusTopic != "" {
		payload.Set("focus", *request.FocusTopic)
	}
	if request.Preview {
		payload.Set("preview", true)
	}
	if request.Partial {
		payload.Set("partial", true)
		payload.Set("keep_last", request.KeepLast)
	}
	return payload, nil
}

// Operation and payload of a /model or /compress line, no operation for a /model that only asks for the current model
func slashMutation(command, arg string) (string, *jsonutil.Object, error) {
	name := strings.TrimLeft(command, "/")
	switch name {
	case "model":
		request := provider.ParseModelSwitchArgs(arg)
		if len(request.Errors) > 0 {
			return "", nil, &gateway.GatewayClientError{Msg: request.ErrorMessages()[0], ExitStatus: 2}
		}
		payload := jsonutil.NewObject()
		for _, field := range [][2]string{{"model", request.Target}, {"provider", request.ExplicitProvider}, {"reasoning", request.ReasoningEffort}} {
			if field[1] != "" {
				payload.Set(field[0], field[1])
			}
		}
		for _, flag := range []struct {
			name string
			set  bool
		}{{"refresh", request.ForceRefresh}, {"once", request.IsOnce}, {"persist", request.IsGlobal}} {
			if flag.set {
				payload.Set(flag.name, true)
			}
		}
		_, named := payload.Get("model")
		_, picked := payload.Get("provider")
		if !named && !picked && !request.ForceRefresh {
			return "", nil, nil
		}
		return name, payload, nil
	}
	payload, err := compressPayload(arg)
	return "compress", payload, err
}

// Fresh random request id as 32 lowercase hex digits
func uuidHex() string { return strings.ReplaceAll(uuid.New().String(), "-", "") }

// Value of a required key, panicking with the KeyError a Python subscript raises when absent
func requireKey(m map[string]any, key string) any {
	v, ok := m[key]
	if !ok {
		panic(fmt.Sprintf("the gateway mutation reply has no %s", key))
	}
	return v
}
