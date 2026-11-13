package pluginsdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// Carries one request frame to the host and returns its reply frame
type link interface {
	roundTrip(ctx context.Context, id int64, frame []byte) ([]byte, error)
	serveContext(id any) (context.Context, context.CancelFunc)
	form() string
}

// Guest end of the plugin wire, holding every callback and object the host may address
type peer struct {
	link     link
	mu       sync.Mutex
	ids      int64
	refs     int64
	funcs    map[int64]Func
	objects  map[int64]*Object
	hosts    map[string]*host
	cancels  map[string]context.CancelFunc
	apis     map[string]APIHandler
	captures map[string][]any
}

func newPeer(l link) *peer {
	return &peer{link: l, funcs: map[int64]Func{}, objects: map[int64]*Object{}, hosts: map[string]*host{},
		cancels: map[string]context.CancelFunc{}, apis: map[string]APIHandler{}, captures: map[string][]any{}}
}

type servingKey struct{}

// Host request the context serves, nil outside one
func serving(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(servingKey{})
}

// Sends one request to the host and returns its decoded result or its error
func (p *peer) call(ctx context.Context, method string, params map[string]any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.ids++
	id := p.ids
	p.mu.Unlock()
	frame, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params, "origin": serving(ctx)})
	if err != nil {
		return nil, err
	}
	raw, err := p.link.roundTrip(ctx, id, frame)
	if err != nil {
		return nil, err
	}
	reply, err := decodeFrame(raw)
	if err != nil {
		return nil, err
	}
	if e, failed := reply["error"]; failed {
		fields, _ := e.(map[string]any)
		return nil, &Error{Type: text(fields["type"]), Message: text(fields["message"])}
	}
	return p.decode(reply["result"]), nil
}

// Serves one host frame and returns the reply frame, nil for a notification
func (p *peer) serve(frame []byte) []byte {
	msg, err := decodeFrame(frame)
	if err != nil {
		return nil
	}
	id, hasID := msg["id"]
	method := text(msg["method"])
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	if method == "cancel" {
		p.mu.Lock()
		cancel := p.cancels[fmt.Sprint(params["id"])]
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	ctx, cancel := p.link.serveContext(id)
	ctx = context.WithValue(ctx, servingKey{}, id)
	key := fmt.Sprint(id)
	if hasID && id != nil {
		p.mu.Lock()
		p.cancels[key] = cancel
		p.mu.Unlock()
	}
	result, err := p.dispatch(ctx, method, params)
	cancel()
	p.mu.Lock()
	delete(p.cancels, key)
	p.mu.Unlock()
	if !hasID || id == nil {
		return nil
	}
	reply := map[string]any{"id": id, "result": p.encode(result)}
	if err != nil {
		reply = map[string]any{"id": id, "error": errorFields(err)}
	}
	out, merr := json.Marshal(reply)
	if merr != nil {
		out, _ = json.Marshal(map[string]any{"id": id, "error": map[string]any{"type": "TypeError", "message": merr.Error()}})
	}
	return out
}

// Type and message an error crosses the wire with
func errorFields(err error) map[string]any {
	if e, ok := err.(*Error); ok {
		return map[string]any{"type": e.Type, "message": e.Message}
	}
	kind := "RuntimeError"
	if err == context.Canceled || err == context.DeadlineExceeded {
		kind = "CancelledError"
	}
	return map[string]any{"type": kind, "message": err.Error()}
}

// Object a request names by ref, a lookup failure when it was released
func (p *peer) requireObject(ref any) (*Object, error) {
	obj := p.object(ref)
	if obj == nil {
		return nil, &Error{Type: "LookupError", Message: fmt.Sprintf("object %v was released", ref)}
	}
	return obj, nil
}

// Runs one host operation, a panic in plugin code answering as an error
func (p *peer) dispatch(ctx context.Context, method string, params map[string]any) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, &Error{Type: "PluginPanic", Message: fmt.Sprint(r)}
		}
	}()
	switch method {
	case "hello":
		return map[string]any{"protocol": ProtocolVersion, "library": "go", "form": p.link.form()}, nil
	case "load":
		return p.load(ctx, params, registered, "the plugin never called pluginsdk.Register")
	case "load_tools":
		if registeredTools == nil {
			return map[string]any{"register_tools": false}, nil
		}
		if _, err := p.load(ctx, params, registeredTools, ""); err != nil {
			return nil, err
		}
		return map[string]any{"register_tools": true}, nil
	case "load_instance":
		return p.loadInstance(ctx, params)
	case "config_schema":
		if declaredSchema == nil {
			return nil, nil
		}
		return schemaFields(*declaredSchema), nil
	case "profile_call":
		return p.profileCall(ctx, params)
	case "http":
		return p.serveAPI(ctx, params)
	case "invoke":
		fn := p.function(params["ref"])
		if fn == nil {
			return nil, &Error{Type: "LookupError", Message: fmt.Sprintf("callback %v was released", params["ref"])}
		}
		value, err := fn(ctx, p.decodeList(params["args"]), p.decodeMap(params["kwargs"]))
		if err != nil {
			return nil, err
		}
		if _, judged := params["judge"]; judged {
			_, isRecord := value.(Record)
			return map[string]any{"value": value, "is_instance": isRecord}, nil
		}
		return value, nil
	case "obj_invoke":
		obj, err := p.requireObject(params["ref"])
		if err != nil {
			return nil, err
		}
		fn := obj.Methods[text(params["method"])]
		if fn == nil {
			return nil, &Error{Type: "AttributeError", Message: text(params["method"])}
		}
		return fn(ctx, p.decodeList(params["args"]), p.decodeMap(params["kwargs"]))
	case "obj_getattr":
		obj, err := p.requireObject(params["ref"])
		if err != nil {
			return nil, err
		}
		value, ok := obj.Attr(text(params["name"]))
		if !ok {
			return nil, &Error{Type: "AttributeError", Message: text(params["name"])}
		}
		return value, nil
	case "obj_setattr":
		obj, err := p.requireObject(params["ref"])
		if err != nil {
			return nil, err
		}
		obj.SetAttr(text(params["name"]), p.decode(params["value"]))
		return nil, nil
	case "obj_next":
		obj, err := p.requireObject(params["ref"])
		if err != nil {
			return nil, err
		}
		if obj.Next == nil {
			return nil, &Error{Type: "TypeError", Message: fmt.Sprintf("%s object is not iterable", obj.Type)}
		}
		item, ok, err := obj.Next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{"done": true}, nil
		}
		return map[string]any{"done": false, "item": item}, nil
	case "login_backend":
		obj, err := p.requireObject(params["ref"])
		if err != nil {
			return nil, err
		}
		op := text(params["op"])
		fn := obj.Methods[op]
		if fn == nil {
			return nil, &Error{Type: "AttributeError", Message: fmt.Sprintf("login backend has no %s operation", op)}
		}
		value, err := fn(ctx, nil, p.decodeMap(params))
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": value}, nil
	case "release":
		p.mu.Lock()
		for _, ref := range listOrEmpty(asList(params["refs"])) {
			if id, ok := integer(ref); ok {
				delete(p.funcs, id)
				delete(p.objects, id)
			}
		}
		p.mu.Unlock()
		return nil, nil
	case "unload":
		return map[string]any{"errors": p.unload(text(params["plugin_key"]))}, nil
	case "shutdown":
		p.mu.Lock()
		keys := make([]string, 0, len(p.hosts))
		for key := range p.hosts {
			keys = append(keys, key)
		}
		p.mu.Unlock()
		for _, key := range keys {
			p.unload(key)
		}
		return nil, nil
	}
	return nil, &Error{Type: "ValueError", Message: fmt.Sprintf("unknown plugin operation %q", method)}
}

// Host bound to the plugin a load request names
func (p *peer) bind(ctx context.Context, params map[string]any) *host {
	manifest, _ := params["manifest"].(map[string]any)
	h := &host{peer: p, key: text(params["plugin_key"]), id: text(params["plugin_id"]), name: text(params["name"]),
		dir: text(params["path"]), profile: text(params["profile_name"]), manifest: manifest, loading: ctx}
	p.mu.Lock()
	p.hosts[h.key] = h
	p.mu.Unlock()
	return h
}

// Runs a register function on the bound host, unloading the plugin again when it fails
func (p *peer) run(h *host, fn RegisterFunc) error {
	err := fn(h)
	h.mu.Lock()
	h.loading = nil
	h.mu.Unlock()
	if err != nil {
		p.unload(h.key)
	}
	return err
}

// Runs one register function of the plugin on a host bound to the plugin the request names
func (p *peer) load(ctx context.Context, params map[string]any, fn RegisterFunc, missing string) (any, error) {
	if fn == nil {
		return nil, &Error{Type: "PluginError", Message: missing}
	}
	if err := p.run(p.bind(ctx, params), fn); err != nil {
		return nil, err
	}
	return map[string]any{"file": nil}, nil
}

// Runs the register function keeping every provider the captured registrar receives and answers them in order
func (p *peer) loadInstance(ctx context.Context, params map[string]any) (any, error) {
	if registered == nil {
		return nil, &Error{Type: "PluginError", Message: "the plugin never called pluginsdk.Register"}
	}
	h := p.bind(ctx, params)
	h.capture = text(params["capture"])
	h.forward, _ = params["forward"].(bool)
	if h.capture == "" {
		p.unload(h.key)
		return nil, &Error{Type: "ValueError", Message: "a provider load names the registrar it captures"}
	}
	if err := p.run(h, registered); err != nil {
		return nil, err
	}
	h.mu.Lock()
	captured := append([]any(nil), h.captured...)
	h.mu.Unlock()
	p.mu.Lock()
	p.captures[h.key] = captured
	p.mu.Unlock()
	out := make([]any, 0, len(captured))
	for _, value := range captured {
		item := map[string]any{"value": p.encode(value)}
		if obj, ok := value.(*Object); ok && obj != nil {
			item["attrs"] = p.encode(obj.attrs())
		}
		out = append(out, item)
	}
	return map[string]any{"captured": out}, nil
}

// Runs one method or callable attribute of a model provider profile the plugin registered
func (p *peer) profileCall(ctx context.Context, params map[string]any) (any, error) {
	name, attr := text(params["profile"]), text(params["attr"])
	p.mu.Lock()
	captured := p.captures[text(params["plugin_key"])]
	p.mu.Unlock()
	for _, value := range captured {
		obj, ok := value.(*Object)
		if !ok || obj == nil {
			continue
		}
		if profile, _ := obj.Attr("name"); text(profile) != name {
			continue
		}
		args, kwargs := p.decodeList(params["args"]), p.decodeMap(params["kwargs"])
		if fn := obj.Methods[attr]; fn != nil {
			return fn(ctx, args, kwargs)
		}
		if field, ok := obj.Attr(attr); ok {
			if fn, ok := field.(Func); ok {
				return fn(ctx, args, kwargs)
			}
		}
		return nil, &Error{Type: "AttributeError", Message: fmt.Sprintf("profile %q has no callable %s", name, attr)}
	}
	return nil, &Error{Type: "LookupError", Message: fmt.Sprintf("this plugin registered no model provider profile %q", name)}
}

// Answers one forwarded dashboard request with the handler the plugin registered
func (p *peer) serveAPI(ctx context.Context, params map[string]any) (any, error) {
	key := text(params["plugin"])
	p.mu.Lock()
	handler := p.apis[key]
	p.mu.Unlock()
	if handler == nil {
		return map[string]any{"status": int64(404), "headers": []any{[]any{"content-type", "application/json"}},
			"body": []byte(`{"detail":"Not Found"}`)}, nil
	}
	req := APIRequest{Method: text(params["method"]), Path: text(params["path"]), Query: text(params["query"])}
	req.Authenticated, _ = params["authenticated"].(bool)
	req.Body, _ = p.decode(params["body"]).([]byte)
	for _, raw := range asList(params["headers"]) {
		pair := asList(raw)
		if len(pair) == 2 {
			req.Headers = append(req.Headers, [2]string{text(pair[0]), text(pair[1])})
		}
	}
	resp, err := handler(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		resp = &APIResponse{Status: 204}
	}
	status := resp.Status
	if status == 0 {
		status = 200
	}
	headers := make([]any, 0, len(resp.Headers))
	for _, h := range resp.Headers {
		headers = append(headers, []any{h[0], h[1]})
	}
	body := resp.Body
	if body == nil {
		body = []byte{}
	}
	return map[string]any{"status": int64(status), "headers": headers, "body": body}, nil
}

// Wire fields of a declared settings schema
func schemaFields(s ConfigSchema) map[string]any {
	fields := make([]any, 0, len(s.Fields))
	for _, f := range s.Fields {
		options := make([]any, 0, len(f.Options))
		for _, o := range f.Options {
			options = append(options, map[string]any{"value": o.Value, "label": o.Label, "description": o.Description})
		}
		kind := f.Kind
		if kind == "" {
			kind = "text"
		}
		scope := f.Scope
		if scope == "" {
			scope = "host"
		}
		field := map[string]any{"key": f.Key, "label": f.Label, "kind": kind, "default": f.Default, "description": f.Description,
			"placeholder": f.Placeholder, "options": options, "aliases": stringsAny(f.Aliases), "env_fallbacks": stringsAny(f.EnvFallbacks),
			"inline": f.Inline, "group": f.Group, "info": f.Info, "scope": scope}
		if f.EnvKey != "" {
			field["env_key"] = f.EnvKey
		}
		fields = append(fields, field)
	}
	storage := s.Storage
	if storage == "" {
		storage = "flat_json"
	}
	return map[string]any{"name": s.Name, "label": s.Label, "storage": storage, "docs_url": s.DocsURL, "fields": fields}
}

func stringsAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Runs the unload callbacks of one plugin newest first and returns the text of each failure
func (p *peer) unload(key string) []any {
	p.mu.Lock()
	h := p.hosts[key]
	delete(p.hosts, key)
	delete(p.apis, key)
	delete(p.captures, key)
	p.mu.Unlock()
	failures := []any{}
	if h == nil {
		return failures
	}
	h.mu.Lock()
	callbacks := h.unloads
	h.unloads = nil
	h.mu.Unlock()
	for i := len(callbacks) - 1; i >= 0; i-- {
		if err := callbacks[i](); err != nil {
			failures = append(failures, err.Error())
		}
	}
	return failures
}

func (p *peer) function(ref any) Func {
	id, _ := integer(ref)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.funcs[id]
}

func (p *peer) object(ref any) *Object {
	id, _ := integer(ref)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.objects[id]
}

// Signature every callback declares, accepting any positional and named arguments
var openSignature = []any{[]any{"args", 2, false}, []any{"kwargs", 4, false}}

// Wire reference of a callback, registering it for the host to invoke
func (p *peer) funcRef(name string, fn Func) map[string]any {
	p.mu.Lock()
	p.refs++
	id := p.refs
	p.funcs[id] = fn
	p.mu.Unlock()
	return map[string]any{"__callable__": id, "name": name, "qualname": name, "module": "plugin", "async": false, "sig": openSignature}
}

// Wire reference of an object, registering it for the host to call and read
func (p *peer) objectRef(o *Object) map[string]any {
	p.mu.Lock()
	p.refs++
	id := p.refs
	p.objects[id] = o
	p.mu.Unlock()
	methods := map[string]any{}
	for name := range o.Methods {
		methods[name] = map[string]any{"async": false, "sig": openSignature}
	}
	live := []any{}
	for name := range o.attrs() {
		live = append(live, name)
	}
	ref := map[string]any{"__object__": id, "type": o.Type, "methods": methods, "live": live, "iterable": o.Next != nil}
	if o.identity != nil {
		ref["identity"] = o.identity
	}
	return ref
}

// Wire form of a value the plugin sends
func (p *peer) encode(v any) any {
	switch x := v.(type) {
	case nil, bool, string, json.Number:
		return x
	case []byte:
		return map[string]any{"__bytes__": base64.StdEncoding.EncodeToString(x)}
	case Func:
		return p.funcRef("callback", x)
	case HookFunc:
		return p.funcRef("callback", func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) { return x(ctx, kwargs) })
	case *Object:
		if x == nil {
			return nil
		}
		return p.objectRef(x)
	case Record:
		return map[string]any{"__record__": x.Type, "fields": p.encode(mapOrEmpty(x.Fields))}
	case *Handle:
		if x == nil {
			return nil
		}
		return map[string]any{"__handle__": x.id, "kind": x.Kind, "key": x.Key}
	case *HostFunc:
		return map[string]any{"__parent_callable__": x.id}
	case *HostObject:
		return map[string]any{"__parent_object__": x.id, "type": x.Type}
	case *Event:
		return map[string]any{"__parent_event__": x.id}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = p.encode(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = p.encode(item)
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = p.encode(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			out := make(map[string]any, rv.Len())
			for it := rv.MapRange(); it.Next(); {
				out[it.Key().String()] = p.encode(it.Value().Interface())
			}
			return out
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	plain, err := decodeJSON(data)
	if err != nil {
		return fmt.Sprint(v)
	}
	return plain
}

// Go form of a value the host sent, numbers as int64 or float64
func (p *peer) decode(v any) any {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, _ := x.Float64()
		return f
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = p.decode(item)
		}
		return out
	case map[string]any:
		if raw, ok := x["__bytes__"].(string); ok {
			data, err := base64.StdEncoding.DecodeString(raw)
			if err == nil {
				return data
			}
		}
		if id, ok := integer(x["__handle__"]); ok {
			return &Handle{peer: p, id: id, Kind: text(x["kind"]), Key: text(x["key"])}
		}
		if id, ok := integer(x["__parent_callable__"]); ok {
			return &HostFunc{peer: p, id: id}
		}
		if id, ok := integer(x["__parent_object__"]); ok {
			return &HostObject{peer: p, id: id, Type: text(x["type"])}
		}
		if id, ok := integer(x["__parent_event__"]); ok {
			return &Event{peer: p, id: id}
		}
		if name, ok := x["__record__"].(string); ok {
			fields, _ := p.decode(x["fields"]).(map[string]any)
			return Record{Type: name, Fields: fields}
		}
		if name, ok := x["__opaque__"].(string); ok {
			return Opaque{Type: name, Text: text(x["repr"])}
		}
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = p.decode(item)
		}
		return out
	}
	return v
}

func (p *peer) decodeList(v any) []any {
	out, _ := p.decode(v).([]any)
	return listOrEmpty(out)
}

func (p *peer) decodeMap(v any) map[string]any {
	out, _ := p.decode(v).(map[string]any)
	return mapOrEmpty(out)
}

// One frame as a JSON object with numbers kept exact
func decodeFrame(data []byte) (map[string]any, error) {
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	msg, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("frame is not a JSON object")
	}
	return msg, nil
}

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Integer value of a decoded or raw wire number
func integer(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), x == float64(int64(x))
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

func asList(v any) []any {
	out, _ := v.([]any)
	return out
}

func listOrEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func mapOrEmpty(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
