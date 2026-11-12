package pluginhost

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

var (
	loginTextFields         = []string{"id", "kind", "label", "created_at"}
	loginOptionalTextFields = []string{"origin", "identifier", "identifier_type"}
)

// An exact str copy of a login value, ok false for any other type
func loginText(p py, v Ref) (string, bool) {
	if !p.isinstance(v, p.attr("builtins", "str")) {
		return "", false
	}
	return p.val(p.m(p.attr("builtins", "str"), "__str__", v)).(string), true
}

// Fields of a login backend's VaultItemMeta copied for the parent, ok false for a value of the wrong type
func loginMetaCopy(p py, meta Ref) (map[string]any, bool) {
	store := p.imp("agent.vault_store")
	if !p.isinstance(meta, p.get(store, "VaultItemMeta")) {
		return nil, false
	}
	fields := map[string]any{}
	valid := true
	for _, f := range loginTextFields {
		text, ok := loginText(p, p.get(meta, f))
		valid = valid && ok
		fields[f] = text
	}
	for _, f := range loginOptionalTextFields {
		value := p.get(meta, f)
		if p.isNone(value) {
			fields[f] = nil
			continue
		}
		text, ok := loginText(p, value)
		valid = valid && ok
		fields[f] = text
	}
	hasOTP, origins := p.get(meta, "has_otp"), p.get(meta, "allowed_origins")
	if !p.isinstance(origins, p.attr("builtins", "tuple")) || !p.is(p.typeOf(hasOTP), p.attr("builtins", "bool")) {
		return nil, false
	}
	allowed := []any{}
	for _, o := range p.items(origins) {
		text, ok := loginText(p, o)
		valid = valid && ok
		allowed = append(allowed, text)
	}
	if !valid || !p.truthy(p.m(p.get(store, "VAULT_KINDS"), "__contains__", fields["kind"])) {
		return nil, false
	}
	fields["has_otp"] = p.truthy(hasOTP)
	fields["allowed_origins"] = allowed
	return fields, true
}

// Name, prefix and needs_unlock of a login backend read without running its code, or the fixed refusal
func loginBackendIdentity(p py, provider Ref) map[string]any {
	cls := p.typeOf(provider)
	base := p.m(p.attr("sys", "modules"), "get", "agent.vault_backends.base")
	if p.isNone(base) || !p.issubclass(cls, p.get(base, "LoginBackend")) {
		return map[string]any{"error": "login backend must subclass LoginBackend"}
	}
	object := p.attr("builtins", "object")
	overrides := !p.is(p.get(cls, "__getattribute__"), p.get(object, "__getattribute__"))
	mro := p.items(p.get(cls, "__mro__"))
	for _, k := range mro[:len(mro)-1] {
		if p.truthy(p.m(p.call(p.attr("builtins", "vars"), k), "__contains__", "__class__")) {
			overrides = true
		}
	}
	if overrides {
		return map[string]any{"error": "login backend must not override attribute access"}
	}
	getattrStatic := p.attr("inspect", "getattr_static")
	name, prefix := p.call(getattrStatic, provider, "name", nil), p.call(getattrStatic, provider, "prefix", nil)
	str := p.attr("builtins", "str")
	if !p.is(p.typeOf(name), str) || !p.is(p.typeOf(prefix), str) {
		return map[string]any{"error": "login backend name and prefix must be plain string attributes"}
	}
	unlock := p.call(getattrStatic, provider, "needs_unlock", false)
	return map[string]any{"name": p.val(name), "prefix": p.val(prefix), "needs_unlock": !p.is(unlock, p.obj(false))}
}

func opLoginBackend(rt *runtime, p py, _ context.Context, params map[string]any) any {
	ref := toInt(params["ref"])
	backend, owner := rt.lookup(ref)
	if backend == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host object %d was released", ref)})
	}
	invalid := map[string]any{"invalid": true}
	switch op := pyStr(params["op"]); op {
	case "list_items":
		items := []any{}
		for _, m := range p.items(p.m(backend, "list_items")) {
			copied, ok := loginMetaCopy(p, m)
			if !ok {
				return invalid
			}
			items = append(items, copied)
		}
		return map[string]any{"value": items}
	case "get_meta":
		meta := p.m(backend, "get_meta", params["handle"])
		if p.isNone(meta) {
			return map[string]any{"value": nil}
		}
		copied, ok := loginMetaCopy(p, meta)
		if !ok {
			return invalid
		}
		return map[string]any{"value": copied}
	case "resolve_password":
		text, ok := loginText(p, p.m(backend, "resolve_password", params["handle"]))
		if !ok {
			return invalid
		}
		return map[string]any{"value": text}
	case "resolve_otp":
		otp := p.m(backend, "resolve_otp", params["handle"])
		if p.isNone(otp) {
			return map[string]any{"value": nil}
		}
		text, ok := loginText(p, otp)
		if !ok {
			return invalid
		}
		return map[string]any{"value": text}
	case "resolve_secret":
		secret := p.m(backend, "resolve_secret", params["handle"])
		if !p.isinstance(secret, p.attr("builtins", "dict")) {
			return invalid
		}
		copied := map[string]any{}
		for _, kv := range p.items(p.m(secret, "items")) {
			k, kok := loginText(p, p.item(kv, 0))
			v, vok := loginText(p, p.item(kv, 1))
			if !kok || !vok {
				return invalid
			}
			copied[k] = v
		}
		return map[string]any{"value": copied}
	case "fill_enter":
		context := p.m(backend, "fill_context")
		p.m(context, "__enter__")
		return map[string]any{"value": rt.remember(p, owner, context)}
	case "fill_exit":
		context := rt.pop(toInt(params["context"]))
		if context == nil {
			throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host object %s was released", pyStr(params["context"]))})
		}
		p.m(context, "__exit__", nil, nil, nil)
		return map[string]any{"value": nil}
	default:
		throw(&pyhost.Exception{Type: "ValueError", Message: fmt.Sprintf("unknown login backend operation %s", p.repr(p.obj(op)))})
	}
	return nil
}
