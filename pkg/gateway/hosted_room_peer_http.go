package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	MaxPeerResponseBytes      = 16 * 1024 * 1024
	MaxPeerErrorResponseBytes = 16 * 1024
	peerResponseChunkBytes    = 64 * 1024
)

var (
	peerErrorCodeRE           = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	peerReceiptScopeFields    = []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id", "target_install_id", "target_profile"}
	peerTerminalRunStates     = map[string]bool{"completed": true, "failed": true, "interrupted": true, "cancelled": true}
	peerActiveRunStates       = map[string]bool{"queued": true, "running": true, "waiting_for_approval": true, "stopping": true}
	peerRunStatusKeys         = []string{"run_id", "status", "output", "error", "approval", "last_event"}
	peerLegacyDispatchCodes   = [][2]string{{"room grant", "invalid_room_grant"}, {"capability catalog changed", "room_capability_catalog_changed"}, {"execution policy changed", "room_execution_policy_changed"}}
	peerGrantRenewalCodes     = map[string]bool{"invalid_room_grant": true, "room_reauthorization_required": true}
	peerExecutionPolicyChange = [2]string{"room_execution_policy_changed", "peer room execution policy needs reauthorization"}
	peerCapabilityChange      = [2]string{"room_capability_catalog_changed", "peer room capabilities need reauthorization"}
	peerReauthorizationCodes  = map[string]bool{"invalid_room_grant": true, "room_reauthorization_required": true, peerExecutionPolicyChange[0]: true, peerCapabilityChange[0]: true}
	peerReauthorizationText   = map[string]string{
		"invalid_room_grant": "peer room authorization needs renewal", "room_reauthorization_required": "peer room authorization needs renewal",
		peerExecutionPolicyChange[0]: peerExecutionPolicyChange[1], peerCapabilityChange[0]: peerCapabilityChange[1],
	}
)

func peerHTTPLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.hosted_room_peer_http")
}

// Controlled peer HTTP failure classified for retry, ambiguity and admission
type PeerRunsHTTPError struct {
	Msg                  string
	Retryable            bool
	Ambiguous            bool
	NotAdmitted          bool
	StatusCode           int
	ErrorCode            string
	NeedsReauthorization bool
	Cause                error
}

func (e *PeerRunsHTTPError) Error() string { return e.Msg }

func (e *PeerRunsHTTPError) Unwrap() error { return e.Cause }

// Python class name of the failure
func (e *PeerRunsHTTPError) PyTypeName() string { return "PeerRunsHTTPError" }

func (e *PeerRunsHTTPError) PeerRetryable() bool { return e.Retryable }

func (e *PeerRunsHTTPError) PeerAmbiguous() bool { return e.Ambiguous }

func (e *PeerRunsHTTPError) attemptProof() (bool, bool) { return e.NotAdmitted, false }

type peerErrorFlags struct {
	retryable, ambiguous, notAdmitted bool
	statusCode                        int
	errorCode                         string
}

func newPeerRunsHTTPError(msg string, flags peerErrorFlags, cause error) *PeerRunsHTTPError {
	return &PeerRunsHTTPError{Msg: msg, Retryable: flags.retryable, Ambiguous: flags.ambiguous, NotAdmitted: flags.notAdmitted,
		StatusCode: flags.statusCode, ErrorCode: flags.errorCode, Cause: cause,
		NeedsReauthorization: (flags.statusCode == 401 || flags.statusCode == 403) && peerReauthorizationCodes[flags.errorCode]}
}

// Fail closed error when a refreshed catalog drifted from the frozen digests, policy checked first
func DigestReauthorizationError(catalog *GatewayRoomCatalog, capabilityDigest, executionPolicyDigest *string) *PeerRunsHTTPError {
	policyDigest := ""
	if catalog.ExecutionPolicy != nil {
		policyDigest = catalog.ExecutionPolicy.PolicyDigest
	}
	for _, check := range []struct {
		expected *string
		actual   string
		change   [2]string
	}{{executionPolicyDigest, policyDigest, peerExecutionPolicyChange}, {capabilityDigest, catalog.CatalogDigest, peerCapabilityChange}} {
		if check.expected != nil && check.actual != *check.expected {
			return newPeerRunsHTTPError(check.change[1], peerErrorFlags{statusCode: 403, errorCode: check.change[0], notAdmitted: true}, nil)
		}
	}
	return nil
}

var errPeerResponseTooLarge = errors.New("peer response too large")

var errPeerResponseDeadline = errors.New("peer response deadline exceeded")

// Body read within the byte budget and the request deadline
func peerReadBoundedResponse(resp *http.Response, maxBytes int, deadline float64) ([]byte, error) {
	declared := int64(-1)
	if n, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64); err == nil {
		declared = n
	}
	if declared > int64(maxBytes) {
		return nil, errPeerResponseTooLarge
	}
	body := []byte{}
	chunk := make([]byte, peerResponseChunkBytes)
	for len(body) <= maxBytes {
		if deadline-pythread.Monotonic() <= 0 {
			return nil, errPeerResponseDeadline
		}
		n, err := resp.Body.Read(chunk[:min(peerResponseChunkBytes, maxBytes+1-len(body))])
		body = append(body, chunk[:n]...)
		if len(body) > maxBytes {
			return nil, errPeerResponseTooLarge
		}
		if errors.Is(err, io.EOF) {
			return body, nil
		}
		if err != nil {
			if pythread.Monotonic() >= deadline {
				return nil, fmt.Errorf("%w: %w", errPeerResponseDeadline, err)
			}
			return nil, err
		}
	}
	return nil, errPeerResponseTooLarge
}

// Bounded body text, budget overruns raised as classified peer errors
func peerReadBody(resp *http.Response, maxBytes int, deadline float64, kind string, flags peerErrorFlags) (string, error) {
	raw, err := peerReadBoundedResponse(resp, maxBytes, deadline)
	switch {
	case err == nil:
		return pystr.DecodeUTF8Replace(raw), nil
	case errors.Is(err, errPeerResponseTooLarge):
		return "", newPeerRunsHTTPError(fmt.Sprintf("peer%s response exceeded the RoomLink size limit", kind), flags, err)
	case errors.Is(err, errPeerResponseDeadline):
		flags.retryable = true
		return "", newPeerRunsHTTPError(fmt.Sprintf("peer%s response exceeded the RoomLink time budget", kind), flags, err)
	}
	return "", err
}

// Whether no HTTP connection could have carried the request
func peerProvenPreAdmissionFailure(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	return peerNotAdmittedErrno(err)
}

func peerValidCode(code any) string {
	text, ok := code.(string)
	if ok && peerErrorCodeRE.MatchString(text) {
		return text
	}
	return ""
}

// Machine error code of an error body without returning response credentials
func peerResponseErrorCode(detail string) string {
	parsed, err := roomLoads(detail)
	if err != nil {
		return ""
	}
	payload, ok := parsed.(map[string]any)
	if !ok {
		return ""
	}
	if errorObj, ok := payload["error"].(map[string]any); ok {
		if _, isStr := errorObj["code"].(string); isStr {
			code := peerValidCode(errorObj["code"])
			if code == "invalid_room_dispatch" {
				message := strings.ToLower(hostedStrOrEmpty(errorObj["message"]))
				for _, legacy := range peerLegacyDispatchCodes {
					if strings.Contains(message, legacy[0]) {
						return legacy[1]
					}
				}
			}
			return code
		}
	}
	return peerValidCode(payload["code"])
}

func peerRunPath(record map[string]any, suffix ...string) string {
	return strings.Join(append([]string{"/v1/runs", pyurl.Quote(pyrepr.Str(record["run_id"]), "")}, suffix...), "/")
}

type peerRunKey struct {
	taskID     string
	generation int64
}

type peerStatusEntry struct {
	status      map[string]any
	err         *PeerRunsHTTPError
	delay       float64
	nextPollAt  float64
	hasPrevious bool
}

type peerBackoff struct{ delay, nextAttemptAt float64 }

// Drives a peer's dedicated group session through its scoped async Runs APIs
type PeerRunsHTTPClient struct {
	ctx               context.Context
	BaseURL           string
	APIKey            string
	TransportSecurity string
	TimeoutSeconds    float64
	ReceiptDBPath     string
	PollMinSeconds    float64
	PollMaxSeconds    float64
	clock             func() float64
	mu                sync.Mutex
	runs              map[peerRunKey]map[string]any
	observationKey    *peerRunKey
	statusCache       map[string]peerStatusEntry
	recoveryBackoff   map[peerRunKey]peerBackoff
	terminalReceipts  map[peerRunKey]bool
	roomScope         map[string]any
}

// Peer client over a validated RoomLink URL, its polling bounds checked
func NewPeerRunsHTTPClient(ctx context.Context, baseURL, apiKey string, timeoutSeconds float64, receiptDBPath string, pollMinSeconds, pollMaxSeconds float64, clock func() float64) (*PeerRunsHTTPClient, error) {
	normalized, security, err := ValidateRoomLinkURL(baseURL)
	if err != nil {
		return nil, err
	}
	if apiKey != "" && len([]rune(apiKey)) < 16 {
		return nil, &RoomValueError{Msg: "peer API key is missing or too short"}
	}
	if pollMinSeconds <= 0 || pollMaxSeconds < pollMinSeconds {
		return nil, &RoomValueError{Msg: "peer polling bounds are invalid"}
	}
	if clock == nil {
		clock = pythread.Monotonic
	}
	return &PeerRunsHTTPClient{ctx: ctx, BaseURL: normalized, APIKey: apiKey, TransportSecurity: security, TimeoutSeconds: timeoutSeconds,
		ReceiptDBPath: receiptDBPath, PollMinSeconds: pollMinSeconds, PollMaxSeconds: pollMaxSeconds, clock: clock,
		runs: map[peerRunKey]map[string]any{}, statusCache: map[string]peerStatusEntry{}, recoveryBackoff: map[peerRunKey]peerBackoff{},
		terminalReceipts: map[peerRunKey]bool{}}, nil
}

// Peer client with the keyword defaults of the Python constructor
func NewDefaultPeerRunsHTTPClient(ctx context.Context, baseURL, apiKey, receiptDBPath string) (*PeerRunsHTTPClient, error) {
	return NewPeerRunsHTTPClient(ctx, baseURL, apiKey, 30, receiptDBPath, 0.1, 2.0, nil)
}

// Attaches the gateway wide durable receipt store once
func (c *PeerRunsHTTPClient) BindReceiptStore(dbPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ReceiptDBPath != "" && c.ReceiptDBPath != dbPath {
		return newPeerRunsHTTPError("peer receipt store changed", peerErrorFlags{}, nil)
	}
	c.ReceiptDBPath = dbPath
	return nil
}

// Fences every in memory and durable receipt to one room authority
func (c *PeerRunsHTTPClient) BindRoomScope(scope PeerRoomScope) error {
	names := []string{scope.RoomID, scope.HomeInstallID, scope.AuthorityGatewayID, scope.MemberID, scope.TargetInstallID, scope.TargetProfile}
	for _, name := range names {
		if name == "" {
			return newPeerRunsHTTPError("peer room receipt scope is incomplete", peerErrorFlags{}, nil)
		}
	}
	if scope.AuthorityEpoch < 1 {
		return newPeerRunsHTTPError("peer room receipt authority epoch is invalid", peerErrorFlags{}, nil)
	}
	next := map[string]any{"room_id": names[0], "home_install_id": names[1], "authority_gateway_id": names[2],
		"authority_epoch": scope.AuthorityEpoch, "member_id": names[3], "target_install_id": names[4], "target_profile": names[5]}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roomScope != nil && pyrepr.Equal(c.roomScope, next) {
		return nil
	}
	c.roomScope, c.observationKey = next, nil
	c.runs, c.statusCache = map[peerRunKey]map[string]any{}, map[string]peerStatusEntry{}
	c.recoveryBackoff, c.terminalReceipts = map[peerRunKey]peerBackoff{}, map[peerRunKey]bool{}
	return nil
}

// In memory receipt of the attempt, the durable store as fallback
func (c *PeerRunsHTTPClient) receipt(taskID string, generation int64) (map[string]any, error) {
	c.mu.Lock()
	record := c.runs[peerRunKey{taskID, generation}]
	scope, dbPath := c.roomScope, c.ReceiptDBPath
	c.mu.Unlock()
	if record != nil || dbPath == "" || scope == nil {
		return record, nil
	}
	query := map[string]any{"task_id": taskID, "execution_generation": generation}
	for k, v := range scope {
		query[k] = v
	}
	return RemoteRunReceipt(c.ctx, dbPath, query)
}

// Pins history and status reads to one exact logical task attempt
func (c *PeerRunsHTTPClient) BindObservation(taskID string, executionGeneration int64) error {
	key := peerRunKey{taskID, executionGeneration}
	if key.taskID == "" || key.generation < 1 {
		return newPeerRunsHTTPError("peer observation identity is invalid", peerErrorFlags{}, nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observationKey != nil && *c.observationKey == key {
		return nil
	}
	for terminal := range c.terminalReceipts {
		if terminal != key {
			delete(c.runs, terminal)
			delete(c.terminalReceipts, terminal)
		}
	}
	c.observationKey = &key
	c.statusCache = map[string]peerStatusEntry{}
	c.recoveryBackoff = map[peerRunKey]peerBackoff{}
	return nil
}

// One credentialed RoomLink request, its JSON object or a classified peer error
func (c *PeerRunsHTTPClient) request(path, method string, body any, headers map[string]string, roomGrant string) (map[string]any, error) {
	deadline, ambiguous := pythread.Monotonic()+c.TimeoutSeconds, method == "POST"
	var reader io.Reader
	if body != nil {
		encoded, err := pyjson.Dumps(body, pyjson.Separators(",", ":"))
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader([]byte(encoded))
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if roomGrant != "" {
		req.Header.Set("Authorization", "HermesRoom "+roomGrant)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Hermes-RoomLink/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	timeout := time.Duration(c.TimeoutSeconds * float64(time.Second))
	resp, err := provider.OpenCredentialedURL(req, timeout, nil, nil)
	if err != nil {
		return nil, c.unreachable(ambiguous, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.httpError(resp, method, path, deadline)
	}
	raw, err := peerReadBody(resp, MaxPeerResponseBytes, deadline, "", peerErrorFlags{ambiguous: ambiguous})
	if err != nil {
		var classified *PeerRunsHTTPError
		if errors.As(err, &classified) {
			return nil, err
		}
		return nil, c.unreachable(ambiguous, err)
	}
	parsed, err := roomLoads(raw)
	if err != nil {
		return nil, newPeerRunsHTTPError("peer returned non-JSON data", peerErrorFlags{}, err)
	}
	payload, ok := parsed.(map[string]any)
	if !ok {
		return nil, newPeerRunsHTTPError("peer returned a non-object response", peerErrorFlags{}, nil)
	}
	return payload, nil
}

func (c *PeerRunsHTTPClient) unreachable(ambiguous bool, cause error) error {
	notAdmitted := ambiguous && peerProvenPreAdmissionFailure(cause)
	return newPeerRunsHTTPError("peer RoomLink endpoint is unreachable",
		peerErrorFlags{retryable: true, ambiguous: ambiguous && !notAdmitted, notAdmitted: notAdmitted}, cause)
}

// Classified peer error of an HTTP error response
func (c *PeerRunsHTTPClient) httpError(resp *http.Response, method, path string, deadline float64) error {
	code := resp.StatusCode
	flags := peerErrorFlags{ambiguous: method == "POST" && code >= 500, statusCode: code,
		notAdmitted: method == "POST" && path == "/v1/runs" && 400 <= code && code < 500}
	detail, err := peerReadBody(resp, MaxPeerErrorResponseBytes, deadline, " error", flags)
	if err != nil {
		var classified *PeerRunsHTTPError
		if errors.As(err, &classified) {
			return err
		}
		detail = ""
	}
	if runes := []rune(detail); len(runes) > 500 {
		detail = string(runes[:500])
	}
	errorCode := peerResponseErrorCode(detail)
	label := errorCode
	if label == "" {
		label = "no-code"
	}
	peerHTTPLog().Debug(fmt.Sprintf("Peer RoomLink request returned HTTP %d (%s)", code, label))
	renewal := (code == 401 || code == 403) && peerGrantRenewalCodes[errorCode]
	drift := code == 403 && (errorCode == peerExecutionPolicyChange[0] || errorCode == peerCapabilityChange[0])
	message := fmt.Sprintf("peer rejected %s %s with HTTP %d", method, path, code)
	if renewal || drift {
		message = peerReauthorizationText[errorCode]
	}
	flags.retryable = code == 408 || code == 425 || code == 429 || code >= 500
	flags.errorCode = errorCode
	return newPeerRunsHTTPError(message, flags, fmt.Errorf("HTTP Error %d: %s", code, http.StatusText(code)))
}

// Deterministic logical room session of the profile, checked against the expected id
func (c *PeerRunsHTTPClient) Prepare(_ context.Context, req PeerPrepare) (map[string]any, error) {
	if req.Source != "bot_room" {
		return nil, newPeerRunsHTTPError("peer room source must be bot_room", peerErrorFlags{}, nil)
	}
	if _, err := peerRequireRoomGrant(req.Grant); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(req.RoomID + "\x00" + req.Profile))
	logical := "roomlink_" + hex.EncodeToString(digest[:])[:32]
	if req.ExpectedSessionID != nil && *req.ExpectedSessionID != "" && *req.ExpectedSessionID != logical {
		return nil, newPeerRunsHTTPError("peer room session identity changed", peerErrorFlags{}, nil)
	}
	return map[string]any{"session_id": logical, "title": "Group: " + req.RoomID, "source": req.Source}, nil
}

func peerDispatchScope(checked *HostedMemberDispatch) PeerRoomScope {
	return PeerRoomScope{RoomID: checked.RoomID, HomeInstallID: checked.HomeInstallID, AuthorityGatewayID: checked.AuthorityGatewayID,
		AuthorityEpoch: checked.AuthorityEpoch.Int64(), MemberID: checked.MemberID, TargetInstallID: checked.TargetInstallID, TargetProfile: checked.TargetProfile}
}

// Validated dispatch with its grant, scope and observation pinned to it
func (c *PeerRunsHTTPClient) checkedDispatch(dispatch any, grant string) (*HostedMemberDispatch, error) {
	checked, err := HostedMemberDispatchFromMapping(dispatch)
	if err != nil {
		return nil, err
	}
	if _, err := peerRequireRoomGrant(grant); err != nil {
		return nil, err
	}
	if err := c.BindRoomScope(peerDispatchScope(checked)); err != nil {
		return nil, err
	}
	if err := c.BindObservation(checked.TaskID, checked.ExecutionGeneration.Int64()); err != nil {
		return nil, err
	}
	return checked, nil
}

func (c *PeerRunsHTTPClient) Dispatch(_ context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error) {
	checked, err := c.checkedDispatch(dispatch, grant)
	if err != nil {
		return nil, err
	}
	return c.admitDispatch(checked, grant)
}

// Recovers one exact admission by receipt or idempotent POST replay
func (c *PeerRunsHTTPClient) RecoverDispatch(dispatch any, grant string) (map[string]any, error) {
	checked, err := c.checkedDispatch(dispatch, grant)
	if err != nil {
		return nil, err
	}
	generation := checked.ExecutionGeneration.Int64()
	existing, err := c.receipt(checked.TaskID, generation)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		scope := peerDispatchScopeFields(checked)
		for _, field := range peerReceiptScopeFields {
			if !pyrepr.Equal(existing[field], scope[field]) {
				return nil, newPeerRunsHTTPError("peer run receipt conflicts with the recovered dispatch", peerErrorFlags{}, nil)
			}
		}
		return peerAccepted(checked, pyrepr.Str(existing["run_id"]), pyrepr.Str(existing["session_id"]), true), nil
	}
	key, now := peerRunKey{checked.TaskID, generation}, c.clock()
	c.mu.Lock()
	backoff, backingOff := c.recoveryBackoff[key]
	c.mu.Unlock()
	if backingOff && now < backoff.nextAttemptAt {
		return nil, newPeerRunsHTTPError("peer admission recovery is backing off", peerErrorFlags{retryable: true, ambiguous: true}, nil)
	}
	recovered, err := c.admitDispatch(checked, grant)
	if err != nil {
		var classified *PeerRunsHTTPError
		if errors.As(err, &classified) && classified == err && (classified.Retryable || classified.Ambiguous) {
			delay := c.nextPollDelay(backingOff, backoff.delay)
			c.mu.Lock()
			c.recoveryBackoff = map[peerRunKey]peerBackoff{key: {delay: delay, nextAttemptAt: now + delay}}
			c.mu.Unlock()
		}
		return nil, err
	}
	c.mu.Lock()
	delete(c.recoveryBackoff, key)
	c.mu.Unlock()
	return recovered, nil
}

func peerDispatchScopeFields(checked *HostedMemberDispatch) map[string]any {
	return map[string]any{"room_id": checked.RoomID, "home_install_id": checked.HomeInstallID, "authority_gateway_id": checked.AuthorityGatewayID,
		"authority_epoch": checked.AuthorityEpoch.Int64(), "member_id": checked.MemberID, "target_install_id": checked.TargetInstallID,
		"target_profile": checked.TargetProfile}
}

func peerAccepted(checked *HostedMemberDispatch, runID, sessionID string, replayed bool) map[string]any {
	return map[string]any{"status": "accepted", "task_id": checked.TaskID, "execution_generation": checked.ExecutionGeneration.Int64(),
		"run_id": runID, "session_id": sessionID, "replayed": replayed}
}

func (c *PeerRunsHTTPClient) admitDispatch(checked *HostedMemberDispatch, grant string) (map[string]any, error) {
	generation := checked.ExecutionGeneration.Int64()
	sessionID, err := c.sessionID(checked, grant)
	if err != nil {
		return nil, err
	}
	admit := func() (map[string]any, error) {
		return c.request("/v1/runs", "POST", pyjson.ObjectOf("input", checked.Prompt, "hosted_room_dispatch", checked.AsMapping()),
			map[string]string{"Idempotency-Key": fmt.Sprintf("room:%s:%d", checked.TaskID, generation)}, grant)
	}
	result, err := admit()
	if err != nil {
		var classified *PeerRunsHTTPError
		if !errors.As(err, &classified) || classified != err || !classified.Ambiguous {
			return nil, err
		}
		if result, err = admit(); err != nil {
			return nil, err
		}
	}
	runID := hostedStrOrEmpty(result["run_id"])
	if runID == "" {
		return nil, newPeerRunsHTTPError("peer did not return a run id", peerErrorFlags{}, nil)
	}
	receipt := map[string]any{"run_id": runID, "session_id": sessionID}
	for field, value := range peerDispatchScopeFields(checked) {
		receipt[field] = value
	}
	receipt["task_id"], receipt["execution_generation"] = checked.TaskID, generation
	c.mu.Lock()
	dbPath := c.ReceiptDBPath
	c.mu.Unlock()
	if dbPath != "" {
		if err := UpsertRemoteRunReceipt(c.ctx, dbPath, receipt, nil); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	c.runs[peerRunKey{checked.TaskID, generation}] = receipt
	delete(c.statusCache, runID)
	c.mu.Unlock()
	replayed, has := result["replayed"]
	if !has {
		replayed = false
	}
	return peerAccepted(checked, runID, sessionID, pyrepr.Truthy(replayed)), nil
}

func (c *PeerRunsHTTPClient) sessionID(dispatch *HostedMemberDispatch, grant string) (string, error) {
	existing, err := c.receipt(dispatch.TaskID, dispatch.ExecutionGeneration.Int64())
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return pyrepr.Str(existing["session_id"]), nil
	}
	prepared, err := c.Prepare(c.ctx, PeerPrepare{RoomID: dispatch.RoomID, Profile: dispatch.TargetProfile, Source: "bot_room", Grant: grant, Create: true})
	if err != nil {
		return "", err
	}
	if prepared == nil {
		return "", newPeerRunsHTTPError("peer room session is unavailable", peerErrorFlags{}, nil)
	}
	if id := hostedStrOrEmpty(prepared["session_id"]); id != "" {
		return id, nil
	}
	return hostedStrOrEmpty(prepared["id"]), nil
}

func (c *PeerRunsHTTPClient) observationReceipt(roomID, profile, sessionID string) (map[string]any, error) {
	c.mu.Lock()
	key := c.observationKey
	c.mu.Unlock()
	if key == nil {
		return nil, nil
	}
	record, err := c.receipt(key.taskID, key.generation)
	if err != nil || record == nil {
		return nil, err
	}
	if !pyrepr.Equal(record["room_id"], roomID) || !pyrepr.Equal(record["target_profile"], profile) || !pyrepr.Equal(record["session_id"], sessionID) {
		return nil, newPeerRunsHTTPError("peer observation receipt changed scope", peerErrorFlags{}, nil)
	}
	return record, nil
}

func (c *PeerRunsHTTPClient) nextPollDelay(cached bool, cachedDelay float64) float64 {
	previous := c.PollMinSeconds / 2
	if cached {
		previous = cachedDelay
	}
	return min(c.PollMaxSeconds, max(c.PollMinSeconds, previous*2))
}

func (c *PeerRunsHTTPClient) pollReceipt(record map[string]any, grant string) (map[string]any, error) {
	runID, now := pyrepr.Str(record["run_id"]), c.clock()
	c.mu.Lock()
	cached, isCached := c.statusCache[runID]
	c.mu.Unlock()
	if isCached {
		if state, _ := cached.status["status"].(string); peerTerminalRunStates[state] {
			return cached.status, nil
		}
		if now < cached.nextPollAt {
			if cached.err != nil {
				return nil, cached.err
			}
			return cached.status, nil
		}
	}
	delay := c.nextPollDelay(isCached, cached.delay)
	status, err := c.fetchRunStatus(record, runID, grant)
	if err != nil {
		var classified *PeerRunsHTTPError
		if errors.As(err, &classified) && classified == err {
			previous := map[string]any{}
			if isCached {
				previous = cached.status
			}
			c.mu.Lock()
			c.statusCache = map[string]peerStatusEntry{runID: {status: previous, err: classified, delay: delay, nextPollAt: now + delay}}
			c.mu.Unlock()
		}
		return nil, err
	}
	c.mu.Lock()
	c.statusCache = map[string]peerStatusEntry{runID: {status: status, delay: delay, nextPollAt: now + delay}}
	if state, _ := status["status"].(string); peerTerminalRunStates[state] {
		generation, _ := pyint.Int64(record["execution_generation"])
		c.terminalReceipts[peerRunKey{pyrepr.Str(record["task_id"]), generation}] = true
	}
	c.mu.Unlock()
	return status, nil
}

func (c *PeerRunsHTTPClient) fetchRunStatus(record map[string]any, runID, grant string) (map[string]any, error) {
	scoped, err := peerRequireRoomGrant(grant)
	if err != nil {
		return nil, err
	}
	full, err := c.request(peerRunPath(record), "GET", nil, nil, scoped)
	if err != nil {
		return nil, err
	}
	if full["status"] == "unknown" && hostedStrOrEmpty(full["run_id"]) == runID {
		if full, err = c.resolveUnknown(record, full, grant); err != nil {
			return nil, err
		}
	}
	status := map[string]any{}
	for _, key := range peerRunStatusKeys {
		if value, has := full[key]; has {
			status[key] = value
		}
	}
	state, _ := status["status"].(string)
	if hostedStrOrEmpty(status["run_id"]) != runID || !(peerTerminalRunStates[state] || peerActiveRunStates[state]) {
		return nil, newPeerRunsHTTPError("peer returned a mismatched run status", peerErrorFlags{}, nil)
	}
	return status, nil
}

// Acknowledges a run lost to a member gateway restart and settles it interrupted
func (c *PeerRunsHTTPClient) resolveUnknown(record, full map[string]any, grant string) (map[string]any, error) {
	if _, err := c.request(peerRunPath(record, "resolve-unknown"), "POST",
		pyjson.ObjectOf("admission_id", full["admission_id"], "execution_generation", full["execution_generation"]), nil, grant); err != nil {
		return nil, err
	}
	return map[string]any{"run_id": full["run_id"], "status": "interrupted", "error": "member gateway restarted"}, nil
}

// Terminal history row of the observed run, empty while it is active
func (c *PeerRunsHTTPClient) History(_ context.Context, roomID, profile, sessionID, grant string) ([]map[string]any, error) {
	receipt, err := c.observationReceipt(roomID, profile, sessionID)
	if err != nil || receipt == nil {
		return []map[string]any{}, err
	}
	status, err := c.pollReceipt(receipt, grant)
	if err != nil {
		return nil, err
	}
	state := hostedStrOrEmpty(status["status"])
	if state != "completed" && state != "failed" && state != "interrupted" {
		return []map[string]any{}, nil
	}
	rowStatus := "failed"
	if state == "completed" {
		rowStatus = "settled"
	}
	content := status["output"]
	if !pyrepr.Truthy(content) {
		content = status["error"]
	}
	if !pyrepr.Truthy(content) {
		content = ""
	}
	return []map[string]any{{"role": "assistant", "task_id": receipt["task_id"], "execution_generation": receipt["execution_generation"],
		"status": rowStatus, "message_id": "peer-run:" + pyrepr.Str(status["run_id"]), "content": content}}, nil
}

// Live state of the observed run
func (c *PeerRunsHTTPClient) Status(_ context.Context, roomID, profile, sessionID, grant string) (map[string]any, error) {
	receipt, err := c.observationReceipt(roomID, profile, sessionID)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return map[string]any{"active": false, "task_id": nil}, nil
	}
	status, err := c.pollReceipt(receipt, grant)
	if err != nil {
		return nil, err
	}
	state, _ := status["status"].(string)
	return map[string]any{"active": peerActiveRunStates[state], "task_id": receipt["task_id"], "execution_generation": receipt["execution_generation"],
		"status": status["status"], "run_id": status["run_id"], "approval": status["approval"]}, nil
}

// Resolves approval of the exact durable remote run, nil when no receipt exists
func (c *PeerRunsHTTPClient) ApproveReceipt(taskID string, executionGeneration int64, requestID, choice, grant string) (map[string]any, error) {
	record, err := c.receipt(taskID, executionGeneration)
	if err != nil || record == nil {
		return nil, err
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, newPeerRunsHTTPError("an exact approval request_id is required", peerErrorFlags{}, nil)
	}
	if _, err := peerRequireRoomGrant(grant); err != nil {
		return nil, err
	}
	return c.postRunAction(record, "approval", pyjson.ObjectOf("choice", choice, "request_id", requestID), grant)
}

func (c *PeerRunsHTTPClient) postRunAction(record map[string]any, action string, body *pyjson.Object, grant string) (map[string]any, error) {
	result, err := c.request(peerRunPath(record, action), "POST", body, nil, grant)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	delete(c.statusCache, pyrepr.Str(record["run_id"]))
	c.mu.Unlock()
	return result, nil
}

func (c *PeerRunsHTTPClient) Stop(ctx context.Context, dispatch *pyjson.Object, grant string) (map[string]any, error) {
	checked, err := HostedMemberDispatchFromMapping(dispatch)
	if err != nil {
		return nil, err
	}
	if err := c.BindRoomScope(peerDispatchScope(checked)); err != nil {
		return nil, err
	}
	return c.StopReceipt(ctx, checked.TaskID, checked.ExecutionGeneration.Int64(), grant)
}

// Stops the exact durable remote run after a home restart, nil when no receipt exists
func (c *PeerRunsHTTPClient) StopReceipt(_ context.Context, taskID string, executionGeneration int64, grant string) (map[string]any, error) {
	record, err := c.receipt(taskID, executionGeneration)
	if err != nil || record == nil {
		return nil, err
	}
	scoped, err := peerRequireRoomGrant(grant)
	if err != nil {
		return nil, err
	}
	result, err := c.postRunAction(record, "stop", pyjson.NewObject(), scoped)
	if err != nil {
		return nil, err
	}
	if state, _ := result["status"].(string); peerTerminalRunStates[state] {
		c.mu.Lock()
		c.terminalReceipts[peerRunKey{taskID, executionGeneration}] = true
		c.mu.Unlock()
	}
	return result, nil
}

// Asks the target gateway to mint a scoped room member grant
func (c *PeerRunsHTTPClient) IssueInvitation(roomID, homeInstallID, authorityGatewayID string, authorityEpoch int64, memberID, grantID string, ttlSeconds float64, statusTTLSeconds *float64) (map[string]any, error) {
	if c.APIKey == "" {
		return nil, newPeerRunsHTTPError("issuing an invitation requires the target gateway API key", peerErrorFlags{}, nil)
	}
	body := pyjson.ObjectOf("room_id", roomID, "home_install_id", homeInstallID, "authority_gateway_id", authorityGatewayID,
		"authority_epoch", authorityEpoch, "member_id", memberID, "grant_id", grantID, "ttl_seconds", ttlSeconds)
	if statusTTLSeconds != nil {
		body.Set("status_ttl_seconds", *statusTTLSeconds)
	}
	return c.request("/v1/room-members/invitations", "POST", body, nil, "")
}

// Renews dispatch access only while its frozen authority is unchanged
func (c *PeerRunsHTTPClient) RefreshGrant(grant string, ttlSeconds float64, capabilityDigest, executionPolicyDigest *string) (map[string]any, error) {
	refreshed, err := c.scopedPost("/v1/room-members/grants/refresh", grant, pyjson.ObjectOf("ttl_seconds", ttlSeconds))
	if err != nil {
		return nil, err
	}
	replacement := hostedStrOrEmpty(refreshed["grant"])
	if replacement == "" {
		return nil, newPeerRunsHTTPError("peer returned no refreshed room grant", peerErrorFlags{}, nil)
	}
	probe, err := c.Probe(replacement)
	if err != nil {
		return nil, err
	}
	catalog, err := GatewayRoomCatalogFromMapping(probe["catalog"])
	if err != nil {
		return nil, err
	}
	if drift := DigestReauthorizationError(catalog, capabilityDigest, executionPolicyDigest); drift != nil {
		return nil, drift
	}
	out := make(map[string]any, len(refreshed)+1)
	for k, v := range refreshed {
		out[k] = v
	}
	out["catalog"] = probe["catalog"]
	return out, nil
}

// Revokes this grant's exact room, home, target and profile scope
func (c *PeerRunsHTTPClient) RevokeGrant(grant string) (map[string]any, error) {
	return c.scopedPost("/v1/room-members/grants/revoke", grant, pyjson.NewObject())
}

// Verifies gateway reachability and the live scoped capability catalog
func (c *PeerRunsHTTPClient) Probe(grant string) (map[string]any, error) {
	scoped, err := peerRequireRoomGrant(grant)
	if err != nil {
		return nil, err
	}
	return c.request("/v1/room-members/capabilities", "GET", nil, nil, scoped)
}

func (c *PeerRunsHTTPClient) scopedPost(path, grant string, body *pyjson.Object) (map[string]any, error) {
	scoped, err := peerRequireRoomGrant(grant)
	if err != nil {
		return nil, err
	}
	return c.request(path, "POST", body, nil, scoped)
}

// Scoped grant, refusing a fallback to broad Bearer auth
func peerRequireRoomGrant(grant string) (string, error) {
	if grant == "" || grant == "compat" || grant == "compatibility-only" {
		return "", newPeerRunsHTTPError("a scoped room grant is required", peerErrorFlags{}, nil)
	}
	return grant, nil
}
