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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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

func gatewayErr(message, code string) *ToolGatewayError {
	return &ToolGatewayError{Message: message, Code: code}
}

// Reports whether an error is a gateway error of the given kind
func gatewayErrorKind(err error, kind string) (*ToolGatewayError, bool) {
	var ge *ToolGatewayError
	if errors.As(err, &ge) && ge.Kind == kind {
		return ge, true
	}
	return nil, false
}

// ParseGatewayError reads every gateway error envelope shape without failing on a malformed body
func ParseGatewayError(status int, body any, headers http.Header) *ToolGatewayError {
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
	e := &ToolGatewayError{Message: message, Code: code, Status: status, RequestID: requestID}
	switch {
	case status == 401 || status == 403:
		e.Kind = gatewayKindAuth
	case status == 404:
		e.Kind = gatewayKindUnavailable
	case status == 409:
		e.Kind = gatewayKindIdempotency
	case status == 429:
		e.Kind = gatewayKindRateLimited
		header := ""
		if headers != nil {
			header = headers.Get("Retry-After")
		}
		if header != "" {
			if f, err := strconv.ParseFloat(header, 64); err == nil {
				e.RetryAfter = f
			} else {
				e.RetryAfter = 1.0
			}
		} else if hasRetryAfterMs {
			e.RetryAfter = retryAfterMs / 1000.0
		} else {
			e.RetryAfter = 1.0
		}
	default:
		e.Retryable = status >= 500
	}
	return e
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

func wireRequire(m map[string]any, loc string, keys ...string) error {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return &wireError{loc + "." + k + ": Field required"}
		}
	}
	return nil
}

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

func wireToolSchema(slug string, raw any) (map[string]any, error) {
	m := asMap(raw)
	if m == nil {
		return nil, &wireError{"schemas." + slug + ": Input should be a valid dictionary"}
	}
	if err := wireRequire(m, "schemas."+slug, "connector", "tool"); err != nil {
		return nil, err
	}
	input := asMap(m["inputSchema"])
	if input == nil {
		input = map[string]any{}
	}
	return map[string]any{"connector": asStr(m["connector"]), "tool": asStr(m["tool"]), "description": asStr(m["description"]), "input_schema": input}, nil
}

func wireSchemasMap(raw any) (map[string]any, error) {
	out := map[string]any{}
	for slug, v := range asMap(raw) {
		s, err := wireToolSchema(slug, v)
		if err != nil {
			return nil, err
		}
		out[slug] = s
	}
	return out, nil
}

func wireSearchResponse(payload map[string]any) (map[string]any, error) {
	schemas, err := wireSchemasMap(payload["schemas"])
	if err != nil {
		return nil, err
	}
	results := []any{}
	for i, r := range asList(payload["results"]) {
		m := asMap(r)
		if m == nil {
			return nil, &wireError{fmt.Sprintf("results.%d: Input should be a valid dictionary", i)}
		}
		if err := wireRequire(m, fmt.Sprintf("results.%d", i), "index"); err != nil {
			return nil, err
		}
		row := map[string]any{"index": int(asFloat(m["index"])), "use_case": asStr(m["useCase"]), "tools": wireStrList(m["tools"]),
			"related_tools": wireStrList(m["relatedTools"]), "connectors": wireStrList(m["connectors"]), "guidance": m["guidance"],
			"plan_steps": m["planSteps"], "pitfalls": m["pitfalls"], "error": m["error"]}
		results = append(results, row)
	}
	connections := []any{}
	for i, c := range asList(payload["connections"]) {
		m := asMap(c)
		if m == nil {
			return nil, &wireError{fmt.Sprintf("connections.%d: Input should be a valid dictionary", i)}
		}
		if err := wireRequire(m, fmt.Sprintf("connections.%d", i), "connector"); err != nil {
			return nil, err
		}
		connections = append(connections, map[string]any{"connector": asStr(m["connector"]), "connected": truthy(m["connected"]), "description": asStr(m["description"])})
	}
	return map[string]any{"results": results, "schemas": schemas, "connections": connections, "next_steps": wireStrList(payload["nextSteps"])}, nil
}

func wireSchemasResponse(payload map[string]any) (map[string]any, error) {
	schemas, err := wireSchemasMap(payload["schemas"])
	if err != nil {
		return nil, err
	}
	suggestions := map[string]any{}
	for k, v := range asMap(payload["suggestions"]) {
		suggestions[k] = wireStrList(v)
	}
	return map[string]any{"schemas": schemas, "not_found": wireStrList(payload["notFound"]), "suggestions": suggestions}, nil
}

func wireToolError(raw any, loc string) (map[string]any, error) {
	m := asMap(raw)
	if m == nil {
		return nil, &wireError{loc + ": Input should be a valid dictionary"}
	}
	if err := wireRequire(m, loc, "code", "message"); err != nil {
		return nil, err
	}
	return map[string]any{"code": asStr(m["code"]), "message": asStr(m["message"]), "connector": m["connector"], "connect_url": m["connectUrl"],
		"connection_id": m["connectionId"], "hint": m["hint"], "family": m["family"], "request_id": m["requestId"]}, nil
}

func wireDataUse(raw any) []string {
	m := asMap(raw)
	if m == nil {
		return nil
	}
	return wireStrList(m["requires"])
}

func wireExecuteSingleResponse(payload map[string]any) (map[string]any, error) {
	if err := wireRequire(payload, "", "result"); err != nil {
		return nil, &wireError{"result: Field required"}
	}
	r := asMap(payload["result"])
	if r == nil {
		return nil, &wireError{"result: Input should be a valid dictionary"}
	}
	return map[string]any{"connector": asStr(r["connector"]), "tool": asStr(r["tool"]), "data": r["data"], "account": r["account"], "data_use": wireDataUse(r["dataUse"])}, nil
}

func wireExecuteResponse(payload map[string]any) ([]map[string]any, error) {
	var results []map[string]any
	for i, raw := range asList(payload["results"]) {
		m := asMap(raw)
		if m == nil {
			return nil, &wireError{fmt.Sprintf("results.%d: Input should be a valid dictionary", i)}
		}
		row := map[string]any{"index": int(asFloat(m["index"])), "connector": asStr(m["connector"]), "tool": asStr(m["tool"]), "data": m["data"], "account": m["account"], "data_use": wireDataUse(m["dataUse"])}
		if m["error"] != nil {
			e, err := wireToolError(m["error"], fmt.Sprintf("results.%d.error", i))
			if err != nil {
				return nil, err
			}
			row["error"] = e
		}
		results = append(results, row)
	}
	return results, nil
}

func wireConnectionsResponse(payload map[string]any) (map[string]any, error) {
	results := []any{}
	for i, raw := range asList(payload["results"]) {
		m := asMap(raw)
		if m == nil {
			return nil, &wireError{fmt.Sprintf("results.%d: Input should be a valid dictionary", i)}
		}
		if err := wireRequire(m, fmt.Sprintf("results.%d", i), "connector", "status"); err != nil {
			return nil, err
		}
		status := asStr(m["status"])
		if status != "active" && status != "initiated" && status != "failed" {
			return nil, &wireError{fmt.Sprintf("results.%d.status: Input should be 'active', 'initiated' or 'failed'", i)}
		}
		results = append(results, map[string]any{"connector": asStr(m["connector"]), "status": status, "connect_url": m["connectUrl"], "connection_id": m["connectionId"],
			"alias": m["alias"], "instruction": m["instruction"], "status_reason": m["statusReason"], "reinitiated": truthy(m["reinitiated"])})
	}
	summary := asMap(payload["summary"])
	return map[string]any{"results": results, "summary": map[string]any{"total": int(asFloat(summary["total"])), "active": int(asFloat(summary["active"])),
		"initiated": int(asFloat(summary["initiated"])), "failed": int(asFloat(summary["failed"]))}}, nil
}

// Status values a gateway connection row may carry, the ConnectionStatus Literal
var ConnectionStatuses = []string{"pending", "active", "failed", "expired", "revoked", "inactive"}

func wireListResponse(payload map[string]any) ([]map[string]any, string, error) {
	if err := wireRequire(payload, "", "items"); err != nil {
		return nil, "", err
	}
	var items []map[string]any
	for i, raw := range asList(payload["items"]) {
		m := asMap(raw)
		if m == nil {
			return nil, "", &wireError{fmt.Sprintf("items.%d: Input should be a valid dictionary", i)}
		}
		if err := wireRequire(m, fmt.Sprintf("items.%d", i), "connector"); err != nil {
			return nil, "", err
		}
		enabled := true
		if v, ok := m["enabled"]; ok && v != nil {
			enabled = truthy(v)
		}
		item := map[string]any{"connector": asStr(m["connector"]), "enabled": enabled, "connected": truthy(m["connected"]), "connectionStatus": m["connectionStatus"],
			"statusReason": m["statusReason"], "disabledTools": wireStrList(m["disabledTools"])}
		items = append(items, item)
	}
	return items, asStr(payload["nextCursor"]), nil
}

func wireAccount(payload map[string]any) (map[string]any, error) {
	if err := wireRequire(payload, "", "connectionId", "connector", "status", "label", "active", "createdAt", "updatedAt"); err != nil {
		return nil, err
	}
	if !slices.Contains(ConnectionStatuses, asStr(payload["status"])) {
		return nil, &wireError{"status: Input should be a known connection status"}
	}
	if asStr(payload["label"]) == "" {
		return nil, &wireError{"label: String should have at least 1 character"}
	}
	return map[string]any{"connectionId": asStr(payload["connectionId"]), "connector": asStr(payload["connector"]), "status": asStr(payload["status"]),
		"statusReason": payload["statusReason"], "label": asStr(payload["label"]), "alias": payload["alias"], "active": truthy(payload["active"]),
		"disabled": truthy(payload["disabled"]), "createdAt": asStr(payload["createdAt"]), "updatedAt": asStr(payload["updatedAt"])}, nil
}

func wireFileUploadResponse(payload map[string]any) (map[string]any, error) {
	if err := wireRequire(payload, "", "uploadUrl", "descriptor", "expiresAt"); err != nil {
		return nil, err
	}
	d := asMap(payload["descriptor"])
	if d == nil {
		return nil, &wireError{"descriptor: Input should be a valid dictionary"}
	}
	if err := wireRequire(d, "descriptor", "name", "mimetype", "s3key"); err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range asMap(payload["headers"]) {
		headers[k] = asStr(v)
	}
	return map[string]any{"upload_url": asStr(payload["uploadUrl"]), "headers": headers,
		"descriptor": map[string]any{"name": asStr(d["name"]), "mimetype": asStr(d["mimetype"]), "s3key": asStr(d["s3key"])}, "expires_at": asStr(payload["expiresAt"])}, nil
}

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

// One call of an execute request body in the field order of the hermes wire model
func wireExecuteCall(plan PlannedCall) *pyjson.Object {
	var typed any
	if plan.Typed != nil {
		typed = plan.Typed
	}
	return pyjson.ObjectOf("connector", plan.Connector, "tool", plan.Tool, "arguments", wireTyped(plan.Arguments, typed))
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

// PartitionCalls treats malformed entries as per entry errors so sibling calls still run
func PartitionCalls(calls []any) Partition {
	var p Partition
	for position, raw := range calls {
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

// RenderRemoteEntry renders one gateway result into the batch entry the model reads
func RenderRemoteEntry(ctx context.Context, planned PlannedCall, remote map[string]any) map[string]any {
	account := optStr(remote["account"])
	withAccount := func(entry map[string]any) map[string]any {
		if account != "" {
			entry["account"] = account
		}
		return entry
	}
	errMap := asMap(remote["error"])
	if errMap == nil {
		entry := withAccount(map[string]any{"index": planned.Position, "name": planned.Name, "response": remote["data"]})
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
	return withAccount(map[string]any{"index": planned.Position, "name": planned.Name, "error": payload})
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
	case float64:
		return t != 0
	case string:
		return !falseStrings[strings.ToLower(strings.TrimSpace(t))]
	}
	return fallback
}

// LoadConnectorConfig reads tools.connectors, malformed values falling back to enabled
func LoadConnectorConfig(env *Env) ConnectorConfig {
	raw := cfgTree(env).Get("tools.connectors")
	switch v := raw.(type) {
	case bool:
		return ConnectorConfig{Enabled: v}
	case map[string]any:
		return ConnectorConfig{Enabled: coerceConnectorBool(v["enabled"], true)}
	}
	return ConnectorConfig{Enabled: true}
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

type connectorReply struct {
	payload   any
	typed     any
	status    int
	requestID string
	route     string
}

// ConnectorClient talks to the managed gateway connector routes
type ConnectorClient struct {
	env       *Env
	http      *http.Client
	storage   *http.Client
	call      *CallRef
	initiator string
	sleep     func(time.Duration)
	origin    func() (string, error)
	headers   func(ctx context.Context, u string) (map[string]string, error)
}

// NewConnectorClient builds the real client for a user or model action
func NewConnectorClient(env *Env, call *CallRef, initiator string) *ConnectorClient {
	c := &ConnectorClient{env: env, http: webHTTPClient(0), storage: webHTTPClient(0), call: call, initiator: initiator, sleep: time.Sleep}
	c.origin = func() (string, error) { return ConnectorGatewayOrigin() }
	c.headers = func(ctx context.Context, u string) (map[string]string, error) {
		return ManagedGatewayAuthHeaders(ctx, env, u)
	}
	return c
}

// Client over the given HTTP client, origin and auth header provider, the transport and endpoint seams of the gateway client
func NewConnectorClientWith(env *Env, call *CallRef, initiator string, client *http.Client, origin func() (string, error), headers func(string) map[string]string) *ConnectorClient {
	return &ConnectorClient{env: env, http: client, storage: client, call: call, initiator: initiator, sleep: time.Sleep, origin: origin,
		headers: func(_ context.Context, u string) (map[string]string, error) { return headers(u), nil }}
}

func (c *ConnectorClient) url(path string) (string, error) {
	origin, err := c.origin()
	if err != nil || origin == "" {
		return "", &ToolGatewayError{Message: "no tool gateway origin resolves", Code: "NO_ORIGIN", Kind: gatewayKindUnavailable}
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
		return nil, &ToolGatewayError{Message: "no portal access token available", Code: "NO_TOKEN", Status: 401, Kind: gatewayKindAuth}
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
		return nil, &ToolGatewayError{Message: "transport failure: " + err.Error(), Code: "TRANSPORT_ERROR", Retryable: true}
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

// Value under the keys of nested decoded objects, nil when a step is not an object or lacks the key
func typedAt(v any, keys ...string) any {
	for _, k := range keys {
		o, ok := v.(*pyjson.Object)
		if !ok {
			return nil
		}
		v = o.Vals[k]
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
			return &connectorReply{payload: safeJSON(resp), typed: typedJSON(resp), status: resp.status, requestID: resp.requestID, route: route}, nil
		}
		gerr := ParseGatewayError(resp.status, safeJSON(resp), resp.header)
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
	return nil, &ToolGatewayError{Message: "connector request retries exhausted", Code: "TRANSPORT_ERROR", Retryable: true}
}

func (c *ConnectorClient) post(ctx context.Context, path string, body any, timeout time.Duration, idempotencyKey string, retries int) (*connectorReply, error) {
	return c.request(ctx, "POST", path, body, timeout, idempotencyKey, retries)
}

func (c *ConnectorClient) log(reply *connectorReply, code string) {
	logConnectorOutcome(c.env, reply.route, reply.status, true, code, reply.requestID, c.call)
}

// Parses a 2xx body with the given wire reader, logging the outcome once
func (c *ConnectorClient) checked(reply *connectorReply, what string, parse func(map[string]any) (any, error)) (any, error) {
	m := asMap(reply.payload)
	if m == nil {
		c.log(reply, "INVALID_RESPONSE")
		return nil, &ToolGatewayError{Message: "invalid " + what, Code: "INVALID_RESPONSE", Status: reply.status, RequestID: reply.requestID}
	}
	if _, has := m["error"]; has {
		c.log(reply, "INVALID_RESPONSE")
		return nil, &ToolGatewayError{Message: "invalid " + what, Code: "INVALID_RESPONSE", Status: reply.status, RequestID: reply.requestID}
	}
	parsed, err := parse(m)
	if err != nil {
		c.log(reply, "INVALID_RESPONSE")
		return nil, &ToolGatewayError{Message: "invalid " + what + ": " + err.Error(), Code: "INVALID_RESPONSE", Status: reply.status, RequestID: reply.requestID}
	}
	c.log(reply, "")
	return parsed, nil
}

// Search runs connector use case queries
func (c *ConnectorClient) Search(ctx context.Context, queries []map[string]any) (map[string]any, error) {
	wireQueries := make([]any, 0, len(queries))
	for _, q := range queries {
		row := pyjson.ObjectOf("useCase", asStr(q["use_case"]))
		if kf := asStr(q["known_fields"]); kf != "" {
			row.Set("knownFields", kf)
		}
		wireQueries = append(wireQueries, row)
	}
	reply, err := c.post(ctx, connectorSearchPath, pyjson.ObjectOf("queries", wireQueries), connectorSearchTimeout, "", 0)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(reply, "connector search", func(m map[string]any) (any, error) { return wireSearchResponse(m) })
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
}

// Schemas fetches the input schemas of vendor slugs
func (c *ConnectorClient) Schemas(ctx context.Context, tools []string) (map[string]any, error) {
	reply, err := c.post(ctx, connectorSchemasPath, pyjson.ObjectOf("tools", wireStrings(tools)), connectorSchemasTimeout, "", connectorMaxRetries)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(reply, "connector schemas", func(m map[string]any) (any, error) { return wireSchemasResponse(m) })
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
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
	body := pyjson.ObjectOf("connectors", wireStrings(req.Connectors), "reinitiate", req.Reinitiate)
	if req.Alias != "" {
		body.Set("alias", req.Alias)
	}
	if req.ConnectionID != "" {
		body.Set("connectionId", req.ConnectionID)
	}
	if req.ReturnTo != "" {
		body.Set("returnTo", req.ReturnTo)
	}
	if req.Op != "" {
		body.Set("op", req.Op)
	}
	if c.initiator != "" {
		body.Set("initiator", c.initiator)
	}
	reply, err := c.post(ctx, connectorConnectionsPath, body, connectorDefaultTimeout, "", 0)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(reply, "connector connections", func(m map[string]any) (any, error) { return wireConnectionsResponse(m) })
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
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
		var next string
		parsed, err := c.checked(reply, "connector list page", func(m map[string]any) (any, error) {
			page, nextCursor, err := wireListResponse(m)
			next = nextCursor
			return page, err
		})
		if err != nil {
			return nil, err
		}
		items = append(items, parsed.([]map[string]any)...)
		cursor = next
		if cursor == "" {
			return items, nil
		}
	}
	return nil, &ToolGatewayError{Message: "connector list pagination incomplete", Code: "INVALID_RESPONSE"}
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
	parsed, err := c.checked(reply, "connector account", func(m map[string]any) (any, error) { return wireAccount(m) })
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
}

// StageFile mints a presigned upload for one file input
func (c *ConnectorClient) StageFile(ctx context.Context, toolkit, tool, filename, mimetype string, size int) (map[string]any, error) {
	body := pyjson.ObjectOf("toolkit", toolkit, "tool", tool, "filename", filename, "mimetype", mimetype, "size", size)
	reply, err := c.post(ctx, connectorFileUploadPath, body, connectorDefaultTimeout, "", 0)
	if err != nil {
		return nil, err
	}
	parsed, err := c.checked(reply, "connector file upload", func(m map[string]any) (any, error) { return wireFileUploadResponse(m) })
	if err != nil {
		return nil, err
	}
	return parsed.(map[string]any), nil
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
		return &ToolGatewayError{Message: "storage upload failed: " + errorTypeName(err), Code: "TRANSPORT_ERROR"}
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
		return &ToolGatewayError{Message: fmt.Sprintf("storage upload answered %d", resp.StatusCode), Code: code, Status: resp.StatusCode}
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
		return &ToolGatewayError{Message: "download failed: " + errorTypeName(err), Code: "TRANSPORT_ERROR"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "HTTP_" + strconv.Itoa(resp.StatusCode)
		logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, code, "", c.call)
		return &ToolGatewayError{Message: fmt.Sprintf("download answered %d", resp.StatusCode), Code: code, Status: resp.StatusCode}
	}
	var received int64
	buf := make([]byte, storageChunkBytes)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			received += int64(n)
			if received > maxBytes {
				logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, "TOO_LARGE", "", c.call)
				return &ToolGatewayError{Message: fmt.Sprintf("file is larger than %d MiB", maxBytes/(1024*1024)), Code: "TOO_LARGE", Status: resp.StatusCode}
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
			return &ToolGatewayError{Message: "download failed: " + errorTypeName(readErr), Code: "TRANSPORT_ERROR"}
		}
	}
	logConnectorOutcome(c.env, storageGetRoute, resp.StatusCode, true, "", "", c.call)
	return nil
}

func resultDict(parsed map[string]any) map[string]any {
	entry := map[string]any{"data": parsed["data"], "error": nil}
	if e := asMap(parsed["error"]); e != nil {
		entry["error"] = errorDict(e, 0, "")
	}
	if a := asStr(parsed["account"]); a != "" {
		entry["account"] = a
	}
	if du, ok := parsed["data_use"].([]string); ok && len(du) > 0 {
		entry["data_use"] = du
	}
	return entry
}

func errorDict(failure map[string]any, status int, requestID string) map[string]any {
	e := map[string]any{"code": asStr(failure["code"]), "message": asStr(failure["message"])}
	if family := executeErrorFamily(asStr(failure["code"]), asStr(failure["family"])); family != "" {
		e["family"] = family
	}
	for _, key := range []string{"connector", "connect_url", "connection_id", "hint"} {
		if v := asStr(failure[key]); v != "" {
			e[key] = v
		}
	}
	if status != 0 {
		e["status"] = status
	}
	if rid := asStr(failure["request_id"]); rid != "" {
		requestID = rid
	}
	if requestID != "" {
		e["request_id"] = requestID
	}
	return e
}

// A single call failure body the gateway marks with a family or a code this client knows
func callFailure(payload any) map[string]any {
	m := asMap(payload)
	if m == nil {
		return nil
	}
	code, ok := m["code"].(string)
	if !ok {
		return nil
	}
	if m["family"] == nil {
		if _, known := executeErrorCodes[code]; !known {
			return nil
		}
	}
	withMessage := map[string]any{"message": "The connector gateway reported an error."}
	for k, v := range m {
		withMessage[k] = v
	}
	e, err := wireToolError(withMessage, "")
	if err != nil {
		return nil
	}
	return e
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
	body := pyjson.ObjectOf("call", wireExecuteCall(plan))
	if returnTo != "" {
		body.Set("returnTo", returnTo)
	}
	if op != "" {
		body.Set("op", op)
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
			reply := &connectorReply{payload: payload, status: resp.status, requestID: resp.requestID, route: executeRoute}
			parsed, err := c.checked(reply, "connector execute result", func(m map[string]any) (any, error) { return wireExecuteSingleResponse(m) })
			if err != nil {
				return nil, false, err
			}
			result := resultDict(parsed.(map[string]any))
			result["data"] = typedAt(typedJSON(resp), "result", "data")
			return result, true, nil
		}
		failure := callFailure(payload)
		code := ""
		if failure != nil {
			code = asStr(failure["code"])
		} else {
			code = ParseGatewayError(resp.status, payload, resp.header).Code
		}
		rid := resp.requestID
		if failure != nil && asStr(failure["request_id"]) != "" {
			rid = asStr(failure["request_id"])
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
			return nil, false, ParseGatewayError(resp.status, payload, resp.header)
		}
		return map[string]any{"data": nil, "error": errorDict(failure, resp.status, resp.requestID)}, true, nil
	}
	return nil, false, &ToolGatewayError{Message: "connector execute retries exhausted", Code: "TRANSPORT_ERROR", Retryable: true}
}

func (c *ConnectorClient) executeLegacy(ctx context.Context, planned []PlannedCall, returnTo, op string) ([]map[string]any, error) {
	tools := make([]any, 0, len(planned))
	for _, plan := range planned {
		tools = append(tools, wireExecuteCall(plan))
	}
	body := pyjson.ObjectOf("tools", tools)
	if returnTo != "" {
		body.Set("returnTo", returnTo)
	}
	if op != "" {
		body.Set("op", op)
	}
	reply, err := c.post(ctx, connectorExecutePath, body, connectorExecuteTimeout, uuid.NewString(), connectorMaxRetries)
	if err != nil {
		return nil, err
	}
	m := asMap(reply.payload)
	if m == nil {
		c.log(reply, "INVALID_RESPONSE")
		return nil, &ToolGatewayError{Message: "invalid connector execute response", Code: "INVALID_RESPONSE", Status: reply.status, RequestID: reply.requestID}
	}
	if _, has := m["error"]; has {
		c.log(reply, "INVALID_RESPONSE")
		return nil, &ToolGatewayError{Message: "invalid connector execute response", Code: "INVALID_RESPONSE", Status: reply.status, RequestID: reply.requestID}
	}
	results, err := wireExecuteResponse(m)
	if err != nil {
		c.log(reply, "INVALID_RESPONSE")
		return nil, &ToolGatewayError{Message: "invalid connector execute response: " + err.Error(), Code: "INVALID_RESPONSE", Status: reply.status, RequestID: reply.requestID}
	}
	logged := false
	for _, r := range results {
		if e := asMap(r["error"]); e != nil {
			c.log(reply, asStr(e["code"]))
			logged = true
		}
	}
	if !logged {
		c.log(reply, "")
	}
	typedResults, _ := typedAt(reply.typed, "results").([]any)
	out := make([]map[string]any, 0, len(results))
	for i, r := range results {
		result := resultDict(r)
		if i < len(typedResults) {
			result["data"] = typedAt(typedResults[i], "data")
		}
		out = append(out, result)
	}
	return out, nil
}

// Execute runs planned calls one request each, falling back to the array form on older gateways
func (c *ConnectorClient) Execute(ctx context.Context, planned []PlannedCall, returnTo, op string) ([]map[string]any, error) {
	origin, _ := c.origin()
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
	target := "icebath-desktop"
	if os.Getenv("ICEBATH_DESKTOP_DEV_SERVER") != "" {
		target = "icebath-desktop-dev"
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
