package plugins

import "context"

const (
	// Object base of a live client a profile's create_client returned, every nested object staying in the guest
	hostedClientBase = "profile_client"
	// Result base that hands object refs back undecoded, used to reload an attribute reached object
	hostedRawRefBase = "raw_ref"
)

// Base nested results of a live client resolve under, empty for every other hosted object
func (o *HostedObject) liveClientBase() string {
	if o.Base == hostedClientBase {
		return o.Base
	}
	return ""
}

// Reload of an object read as an attribute of a live client, the parent reloaded first then the attribute read again
func (o *HostedObject) attributeReload(name string) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		ref, generation, err := o.addr(ctx)
		if err != nil {
			return nil, err
		}
		raw, err := o.host.callIn(ctx, hostedRawRefBase, "obj_getattr", map[string]any{"ref": ref, "name": name}, &generation, 0, nil)
		if err != nil {
			return nil, err
		}
		fresh, _ := raw.(map[string]any)
		if _, ok := fresh["__object__"]; !ok {
			return nil, &HostBuiltinError{Type: "AttributeError", Message: name}
		}
		return o.host.stampGeneration(fresh), nil
	}
}

// Object ref marked with the child generation that issued it, so the slot addresses that child
func (h *PluginHost) stampGeneration(ref map[string]any) map[string]any {
	if ref == nil {
		return nil
	}
	h.mu.Lock()
	ref["generation"] = int64(h.generation)
	h.mu.Unlock()
	return ref
}
