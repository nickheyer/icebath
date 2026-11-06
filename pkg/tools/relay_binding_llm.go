package tools

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Binding surface of nemo_relay.llm, each nil field a missing attribute
type NemoRelayLLM struct {
	Execute       func(ctx context.Context, operation string, request any, callback RelayProviderCallback, kwargs map[string]any) (any, error)
	StreamExecute func(ctx context.Context, operation string, request any, provider RelayProviderStreamCallback, observe func(ctx context.Context, chunk any) error, finalizer func(ctx context.Context) (any, error), kwargs map[string]any) (any, error)
	Call          func(ctx context.Context, name string, request any, kwargs map[string]any) (any, error)
	CallEnd       func(ctx context.Context, handle, response any, kwargs map[string]any) error
}

// Provider callback Relay invokes with its possibly rewritten request, a RelayAwaitable result is awaited
type RelayProviderCallback func(ctx context.Context, request any) (any, error)

// Provider stream callback Relay invokes to open the async chunk generator
type RelayProviderStreamCallback func(ctx context.Context, request any) (*RelayAsyncIterator, error)

// Async iterator whose Next returns io.EOF when exhausted and whose nil Aclose is a missing aclose attribute
type RelayAsyncIterator struct {
	Next   func(ctx context.Context) (any, error)
	Aclose func(ctx context.Context) error
}

// nemo_relay.LLMRequest value with its headers and JSON content
type RelayLLMRequest struct {
	Headers map[string]any
	Content any
}

// nemo_relay.codecs classes, a nil constructor is a missing codec class
type NemoRelayCodecs struct {
	OpenAIChatCodec        func() (*NemoRelayCodec, error)
	OpenAIResponsesCodec   func() (*NemoRelayCodec, error)
	AnthropicMessagesCodec func() (*NemoRelayCodec, error)
}

// Typed request codec instance with decode and encode
type NemoRelayCodec struct {
	Decode func(ctx context.Context, request any) (any, error)
	Encode func(ctx context.Context, decoded, request any) (any, error)
}

// Python identity comparison of two scope handles
func RelayIdentical(a, b any) bool { return relayIdentical(a, b) }

// Lock serialising logical call finalization against turn close
func (t *RelayTurnContext) FinalizeLock() *sync.Mutex { return &t.finalizeMu }

// Python AttributeError text for a binding attribute the native module lacks
func RelayAttributeError(object, name string) error { return relayAttributeError(object, name) }

type relayLoopKey struct{}

// Context of code driven by a Relay event loop, the running loop asyncio reports
func RelayWithRunningLoop(ctx context.Context) context.Context {
	return context.WithValue(ctx, relayLoopKey{}, true)
}

// Context of a worker thread spawned off the loop, which has no running loop
func RelayWithoutRunningLoop(ctx context.Context) context.Context {
	return context.WithValue(ctx, relayLoopKey{}, false)
}

// Whether ctx runs on a Relay event loop
func RelayHasRunningEventLoop(ctx context.Context) bool {
	on, _ := ctx.Value(relayLoopKey{}).(bool)
	return on
}

// Awaits a RelayAwaitable on a fresh loop, refusing when ctx already runs on one
func RelayRunAwaitable(ctx context.Context, value any, loopError string) (any, error) {
	aw, ok := value.(RelayAwaitable)
	if !ok {
		return value, nil
	}
	if loopError == "" {
		loopError = "Synchronous Relay LLM execution cannot run on an event-loop thread"
	}
	if RelayHasRunningEventLoop(ctx) {
		return nil, errors.New(loopError)
	}
	return aw(RelayWithRunningLoop(ctx))
}

// Model dump hook of a value that serialises itself like a pydantic model
type RelayModelDumper interface{ ModelDump() (any, error) }

// JSON shaped copy of a provider value with stringified keys, public attributes, or its str form
func RelayJSONable(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = RelayJSONable(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RelayJSONable(item)
		}
		return out
	case *pyjson.Object:
		return RelayJSONable(pyjson.Plain(v))
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
			out[pyrepr.Str(iter.Key().Interface())] = RelayJSONable(iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return pyrepr.Str(value)
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = RelayJSONable(rv.Index(i).Interface())
		}
		return out
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
	}
	if d, ok := value.(RelayModelDumper); ok {
		if dumped, err := d.ModelDump(); err == nil {
			return RelayJSONable(dumped)
		}
	}
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return pyrepr.Str(value)
	}
	attributes := map[string]any{}
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == "-" {
			continue
		} else if tag != "" {
			name = tag
		}
		if strings.HasPrefix(name, "_") {
			continue
		}
		attributes[name] = rv.Field(i).Interface()
	}
	if len(attributes) == 0 {
		return pyrepr.Str(value)
	}
	return RelayJSONable(attributes)
}

// Sorted compact JSON of the encoded value, RelayJSONable when encode is nil
func RelayCanonicalJSON(value any, encode func(any) any) (string, error) {
	if encode == nil {
		encode = RelayJSONable
	}
	return pyjson.Dumps(encode(value), pyjson.SortKeys(true), pyjson.Separators(",", ":"))
}

// Waits up to timeout seconds for every operation lease to be released
func (r *RelayRuntime) WaitOperationsIdle(timeout float64) bool {
	return r.operationsIdle.Wait(timeout)
}
