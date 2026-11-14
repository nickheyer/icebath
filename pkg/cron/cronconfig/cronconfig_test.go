package cronconfig

import "testing"

func TestDefaultProviderReadsCronModelProvider(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"absent", map[string]any{}, ""},
		{"nil config", nil, ""},
		{"cron not a mapping", map[string]any{"cron": "openrouter"}, ""},
		{"unset", map[string]any{"cron": map[string]any{}}, ""},
		{"null", map[string]any{"cron": map[string]any{"model_provider": nil}}, ""},
		{"empty", map[string]any{"cron": map[string]any{"model_provider": ""}}, ""},
		{"stripped", map[string]any{"cron": map[string]any{"model_provider": "  anthropic \n"}}, "anthropic"},
		{"number rendered by str", map[string]any{"cron": map[string]any{"model_provider": 7}}, "7"},
		{"false is falsy", map[string]any{"cron": map[string]any{"model_provider": false}}, ""},
		{"true rendered by str", map[string]any{"cron": map[string]any{"model_provider": true}}, "True"},
	}
	for _, c := range cases {
		if got := DefaultProvider(c.cfg); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
