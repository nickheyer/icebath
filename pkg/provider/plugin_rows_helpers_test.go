package provider

import (
	"context"
	"reflect"
	"testing"
)

// Registered profile of a bundled plugin under an empty home, failing the test when absent
func bundledPluginProfile(t *testing.T, name string) Profile {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	p, ok := GetProviderProfile(context.Background(), name)
	if !ok {
		t.Fatalf("no profile registered for %q", name)
	}
	return p
}

// Data fields of a profile keyed by the Python ProviderProfile attribute names
func profileDataFields(p Profile) map[string]any {
	streaming := any(nil)
	if p.SupportsStreaming != nil {
		streaming = *p.SupportsStreaming
	}
	return map[string]any{
		"name": p.Name, "api_mode": string(p.Mode), "aliases": p.Aliases, "display_name": p.DisplayName,
		"description": p.Description, "signup_url": p.SignupURL, "hidden": p.Hidden, "env_vars": p.KeyEnv,
		"base_url": p.BaseURL, "models_url": p.ModelsURL, "auth_type": p.AuthType, "supports_streaming": streaming,
		"supports_health_check": p.HealthCheck, "supports_model_listing": p.ModelListing, "supports_vision": p.Vision,
		"supports_vision_tool_messages": p.VisionToolMessages, "fallback_models": p.FallbackModels,
		"default_headers": p.Headers, "default_max_tokens": p.DefaultMaxTokens, "default_aux_model": p.DefaultAuxModel,
		"aggregator": p.Aggregator, "base_url_env_var": p.BaseURLEnv, "registry_auth_type": p.RegistryAuthType,
		"registry_after": p.RegistryAfter, "picker_after": p.PickerAfter, "catalog_aliases": p.CatalogAliases,
		"models_dev_id": p.ModelsDevID, "hostname": p.Hostname,
	}
}

// Asserts every listed Python attribute of the registered profile, nil slices and maps equal to empty ones
func assertProfileFields(t *testing.T, p Profile, want map[string]any) {
	t.Helper()
	got := profileDataFields(p)
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("%s: unknown field %s", p.Name, key)
			continue
		}
		if emptyValue(g) && emptyValue(w) {
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s.%s = %#v, want %#v", p.Name, key, g, w)
		}
	}
}

func emptyValue(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map:
		return rv.Len() == 0
	}
	return false
}
