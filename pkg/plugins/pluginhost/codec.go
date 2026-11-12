package pluginhost

import (
	"encoding/base64"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// Turns a callable or object into a reference dict, nil leaving the value to the default encoding
type refsFunc func(p py, o Ref) map[string]any

// JSON safe form of a Python value, refs turning callables and objects into reference dicts
func (rt *runtime) encode(p py, o Ref, refs refsFunc) any { return rt.encodeDepth(p, o, refs, 0) }

func (rt *runtime) encodeDepth(p py, o Ref, refs refsFunc, depth int) any {
	switch p.g.KindOf(o.Ptr()) {
	case pyhost.KindNone, pyhost.KindBool, pyhost.KindInt, pyhost.KindFloat, pyhost.KindStr:
		return p.val(o)
	}
	if depth > 64 {
		return map[string]any{"__opaque__": p.typeName(o), "repr": "<too deep>"}
	}
	if p.isinstance(o, p.attr("enum", "Enum")) {
		return rt.encodeDepth(p, p.get(o, "value"), refs, depth+1)
	}
	if p.isinstance(o, p.attr("pathlib", "PurePath")) {
		return p.str(o)
	}
	switch p.g.KindOf(o.Ptr()) {
	case pyhost.KindBytes, pyhost.KindByteArray:
		return map[string]any{"__bytes__": base64.StdEncoding.EncodeToString(p.val(p.call(p.attr("builtins", "bytes"), o)).([]byte))}
	case pyhost.KindDict:
		out := map[string]any{}
		for _, kv := range p.items(p.m(o, "items")) {
			out[p.str(p.item(kv, 0))] = rt.encodeDepth(p, p.item(kv, 1), refs, depth+1)
		}
		return out
	case pyhost.KindList, pyhost.KindTuple:
		return rt.encodeItems(p, o, refs, depth)
	}
	if p.isinstance(o, p.call(p.attr("builtins", "tuple"), []any{p.attr("builtins", "set"), p.attr("builtins", "frozenset")})) {
		return rt.encodeItems(p, o, refs, depth)
	}
	if p.isinstance(o, rt.opaqueClass) {
		return map[string]any{"__opaque__": p.val(p.get(o, "type_name")), "repr": p.val(p.get(o, "text"))}
	}
	if own := p.getOr(p.typeOf(o), "__icebath_ref__"); own != nil && p.callable(own) {
		return p.val(p.call(own, o))
	}
	if refs != nil {
		if ref := refs(p, o); ref != nil {
			return ref
		}
	}
	if !p.isType(o) && p.truthy(p.call(p.attr("dataclasses", "is_dataclass"), o)) {
		fields := map[string]any{}
		for _, f := range p.items(p.call(p.attr("dataclasses", "fields"), o)) {
			name := p.str(p.get(f, "name"))
			v := p.getOr(o, name)
			if v == nil {
				fields[name] = nil
				continue
			}
			fields[name] = rt.encodeDepth(p, v, refs, depth+2)
		}
		return map[string]any{"__record__": p.typeName(o), "fields": fields}
	}
	if dump := p.getOr(o, "model_dump"); dump != nil && p.callable(dump) && !p.isType(o) {
		var record map[string]any
		if p.attempt(func() {
			record = map[string]any{"__record__": p.typeName(o), "fields": rt.encodeDepth(p, p.call(dump), refs, depth+1)}
		}, "Exception") == nil {
			return record
		}
	}
	text := ""
	_ = p.attempt(func() { text = p.repr(o) }, "Exception")
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	return map[string]any{"__opaque__": p.typeName(o), "repr": text}
}

func (rt *runtime) encodeItems(p py, o Ref, refs refsFunc, depth int) []any {
	out := []any{}
	for _, item := range p.items(o) {
		out = append(out, rt.encodeDepth(p, item, refs, depth+1))
	}
	return out
}

// Resolves a reference dict into the Python object it names
type resolveFunc func(p py, ref map[string]any) Ref

var refKeys = []string{"__callable__", "__object__", "__handle__", "__parent_callable__", "__parent_object__", "__parent_event__", "__child_event__"}

// Python object for a decoded wire value, records with attribute access and references through resolve
func (rt *runtime) decode(p py, v any, resolve resolveFunc) Ref {
	return p.obj(rt.decodeValue(p, v, resolve))
}

func (rt *runtime) decodeValue(p py, v any, resolve resolveFunc) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = rt.decodeValue(p, item, resolve)
		}
		return out
	case map[string]any:
		if fields, ok := x["__record__"]; ok {
			_ = fields
			return p.call(rt.recordClass, rt.decodeValue(p, orEmptyMap(x["fields"]), resolve))
		}
		if b, ok := x["__bytes__"]; ok {
			raw, err := base64.StdEncoding.DecodeString(str(b))
			if err != nil {
				throw(&pyhost.Exception{Type: "binascii.Error", Message: err.Error()})
			}
			return raw
		}
		if t, ok := x["__opaque__"]; ok {
			return rt.newOpaque(p, str(t), str(x["repr"]))
		}
		if resolve != nil {
			for _, k := range refKeys {
				if _, ok := x[k]; ok {
					return resolve(p, x)
				}
			}
		}
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = rt.decodeValue(p, item, resolve)
		}
		return out
	}
	return v
}

func orEmptyMap(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// Opaque stand in for a live object the parent owns
func (rt *runtime) newOpaque(p py, typeName, text string) Ref {
	return p.call(rt.opaqueClass, typeName, text)
}

// Value rebuilt as the type hint declares, a value that does not fit staying as decoded
func (rt *runtime) restore(p py, value, hint Ref) Ref {
	if p.isNone(value) || p.isNone(hint) || p.is(hint, p.attr("typing", "Any")) {
		return value
	}
	typing := p.imp("typing")
	origin := p.m(typing, "get_origin", hint)
	if p.is(origin, p.get(typing, "Union")) || p.is(origin, p.attr("types", "UnionType")) {
		var arms []Ref
		noneType := p.typeOf(p.obj(nil))
		for _, arm := range p.items(p.m(typing, "get_args", hint)) {
			if !p.is(arm, noneType) {
				arms = append(arms, arm)
			}
		}
		if len(arms) == 1 {
			return rt.restore(p, value, arms[0])
		}
		return value
	}
	if !p.isNone(origin) {
		return rt.restoreContainer(p, value, origin, p.items(p.m(typing, "get_args", hint)))
	}
	if !p.isType(hint) || p.isinstance(value, hint) {
		return value
	}
	var out Ref
	_ = p.attempt(func() {
		if p.truthy(p.call(p.attr("dataclasses", "is_dataclass"), hint)) && p.isinstance(value, rt.recordClass) {
			hints := p.m(typing, "get_type_hints", hint)
			kw := map[string]any{}
			for _, f := range p.items(p.call(p.attr("dataclasses", "fields"), hint)) {
				name := p.str(p.get(f, "name"))
				if !p.truthy(p.get(f, "init")) || !p.truthy(p.m(value, "__contains__", name)) {
					continue
				}
				kw[name] = rt.restore(p, p.item(value, name), p.m(hints, "get", name))
			}
			out = p.callKw(hint, nil, kw)
			return
		}
		if p.issubclass(hint, p.attr("enum", "Enum")) || p.issubclass(hint, p.attr("pathlib", "PurePath")) {
			out = p.call(hint, value)
		}
	}, "TypeError", "ValueError", "NameError")
	if out != nil {
		return out
	}
	return value
}

// Container value rebuilt per its origin class and item hints
func (rt *runtime) restoreContainer(p py, value, origin Ref, args []Ref) Ref {
	builtins := p.imp("builtins")
	abc := p.imp("collections.abc")
	hintAt := func(i int) Ref {
		if i < len(args) {
			return args[i]
		}
		return p.obj(nil)
	}
	if (p.is(origin, p.get(builtins, "dict")) || p.is(origin, p.get(abc, "Mapping"))) && p.isinstance(value, p.get(builtins, "dict")) {
		keyHint, itemHint := p.obj(nil), p.obj(nil)
		if len(args) == 2 {
			keyHint, itemHint = args[0], args[1]
		}
		out := p.call(p.get(builtins, "dict"))
		for _, kv := range p.items(p.m(value, "items")) {
			p.m(out, "__setitem__", rt.restore(p, p.item(kv, 0), keyHint), rt.restore(p, p.item(kv, 1), itemHint))
		}
		return out
	}
	var kind Ref
	for _, pair := range [][2]Ref{
		{p.get(builtins, "list"), p.get(builtins, "list")}, {p.get(builtins, "tuple"), p.get(builtins, "tuple")},
		{p.get(builtins, "set"), p.get(builtins, "set")}, {p.get(builtins, "frozenset"), p.get(builtins, "frozenset")},
		{p.get(abc, "Sequence"), p.get(builtins, "list")}, {p.get(abc, "Set"), p.get(builtins, "frozenset")},
		{p.get(abc, "Iterable"), p.get(builtins, "list")},
	} {
		if p.is(origin, pair[0]) {
			kind = pair[1]
			break
		}
	}
	if kind == nil || !p.isinstance(value, p.get(builtins, "list")) {
		return value
	}
	items := p.items(value)
	if p.is(kind, p.get(builtins, "tuple")) && len(args) > 0 && !p.is(args[len(args)-1], p.get(builtins, "Ellipsis")) {
		if len(args) != len(items) {
			return p.call(kind, value)
		}
		restored := make([]any, len(items))
		for i, v := range items {
			restored[i] = rt.restore(p, v, args[i])
		}
		return p.call(kind, restored)
	}
	restored := make([]any, len(items))
	for i, v := range items {
		restored[i] = rt.restore(p, v, hintAt(0))
	}
	return p.call(kind, restored)
}

// Function a class declares for a method or property name, nil when it declares none
func declaredMember(p py, cls Ref, name string) Ref {
	raw := p.callKw(p.attr("inspect", "getattr_static"), []any{cls, name, nil}, nil)
	if p.isinstance(raw, p.attr("builtins", "property")) {
		raw = p.get(raw, "fget")
	}
	if p.truthy(p.m(p.imp("inspect"), "isfunction", raw)) {
		return raw
	}
	return nil
}

// Arguments for a call to the unbound method fn, each rebuilt as its declared parameter type
func (rt *runtime) restoreArguments(p py, fn Ref, args []Ref, kwargs map[string]Ref) ([]Ref, map[string]Ref) {
	if fn == nil {
		return args, kwargs
	}
	var hints Ref
	var params []Ref
	if p.attempt(func() {
		hints = p.m(p.imp("typing"), "get_type_hints", fn)
		params = p.items(p.call(p.attr("builtins", "list"), p.m(p.get(p.m(p.imp("inspect"), "signature", fn), "parameters"), "values")))
	}, "NameError", "TypeError", "ValueError", "AttributeError") != nil {
		return args, kwargs
	}
	if len(params) > 0 {
		params = params[1:]
	}
	var byPosition []string
	byName := map[string]bool{}
	for _, prm := range params {
		kind := p.val(p.call(p.attr("builtins", "int"), p.get(prm, "kind"))).(int64)
		name := p.str(p.get(prm, "name"))
		if kind == 0 || kind == 1 {
			byPosition = append(byPosition, name)
		}
		if kind == 1 || kind == 3 {
			byName[name] = true
		}
	}
	restored := make([]Ref, 0, len(args))
	for i, v := range args {
		if i < len(byPosition) {
			restored = append(restored, rt.restore(p, v, p.m(hints, "get", byPosition[i])))
		} else {
			restored = append(restored, v)
		}
	}
	kw := make(map[string]Ref, len(kwargs))
	for k, v := range kwargs {
		if byName[k] {
			kw[k] = rt.restore(p, v, p.m(hints, "get", k))
		} else {
			kw[k] = v
		}
	}
	return restored, kw
}

// Whether a callable is a coroutine function or an instance whose call method is one
func isAsyncCallable(p py, fn Ref) bool {
	inspect := p.imp("inspect")
	target := p.getOr(fn, "__func__")
	if target == nil {
		target = fn
	}
	if p.truthy(p.m(inspect, "iscoroutinefunction", target)) {
		return true
	}
	call := p.getOr(fn, "__call__")
	if call == nil {
		call = p.obj(nil)
	}
	return p.truthy(p.m(inspect, "iscoroutinefunction", call))
}

// Parameter list of a callable as name, kind and has default triples, nil when it has no signature
func describeSignature(p py, fn Ref) any {
	var sig Ref
	if p.attempt(func() { sig = p.m(p.imp("inspect"), "signature", fn) }, "TypeError", "ValueError") != nil {
		return nil
	}
	empty := p.attr("inspect", "Parameter")
	empty = p.get(empty, "empty")
	out := []any{}
	for _, prm := range p.items(p.m(p.get(sig, "parameters"), "values")) {
		out = append(out, []any{p.str(p.get(prm, "name")), p.val(p.call(p.attr("builtins", "int"), p.get(prm, "kind"))),
			!p.is(p.get(prm, "default"), empty)})
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return pyStr(v)
}
