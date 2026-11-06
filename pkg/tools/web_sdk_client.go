package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Every vendor here caps max_results at 20 server side
const searchLimitCap = 20

// Provider credential through the bound profile scope, failing closed while multiplexing without one
func providerEnv(ctx context.Context, env *Env, name string) (string, error) {
	if !IsGlobalEnv(name) {
		if ProfileSecrets != nil {
			if scope := ProfileSecrets.CurrentSecretScope(ctx); scope != nil {
				if v, ok := scope[name]; ok {
					return pystr.Strip(v), nil
				}
				return dotenvValue(env, name), nil
			}
		}
		if isMultiplexActive(env) && (env == nil || env.Secrets == nil) {
			return "", ErrUnscopedSecret
		}
	}
	return secretValue(env, name), nil
}

// Raw process level setting a vendor SDK reads from its environment, the .env layer included
func sdkEnvLookup(env *Env, name string) (string, bool) {
	if env != nil && env.Secrets != nil {
		return env.Secrets.Get(name)
	}
	return os.LookupEnv(name)
}

type sdkClientSlot struct {
	key    string
	client any
}

// One client per slot for the process, each stamped with the key it was built for
var sdkClientSlots = struct {
	sync.Mutex
	bySlot map[string]sdkClientSlot
}{bySlot: map[string]sdkClientSlot{}}

// Client of the slot built for the current key, rebuilt when the key changed and refused when it is unset
func cachedSDKClient[T any](ctx context.Context, env *Env, slot, envVar, missingKeyError string, factory func(apiKey string) T) (T, error) {
	var zero T
	apiKey, err := providerEnv(ctx, env, envVar)
	if err != nil {
		return zero, err
	}
	if apiKey == "" {
		return zero, verbatim(missingKeyError)
	}
	sdkClientSlots.Lock()
	defer sdkClientSlots.Unlock()
	if cached, ok := sdkClientSlots.bySlot[slot]; ok && cached.key == apiKey {
		if client, ok := cached.client.(T); ok {
			return client, nil
		}
	}
	client := factory(apiKey)
	sdkClientSlots.bySlot[slot] = sdkClientSlot{key: apiKey, client: client}
	return client, nil
}

// ---- vendor SDK transport ---------------------------------------------------------------------------

// Limits of one SDK request, connect bounding the dial and idle bounding the header wait and each body read
type sdkTimeouts struct {
	connect time.Duration
	idle    time.Duration
}

var sdkTransports sync.Map

// Shared transport per limit pair so vendor clients reuse connections
func sdkTransport(t sdkTimeouts) *http.Transport {
	if v, ok := sdkTransports.Load(t); ok {
		return v.(*http.Transport)
	}
	tr := webHTTPTransport.Clone()
	tr.DialContext = (&net.Dialer{Timeout: t.connect, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = t.connect
	tr.ResponseHeaderTimeout = t.idle
	v, _ := sdkTransports.LoadOrStore(t, tr)
	return v.(*http.Transport)
}

// sdkResponse is one vendor answer read whole
type sdkResponse struct {
	status int
	header http.Header
	body   []byte
}

// Decoded text of the answer with its declared charset, else UTF-8
func (r *sdkResponse) text() string {
	return responseText(&http.Response{Header: r.header}, r.body)
}

// sdkTimeoutError reports a connect, header or body read limit that elapsed
type sdkTimeoutError struct {
	phase string
	limit time.Duration
	err   error
}

func (e *sdkTimeoutError) Error() string {
	return fmt.Sprintf("%s timed out. (%s timeout=%s)", pystr.Capitalize(e.phase), e.phase, pyrepr.Str(e.limit.Seconds()))
}

func (e *sdkTimeoutError) Unwrap() error { return e.err }

// Sends one request and reads the answer, every body read restarting the idle limit
func sdkSend(ctx context.Context, t sdkTimeouts, method, target string, body []byte, header http.Header) (*sdkResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	resp, err := (&http.Client{Transport: sdkTransport(t)}).Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && ctx.Err() == nil {
			phase := "read"
			if !strings.Contains(err.Error(), "awaiting response headers") {
				phase = "connect"
			}
			limit := t.idle
			if phase == "connect" {
				limit = t.connect
			}
			return nil, &sdkTimeoutError{phase: phase, limit: limit, err: err}
		}
		return nil, err
	}
	defer resp.Body.Close()
	var idleFired atomic.Bool
	timer := time.AfterFunc(t.idle, func() {
		idleFired.Store(true)
		cancel()
	})
	defer timer.Stop()
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			timer.Reset(t.idle)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if idleFired.Load() {
				return nil, &sdkTimeoutError{phase: "read", limit: t.idle, err: rerr}
			}
			return nil, rerr
		}
	}
	return &sdkResponse{status: resp.StatusCode, header: resp.Header, body: buf.Bytes()}, nil
}

// Reports whether a transport failure was an elapsed limit
func isSDKTimeout(err error) bool {
	var te *sdkTimeoutError
	return errors.As(err, &te)
}

// ---- Python object semantics of decoded vendor payloads --------------------------------------------

// Dict view of a decoded JSON value, false for every other type
func pyDict(v any) (*pyjson.Object, bool) {
	switch x := v.(type) {
	case *pyjson.Object:
		return x, true
	case map[string]any:
		o := pyjson.NewObject()
		for k, val := range x {
			o.Set(k, val)
		}
		return o, true
	}
	return nil, false
}

// dict.get of a decoded value, the AttributeError of a non dict
func pyDictGet(v any, key string) (any, error) {
	d, ok := pyDict(v)
	if !ok {
		return nil, pyAttributeError(v, "get")
	}
	val, _ := d.Get(key)
	return val, nil
}

// AttributeError text for a missing attribute of a decoded value
func pyAttributeError(v any, attr string) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)
}

// Items of a for loop over a decoded value, dict keys and string characters included
func pyIter(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		out := []any{}
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	}
	if d, ok := pyDict(v); ok {
		out := make([]any, 0, d.Len())
		for _, k := range d.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// Value or a fallback when the value is falsy, the Python x or y
func pyOr(v any, fallback any) any {
	if pyTruthy(v) {
		return v
	}
	return fallback
}

// sep.join over a decoded value, the TypeError of a non string item
func pyJoin(sep string, v any) (string, error) {
	items, err := pyIter(v)
	if err != nil {
		return "", errors.New("can only join an iterable")
	}
	parts := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return "", fmt.Errorf("sequence item %d: expected str instance, %s found", i, pyrepr.TypeName(item))
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, sep), nil
}

// String field of a vendor row, the value itself for a string and its str form otherwise
func pyStrField(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

var (
	camelWordRe  = regexp.MustCompile(`(.)([A-Z][a-z]+)`)
	camelUpperRe = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

// camelCase key in snake_case the way the Exa SDK converts response keys
func camelToSnake(s string) string {
	s = camelWordRe.ReplaceAllString(s, "${1}_${2}")
	return pystr.Lower(camelUpperRe.ReplaceAllString(s, "${1}_${2}"))
}

// Top level keys of a response row in snake_case, a later key winning a collision
func toSnakeCase(d *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range d.Keys {
		v, _ := d.Get(k)
		if inner, ok := pyDict(v); ok {
			v = toSnakeCase(inner)
		}
		out.Set(camelToSnake(k), v)
	}
	return out
}
