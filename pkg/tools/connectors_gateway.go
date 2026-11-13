package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// ---- names --------------------------------------------------------------------------------------

// Reports whether a connector prefixed name parses
func connectorNameValid(name string) bool {
	_, ok := ParseConnectorName(name)
	return ok
}

// ---- errors -------------------------------------------------------------------------------------

// ToolGatewayError is an HTTP level gateway failure
type ToolGatewayError struct {
	Message    string
	Code       string
	Status     int
	RequestID  string
	Retryable  bool
	Kind       string
	RetryAfter float64
}

func (e *ToolGatewayError) Error() string { return e.Message }

const (
	gatewayKindAuth        = "auth"
	gatewayKindUnavailable = "unavailable"
	gatewayKindIdempotency = "idempotency"
	gatewayKindRateLimited = "rate_limited"
)

// Gateway failure of the given kind with its code, status, request id and retryable flag, the code defaulting to GATEWAY_ERROR
func newToolGatewayError(kind, message, code string, status int, requestID string, retryable bool) *ToolGatewayError {
	if code == "" {
		code = "GATEWAY_ERROR"
	}
	return &ToolGatewayError{Message: message, Code: code, Status: status, RequestID: requestID, Retryable: retryable, Kind: kind}
}

// Rate limited gateway failure carrying the retry hint in seconds the caller decides whether to wait for
func newRateLimited(message string, retryAfter float64, code string, status int, requestID string) *ToolGatewayError {
	e := newToolGatewayError(gatewayKindRateLimited, message, code, status, requestID, false)
	e.RetryAfter = retryAfter
	return e
}

// Reports whether an error is a gateway error of the given kind
func gatewayErrorKind(err error, kind string) (*ToolGatewayError, bool) {
	var ge *ToolGatewayError
	if errors.As(err, &ge) && ge.Kind == kind {
		return ge, true
	}
	return nil, false
}

// ParseGatewayError reads every gateway error envelope shape, failing only on a 429 whose Retry-After is no number
func ParseGatewayError(status int, body any, headers http.Header) (*ToolGatewayError, error) {
	code := "HTTP_" + strconv.Itoa(status)
	message := ""
	requestID := ""
	retryAfterMs := 0.0
	hasRetryAfterMs := false
	if m := asMap(body); m != nil {
		switch envelope := m["error"].(type) {
		case map[string]any:
			if c := asStr(envelope["code"]); c != "" {
				code = c
			}
			message = asStr(envelope["message"])
		case string:
			if envelope != "" {
				code = envelope
			}
		default:
			if c := asStr(m["code"]); c != "" {
				code = c
				message = asStr(m["message"])
			}
		}
		if m["requestId"] != nil {
			requestID = asStr(m["requestId"])
		}
		switch v := m["retryAfterMs"].(type) {
		case float64:
			retryAfterMs, hasRetryAfterMs = v, true
		case int:
			retryAfterMs, hasRetryAfterMs = float64(v), true
		}
	} else if body != nil {
		message = truncateRunes(asStr(body), 500)
	}
	if message == "" {
		message = fmt.Sprintf("tool gateway request failed with status %d", status)
	}
	switch {
	case status == 401 || status == 403:
		return newToolGatewayError(gatewayKindAuth, message, code, status, requestID, false), nil
	case status == 404:
		return newToolGatewayError(gatewayKindUnavailable, message, code, status, requestID, false), nil
	case status == 409:
		return newToolGatewayError(gatewayKindIdempotency, message, code, status, requestID, false), nil
	case status == 429:
		header := ""
		if headers != nil {
			header = headers.Get("Retry-After")
		}
		retryAfter := 1.0
		if header != "" {
			seconds, err := pyint.ParseFloat(header)
			if err != nil {
				return nil, err
			}
			retryAfter = seconds
		} else if hasRetryAfterMs && retryAfterMs != 0 {
			retryAfter = retryAfterMs / 1000.0
		}
		return newRateLimited(message, retryAfter, code, status, requestID), nil
	}
	return newToolGatewayError("", message, code, status, requestID, status >= 500), nil
}

const (
	surfaceCard = "card"
	surfaceSide = "side"
	surfaceLink = "link"
)

// SurfaceCard, SurfaceSide and SurfaceLink export the connection surfaces
const SurfaceCard, SurfaceSide, SurfaceLink = surfaceCard, surfaceSide, surfaceLink

// SideAgentHint tells a delegated agent it cannot connect apps itself
const SideAgentHint = "This app is not connected. Only the main agent can connect it, so report that back instead of trying to connect it here."

const cardHint = "This app is not connected. Call manage_connections to open a connection card for it; there is no link to show the user."
const expiredSideAgentHint = "This app's connection expired. Only the main agent can reconnect it, so report that back instead of trying to reconnect it here."
const expiredCardHint = "This app's connection expired. Call manage_connections with action 'reconnect' to open a connection card for it; there is no link to show the user."
const expiredLinkHint = "The connection expired. Show the user the connect_url to reconnect, then retry the call."

type connectionSurfaceKey struct{}

// WithConnectionSurface binds the surface connector refusals are rendered for
func WithConnectionSurface(ctx context.Context, surface string) context.Context {
	return context.WithValue(ctx, connectionSurfaceKey{}, surface)
}

// ConnectionSurface reads the bound surface, link when none is bound
func ConnectionSurface(ctx context.Context) string {
	if ctx != nil {
		if s, ok := ctx.Value(connectionSurfaceKey{}).(string); ok && s != "" {
			return s
		}
	}
	return surfaceLink
}

// SideAgentToolDrops names the tools a delegated side agent never sees
var SideAgentToolDrops = map[string]bool{"manage_connections": true}

func connectionDefaultMessage(code, connector string) string {
	app := "This connector"
	if connector != "" {
		app = "The " + connector + " connector"
	}
	if code == "CONNECTION_EXPIRED" {
		return app + " connection for this account has expired and must be reconnected."
	}
	return app + " is not connected for this account."
}

// RenderConnectionRequired builds the model facing shape for an app the user must connect first
func RenderConnectionRequired(connector, message, connectURL, hint, surface, code string) map[string]any {
	if code == "" {
		code = "CONNECTION_REQUIRED"
	}
	sideHint, cardHintText, linkHint := SideAgentHint, cardHint, ""
	if code == "CONNECTION_EXPIRED" {
		sideHint, cardHintText, linkHint = expiredSideAgentHint, expiredCardHint, expiredLinkHint
	}
	if message == "" {
		message = connectionDefaultMessage(code, connector)
	}
	payload := map[string]any{"code": code, "message": message}
	if connector != "" {
		payload["connector"] = connector
	}
	switch surface {
	case surfaceSide:
		payload["hint"] = sideHint
		return payload
	case surfaceCard:
		payload["connect_card_available"] = true
		payload["hint"] = cardHintText
		return payload
	}
	if connectURL != "" {
		payload["connect_url"] = connectURL
		if hint == "" {
			hint = linkHint
		}
	}
	if hint != "" {
		payload["hint"] = hint
	}
	return payload
}

// ---- wire ---------------------------------------------------------------------------------------

const (
	connectorsPath           = "v1/connectors"
	connectorSearchPath      = connectorsPath + "/search"
	connectorSchemasPath     = connectorsPath + "/schemas"
	connectorExecutePath     = connectorsPath + "/execute"
	connectorConnectionsPath = connectorsPath + "/connections"
	connectorAccountsPath    = connectorsPath + "/accounts"
	connectorFileUploadPath  = connectorsPath + "/files/upload"
	connectorFileMaxBytes    = 20 * 1024 * 1024
)

var executeErrorCodes = map[string]string{
	"TOOL_NOT_ALLOWED": "product", "CONNECTION_REQUIRED": "product", "CONNECTION_EXPIRED": "product", "ALIAS_REQUIRED": "product",
	"ALIAS_UNKNOWN": "product", "INVALID_ARGUMENTS": "product", "TOOL_NOT_FOUND": "product", "PROVIDER_ERROR": "backend",
	"PROVIDER_RATE_LIMITED": "backend", "PROVIDER_TIMEOUT": "backend", "POLICY_UNAVAILABLE": "backend", "GATEWAY_ERROR": "backend",
}

var fileErrorFamilies = map[string]string{"FILE_NOT_ALLOWED": "product", "FILE_NOT_FOUND": "product", "FILE_TOO_LARGE": "product", "FILE_UPLOAD_FAILED": "backend"}

// The gateway's family when sent, else the table's, empty for an unknown code
func executeErrorFamily(code, family string) string {
	if family == "product" || family == "backend" {
		return family
	}
	if f, ok := executeErrorCodes[code]; ok {
		return f
	}
	return fileErrorFamilies[code]
}

// Gateway error family of a connector execute error code, empty when the code is unknown
func ExecuteErrorFamily(code, family string) string {
	return executeErrorFamily(code, family)
}

type wireError struct{ msg string }

func (e *wireError) Error() string { return e.msg }

func wireStrList(v any) []string {
	var out []string
	for _, item := range asList(v) {
		out = append(out, asStr(item))
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// Status values a gateway connection row may carry, the ConnectionStatus Literal
var ConnectionStatuses = []string{"pending", "active", "failed", "expired", "revoked", "inactive"}

// ---- merge --------------------------------------------------------------------------------------

// PlannedCall is one connector call positioned in its originating batch
type PlannedCall struct {
	Position  int
	Name      string
	Connector string
	Tool      string
	Arguments map[string]any
	Typed     *pyjson.Object
}

// Arguments for the wire, the handler shaped tree given the key order and number types of the typed one
func wireTyped(plain, typed any) any {
	switch p := plain.(type) {
	case map[string]any:
		o := pyjson.NewObject()
		t, _ := typed.(*pyjson.Object)
		if t != nil {
			for _, k := range t.Keys {
				if v, has := p[k]; has {
					o.Set(k, wireTyped(v, t.Vals[k]))
				}
			}
		}
		for _, k := range sortedConnectorKeys(p) {
			if _, done := o.Get(k); !done {
				o.Set(k, wireTyped(p[k], nil))
			}
		}
		return o
	case []any:
		t, _ := typed.([]any)
		out := make([]any, len(p))
		for i := range p {
			var tv any
			if i < len(t) {
				tv = t[i]
			}
			out[i] = wireTyped(p[i], tv)
		}
		return out
	case float64:
		switch tv := typed.(type) {
		case int64:
			if float64(tv) == p {
				return tv
			}
		case *big.Int:
			if f, _ := new(big.Float).SetInt(tv).Float64(); f == p {
				return tv
			}
		}
	}
	return plain
}

// String list as the JSON array a wire model dump holds
func wireStrings(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// One validated call of an execute request, its arguments in the key order and number types of the typed tree
func wireExecuteCall(plan PlannedCall) (*pydantic.Instance, error) {
	var typed any
	if plan.Typed != nil {
		typed = plan.Typed
	}
	call, verr := ConnectorExecuteCall.Validate(pyjson.ObjectOf("connector", plan.Connector, "tool", plan.Tool, "arguments", wireTyped(plan.Arguments, typed)))
	if verr != nil {
		return nil, verr
	}
	return call, nil
}

// Partition splits a batch into local calls, remote connector calls, and per entry errors
type Partition struct {
	Local  []LocalCall
	Remote []PlannedCall
	Errors []map[string]any
}

// LocalCall is a non connector batch entry with its position
type LocalCall struct {
	Position int
	Call     map[string]any
}

var connectionCodes = map[string]bool{"CONNECTION_REQUIRED": true, "CONNECTION_EXPIRED": true}

func connectorErrorEntry(position int, name, code, message string) map[string]any {
	return map[string]any{"index": position, "name": name, "error": map[string]any{"code": code, "message": message}}
}

// Items of a list or tuple, none for a string, bytes or any other value so it never splits into characters
func asSequence(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case pyrepr.Tuple:
		return []any(v)
	}
	return nil
}

// PartitionCalls treats malformed entries as per entry errors so sibling calls still run
func PartitionCalls(calls any) Partition {
	var p Partition
	for position, raw := range asSequence(calls) {
		call := asMap(raw)
		name := ""
		if call != nil {
			name = asStr(call["name"])
		}
		if parsed, ok := ParseConnectorName(name); ok {
			args := asMap(call["arguments"])
			copied := map[string]any{}
			for k, v := range args {
				copied[k] = v
			}
			p.Remote = append(p.Remote, PlannedCall{Position: position, Name: parsed.Raw, Connector: parsed.Connector, Tool: parsed.Tool, Arguments: copied})
			continue
		}
		if strings.HasPrefix(name, ConnectorNamePrefix) {
			p.Errors = append(p.Errors, connectorErrorEntry(position, name, "TOOL_NOT_FOUND", "Malformed connector tool name; expected connectors__<connector>__{tool}."))
			continue
		}
		if call == nil {
			call = map[string]any{}
		}
		p.Local = append(p.Local, LocalCall{Position: position, Call: call})
	}
	return p
}

func optStr(v any) string {
	s, _ := v.(string)
	return s
}

// Entry carrying the account alias the call ran under when there is one
func withAccount(entry map[string]any, account string) map[string]any {
	if account != "" {
		entry["account"] = account
	}
	return entry
}

// RenderRemoteEntry renders one gateway result into the batch entry the model reads
func RenderRemoteEntry(ctx context.Context, planned PlannedCall, remote map[string]any) map[string]any {
	account := optStr(remote["account"])
	errMap := asMap(remote["error"])
	if errMap == nil {
		entry := withAccount(map[string]any{"index": planned.Position, "name": planned.Name, "response": remote["data"]}, account)
		if du := asList(remote["data_use"]); len(du) > 0 {
			entry["data_use"] = wireStrList(du)
		} else if dus, ok := remote["data_use"].([]string); ok && len(dus) > 0 {
			entry["data_use"] = dus
		}
		return entry
	}
	code := optStr(errMap["code"])
	if code == "" {
		code = "PROVIDER_ERROR"
	}
	message := optStr(errMap["message"])
	if message == "" {
		message = "The gateway reported an error."
	}
	var payload map[string]any
	if connectionCodes[code] {
		connector := optStr(errMap["connector"])
		if connector == "" {
			connector = planned.Connector
		}
		payload = RenderConnectionRequired(connector, message, optStr(errMap["connect_url"]), optStr(errMap["hint"]), ConnectionSurface(ctx), code)
		if cid := optStr(errMap["connection_id"]); cid != "" {
			payload["connection_id"] = cid
		}
	} else {
		payload = map[string]any{"code": code, "message": message}
		if c := optStr(errMap["connector"]); c != "" {
			payload["connector"] = c
		}
		if h := optStr(errMap["hint"]); h != "" {
			payload["hint"] = h
		}
	}
	if f := optStr(errMap["family"]); f != "" {
		payload["family"] = f
	}
	return withAccount(map[string]any{"index": planned.Position, "name": planned.Name, "error": payload}, account)
}

// SpliceRemoteResults correlates results by request slot, missing slots becoming errors
func SpliceRemoteResults(ctx context.Context, planned []PlannedCall, results []map[string]any) []map[string]any {
	entries := make([]map[string]any, 0, len(planned))
	for slot, plan := range planned {
		if slot < len(results) && results[slot] != nil {
			entries = append(entries, RenderRemoteEntry(ctx, plan, results[slot]))
		} else {
			entries = append(entries, connectorErrorEntry(plan.Position, plan.Name, "PROVIDER_ERROR", "The gateway returned no result for this call."))
		}
	}
	return entries
}

// FillRemoteFailure gives every planned call the same error entry
func FillRemoteFailure(planned []PlannedCall, message, code string) []map[string]any {
	if code == "" {
		code = "PROVIDER_ERROR"
	}
	out := make([]map[string]any, 0, len(planned))
	for _, plan := range planned {
		out = append(out, connectorErrorEntry(plan.Position, plan.Name, code, message))
	}
	return out
}

// AssembleResults orders entries by position and fills unclaimed slots with errors
func AssembleResults(total int, groups ...[]*pyjson.Object) *pyjson.Object {
	if total < 0 {
		total = 0
	}
	slots := make([]*pyjson.Object, total)
	for _, group := range groups {
		for _, entry := range group {
			idx, ok := entry.Vals["index"].(int)
			if n, isInt := entry.Vals["index"].(int64); isInt {
				idx, ok = int(n), true
			}
			if ok && idx >= 0 && idx < len(slots) && slots[idx] == nil {
				slots[idx] = entry.Copy()
			}
		}
	}
	merged := make([]any, 0, len(slots))
	errorCount := 0
	for position, entry := range slots {
		if entry == nil {
			entry = modelEntry(connectorErrorEntry(position, "", "PROVIDER_ERROR", "No result was produced for this call."))
		}
		if _, has := entry.Get("error"); has {
			errorCount++
		}
		merged = append(merged, entry)
	}
	return pyjson.ObjectOf("results", merged, "success_count", len(merged)-errorCount, "error_count", errorCount, "total_count", len(merged))
}

// ---- identity -----------------------------------------------------------------------------------

// CallRef carries the model's ids for the call a gateway request serves
type CallRef struct {
	TurnID     string
	ToolCallID string
}

const (
	sessionHeader   = "x-nous-hermes-session"
	turnHeader      = "x-nous-hermes-turn"
	toolCallHeader  = "x-nous-hermes-tool-call"
	requestIDHeader = "x-request-id"
)

var executionSurfaces = map[string]bool{"acp": true, "api": true, "batch": true, "cli": true, "desktop": true, "gateway": true, "python": true, "scheduled_task": true, "tui": true, "other": true, "unknown": true}
var surfaceAliases = map[string]string{"api_server": "api", "relay": "gateway", "cron": "scheduled_task", "scheduler": "scheduled_task", "scheduled": "scheduled_task"}
var knownGatewayPlatforms = map[string]bool{"discord": true, "email": true, "slack": true, "telegram": true, "teams": true, "whatsapp": true}

// TelemetrySendConsented reports whether the profile opted into sending telemetry, installed by the connectors wiring
var TelemetrySendConsented func(ctx context.Context, env *Env) (bool, error)

// ConnectorConversationContext is the conversation lineage root the context carries, installed by the connectors wiring
var ConnectorConversationContext func(ctx context.Context) string

// Normalizes a platform value to the shared metrics execution surface
func executionSurface(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		v = "unknown"
	}
	if executionSurfaces[v] {
		return v
	}
	if alias, ok := surfaceAliases[v]; ok {
		return alias
	}
	if PlatformRegistered != nil && PlatformRegistered(v) {
		return "gateway"
	}
	if knownGatewayPlatforms[v] {
		return "gateway"
	}
	return "other"
}

func sessionPlatformName(env *Env) string {
	platform := getSessionPlatform(env)
	if platform == "" {
		platform = sessionValue(env, "source", "ICEBATH_SESSION_SOURCE")
	}
	return strings.ToLower(strings.TrimSpace(platform))
}

// Running turn's lease platform, else the bound session platform or source, else cron or cli
func connectorPlatformName(ctx context.Context, env *Env) string {
	if turn := CurrentRelayTurn(ctx); turn != nil && turn.Lease != nil && turn.Lease.Platform != "" {
		return turn.Lease.Platform
	}
	if p := sessionPlatformName(env); p != "" {
		return p
	}
	if isCronApprovalContext(env) {
		return "cron"
	}
	return "cli"
}

func requestSurface(ctx context.Context, env *Env) string {
	surface := executionSurface(connectorPlatformName(ctx, env))
	if surface == "unknown" {
		return "other"
	}
	return surface
}

var osFamilyNames = map[string]string{"darwin": "macos", "linux": "linux", "win32": "windows"}

// ConnectorUserAgent is the user agent every first party gateway request carries, failing when the install stamp does
func ConnectorUserAgent(ctx context.Context, env *Env) (string, error) {
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return "", err
	}
	osName := osFamilyNames[hostinfo.OSFamily()]
	if osName == "" {
		osName = "unknown"
	}
	return fmt.Sprintf("icebath-agent/%s (%s; %s)", info.BaseVersion, requestSurface(ctx, env), osName), nil
}

func conversationID(ctx context.Context, env *Env) string {
	if ConnectorConversationContext == nil {
		panic("tools.ConnectorConversationContext is unassigned, pkg/mcp installs it at startup")
	}
	if v := ConnectorConversationContext(ctx); v != "" {
		return v
	}
	return sessionValue(env, "id", "ICEBATH_SESSION_ID")
}

// CorrelationHeaders names the conversation, turn, and tool call once telemetry is consented
func CorrelationHeaders(ctx context.Context, env *Env, call *CallRef) (map[string]string, error) {
	if TelemetrySendConsented == nil {
		panic("tools.TelemetrySendConsented is unassigned, pkg/mcp installs it at startup")
	}
	if consented, err := TelemetrySendConsented(ctx, env); err != nil || !consented {
		return map[string]string{}, err
	}
	headers := map[string]string{requestIDHeader: uuid.NewString()}
	if v := conversationID(ctx, env); v != "" {
		headers[sessionHeader] = v
	}
	if call != nil {
		if call.TurnID != "" {
			headers[turnHeader] = call.TurnID
		}
		if call.ToolCallID != "" {
			headers[toolCallHeader] = call.ToolCallID
		}
	}
	return headers, nil
}

func connectorHeaders(ctx context.Context, env *Env, call *CallRef) (map[string]string, error) {
	agent, err := ConnectorUserAgent(ctx, env)
	if err != nil {
		return nil, err
	}
	headers, err := CorrelationHeaders(ctx, env, call)
	if err != nil {
		return nil, err
	}
	headers["User-Agent"] = agent
	return headers, nil
}

func responseRequestID(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	if v := resp.Header.Get(requestIDHeader); v != "" {
		return v
	}
	return resp.Header.Get("X-Request-Id")
}

var expectedGatewayMisses = map[string]bool{"connection_not_found": true}

func logConnectorOutcome(env *Env, route string, status int, hasStatus bool, code, requestID string, call *CallRef) {
	statusText := "transport"
	if hasStatus {
		statusText = strconv.Itoa(status)
	}
	dispatch := "-"
	if call != nil && call.ToolCallID != "" {
		dispatch = call.ToolCallID
	}
	if code == "" {
		code = "-"
	}
	if requestID == "" {
		requestID = "-"
	}
	logger := envLog(env)
	if code != "-" && !expectedGatewayMisses[code] {
		logger.Warn("connector outcome", "route", route, "status", statusText, "code", code, "request_id", requestID, "dispatch", dispatch)
		return
	}
	logger.Debug("connector outcome", "route", route, "status", statusText, "code", code, "request_id", requestID, "dispatch", dispatch)
}

// ---- auth ---------------------------------------------------------------------------------------

// ManagedGatewayOrigin is the first party origin for on origin vendors and media uploads
func ManagedGatewayOrigin() (string, error) { return BuildVendorGatewayURL("tool") }

// ConnectorGatewayOrigin is the separate connector deployment honoring CONNECTOR_GATEWAY_URL
func ConnectorGatewayOrigin() (string, error) { return BuildVendorGatewayURL("connector") }

// IsManagedNousGatewayURL reports whether a URL sits exactly on one of the first party origins
func IsManagedNousGatewayURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	actual, err := url.Parse(raw)
	if err != nil || actual.Scheme == "" {
		return false
	}
	for _, label := range []string{"tool", "connector"} {
		origin, err := BuildVendorGatewayURL(label)
		if err != nil {
			return false
		}
		expected, err := url.Parse(origin)
		if err != nil {
			return false
		}
		if actual.Scheme == expected.Scheme && actual.Host == expected.Host {
			return true
		}
	}
	return false
}

// ManagedGatewayAuthHeaders returns live auth headers for a managed gateway URL or none
func ManagedGatewayAuthHeaders(ctx context.Context, env *Env, raw string) (map[string]string, error) {
	if !IsManagedNousGatewayURL(raw) {
		return map[string]string{}, nil
	}
	token := readNousAccessToken(env)
	if token == "" {
		return map[string]string{}, nil
	}
	agent, err := ConnectorUserAgent(ctx, env)
	if err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + token, "User-Agent": agent}, nil
}

// ---- config -------------------------------------------------------------------------------------

// MaxCallsPerDispatch bounds one connector batch
const MaxCallsPerDispatch = 10

var falseStrings = map[string]bool{"false": true, "0": true, "no": true, "off": true, "": true}

// ConnectorConfig is the tools.connectors config gate
type ConnectorConfig struct{ Enabled bool }

func coerceConnectorBool(v any, fallback bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case nil:
		return fallback
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case string:
		return !falseStrings[strings.ToLower(strings.TrimSpace(t))]
	}
	return fallback
}

// Config of a raw tools.connectors value, a bool or a mapping, anything malformed falling back to enabled
func connectorConfigFromRaw(raw any) ConnectorConfig {
	switch v := raw.(type) {
	case bool:
		return ConnectorConfig{Enabled: v}
	case map[string]any:
		return ConnectorConfig{Enabled: coerceConnectorBool(v["enabled"], true)}
	}
	return ConnectorConfig{Enabled: true}
}

// LoadConnectorConfig reads tools.connectors, malformed values falling back to enabled
func LoadConnectorConfig(env *Env) ConnectorConfig {
	return connectorConfigFromRaw(cfgTree(env).Get("tools.connectors"))
}

// Reports whether a Nous identity exists and whether it is the guest tier
func nousIdentityState(env *Env) (hasIdentity, isGuest bool) {
	if NousAccount == nil {
		return false, false
	}
	return NousAccount.IdentityState(env)
}

// Reports whether no identity exists yet and a guest one may still be minted
func guestIdentityPending(env *Env) bool {
	if NousAccount == nil {
		return false
	}
	hasIdentity, _ := nousIdentityState(env)
	if !NousAccount.GuestAllowed(env) || hasIdentity || peekNousAccessToken(env) != "" {
		return false
	}
	failure, ok := NousAccount.LastMintFailure(env)
	return !ok || failure.Retryable
}

// ConnectorsAvailable is the one availability gate, failing closed on every error
func ConnectorsAvailable(env *Env) bool {
	if !LoadConnectorConfig(env).Enabled {
		return false
	}
	if NousAccount == nil {
		return false
	}
	_, isGuest := nousIdentityState(env)
	if isGuest || guestIdentityPending(env) {
		return true
	}
	return NousAccount.ManagedToolsRolledOut(env)
}

const guestSetupFailedPrefix = "Hosted connectors could not be set up. "

// The last failed guest creation worded with the wait still left, empty when none
func guestSetupFailure(env *Env) string {
	if NousAccount == nil {
		return ""
	}
	failure, ok := NousAccount.LastMintFailure(env)
	if !ok {
		return ""
	}
	return guestSetupFailedPrefix + NousAccount.AnonFailureCopy(env, failure.ErrorCode, failure.RetryAfter)
}

// Creates the guest identity on the first hosted connector action, empty on success
func ensureGuestIdentity(env *Env) string {
	if !guestIdentityPending(env) {
		return ""
	}
	if err := NousAccount.EnsurePortalIdentity(env); err != nil {
		var ae *AuthHookError
		if errors.As(err, &ae) {
			return guestSetupFailedPrefix + ae.Error()
		}
		if msg := guestSetupFailure(env); msg != "" {
			return msg
		}
		return guestSetupFailedPrefix + "Try again shortly."
	}
	return ""
}

// AuthHookError is a Portal auth refusal the account source reports with a code
type AuthHookError struct {
	Msg  string
	Code string
}

func (e *AuthHookError) Error() string { return e.Msg }

// MintFailure records why the last guest identity mint failed
type MintFailure struct {
	ErrorCode  string
	RetryAfter float64
	Retryable  bool
}

// The key an operation is registered under, the gateway session key else the agent session id
func operationSessionKey(env *Env, sessionID string) string {
	if key := sessionValue(env, "key", "ICEBATH_SESSION_KEY"); key != "" {
		return key
	}
	return sessionID
}

// ---- client -------------------------------------------------------------------------------------

const (
	connectorDefaultTimeout = 30 * time.Second
	connectorExecuteTimeout = 60 * time.Second
	connectorSearchTimeout  = 30 * time.Second
	connectorSchemasTimeout = 10 * time.Second
	connectorStorageTimeout = 60 * time.Second
	storageChunkBytes       = 64 * 1024
	storagePutRoute         = "connectors.storage.put"
	storageGetRoute         = "connectors.storage.get"
	connectorMaxRetries     = 1
	maxRetryAfterSeconds    = 10.0
	executeRoute            = "connectors.execute"
)

var retryOnceStatuses = map[int]bool{502: true, 503: true, 504: true}

var legacyExecuteOrigins = struct {
	sync.Mutex
	set map[string]bool
}{set: map[string]bool{}}

// A 2xx answer before its body is checked, the outcome line written once the body is read
type connectorReply struct {
	payload   any
	status    int
	requestID string
	route     string
}

// Plain HTTP to presigned storage URLs that never carries the gateway auth headers
type StorageTransport interface {
	Do(req *http.Request) (*http.Response, error)
}

// ConnectorClient talks to the managed gateway connector routes
type ConnectorClient struct {
	env       *Env
	http      *http.Client
	storage   StorageTransport
	call      *CallRef
	initiator string
	sleep     func(time.Duration)
	origin    func() string
	headers   func(ctx context.Context, u string) (map[string]string, error)
}

// Seams a connector client takes, each one left unset taking its production default
type ConnectorClientOptions struct {
	Transport        *http.Client
	EndpointResolver func() string
	HeaderProvider   func(ctx context.Context, u string) (map[string]string, error)
	Call             *CallRef
	Initiator        string
	Sleep            func(time.Duration)
	Storage          StorageTransport
}

// Connector deployment origin resolved directly, empty when the configured gateway scheme is invalid
func defaultEndpointResolver() string {
	origin, err := ConnectorGatewayOrigin()
	if err != nil {
		return ""
	}
	return origin
}

// Live managed gateway auth headers for a connector route URL
func defaultHeaderProvider(ctx context.Context, env *Env, u string) (map[string]string, error) {
	return ManagedGatewayAuthHeaders(ctx, env, u)
}

// Client over the given seams, the model call it serves and who asked for a connect both going on the wire
func NewConnectorClientFrom(env *Env, o ConnectorClientOptions) *ConnectorClient {
	c := &ConnectorClient{env: env, http: o.Transport, storage: o.Storage, origin: o.EndpointResolver, headers: o.HeaderProvider,
		call: o.Call, initiator: o.Initiator, sleep: o.Sleep}
	if c.http == nil {
		c.http = webHTTPClient(0)
	}
	if c.storage == nil {
		c.storage = webHTTPClient(0)
	}
	if c.origin == nil {
		c.origin = defaultEndpointResolver
	}
	if c.headers == nil {
		c.headers = func(ctx context.Context, u string) (map[string]string, error) {
			return defaultHeaderProvider(ctx, env, u)
		}
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
	return c
}

// NewConnectorClient builds the real client for a user or model action
func NewConnectorClient(env *Env, call *CallRef, initiator string) *ConnectorClient {
	return NewConnectorClientFrom(env, ConnectorClientOptions{Call: call, Initiator: initiator})
}

// Client over the given HTTP client, origin and auth header provider, the transport and endpoint seams of the gateway client
func NewConnectorClientWith(env *Env, call *CallRef, initiator string, client *http.Client, origin func() (string, error), headers func(string) map[string]string) *ConnectorClient {
	return NewConnectorClientFrom(env, ConnectorClientOptions{Transport: client, Storage: client, Call: call, Initiator: initiator,
		EndpointResolver: func() string {
			o, err := origin()
			if err != nil {
				return ""
			}
			return o
		},
		HeaderProvider: func(_ context.Context, u string) (map[string]string, error) { return headers(u), nil }})
}

func (c *ConnectorClient) url(path string) (string, error) {
	origin := c.origin()
	if origin == "" {
		return "", newToolGatewayError(gatewayKindUnavailable, "no tool gateway origin resolves", "NO_ORIGIN", 0, "", false)
	}
	return strings.TrimRight(origin, "/") + "/" + path, nil
}

func routeKey(path string) string {
	parts := strings.Split(strings.Trim(strings.SplitN(path, "?", 2)[0], "/"), "/")
	if len(parts) > 1 {
		parts = parts[1:]
	} else {
		parts = nil
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	if len(parts) == 0 {
		return path
	}
	return strings.Join(parts, ".")
}

func (c *ConnectorClient) attemptHeaders(ctx context.Context, u, idempotencyKey string) (map[string]string, error) {
	auth, err := c.headers(ctx, u)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range auth {
		headers[k] = v
	}
	if len(headers) == 0 {
		return nil, newToolGatewayError(gatewayKindAuth, "no portal access token available", "NO_TOKEN", 401, "", false)
	}
	identity, err := connectorHeaders(ctx, c.env, c.call)
	if err != nil {
		return nil, err
	}
	for k, v := range identity {
		headers[k] = v
	}
	headers["Content-Type"] = "application/json"
	if idempotencyKey != "" {
		headers["x-idempotency-key"] = idempotencyKey
	}
	return headers, nil
}

type connectorHTTPResponse struct {
	status    int
	header    http.Header
	body      []byte
	requestID string
}

func (c *ConnectorClient) send(ctx context.Context, method, u string, body any, timeout time.Duration, idempotencyKey string, retryTransport bool) (*connectorHTTPResponse, error) {
	headers, err := c.attemptHeaders(ctx, u, idempotencyKey)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		text, err := pyjson.Dumps(body, pyjson.AllowNaN(false))
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader([]byte(text))
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, u, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		logConnectorOutcome(c.env, routeKey(strings.SplitN(u, "/", 4)[len(strings.SplitN(u, "/", 4))-1]), 0, false, "TRANSPORT_ERROR", "", c.call)
		if retryTransport {
			return nil, nil
		}
		return nil, newToolGatewayError("", "transport failure: "+err.Error(), "TRANSPORT_ERROR", 0, "", true)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return &connectorHTTPResponse{status: resp.StatusCode, header: resp.Header, body: data, requestID: responseRequestID(resp)}, nil
}

func safeJSON(r *connectorHTTPResponse) any {
	v, err := pyjson.LoadsBytes(r.body)
	if err != nil {
		return string(r.body)
	}
	return pyjson.WTF8Shape(v)
}

// Body as json.loads returns it, ordered objects and Python number types, nil when it is not JSON
func typedJSON(r *connectorHTTPResponse) any {
	v, err := pyjson.LoadsBytes(r.body)
	if err != nil {
		return nil
	}
	return v
}

var modelEntryKeys = []string{"index", "name", "response", "error", "account", "data_use"}
var modelErrorKeys = []string{"code", "message", "connector", "connect_card_available", "connect_url", "hint", "connection_id", "family"}

// Mapping with the named keys first in that order and every other key after them sorted
func orderedObject(m map[string]any, order ...string) *pyjson.Object {
	o := pyjson.NewObject()
	for _, k := range order {
		if v, ok := m[k]; ok {
			o.Set(k, v)
		}
	}
	for _, k := range sortedConnectorKeys(m) {
		if _, done := o.Get(k); !done {
			o.Set(k, m[k])
		}
	}
	return o
}

// Batch entry and its error payload in the key order hermes builds them
func modelEntry(entry map[string]any) *pyjson.Object {
	o := orderedObject(entry, modelEntryKeys...)
	if e, ok := entry["error"].(map[string]any); ok {
		o.Set("error", orderedObject(e, modelErrorKeys...))
	}
	return o
}

func modelEntries(entries []map[string]any) []*pyjson.Object {
	out := make([]*pyjson.Object, 0, len(entries))
	for _, e := range entries {
		out = append(out, modelEntry(e))
	}
	return out
}

func (c *ConnectorClient) request(ctx context.Context, method, path string, body any, timeout time.Duration, idempotencyKey string, retries int) (*connectorReply, error) {
	u, err := c.url(path)
	if err != nil {
		return nil, err
	}
	route := routeKey(path)
	for attempt := 0; attempt <= retries; attempt++ {
		resp, err := c.send(ctx, method, u, body, timeout, idempotencyKey, attempt < retries)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			continue
		}
		if resp.status >= 200 && resp.status < 300 {
			return &connectorReply{payload: typedJSON(resp), status: resp.status, requestID: resp.requestID, route: route}, nil
		}
		gerr, err := ParseGatewayError(resp.status, safeJSON(resp), resp.header)
		if err != nil {
			return nil, err
		}
		if gerr.RequestID == "" {
			gerr.RequestID = resp.requestID
		}
		logConnectorOutcome(c.env, route, resp.status, true, gerr.Code, gerr.RequestID, c.call)
		if gerr.Retryable && attempt < retries {
			envLog(c.env).Debug("Connector request retrying with same key", "path", path, "attempt", attempt+1, "status", resp.status)
			continue
		}
		return nil, gerr
	}
	return nil, newToolGatewayError("", "connector request retries exhausted", "TRANSPORT_ERROR", 0, "", true)
}

func (c *ConnectorClient) post(ctx context.Context, path string, body any, timeout time.Duration, idempotencyKey string, retries int) (*connectorReply, error) {
	return c.request(ctx, "POST", path, body, timeout, idempotencyKey, retries)
}

func (c *ConnectorClient) log(reply *connectorReply, code string) {
	logConnectorOutcome(c.env, reply.route, reply.status, true, code, reply.requestID, c.call)
}

// A 2xx body validated by the wire model, any other body or a validation failure an INVALID_RESPONSE error
func parseWire(model *pydantic.Model, payload any, what string) (*pydantic.Instance, *ToolGatewayError) {
	o, ok := payload.(*pyjson.Object)
	if !ok {
		return nil, newToolGatewayError("", "invalid "+what, "INVALID_RESPONSE", 0, "", false)
	}
	if _, has := o.Get("error"); has {
		return nil, newToolGatewayError("", "invalid "+what, "INVALID_RESPONSE", 0, "", false)
	}
	inst, verr := model.Validate(o)
	if verr != nil {
		first := verr.Errors()[0]
		return nil, newToolGatewayError("", "invalid "+what+": "+first.LocText()+": "+first.Msg, "INVALID_RESPONSE", 0, "", false)
	}
	return inst, nil
}

// Parses a 2xx body and writes its outcome line, one failure line per failed call of an execute envelope
func (c *ConnectorClient) checked(model *pydantic.Model, reply *connectorReply, what string) (*pydantic.Instance, error) {
	parsed, gerr := parseWire(model, reply.payload, what)
	if gerr != nil {
		gerr.Status, gerr.RequestID = reply.status, reply.requestID
		c.log(reply, gerr.Code)
		return nil, gerr
	}
	if model != ConnectorExecuteResponse {
		c.log(reply, "")
		return parsed, nil
	}
	var failed []string
	for _, result := range parsed.Get("results").([]any) {
		if e, ok := result.(*pydantic.Instance).Get("error").(*pydantic.Instance); ok {
			failed = append(failed, e.Get("code").(string))
		}
	}
	if len(failed) == 0 {
		failed = []string{""}
	}
	for _, code := range failed {
		c.log(reply, code)
	}
	return parsed, nil
}

// Search runs connector use case queries
func (c *ConnectorClient) Search(ctx context.Context, queries []map[string]any) (map[string]any, error) {
	wireQueries := make([]any, 0, len(queries))
	for _, q := range queries {
		query, verr := ConnectorSearchQuery.Validate(q)
		if verr != nil {
			return nil, verr
		}
		wireQueries = append(wireQueries, query)
	}
	body, err := wireBody(ConnectorSearchRequest, pyjson.ObjectOf("queries", wireQueries))
	if err != nil {
		return nil, err
	}
	reply, err := c.post(ctx, connectorSearchPath, body, connectorSearchTimeout, "", 0)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(ConnectorSearchResponse, reply, "connector search")
	if err != nil {
		return nil, err
	}
	return wireDump(parsed, false), nil
}

// Schemas fetches the input schemas of vendor slugs
func (c *ConnectorClient) Schemas(ctx context.Context, tools []string) (map[string]any, error) {
	inst, verr := ConnectorSchemasRequest.Validate(pyjson.ObjectOf("tools", wireStrings(tools)))
	if verr != nil {
		return nil, verr
	}
	body, _ := inst.Dump(pydantic.DumpOptions{ByAlias: true}).(*pyjson.Object)
	reply, err := c.post(ctx, connectorSchemasPath, body, connectorSchemasTimeout, "", connectorMaxRetries)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(ConnectorSchemasResponse, reply, "connector schemas")
	if err != nil {
		return nil, err
	}
	return wireDump(parsed, false), nil
}

// ConnectionsRequest describes one authorization start
type ConnectionsRequest struct {
	Connectors   []string
	Reinitiate   bool
	Alias        string
	ConnectionID string
	ReturnTo     string
	Op           string
}

// Connections starts authorizations, never retried because the gateway cannot deduplicate them
func (c *ConnectorClient) Connections(ctx context.Context, req ConnectionsRequest) (map[string]any, error) {
	body, err := wireBody(ConnectorConnectionsRequest, pyjson.ObjectOf("connectors", wireStrings(req.Connectors), "reinitiate", req.Reinitiate,
		"alias", wireOptional(req.Alias), "connection_id", wireOptional(req.ConnectionID), "return_to", wireOptional(req.ReturnTo),
		"op", wireOptional(req.Op), "initiator", wireOptional(c.initiator)))
	if err != nil {
		return nil, err
	}
	reply, err := c.post(ctx, connectorConnectionsPath, body, connectorDefaultTimeout, "", 0)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(ConnectorConnectionsResponse, reply, "connector connections")
	if err != nil {
		return nil, err
	}
	return wireDump(parsed, false), nil
}

// ListConnectors reads every page of the session's toolkit list
func (c *ConnectorClient) ListConnectors(ctx context.Context, timeout time.Duration) ([]map[string]any, error) {
	if timeout <= 0 {
		timeout = connectorDefaultTimeout
	}
	var items []map[string]any
	cursor := ""
	for i := 0; i < 20; i++ {
		path := connectorsPath + "?limit=50"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		reply, err := c.request(ctx, "GET", path, nil, timeout, "", connectorMaxRetries)
		if err != nil {
			return nil, err
		}
		page, err := c.checked(ConnectorListResponse, reply, "connector list page")
		if err != nil {
			return nil, err
		}
		for _, item := range page.Get("items").([]any) {
			items = append(items, wireDump(item.(*pydantic.Instance), true))
		}
		cursor, _ = page.Get("next_cursor").(string)
		if cursor == "" {
			return items, nil
		}
	}
	return nil, newToolGatewayError("", "connector list pagination incomplete", "INVALID_RESPONSE", 0, "", false)
}

// AccountStatus reads one account row, nil when the gateway no longer knows it
func (c *ConnectorClient) AccountStatus(ctx context.Context, connectionID string, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = connectorDefaultTimeout
	}
	reply, err := c.request(ctx, "GET", connectorAccountsPath+"/"+url.PathEscape(connectionID), nil, timeout, "", connectorMaxRetries)
	if err != nil {
		if ge, ok := gatewayErrorKind(err, gatewayKindUnavailable); ok && ge.Code == "connection_not_found" {
			return nil, nil
		}
		return nil, err
	}
	parsed, err := c.checked(ConnectorAccount, reply, "connector account")
	if err != nil {
		return nil, err
	}
	return wireDump(parsed, true), nil
}

// StageFile mints a presigned upload for one file input
func (c *ConnectorClient) StageFile(ctx context.Context, toolkit, tool, filename, mimetype string, size int) (map[string]any, error) {
	inst, verr := ConnectorFileUploadRequest.Validate(pyjson.ObjectOf("toolkit", toolkit, "tool", tool, "filename", filename, "mimetype", mimetype, "size", size))
	if verr != nil {
		return nil, verr
	}
	body, _ := inst.Dump(pydantic.DumpOptions{ByAlias: true}).(*pyjson.Object)
	reply, err := c.post(ctx, connectorFileUploadPath, body, connectorDefaultTimeout, "", 0)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(ConnectorFileUploadResponse, reply, "connector file upload")
	if err != nil {
		return nil, err
	}
	return wireDump(parsed, false), nil
}

// PutFile uploads bytes to a presigned storage URL with exactly the returned headers
func (c *ConnectorClient) PutFile(ctx context.Context, u string, headers map[string]string, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, connectorStorageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "PUT", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.storage.Do(req)
	if err != nil {
		logConnectorOutcome(c.env, storagePutRoute, 0, false, "TRANSPORT_ERROR", "", c.call)
		return newToolGatewayError("", "storage upload failed: "+errorTypeName(err), "TRANSPORT_ERROR", 0, "", false)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	failed := resp.StatusCode < 200 || resp.StatusCode >= 300
	code := ""
	if failed {
		code = "HTTP_" + strconv.Itoa(resp.StatusCode)
	}
	logConnectorOutcome(c.env, storagePutRoute, resp.StatusCode, true, code, "", c.call)
	if failed {
		return newToolGatewayError("", fmt.Sprintf("storage upload answered %d", resp.StatusCode), code, resp.StatusCode, "", false)
	}
	return nil
}

func errorTypeName(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "Timeout"
		}
		return "ConnectionError"
	}
	return "RequestException"
}

// GetFile streams a presigned storage GET into the sink, refusing more than maxBytes
func (c *ConnectorClient) GetFile(ctx context.Context, u string, sink func([]byte) error, maxBytes int64) error {
	reqCtx, cancel := context.WithTimeout(ctx, connectorStorageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := c.storage.Do(req)
	if err != nil {
		logConnectorOutcome(c.env, storageGetRoute, 0, false, "TRANSPORT_ERROR", "", c.call)
		return newToolGatewayError("", "download failed: "+errorTypeName(err), "TRANSPORT_ERROR", 0, "", false)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "HTTP_" + strconv.Itoa(resp.StatusCode)
		logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, code, "", c.call)
		return newToolGatewayError("", fmt.Sprintf("download answered %d", resp.StatusCode), code, resp.StatusCode, "", false)
	}
	var received int64
	buf := make([]byte, storageChunkBytes)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			received += int64(n)
			if received > maxBytes {
				logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, "TOO_LARGE", "", c.call)
				return newToolGatewayError("", fmt.Sprintf("file is larger than %d MiB", maxBytes/(1024*1024)), "TOO_LARGE", resp.StatusCode, "", false)
			}
			if err := sink(buf[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, "TRANSPORT_ERROR", "", c.call)
			return newToolGatewayError("", "download failed: "+errorTypeName(readErr), "TRANSPORT_ERROR", 0, "", false)
		}
	}
	logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, "", "", c.call)
	return nil
}

// Plain result entry of a validated execute result, the account and data use guarantees only when present
func resultDict(result *pydantic.Instance) map[string]any {
	entry := map[string]any{"data": result.Get("data"), "error": nil}
	if result.PyTypeName() == ConnectorExecuteResult.Name {
		if e, ok := result.Get("error").(*pydantic.Instance); ok {
			entry["error"] = errorDict(e, 0, "")
		}
	}
	if a, _ := result.Get("account").(string); a != "" {
		entry["account"] = a
	}
	if du, ok := result.Get("data_use").(*pydantic.Instance); ok {
		if requires := du.Get("requires").([]any); len(requires) > 0 {
			entry["data_use"] = wireStrList(requires)
		}
	}
	return entry
}

// Plain error entry merge renders, the status and request id kept for logs and never shown to the model
func errorDict(failure *pydantic.Instance, status int, requestID string) map[string]any {
	code := failure.Get("code").(string)
	e := map[string]any{"code": code, "message": failure.Get("message")}
	family, _ := failure.Get("family").(string)
	if family = executeErrorFamily(code, family); family != "" {
		e["family"] = family
	}
	for _, key := range []string{"connector", "connect_url", "connection_id", "hint"} {
		if v, _ := failure.Get(key).(string); v != "" {
			e[key] = v
		}
	}
	if status != 0 {
		e["status"] = status
	}
	if failure.PyTypeName() == ConnectorExecuteFailure.Name {
		if rid, _ := failure.Get("request_id").(string); rid != "" {
			requestID = rid
		}
	}
	if requestID != "" {
		e["request_id"] = requestID
	}
	return e
}

// A single call failure body the gateway marks with a family or a code this client knows
func callFailure(payload any) *pydantic.Instance {
	o, ok := payload.(*pyjson.Object)
	if !ok {
		return nil
	}
	rawCode, _ := o.Get("code")
	code, ok := rawCode.(string)
	if !ok {
		return nil
	}
	if family, _ := o.Get("family"); family == nil {
		if _, known := executeErrorCodes[code]; !known {
			return nil
		}
	}
	withMessage := pyjson.ObjectOf("message", "The connector gateway reported an error.")
	for _, k := range o.Keys {
		withMessage.Set(k, o.Vals[k])
	}
	failure, verr := ConnectorExecuteFailure.Validate(withMessage)
	if verr != nil {
		return nil
	}
	return failure
}

// Reports whether an older gateway rejected the single call form by naming tools in its 400
func rejectsSingleCallForm(payload any) bool {
	m := asMap(payload)
	if m == nil {
		return false
	}
	for _, issue := range asList(m["issues"]) {
		if im := asMap(issue); im != nil {
			if path := asList(im["path"]); len(path) > 0 && asStr(path[0]) == "tools" {
				return true
			}
		}
	}
	envelope := asMap(m["error"])
	code := ""
	text := m
	if envelope != nil {
		code = asStr(envelope["code"])
		text = envelope
	} else {
		code = asStr(m["code"])
	}
	if code != "VALIDATION_ERROR" {
		return false
	}
	return strings.Contains(asStr(text["message"])+" "+asStr(text["details"]), "tools")
}

// Seconds to wait before the one retry, ok false when the status is not retried
func retryWait(status int, headers http.Header) (float64, bool) {
	if retryOnceStatuses[status] {
		return 0, true
	}
	if status != 429 {
		return 0, false
	}
	raw := ""
	if headers != nil {
		raw = headers.Get("Retry-After")
	}
	wait, err := strconv.ParseFloat(raw, 64)
	if err != nil || wait < 0 || wait > maxRetryAfterSeconds {
		return 0, false
	}
	return wait, true
}

func (c *ConnectorClient) executeOne(ctx context.Context, plan PlannedCall, returnTo, op string) (map[string]any, bool, error) {
	call, err := wireExecuteCall(plan)
	if err != nil {
		return nil, false, err
	}
	body, err := wireBody(ConnectorExecuteSingleRequest, pyjson.ObjectOf("call", call, "return_to", wireOptional(returnTo), "op", wireOptional(op)))
	if err != nil {
		return nil, false, err
	}
	u, err := c.url(connectorExecutePath)
	if err != nil {
		return nil, false, err
	}
	idempotencyKey := uuid.NewString()
	for attempt := 0; attempt <= connectorMaxRetries; attempt++ {
		resp, err := c.send(ctx, "POST", u, body, connectorExecuteTimeout, idempotencyKey, attempt < connectorMaxRetries)
		if err != nil {
			return nil, false, err
		}
		if resp == nil {
			continue
		}
		payload := safeJSON(resp)
		if resp.status >= 200 && resp.status < 300 {
			reply := &connectorReply{payload: typedJSON(resp), status: resp.status, requestID: resp.requestID, route: executeRoute}
			parsed, err := c.checked(ConnectorExecuteSingleResponse, reply, "connector execute result")
			if err != nil {
				return nil, false, err
			}
			return resultDict(parsed.Get("result").(*pydantic.Instance)), true, nil
		}
		failure := callFailure(typedJSON(resp))
		code := ""
		if failure != nil {
			code = failure.Get("code").(string)
		} else {
			gerr, err := ParseGatewayError(resp.status, payload, resp.header)
			if err != nil {
				return nil, false, err
			}
			code = gerr.Code
		}
		rid := resp.requestID
		if failure != nil {
			if frid, _ := failure.Get("request_id").(string); frid != "" {
				rid = frid
			}
		}
		logConnectorOutcome(c.env, executeRoute, resp.status, true, code, rid, c.call)
		if resp.status == 400 && rejectsSingleCallForm(payload) {
			return nil, false, nil
		}
		if attempt < connectorMaxRetries {
			if wait, ok := retryWait(resp.status, resp.header); ok {
				envLog(c.env).Debug("Connector execute retrying with same key", "attempt", attempt+1, "status", resp.status)
				if wait > 0 {
					c.sleep(time.Duration(wait * float64(time.Second)))
				}
				continue
			}
		}
		if failure == nil {
			gerr, err := ParseGatewayError(resp.status, payload, resp.header)
			if err != nil {
				return nil, false, err
			}
			return nil, false, gerr
		}
		return map[string]any{"data": nil, "error": errorDict(failure, resp.status, resp.requestID)}, true, nil
	}
	return nil, false, newToolGatewayError("", "connector execute retries exhausted", "TRANSPORT_ERROR", 0, "", true)
}

func (c *ConnectorClient) executeLegacy(ctx context.Context, planned []PlannedCall, returnTo, op string) ([]map[string]any, error) {
	tools := make([]any, 0, len(planned))
	for _, plan := range planned {
		call, err := wireExecuteCall(plan)
		if err != nil {
			return nil, err
		}
		tools = append(tools, call)
	}
	body, err := wireBody(ConnectorExecuteRequest, pyjson.ObjectOf("tools", tools, "return_to", wireOptional(returnTo), "op", wireOptional(op)))
	if err != nil {
		return nil, err
	}
	reply, err := c.post(ctx, connectorExecutePath, body, connectorExecuteTimeout, uuid.NewString(), connectorMaxRetries)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(ConnectorExecuteResponse, reply, "connector execute response")
	if err != nil {
		return nil, err
	}
	results := parsed.Get("results").([]any)
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		out = append(out, resultDict(r.(*pydantic.Instance)))
	}
	return out, nil
}

// Execute runs planned calls one request each, falling back to the array form on older gateways
func (c *ConnectorClient) Execute(ctx context.Context, planned []PlannedCall, returnTo, op string) ([]map[string]any, error) {
	origin := c.origin()
	legacyExecuteOrigins.Lock()
	legacy := origin != "" && legacyExecuteOrigins.set[origin]
	legacyExecuteOrigins.Unlock()
	if legacy {
		return c.executeLegacy(ctx, planned, returnTo, op)
	}
	var results []map[string]any
	for index, plan := range planned {
		result, ok, err := c.executeOne(ctx, plan, returnTo, op)
		if err != nil {
			return nil, err
		}
		if !ok {
			if origin != "" {
				legacyExecuteOrigins.Lock()
				legacyExecuteOrigins.set[origin] = true
				legacyExecuteOrigins.Unlock()
			}
			envLog(c.env).Info("Connector gateway predates single-call execute; using the array form")
			rest, err := c.executeLegacy(ctx, planned[index:], returnTo, op)
			if err != nil {
				return nil, err
			}
			return append(results, rest...), nil
		}
		results = append(results, result)
	}
	return results, nil
}

// ReturnToArgs are the returnTo and op fields only the desktop surface sends
func ReturnToArgs(env *Env, op string) (string, string) {
	if sessionPlatformName(env) != "desktop" {
		return "", ""
	}
	target := "hermes-desktop"
	if os.Getenv("ICEBATH_DESKTOP_DEV_SERVER") != "" {
		target = "hermes-desktop-dev"
	}
	return target, op
}

func sortedConnectorKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
