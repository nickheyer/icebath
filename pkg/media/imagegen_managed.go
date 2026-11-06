package media

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	managedFal    = "fal"
	managedKrea   = "krea"
	managedPortal = "portal"
)

var portalDuplicateOf = map[string]string{
	"openai/gpt-image-2":                 "fal-ai/gpt-image-2",
	"google/gemini-3-pro-image":          "fal-ai/nano-banana-pro",
	"google/gemini-3.1-flash-image":      "fal-ai/nano-banana-2",
	"google/gemini-3.1-flash-lite-image": "google/nano-banana-2-lite",
	"krea/krea-2-medium":                 "krea-2-medium",
	"krea/krea-2-medium-turbo":           "krea-2-medium-turbo",
	"krea/krea-2-large":                  "krea-2-large",
	"krea/krea-3":                        "krea-3",
}

const falKreaPrefix = "fal-ai/krea/"

// Gateway serving the model under the managed selection, FAL for an unset id
func managedBackendForModel(modelID any) string {
	s, _ := modelID.(string)
	candidate := strings.TrimSpace(s)
	if _, ok := falModels[candidate]; candidate == "" || ok {
		return managedFal
	}
	if kreaModelIDs[candidate] {
		return managedKrea
	}
	return managedPortal
}

// Trimmed non empty string of a raw config value, empty for blank or non strings
func configString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// Gateway a request goes to for the stored provider and model, empty when a direct provider owns it
func managedRoute(provider, modelID any) string {
	p, m := configString(provider), configString(modelID)
	if p != "" && p != tools.NousManagedProvider {
		return ""
	}
	backend := managedBackendForModel(m)
	if backend == managedPortal && p != tools.NousManagedProvider {
		return managedFal
	}
	return backend
}

// Model rows of a registered image gen plugin, empty when it is unavailable or raises
func managedPluginRows(ctx context.Context, env *tools.Env, name string) (rows []map[string]any) {
	defer func() {
		if recover() != nil {
			rows = nil
		}
	}()
	p := getPluginProvider(ctx, env, name, false)
	if p == nil {
		return nil
	}
	return p.ListModels()
}

// Copy of a catalog row with the serving backend stamped on it
func withBackend(row map[string]any, backend string) map[string]any {
	out := make(map[string]any, len(row)+1)
	for k, v := range row {
		out[k] = v
	}
	out["backend"] = backend
	return out
}

// Managed picker catalog in FAL, native Krea, then non duplicate Portal order with the default model
func managedImageCatalog(ctx context.Context, env *tools.Env, includeKrea, includePortal bool) ([]string, map[string]map[string]any, string) {
	var order []string
	catalog := map[string]map[string]any{}
	put := func(id string, row map[string]any) {
		if _, ok := catalog[id]; !ok {
			order = append(order, id)
		}
		catalog[id] = row
	}
	for _, id := range falModelOrder {
		if !strings.HasPrefix(id, falKreaPrefix) {
			put(id, withBackend(falModels[id], managedFal))
		}
	}
	if includeKrea {
		for _, row := range managedPluginRows(ctx, env, "krea") {
			id, _ := row["id"].(string)
			put(id, withBackend(row, managedKrea))
		}
	}
	if includePortal {
		for _, row := range managedPluginRows(ctx, env, "nous") {
			id, _ := row["id"].(string)
			if _, dup := catalog[id]; dup {
				continue
			}
			if alias, ok := portalDuplicateOf[id]; ok {
				if _, dup := catalog[alias]; dup {
					continue
				}
			}
			put(id, withBackend(row, managedPortal))
		}
	}
	return order, catalog, falDefaultModel
}
