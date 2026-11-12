package plugins

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const ProtocolVersion = 1

// Remote raise naming a class plugins do not commonly catch, or the parent raising while serving
type PluginHostError struct {
	TypeName      string
	RemoteMessage string
	ExitCode      string
	Bases         []string
}

func (e *PluginHostError) Error() string {
	if e.TypeName != "" {
		return e.TypeName + ": " + e.RemoteMessage
	}
	return e.RemoteMessage
}

// Channel closed because the host process exited or never started
type PluginHostUnavailable struct{ Reason string }

func (e *PluginHostUnavailable) Error() string { return e.Reason }

// Context method that cannot cross the plugin host boundary
type PluginHostUnsupported struct{ Msg string }

func (e *PluginHostUnsupported) Error() string { return e.Msg }

// Remote raise of a builtin exception class plugins commonly catch, message only like str(exc)
type HostBuiltinError struct {
	Type    string
	Message string
	Bases   []string
}

func (e *HostBuiltinError) Error() string { return e.Message }

// Class names of the remote exception through BaseException, its type(exc).__mro__ without object
func (e *HostBuiltinError) PyExceptionMRO() []string { return append([]string{e.Type}, e.Bases...) }

// Class names of the remote exception through BaseException, the parent side raise standing as PluginHostError
func (e *PluginHostError) PyExceptionMRO() []string {
	if e.TypeName == "" {
		return []string{"PluginHostError", "Exception", "BaseException"}
	}
	return append([]string{e.TypeName}, e.Bases...)
}

var (
	_ interface{ PyExceptionMRO() []string } = (*HostBuiltinError)(nil)
	_ interface{ PyExceptionMRO() []string } = (*PluginHostError)(nil)
)

var remoteBuiltinErrors = map[string]bool{
	"ValueError": true, "TypeError": true, "KeyError": true, "FileNotFoundError": true, "PermissionError": true,
	"NotImplementedError": true, "TimeoutError": true, "LookupError": true, "RuntimeError": true,
}

// Remote error as the builtin type when plugins commonly catch it, else a PluginHostError
func raiseRemote(e map[string]any) error {
	typeName, message := wireStr(e["type"]), wireStr(e["message"])
	if typeName == "PluginHostUnsupported" {
		return &PluginHostUnsupported{Msg: message}
	}
	var bases []string
	for _, b := range listOrEmpty(e["bases"]) {
		bases = append(bases, wireStr(b))
	}
	if remoteBuiltinErrors[typeName] {
		return &HostBuiltinError{Type: typeName, Message: message, Bases: bases}
	}
	return &PluginHostError{TypeName: typeName, RemoteMessage: message, ExitCode: wireStr(e["code"]), Bases: bases}
}

// Whether a remote raise is an instance of the named Python exception class, by its own name or a base
func RemoteIsInstance(err error, class string) bool {
	var builtin *HostBuiltinError
	if errors.As(err, &builtin) {
		return builtin.Type == class || slices.Contains(builtin.Bases, class)
	}
	var remote *PluginHostError
	if errors.As(err, &remote) {
		return remote.TypeName == class || slices.Contains(remote.Bases, class)
	}
	return false
}

// Python str() of an optional wire field, empty for null or missing
func wireStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return config.PyStr(v)
}

// Dataclass that crossed the boundary with item and attribute access
type Record map[string]any

// Live object the other process owns, any attribute access failing with a readable reason
type Opaque struct {
	TypeName string
	Text     string
}

func (o *Opaque) String() string { return "<opaque " + o.TypeName + ">" }

// Attribute access on an Opaque, which always fails naming the live object
func (o *Opaque) Attr(name string) error {
	return &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("%s is a live object in the Icebath process and is not available inside the plugin host (attribute %s)", config.PyRepr(o.TypeName), config.PyRepr(name))}
}

// Python class name a Go value reports across the wire
func wireTypeName(v any) string {
	if n, ok := v.(pyrepr.PyTypeNamed); ok && n.PyTypeName() != "" {
		return n.PyTypeName()
	}
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "NoneType"
	}
	if t.Name() != "" {
		return t.Name()
	}
	return t.Kind().String()
}

// JSON safe form of a value, refs turning callables and objects into reference dicts
func wireEncode(value any, refs func(any) map[string]any) any {
	return wireEncodeDepth(value, refs, 0)
}

func wireEncodeDepth(value any, refs func(any) map[string]any, depth int) any {
	switch v := value.(type) {
	case nil, bool, string, float64, int64, *big.Int:
		return v
	case int:
		return int64(v)
	case int8, int16, int32, uint8, uint16, uint32:
		return reflect.ValueOf(v).Convert(reflect.TypeOf(int64(0))).Interface()
	case uint:
		return encodeUnsigned(uint64(v))
	case uint64:
		return encodeUnsigned(v)
	case float32:
		return float64(v)
	}
	if depth > 64 {
		return map[string]any{"__opaque__": wireTypeName(value), "repr": "<too deep>"}
	}
	switch v := value.(type) {
	case []byte:
		return map[string]any{"__bytes__": base64.StdEncoding.EncodeToString(v)}
	case *Opaque:
		return map[string]any{"__opaque__": v.TypeName, "repr": v.Text}
	case *hostCallable:
		if refs != nil {
			if ref := refs(v); ref != nil {
				return ref
			}
		}
		return map[string]any{"__opaque__": "function", "repr": "<icebath callable>"}
	case Record:
		out := map[string]any{}
		for k, item := range v {
			out[k] = wireEncodeDepth(item, refs, depth+1)
		}
		return out
	case *pyjson.Object:
		if v == nil {
			return nil
		}
		out := pyjson.NewObject()
		for _, k := range v.Keys {
			out.Set(k, wireEncodeDepth(v.Vals[k], refs, depth+1))
		}
		return out
	}
	if e, ok := value.(interface{ PythonEnumValue() any }); ok {
		return wireEncodeDepth(e.PythonEnumValue(), refs, depth+1)
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		if rv.Elem().Kind() != reflect.Struct {
			return wireEncodeDepth(rv.Elem().Interface(), refs, depth+1)
		}
	}
	switch rv.Kind() {
	case reflect.Map:
		out := map[string]any{}
		iter := rv.MapRange()
		for iter.Next() {
			out[config.PyStr(iter.Key().Interface())] = wireEncodeDepth(iter.Value().Interface(), refs, depth+1)
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = wireEncodeDepth(rv.Index(i).Interface(), refs, depth+1)
		}
		return out
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	if refs != nil {
		if ref := refs(value); ref != nil {
			return ref
		}
	}
	if fields, ok := recordFields(rv); ok {
		return map[string]any{"__record__": wireTypeName(value), "fields": wireEncodeDepth(fields, refs, depth+1)}
	}
	text := fmt.Sprintf("<%s object>", wireTypeName(value))
	if st, ok := value.(fmt.Stringer); ok {
		text = st.String()
	}
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	return map[string]any{"__opaque__": wireTypeName(value), "repr": text}
}

func encodeUnsigned(v uint64) any {
	if v <= math.MaxInt64 {
		return int64(v)
	}
	return new(big.Int).SetUint64(v)
}

// Exported struct fields keyed by their json tag or snake case name, the dataclass field table
func recordFields(rv reflect.Value) (map[string]any, bool) {
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	out := map[string]any{}
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Type.Kind() == reflect.Func || f.Type.Kind() == reflect.Chan {
			continue
		}
		name := snakeCase(f.Name)
		if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag == "-" {
			continue
		} else if tag != "" {
			name = tag
		}
		out[name] = rv.Field(i).Interface()
	}
	return out, true
}

// Snake case of an exported Go field name, ToolsetID becoming toolset_id
func snakeCase(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		upper := r >= 'A' && r <= 'Z'
		if upper && i > 0 {
			prevLower := runes[i-1] >= 'a' && runes[i-1] <= 'z' || runes[i-1] >= '0' && runes[i-1] <= '9'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if prevLower || (nextLower && runes[i-1] >= 'A' && runes[i-1] <= 'Z') {
				b.WriteByte('_')
			}
		}
		if upper {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Inverse of wireEncode, resolve turning callable, object and handle refs into proxies
func wireDecode(value any, resolve func(map[string]any) (any, error)) (any, error) {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			d, err := wireDecode(item, resolve)
			if err != nil {
				return nil, err
			}
			out[i] = d
		}
		return out, nil
	case map[string]any:
		if _, ok := v["__record__"]; ok {
			fields, _ := v["fields"].(map[string]any)
			d, err := wireDecode(fields, resolve)
			if err != nil {
				return nil, err
			}
			rec := Record{}
			if m, ok := d.(map[string]any); ok {
				for k, item := range m {
					rec[k] = item
				}
			}
			return rec, nil
		}
		if raw, ok := v["__bytes__"]; ok {
			s, _ := raw.(string)
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, &HostBuiltinError{Type: "ValueError", Message: err.Error()}
			}
			return b, nil
		}
		if raw, ok := v["__opaque__"]; ok {
			return &Opaque{TypeName: wireStr(raw), Text: wireStr(v["repr"])}, nil
		}
		_, c := v["__callable__"]
		_, o := v["__object__"]
		_, h := v["__handle__"]
		_, pe := v["__parent_event__"]
		_, ce := v["__child_event__"]
		if (c || o || h || pe || ce) && resolve != nil {
			return resolve(v)
		}
		out := map[string]any{}
		for k, item := range v {
			d, err := wireDecode(item, resolve)
			if err != nil {
				return nil, err
			}
			out[k] = d
		}
		return out, nil
	}
	return value, nil
}

// One parameter of a peer signature, kind numbered like inspect.Parameter kinds
type wireParam struct {
	Name       string
	Kind       int
	HasDefault bool
}

const (
	paramPositionalOnly = iota
	paramPositionalOrKeyword
	paramVarPositional
	paramKeywordOnly
	paramVarKeyword
)

// Peer signature from its [[name, kind, has_default], ...] description, nil when absent or malformed
func signatureFrom(spec any) []wireParam {
	items, ok := spec.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]wireParam, 0, len(items))
	for _, raw := range items {
		triple, ok := raw.([]any)
		if !ok || len(triple) != 3 {
			return nil
		}
		name, ok := triple[0].(string)
		kind, kok := triple[1].(int64)
		if !ok || !kok || kind < 0 || kind > paramVarKeyword {
			return nil
		}
		def, _ := triple[2].(bool)
		out = append(out, wireParam{Name: name, Kind: int(kind), HasDefault: def})
	}
	return out
}

// Slice of a hook payload a callback signature accepts, everything for var keyword or unknown signatures
func signatureKwargs(sig []wireParam, payload map[string]any) map[string]any {
	out := map[string]any{}
	if sig == nil {
		for k, v := range payload {
			out[k] = v
		}
		return out
	}
	accepted := map[string]bool{}
	for _, p := range sig {
		if p.Kind == paramVarKeyword {
			for k, v := range payload {
				out[k] = v
			}
			return out
		}
		if p.Kind == paramPositionalOrKeyword || p.Kind == paramKeywordOnly {
			accepted[p.Name] = true
		}
	}
	for k, v := range payload {
		if accepted[k] {
			out[k] = v
		}
	}
	return out
}

type servingKey struct{}

// Context marked as working for peer request id, the origin of every call made under it
func withServingRequest(ctx context.Context, id any) context.Context {
	return context.WithValue(ctx, servingKey{}, id)
}

// Peer request the code under ctx is running for, nil when none
func servingRequest(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(servingKey{})
}

// Handler serving one peer request, origin naming the request of ours the peer was serving
type wireHandler func(ctx context.Context, method string, params map[string]any, origin any) (any, error)

// Bidirectional request and response over a pair of byte streams, one JSON document per line
type Channel struct {
	reader           io.ReadCloser
	writer           io.WriteCloser
	handler          wireHandler
	name             string
	onClose          func(reason string)
	contextForOrigin func(origin any) context.Context
	writeMu          sync.Mutex
	ids              atomic.Int64
	pendingMu        sync.Mutex
	pending          map[int64]chan map[string]any
	contexts         map[int64]context.Context
	closedReason     *string
	closed           chan struct{}
}

func newChannel(reader io.ReadCloser, writer io.WriteCloser, handler wireHandler, name string,
	onClose func(string), contextForOrigin func(any) context.Context) *Channel {
	return &Channel{reader: reader, writer: writer, handler: handler, name: name, onClose: onClose,
		contextForOrigin: contextForOrigin, pending: map[int64]chan map[string]any{},
		contexts: map[int64]context.Context{}, closed: make(chan struct{})}
}

// Starts the reader goroutine
func (c *Channel) Start() *Channel {
	go c.readLoop()
	return c
}

// Close reason, empty while open
func (c *Channel) ClosedReason() (string, bool) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.closedReason == nil {
		return "", false
	}
	return *c.closedReason, true
}

// Caller context recorded when this side sent the still pending request id
func (c *Channel) contextOf(id any) context.Context {
	n, ok := wireInt(id)
	if !ok {
		return nil
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return c.contexts[n]
}

// Sends one request and waits for its reply frame, a zero timeout waiting forever
func (c *Channel) Request(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	c.pendingMu.Lock()
	if c.closedReason != nil {
		reason := *c.closedReason
		c.pendingMu.Unlock()
		return nil, &PluginHostUnavailable{Reason: reason}
	}
	id := c.ids.Add(1)
	reply := make(chan map[string]any, 1)
	c.pending[id] = reply
	c.contexts[id] = ctx
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		delete(c.contexts, id)
		c.pendingMu.Unlock()
	}()
	if params == nil {
		params = map[string]any{}
	}
	if err := c.send(map[string]any{"id": id, "method": method, "params": params, "origin": servingRequest(ctx)}); err != nil {
		return nil, err
	}
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case msg := <-reply:
		if msg == nil {
			reason, _ := c.ClosedReason()
			return nil, &PluginHostUnavailable{Reason: reason}
		}
		return msg, nil
	case <-expired:
		return nil, &HostBuiltinError{Type: "TimeoutError"}
	case <-ctx.Done():
		return nil, &UserInterrupt{Err: ctx.Err()}
	}
}

// Writes one frame under the write lock, closing the channel when the pipe fails
func (c *Channel) send(message map[string]any) error {
	data, err := pyJSONDumps(message)
	if err != nil {
		return &HostBuiltinError{Type: "TypeError", Message: err.Error()}
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if reason, closed := c.ClosedReason(); closed {
		return &PluginHostUnavailable{Reason: reason}
	}
	if _, err := c.writer.Write(data); err != nil {
		c.close(fmt.Sprintf("%s channel write failed: %v", c.name, err))
		reason, _ := c.ClosedReason()
		return &PluginHostUnavailable{Reason: reason}
	}
	return nil
}

func (c *Channel) readLoop() {
	reason := c.name + " channel closed"
	r := bufio.NewReaderSize(c.reader, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			c.dispatchFrame(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				reason = fmt.Sprintf("%s channel read failed: %v", c.name, err)
			}
			break
		}
	}
	c.close(reason)
}

func (c *Channel) dispatchFrame(line []byte) {
	parsed, err := pyJSONLoads(line)
	message, ok := parsed.(map[string]any)
	if err != nil || !ok {
		text := string(line)
		if r := []rune(text); len(r) > 200 {
			text = string(r[:200])
		}
		logWarn(fmt.Sprintf("%s: dropped a malformed frame: %s", c.name, config.PyRepr(text)))
		return
	}
	if _, isRequest := message["method"]; isRequest {
		go c.serve(message)
		return
	}
	id, _ := wireInt(message["id"])
	c.pendingMu.Lock()
	reply := c.pending[id]
	if reply != nil {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if reply != nil {
		reply <- message
	}
}

// Serves one peer request on its own goroutine in the caller context its origin names
func (c *Channel) serve(message map[string]any) {
	id := message["id"]
	origin := message["origin"]
	var ctx context.Context
	if c.contextForOrigin != nil {
		ctx = c.contextForOrigin(origin)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = withServingRequest(ctx, id)
	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	reply := func() (reply map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				typeName := "SystemExit"
				if ui, ok := r.(*UserInterrupt); ok {
					typeName, r = "KeyboardInterrupt", ui.Err
				}
				logWarn(fmt.Sprintf("%s: request %s raised %s", c.name, config.PyStr(message["method"]), typeName))
				reply = map[string]any{"id": id, "error": map[string]any{"type": typeName, "message": config.PyStr(r)}}
			}
		}()
		result, err := c.handler(ctx, config.PyStr(message["method"]), params, origin)
		if err != nil {
			return map[string]any{"id": id, "error": map[string]any{"type": pyErrorTypeName(err), "message": err.Error()}}
		}
		return map[string]any{"id": id, "result": result}
	}()
	if id == nil {
		return
	}
	if err := c.send(reply); err != nil {
		var te *HostBuiltinError
		if errors.As(err, &te) && te.Type == "TypeError" {
			_ = c.send(map[string]any{"id": id, "error": map[string]any{"type": "TypeError", "message": te.Message}})
		}
	}
}

// Result of a reply frame, its error re-raised as the remote type
func (c *Channel) resultOf(reply map[string]any) (any, error) {
	if e, ok := reply["error"]; ok {
		m, _ := e.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return nil, raiseRemote(m)
	}
	return reply["result"], nil
}

// Sends a request and returns its result or remote error
func (c *Channel) Call(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	reply, err := c.Request(ctx, method, params, timeout)
	if err != nil {
		return nil, err
	}
	return c.resultOf(reply)
}

func (c *Channel) close(reason string) {
	c.pendingMu.Lock()
	if c.closedReason != nil {
		c.pendingMu.Unlock()
		return
	}
	c.closedReason = &reason
	pending := c.pending
	c.pending = map[int64]chan map[string]any{}
	c.pendingMu.Unlock()
	close(c.closed)
	for _, ch := range pending {
		ch <- nil
	}
	if c.onClose != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logDebug(fmt.Sprintf("%s: on_close raised", c.name))
				}
			}()
			c.onClose(reason)
		}()
	}
}

// Marks the channel closed and closes both streams
func (c *Channel) Close(reason string) {
	c.close(reason)
	_ = c.writer.Close()
	_ = c.reader.Close()
}

// Integer value of a decoded wire number
func wireInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) {
			return int64(n), true
		}
	}
	return 0, false
}

// Python json.dumps with compact separators, allow_nan and ensure_ascii
func pyJSONDumps(v any) ([]byte, error) {
	var b strings.Builder
	if err := pyJSONWrite(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func pyJSONWrite(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case *big.Int:
		b.WriteString(x.String())
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(pyFloatRepr(x))
		}
	case string:
		pyJSONString(b, x)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := pyJSONWrite(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			pyJSONString(b, k)
			b.WriteByte(':')
			if err := pyJSONWrite(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case *pyjson.Object:
		b.WriteByte('{')
		for i, k := range x.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			pyJSONString(b, k)
			b.WriteByte(':')
			if err := pyJSONWrite(b, x.Vals[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("Object of type %s is not JSON serializable", wireTypeName(v))
	}
	return nil
}

// Python float repr, the shortest round tripping form with a trailing .0 for integral values
func pyFloatRepr(f float64) string {
	abs := math.Abs(f)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[0]
		exp = strings.TrimLeft(exp[1:], "0")
		if len(exp) < 2 {
			exp = strings.Repeat("0", 2-len(exp)) + exp
		}
		return mant + "e" + string(sign) + exp
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// Writes a JSON string with every non ASCII code unit escaped, WTF-8 lone surrogates included
func pyJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				if c < 0x20 {
					fmt.Fprintf(b, `\u%04x`, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		if c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
			unit := 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
			fmt.Fprintf(b, `\u%04x`, unit)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(b, `\u%04x`, 0xDC00|rune(c))
			i++
			continue
		}
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
		} else {
			fmt.Fprintf(b, `\u%04x`, r)
		}
		i += size
	}
	b.WriteByte('"')
}

// Python json.loads of one document, NaN and Infinity accepted, integers kept exact
func pyJSONLoads(data []byte) (any, error) {
	p := &pyJSONParser{s: data}
	p.skip()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("Extra data: char %d", p.i)
	}
	return v, nil
}

type pyJSONParser struct {
	s []byte
	i int
}

func (p *pyJSONParser) skip() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *pyJSONParser) fail() error { return fmt.Errorf("Expecting value: char %d", p.i) }

func (p *pyJSONParser) lit(word string, v any) (any, error) {
	if strings.HasPrefix(string(p.s[p.i:]), word) {
		p.i += len(word)
		return v, nil
	}
	return nil, p.fail()
}

func (p *pyJSONParser) value() (any, error) {
	if p.i >= len(p.s) {
		return nil, p.fail()
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 'n':
		return p.lit("null", nil)
	case c == 't':
		return p.lit("true", true)
	case c == 'f':
		return p.lit("false", false)
	case c == 'N':
		return p.lit("NaN", math.NaN())
	case c == 'I':
		return p.lit("Infinity", math.Inf(1))
	case c == '-' && strings.HasPrefix(string(p.s[p.i:]), "-Infinity"):
		return p.lit("-Infinity", math.Inf(-1))
	case c == '-' || c >= '0' && c <= '9':
		return p.number()
	}
	return nil, p.fail()
}

func (p *pyJSONParser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	isFloat := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c >= '0' && c <= '9' {
			p.i++
			continue
		}
		if c == '.' || c == 'e' || c == 'E' || c == '+' || (c == '-' && isFloat) {
			isFloat = true
			p.i++
			continue
		}
		break
	}
	text := string(p.s[start:p.i])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, p.fail()
		}
		return f, nil
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, nil
	}
	n, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, p.fail()
	}
	return n, nil
}

func (p *pyJSONParser) str() (any, error) {
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c == '\\':
			if p.i+1 >= len(p.s) {
				return nil, p.fail()
			}
			esc := p.s[p.i+1]
			p.i += 2
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				unit, ok := p.hex4()
				if !ok {
					return nil, p.fail()
				}
				if utf16.IsSurrogate(rune(unit)) && unit < 0xDC00 && p.i+1 < len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
					save := p.i
					p.i += 2
					if low, ok := p.hex4(); ok && low >= 0xDC00 && low <= 0xDFFF {
						b.WriteRune(utf16.DecodeRune(rune(unit), rune(low)))
						continue
					}
					p.i = save
				}
				if utf16.IsSurrogate(rune(unit)) {
					b.WriteByte(0xED)
					b.WriteByte(byte(0xA0 | (unit>>6)&0x1F))
					b.WriteByte(byte(0x80 | unit&0x3F))
					continue
				}
				b.WriteRune(rune(unit))
			default:
				return nil, p.fail()
			}
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return nil, fmt.Errorf("Unterminated string")
}

func (p *pyJSONParser) hex4() (uint16, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(p.s[p.i:p.i+4]), 16, 16)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return uint16(n), true
}

func (p *pyJSONParser) array() (any, error) {
	p.i++
	out := []any{}
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		p.skip()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skip()
		if p.i >= len(p.s) {
			return nil, p.fail()
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		return nil, p.fail()
	}
}

func (p *pyJSONParser) object() (any, error) {
	p.i++
	out := map[string]any{}
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.fail()
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.fail()
		}
		p.i++
		p.skip()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out[k.(string)] = v
		p.skip()
		if p.i >= len(p.s) {
			return nil, p.fail()
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		return nil, p.fail()
	}
}
