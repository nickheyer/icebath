package tools

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const relayToolLoopError = "Synchronous Icebath Relay tool execution cannot run on an active event-loop thread"

// nemo_relay.ToolExecutionResult value carrying the JSON result of one tool dispatch
type RelayToolExecutionResult struct{ Result any }

// Runs one tool call through Relay returning its result and the final arguments the tool saw
func RelayExecuteTool(ctx context.Context, toolName string, args map[string]any, callback func(ctx context.Context, args map[string]any) (any, error), sessionID, toolCallID string, metadata map[string]any) (any, map[string]any, error) {
	rt, session, parent, err := ResolveRelayExecutionContext(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if rt == nil || session == nil || !rt.ManagedExecutionEnabled() {
		v, err := callback(ctx, args)
		return v, args, err
	}
	var mu sync.Mutex
	observed := args
	var hasValue bool
	var rawValue, rawJSON any
	var callbackErr error
	callbackContext := pyctx.CopyContext(ctx)
	invoke := func(c context.Context, next any) (any, error) {
		final, ok := next.(map[string]any)
		if !ok {
			final = args
		}
		mu.Lock()
		observed = final
		mu.Unlock()
		result, err := callback(RelayManagedCallbackGuard(pyctx.With(c, callbackContext.Copy())), final)
		if err != nil {
			mu.Lock()
			callbackErr = err
			mu.Unlock()
			return nil, err
		}
		encoded := relayToolJSONable(result)
		mu.Lock()
		hasValue, rawValue, rawJSON = true, result, encoded
		mu.Unlock()
		if rt.Relay.ToolExecutionResult == nil {
			return nil, RelayAttributeError("NemoRelay", "ToolExecutionResult")
		}
		return rt.Relay.ToolExecutionResult(encoded)
	}
	var managed any
	if RelayHasRunningEventLoop(ctx) {
		err = errors.New(relayToolLoopError)
	} else {
		managed, err = rt.RunInSessionAsync(RelayWithRunningLoop(ctx), session, func(c context.Context) (any, error) {
			if rt.Relay.Tools == nil {
				return nil, RelayAttributeError("NemoRelay", "tools")
			}
			if rt.Relay.Tools.Execute == nil {
				return nil, RelayAttributeError("NemoRelayTools", "execute")
			}
			var callID any
			if toolCallID != "" {
				callID = toolCallID
			}
			meta := metadata
			if len(meta) == 0 {
				meta = map[string]any{}
			}
			return rt.Relay.Tools.Execute(c, toolName, relayToolJSONable(args), invoke, map[string]any{
				"handle": parent, "metadata": relayToolJSONable(meta), "tool_call_id": callID,
			})
		}, false)
	}
	mu.Lock()
	cbErr, hv, value, encoded, final := callbackErr, hasValue, rawValue, rawJSON, observed
	mu.Unlock()
	if err != nil {
		if cbErr != nil && IsRelayWrappedCallbackError(err, cbErr) {
			return nil, nil, cbErr
		}
		if !errors.Is(err, context.Canceled) && cbErr == nil && hv {
			slog.WarnContext(ctx, "NeMo Relay tool post-processing failed after dispatch success; returning the Icebath tool result", "error", err)
			return value, final, nil
		}
		return nil, nil, err
	}
	result, ok := managed.(*RelayToolExecutionResult)
	if !ok || result == nil {
		return nil, nil, relayAttributeError(pyrepr.TypeName(managed), "result")
	}
	if hv && relayToolJSONEqual(result.Result, encoded) {
		return value, final, nil
	}
	if s, ok := result.Result.(string); ok {
		return s, final, nil
	}
	out, err := pyjson.Dumps(relayToolJSONable(result.Result), pyjson.EnsureASCII(false))
	if err != nil {
		return nil, nil, err
	}
	return out, final, nil
}

// JSON shaped copy of a tool value where objects contribute every attribute, private ones included
func relayToolJSONable(value any) any {
	switch v := value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = relayToolJSONable(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = relayToolJSONable(item)
		}
		return out
	case *pyjson.Object:
		return relayToolJSONable(pyjson.Plain(v))
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8,
		reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64, reflect.String:
		return value
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[pyrepr.Str(iter.Key().Interface())] = relayToolJSONable(iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return pyrepr.Str(value)
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = relayToolJSONable(rv.Index(i).Interface())
		}
		return out
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
	}
	if d, ok := value.(RelayModelDumper); ok {
		if dumped, err := d.ModelDump(); err == nil {
			return relayToolJSONable(dumped)
		}
	}
	if attributes, ok := relayToolVars(rv); ok {
		return relayToolJSONable(attributes)
	}
	return pyrepr.Str(value)
}

// Every field of a struct value as an attribute dict, false for values without attributes
func relayToolVars(rv reflect.Value) (map[string]any, bool) {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	addressable := reflect.New(rv.Type()).Elem()
	addressable.Set(rv)
	attributes := map[string]any{}
	for i := 0; i < addressable.NumField(); i++ {
		f := addressable.Type().Field(i)
		name := f.Name
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == "-" {
			continue
		} else if tag != "" {
			name = tag
		}
		field := addressable.Field(i)
		if !f.IsExported() {
			field = reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
		}
		attributes[name] = field.Interface()
	}
	return attributes, true
}

// Canonical JSON equality of two tool values, plain equality when either cannot encode
func relayToolJSONEqual(left, right any) bool {
	l, lerr := RelayCanonicalJSON(left, relayToolJSONable)
	r, rerr := RelayCanonicalJSON(right, relayToolJSONable)
	if lerr != nil || rerr != nil {
		return reflect.DeepEqual(left, right)
	}
	return l == r
}
