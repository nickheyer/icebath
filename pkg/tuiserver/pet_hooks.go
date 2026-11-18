package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/linktier"
)

// Pet method handlers by name, bound by the extras tier that carries pets and empty in a build without it
var petHandlers = map[string]Handler{}

var petMethodsRegistry = &handlerRegistry{}

// Registers every pet method, profile scoped where it reads the bound profile, refused with the build line when pets are absent
func init() {
	methods := []struct {
		name   string
		scoped bool
	}{
		{"pet.info", true}, {"pet.info.meta", true}, {"pet.cells", true}, {"pet.gallery", true},
		{"pet.select", true}, {"pet.remove", true}, {"pet.export", true}, {"pet.rename", true},
		{"pet.disable", true}, {"pet.enable", true}, {"pet.scale", true},
		{"pet.cancel", false}, {"pet.generate", false}, {"pet.hatch", false},
	}
	for _, m := range methods {
		name := m.name
		handler := func(ctx context.Context, rid any, params map[string]any) map[string]any {
			h := petHandlers[name]
			if h == nil {
				return rpcErr(rid, 4015, linktier.Missing("pets", linktier.Extras), nil)
			}
			return h(ctx, rid, params)
		}
		if m.scoped {
			petMethodsRegistry.scopedMethod(name, handler)
		} else {
			petMethodsRegistry.method(name, handler)
		}
	}
	petMethodsRegistry.install()
}
