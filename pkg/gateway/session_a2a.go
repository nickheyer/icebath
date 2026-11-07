package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// Meta key prefix of an adopted pre authority local row, hermes_state_local_migration LEGACY_PREFIX
const localLegacyPrefix = "gateway.local_legacy.v1:"

// Owner creation label a LOCAL peer refresh keeps, the fallback for every other transport
func a2aStorageSource(ctx context.Context, db SessionStoreDB, source *SessionSource, sessionID, fallback string) (string, error) {
	if source.Platform != PlatformLocal {
		return fallback, nil
	}
	if !strings.HasPrefix(source.ChatID, "local-") {
		_, adopted, err := db.GetMeta(ctx, localLegacyPrefix+source.ChatID)
		if err != nil {
			return "", err
		}
		if !adopted {
			return fallback, nil
		}
	}
	receipt, err := db.LocalReceipt(ctx, source.ChatID)
	if err != nil {
		return "", err
	}
	lineage, err := receiptLineage(receipt)
	if err != nil {
		return "", err
	}
	if !lineage[sessionID] {
		return "", state.NewRuntimeStoreError("admission_conflict", nil, nil)
	}
	policy, ok := receipt.Get("policy")
	if !ok {
		return "", errors.New("KeyError: 'policy'")
	}
	policyObj, ok := policy.(*pyjson.Object)
	if !ok {
		return "", fmt.Errorf("TypeError: receipt policy is %T", policy)
	}
	labelValue, ok := policyObj.Get("source")
	if !ok {
		return "", errors.New("KeyError: 'source'")
	}
	label, _ := labelValue.(string)
	if legacyID, ok := receipt.Get("legacy_session_id"); ok {
		id, _ := legacyID.(string)
		root, err := db.GetSession(ctx, id)
		if err != nil {
			return "", err
		}
		if root != nil {
			if stored, _ := root.Get("source"); stored != nil {
				if text, isText := stored.(string); isText && text != "" {
					return text, nil
				}
			}
		}
		return label, nil
	}
	if storedAsLocal[label] {
		return fallback, nil
	}
	return label, nil
}

// Session ids a local receipt names, its lineage or the session id alone, the session id always required
func receiptLineage(receipt *pyjson.Object) (map[string]bool, error) {
	ids := map[string]bool{}
	sid, ok := receipt.Get("session_id")
	if !ok {
		return nil, errors.New("KeyError: 'session_id'")
	}
	if lineage, ok := receipt.Get("lineage"); ok {
		items, isList := lineage.([]any)
		if !isList {
			return nil, fmt.Errorf("TypeError: receipt lineage is %T", lineage)
		}
		for _, item := range items {
			if id, isText := item.(string); isText {
				ids[id] = true
			}
		}
		return ids, nil
	}
	if id, isText := sid.(string); isText {
		ids[id] = true
	}
	return ids, nil
}

// One forwarded A2A turn the owner admits under the exact peer conversation identity
type A2AForward struct {
	Agent     string
	Tenant    string
	Peer      string
	ContextID string
	InputID   string
	Text      string
}

// Owner ensure and ticket mint a forward dials through, package vars so tests point them at a fixture owner
var (
	forwardEnsureRuntime = func(ctx context.Context, home string) (ObservedEndpoint, error) {
		return ensureGatewayRuntime(ctx, home, false)
	}
	forwardSessionTicket = func(home string, endpoint *GatewayEndpoint) (string, error) {
		return sessionTicket(home, endpoint, "", "", "", false)
	}
)

// Runs a blocking call off the caller so the forward deadline still ends the wait
func forwardOffThread[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type answer struct {
		value T
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		value, err := fn()
		done <- answer{value, err}
	}()
	select {
	case got := <-done:
		return got.value, got.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Forwards one turn to the home's owner and waits for its terminal receipt, a timeout never cancels accepted work
func ForwardToOwner(ctx context.Context, home string, forward A2AForward, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolved, err := forwardOffThread(ctx, func() (string, error) { return pypath.Resolve(home) })
	if err != nil {
		return nil, err
	}
	ensure, mint := forwardEnsureRuntime, forwardSessionTicket
	ready, err := forwardOffThread(ctx, func() (ObservedEndpoint, error) { return ensure(ctx, resolved) })
	if err != nil {
		return nil, err
	}
	if ready.State != "ready" || ready.Endpoint == nil {
		reason := ready.ReasonCode
		if reason == "" {
			reason = "not_ready"
		}
		return nil, &GatewayClientError{Msg: "gateway_" + ready.State + ":" + reason}
	}
	ticket, err := forwardOffThread(ctx, func() (string, error) { return mint(resolved, ready.Endpoint) })
	if err != nil {
		return nil, err
	}
	target, protocols := GatewayWSTarget(ready.Endpoint, ticket)
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 10 * time.Second, Subprotocols: protocols}
	conn, _, err := dialer.DialContext(ctx, target, http.Header{})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	conn.SetReadLimit(8 * 1024 * 1024)
	sock := &wsSocket{conn: conn}
	if sock.Subprotocol() != CanonicalGatewayProtocol {
		sock.Close()
		return nil, &GatewayClientError{Msg: "gateway_protocol_mismatch"}
	}
	client := NewGatewayClient(sock).Start()
	defer client.Close()
	params := map[string]any{"agent": forward.Agent, "tenant": forward.Tenant, "peer": forward.Peer,
		"context_id": forward.ContextID, "input_id": forward.InputID, "text": forward.Text}
	backoff := 250 * time.Millisecond
	for {
		receipt, err := client.RPC(ctx, "a2a.forward", params)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		status, ok := receipt["status"]
		if !ok {
			return nil, errors.New("KeyError: 'status'")
		}
		if status == "unknown" {
			return nil, &GatewayClientError{Msg: "unknown_execution"}
		}
		if status == "terminal" {
			return receipt, nil
		}
		admissionID, ok := receipt["admission_id"]
		if !ok {
			return nil, errors.New("KeyError: 'admission_id'")
		}
		tick, stop := context.WithTimeout(ctx, backoff)
		err = a2aCompletion(tick, client, admissionID)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			backoff = min(backoff*2, 5*time.Second)
		}
	}
}

// Consumes projections until the admission completion frame, a transport error returned
func a2aCompletion(ctx context.Context, client *GatewayClient, admissionID any) error {
	for {
		event, err := client.Events.Get(ctx)
		if err != nil {
			return err
		}
		if failure, isErr := event.(error); isErr {
			return failure
		}
		frame, _ := pyjson.Plain(event).(map[string]any)
		params, _ := frame["params"].(map[string]any)
		payload, _ := params["payload"].(map[string]any)
		if params["type"] == "message.complete" && payload != nil && pyjson.Equal(payload["admission_id"], admissionID) {
			return nil
		}
	}
}

// Context key of the authenticated peer identity the forwarding handler mints a policy under
type forwardIdentityKey struct{}

// Binds the four part peer identity a forwarded create mints its a2a policy under
func withForwardIdentity(ctx context.Context, identity []string) context.Context {
	return context.WithValue(ctx, forwardIdentityKey{}, identity)
}

// Forwarded turn policy, a cli launch relabelled a2a with the peer identity, minted only by the forwarding handler
func buildForwardPolicy(ctx context.Context, params, cfg map[string]any, private *RecoveredSecrets) (*LocalSessionPolicy, error) {
	identity, _ := ctx.Value(forwardIdentityKey{}).([]string)
	_, hasSource := params["source"]
	_, hasRequest := params["request_id"]
	if identity == nil || len(params) != 2 || !hasSource || !hasRequest {
		return nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
	}
	launch := map[string]any{"source": "cli", "request_id": params["request_id"]}
	policy, err := buildPolicy(ctx, launch, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		return nil, err
	}
	request := policyObject(policy.RequestJSON)
	peer := make([]any, len(identity))
	for i, part := range identity {
		peer[i] = part
	}
	request["source"], request["a2a_identity"] = "a2a", peer
	text, err := pyjson.Dumps(request, pyjson.SortKeys(true))
	if err != nil {
		return nil, err
	}
	forwarded := *policy
	forwarded.Source, forwarded.Platform, forwarded.RequestJSON = "a2a", "a2a", text
	return &forwarded, nil
}

// Whether a policy is one only the forwarding handler mints, an a2a source carrying a four part peer identity
func isForwardPolicy(policy *LocalSessionPolicy) bool {
	if policy == nil || policy.Source != "a2a" || policy.Platform != "a2a" {
		return false
	}
	parsed, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return false
	}
	request, ok := pyjson.Plain(parsed).(map[string]any)
	if !ok {
		return false
	}
	identity, ok := request["a2a_identity"]
	if !ok {
		return false
	}
	source, ok := request["source"]
	if !ok || source != any("a2a") {
		return false
	}
	parts, ok := identity.([]any)
	if !ok || len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if _, isStr := part.(string); !isStr {
			return false
		}
	}
	return true
}

// Authenticated peer that wrote a forwarded turn, nil for any other policy
func forwardAuthor(policy *LocalSessionPolicy) map[string]any {
	if !isForwardPolicy(policy) {
		return nil
	}
	request := policyObject(policy.RequestJSON)
	peer := request["a2a_identity"].([]any)[2]
	return map[string]any{"id": peer, "name": peer, "is_bot": true}
}
