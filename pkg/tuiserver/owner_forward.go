package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Ledger states a forwarded prompt may wait behind, each ends on its own
var forwardableLedgerStates = map[string]bool{"started": true, "queued": true, "registered": true, "running": true}

// Owner admission wait, relay ceiling and the idle relay recheck of the desktop session
const (
	ownerAdmitTimeout      = 30 * time.Second
	ownerRelayTimeout      = 6 * time.Hour
	ownerRelayFinalizePoll = time.Second
)

// Owner turn frames relayed for the forwarded admission
var ownerRelayed = map[string]bool{"message.start": true, "message.complete": true}

// Owner prompt.submit refusal reasons raised before its durable write, the only proof nothing was queued
var ownerRefusedBeforeCommit = map[string]bool{
	"invalid_params": true, "runtime_draining": true, "runtime_retired": true, "profile_mismatch": true, "permission_denied": true,
	"not_found": true, "stale_epoch": true, "admission_conflict": true, "queue_full": true}

// Owner could not take the prompt, NotRunning when no gateway serves the home and Unconfirmed when it may be queued
type OwnerUnavailableError struct {
	Msg         string
	NotRunning  bool
	Unconfirmed bool
	Cause       error
}

func (e *OwnerUnavailableError) Error() string { return e.Msg }

func (e *OwnerUnavailableError) Unwrap() error { return e.Cause }

// Ready gateway endpoint serving the home, discovery only and never a start
func discoverOwner(ctx context.Context, profileHome string) (*gateway.GatewayEndpoint, error) {
	discovery := gateway.DiscoverGatewayEndpoint(ctx, profileHome, 5*time.Second)
	if discovery.State != "ready" || discovery.Endpoint == nil {
		return nil, &OwnerUnavailableError{Msg: "the gateway that owns it is " + discovery.State, NotRunning: true}
	}
	return discovery.Endpoint, nil
}

// Started client on the ready endpoint serving the home with the ticket scope and the largest accepted frame
func connectOwner(ctx context.Context, profileHome string, endpoint *gateway.GatewayEndpoint, scope string, maxSize int64) (*gateway.GatewayClient, error) {
	sock, err := gateway.DialOwnerSocket(ctx, profileHome, endpoint, scope, maxSize)
	if err != nil {
		return nil, err
	}
	if sock.Subprotocol() != gateway.CanonicalGatewayProtocol {
		sock.Close()
		return nil, &OwnerUnavailableError{Msg: "the gateway answered with another protocol"}
	}
	return gateway.NewGatewayClient(sock).Start(), nil
}

// Whether a ledger refusal of this prompt.submit can go to the owner instead
func ownerForwardable(refusal *activesessions.Refusal, session *tuigateway.Session, text any, params map[string]any) bool {
	if refusal == nil || !forwardableLedgerStates[refusal.LedgerStatus] {
		return false
	}
	s, isStr := text.(string)
	if !isStr || strings.TrimSpace(s) == "" || truthy(session.Get("attached_images")) {
		return false
	}
	for _, key := range truncationParams {
		if params[key] != nil {
			return false
		}
	}
	return true
}

// Owner payload projected onto this wire's contract for the event, nil to drop it
func ownerPayloadFor(eventType string, payload any) map[string]any {
	contract := tuigateway.Events[eventType]
	fields, isDict := payload.(map[string]any)
	if contract == nil || contract.Payload == nil || !isDict {
		return nil
	}
	shaped := map[string]any{}
	for _, name := range contract.Payload.ModelFields() {
		if value, present := fields[name]; present {
			shaped[name] = value
		}
	}
	if contract.Payload.ModelValidate(shaped) != nil {
		if eventType != "message.complete" {
			return nil
		}
		text, _ := fields["text"].(string)
		shaped = map[string]any{"text": text}
	}
	return shaped
}

// Admission receipt or the failure handed to the waiting submit
type ownerAdmission struct {
	receipt map[string]any
	err     error
}

// One forwarded prompt, its admission future and the relay of its owner frames onto the desktop session
type ownerRelay struct {
	sid, key, home, text string
	session              *tuigateway.Session
	endpoint             *gateway.GatewayEndpoint
	emit                 func(event, sid string, payload map[string]any)
	listed               []map[string]any
	listedSet            bool
	requestID            string
	admitMu              sync.Mutex
	admitDone            bool
	admitted             chan ownerAdmission
}

// Settles the admission once, false when it was already settled
func (r *ownerRelay) settle(receipt map[string]any, err error) bool {
	r.admitMu.Lock()
	defer r.admitMu.Unlock()
	if r.admitDone {
		return false
	}
	r.admitDone = true
	r.admitted <- ownerAdmission{receipt: receipt, err: err}
	return true
}

// Thread body handing a failure to the waiting submit or logging it once the admission settled
func (r *ownerRelay) run(ctx context.Context) {
	var err error
	if failure := catchPanic(func() { err = r.attach(ctx) }); failure != nil {
		if asErr, isErr := failure.(error); isErr {
			err = asErr
		} else {
			err = fmt.Errorf("%s", pyrepr.Str(failure))
		}
	}
	if err != nil && !r.settle(nil, err) {
		logger().Warn(fmt.Sprintf("owner relay for %s ended: %s", r.key, err), "error", err)
	}
}

// Attaches to the owner session, admits the prompt and relays the admission frames
func (r *ownerRelay) attach(ctx context.Context) error {
	client, err := connectOwner(ctx, r.home, r.endpoint, "", 1<<20)
	if err != nil {
		return err
	}
	defer client.Close()
	attached, err := client.RPC(ctx, "session.resume", map[string]any{"session_id": r.key})
	if err != nil {
		return err
	}
	target := r.key
	if sessionID := attached["session_id"]; truthy(sessionID) {
		target = pyrepr.Str(sessionID)
	}
	receipt, err := r.submit(ctx, client, target)
	if err != nil {
		return err
	}
	if receipt["voice_stopped"] == true {
		r.settle(receipt, nil)
		return nil
	}
	admissionID, isStr := receipt["admission_id"].(string)
	status := receipt["status"]
	if !isStr || (status != "queued" && status != "started" && status != "terminal") {
		return &OwnerUnavailableError{Msg: "the gateway did not admit it"}
	}
	r.settle(receipt, nil)
	logger().Info(fmt.Sprintf("forwarded Desktop prompt for %s to the owner FIFO: admission=%s", r.key, admissionID))
	return r.relay(ctx, client, admissionID)
}

// Owner prompt.submit, unconfirmed unless the owner refused before its durable write
func (r *ownerRelay) submit(ctx context.Context, client *gateway.GatewayClient, target string) (map[string]any, error) {
	receipt, err := client.RPC(ctx, "prompt.submit", map[string]any{"session_id": target, "text": r.text, "submission_id": r.requestID})
	if err == nil {
		return receipt, nil
	}
	var rpcErr *gateway.GatewayRPCError
	if errors.As(err, &rpcErr) {
		reason, present := rpcErr.Data["reason"]
		if !present {
			reason = rpcErr.Error()
		}
		if name, isStr := reason.(string); isStr && ownerRefusedBeforeCommit[name] {
			return nil, err
		}
	}
	return nil, &OwnerUnavailableError{Msg: "the gateway did not confirm it", Unconfirmed: true, Cause: err}
}

// Emits the owner pending list for this admission only as this wire's pending_submissions
func (r *ownerRelay) pending(params map[string]any, admissionID string) error {
	payload := params["payload"]
	var rows any
	if truthy(payload) {
		fields, isDict := payload.(map[string]any)
		if !isDict {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(payload))
		}
		rows = fields["pending"]
	}
	list, isList := rows.([]any)
	if !isList {
		return nil
	}
	listed := []map[string]any{}
	for _, item := range list {
		row, isDict := item.(map[string]any)
		if isDict && row["admission_id"] == admissionID && row["status"] == "queued" {
			listed = append(listed, map[string]any{"admission_id": admissionID, "status": "queued", "user": r.text, "input_id": r.requestID})
		}
	}
	if !r.listedSet || !pyjson.Equal(listed, r.listed) {
		r.listed, r.listedSet = listed, true
		r.emit("session.info", r.sid, map[string]any{"stored_session_id": r.key, "pending_submissions": listed})
	}
	return nil
}

// Relays the admission's start and completion, settling the bubble with an empty completion when the owner is lost
func (r *ownerRelay) relay(ctx context.Context, client *gateway.GatewayClient, admissionID string) error {
	settled := false
	defer func() {
		if !settled && !truthy(r.session.Get("_finalized")) {
			r.emit("message.complete", r.sid, map[string]any{"text": ""})
		}
	}()
	relayCtx, cancel := context.WithTimeout(ctx, ownerRelayTimeout)
	defer cancel()
	for !settled && !truthy(r.session.Get("_finalized")) {
		pollCtx, pollCancel := context.WithTimeout(relayCtx, ownerRelayFinalizePoll)
		frame, err := client.Events.Get(pollCtx)
		pollCancel()
		if err != nil {
			if relayCtx.Err() != nil {
				return relayCtx.Err()
			}
			continue
		}
		if _, isErr := frame.(error); isErr {
			return nil
		}
		dict, _ := pyjson.Plain(frame).(map[string]any)
		params, isDict := dict["params"].(map[string]any)
		if !isDict {
			continue
		}
		if params["type"] == "session.info" {
			if err := r.pending(params, admissionID); err != nil {
				return err
			}
			continue
		}
		if params["admission_id"] != admissionID {
			continue
		}
		eventType, _ := params["type"].(string)
		if !ownerRelayed[eventType] {
			continue
		}
		payload := ownerPayloadFor(eventType, params["payload"])
		if payload == nil {
			continue
		}
		r.emit(eventType, r.sid, payload)
		settled = eventType == "message.complete"
	}
	return nil
}

// One request and answer on the owner serving the home under the host scoped grant native Desktop holds
func forwardRPC(ctx context.Context, profileHome string, endpoint *gateway.GatewayEndpoint, method string, params map[string]any) (any, error) {
	frame := int64((gateway.MaxAttachmentBytes+2)/3*4 + 1<<20)
	client, err := connectOwner(ctx, profileHome, endpoint, "host", frame)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.RPCTimeout(ctx, method, params, 0)
}

// Admits the text into the owner FIFO for this session and returns the owner receipt
func forwardPrompt(ctx context.Context, sid string, session *tuigateway.Session, text string, emit func(event, sid string, payload map[string]any), profileHome string) (map[string]any, error) {
	key := pyrepr.Str(orEmpty(session.Get("session_key")))
	if key == "" {
		return nil, &OwnerUnavailableError{Msg: "the chat has no stored session yet"}
	}
	if profileHome == "" {
		profileHome = strOr(session.Get("profile_home"))
	}
	if profileHome == "" {
		profileHome = hermesHome
	}
	resolved, err := pypath.Realpath(profileHome, pypath.Lax)
	if err != nil {
		return nil, err
	}
	endpoint, err := discoverOwner(ctx, resolved)
	if err != nil {
		return nil, err
	}
	if emit == nil {
		emit = func(event, sid string, payload map[string]any) { tuigateway.Emit(ctx, event, sid, payload) }
	}
	relay := &ownerRelay{sid: sid, session: session, key: key, home: resolved, endpoint: endpoint, text: text, emit: emit,
		requestID: "desktop-forward-" + strings.ReplaceAll(uuid.NewString(), "-", ""), admitted: make(chan ownerAdmission, 1)}
	name := []rune(key)
	if len(name) > 24 {
		name = name[:24]
	}
	memory.SpawnContextThread(ctx, "owner-forward-"+string(name), relay.run).Start()
	select {
	case admission := <-relay.admitted:
		return admission.receipt, admission.err
	case <-time.After(ownerAdmitTimeout):
		return nil, &OwnerUnavailableError{Msg: "the gateway did not confirm it in time", Unconfirmed: true}
	}
}
