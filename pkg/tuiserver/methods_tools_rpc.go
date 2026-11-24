package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// One required param with the test its value must pass
type requiredParam struct {
	key     string
	present func(v any) bool
}

// Body of a guarded RPC, answering a response or raising an error the guard maps to a refusal
type rpcBody func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)

// Shape of a guarded RPC, its required params, profile scope and failure prefix
type rpcShape struct {
	required         []requiredParam
	propagateResolve bool
	prefix           string
	unscoped         bool
}

// Whether a value reads as a non blank string
func strippedPresent(v any) bool { return strings.TrimSpace(strOr(v)) != "" }

// Whether a value is neither None nor the empty string
func nonemptyPresent(v any) bool { return v != nil && anyutil.String(v) != "" }

// Required name param of the mcp server RPCs
var nameParam = []requiredParam{{"name", strippedPresent}}

// Required name and session id params of the mcp oauth RPCs
var nameSessionParams = []requiredParam{{"name", strippedPresent}, {"session_id", strippedPresent}}

// Stripped text of a param with None and falsy values read as empty
func strArg(params map[string]any, key string) string { return strings.TrimSpace(strOr(params[key])) }

// Wraps a body in the optional profile runtime scope, its failures answered under the fail code
func profileScopedRPC(failCode int, shape rpcShape, body rpcBody) Handler {
	return func(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
		for _, req := range shape.required {
			if !req.present(params[req.key]) {
				return rpcErr(rid, 4063, req.key+" required", nil)
			}
		}
		if !shape.unscoped {
			profile := strArg(params, "profile")
			var scoped context.Context
			failure := catchPanic(func() {
				h, err := profileHome(profile)
				panicIf(err)
				scoped = profileRuntimeScope(ctx, h, true)
			})
			if failure != nil {
				var unavailable *ProfileUnavailableError
				if err, isErr := failure.(error); isErr && errors.As(err, &unavailable) {
					return rpcErr(rid, 4064, fmt.Sprintf("profile '%s' not found", profile), nil)
				}
				if shape.propagateResolve {
					panic(failure)
				}
				return rpcErr(rid, failCode, panicMessage(failure), nil)
			}
			ctx = scoped
		}
		defer func() {
			if r := recover(); r != nil {
				var unavailable *ProfileUnavailableError
				if err, isErr := r.(error); isErr && errors.As(err, &unavailable) {
					panic(r)
				}
				resp = rpcErr(rid, failCode, shape.prefix+panicMessage(r), nil)
			}
		}()
		out, err := body(ctx, rid, params)
		if err != nil {
			panic(err)
		}
		return out
	}
}

// Registers a guarded RPC with no profile scope
func guardedRPC(reg *handlerRegistry, name string, failCode int, shape rpcShape, body rpcBody) {
	shape.unscoped = true
	reg.method(name, profileScopedRPC(failCode, shape, body))
}

// Registers an RPC in the optional profile scope of its params
func scopedRPC(reg *handlerRegistry, name string, failCode int, shape rpcShape, body rpcBody) {
	reg.method(name, profileScopedRPC(failCode, shape, body))
}

// Registers an mcp.servers RPC, profile scoped, its resolve failures left to dispatch and body failures 5024
func mcpRPC(reg *handlerRegistry, name string, required []requiredParam, body rpcBody) {
	scopedRPC(reg, "mcp.servers."+name, 5024, rpcShape{required: required, propagateResolve: true}, body)
}

var toolsRPCRegistry = &handlerRegistry{}

func init() {
	toolsRPCRegistry.method("system.battery", systemBattery)
	toolsRPCRegistry.install()
}

// Host battery for the status bar, unavailable on any read failure
func systemBattery(ctx context.Context, rid any, params map[string]any) map[string]any {
	out := tuigateway.SystemBatteryResult{Category: "dim"}
	if catchPanic(func() {
		batt := hostinfo.ReadBattery(true)
		out = tuigateway.SystemBatteryResult{Available: batt.Available, Plugged: batt.Plugged,
			Category: tuigateway.BatteryCategory(hostinfo.BatteryCategory(batt))}
		if batt.Percent != nil {
			percent := int64(*batt.Percent)
			out.Percent = &percent
		}
	}) != nil {
		out = tuigateway.SystemBatteryResult{Category: "dim"}
	}
	return ok(rid, tuigateway.Plain(out))
}
