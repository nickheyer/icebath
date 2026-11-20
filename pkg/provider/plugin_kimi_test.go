package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestKimiPluginProfilesMatchUpstreamRegistration(t *testing.T) {
	for _, c := range []struct {
		name    string
		aliases []string
		keys    []string
		base    string
	}{
		{"kimi-coding", []string{"kimi", "moonshot", "kimi-for-coding"}, []string{"KIMI_API_KEY", "KIMI_CODING_API_KEY"}, "https://api.moonshot.ai/v1"},
		{"kimi-coding-cn", []string{"kimi-cn", "moonshot-cn"}, []string{"KIMI_CN_API_KEY"}, "https://api.moonshot.cn/v1"},
	} {
		p := bundledPluginProfile(t, c.name)
		assertProfileFields(t, p, map[string]any{"name": c.name, "aliases": c.aliases, "env_vars": c.keys, "base_url": c.base,
			"default_max_tokens": 32000, "default_aux_model": "kimi-k2-turbo-preview", "base_url_env_var": "", "display_name": ""})
		if v, omit := p.TemperatureOverride(); v != nil || !omit {
			t.Fatalf("%s temperature %v %v", c.name, v, omit)
		}
		enc := ""
		for k, v := range p.DefaultHeaders() {
			if strings.ToLower(k) == "accept-encoding" {
				enc = strings.ToLower(v)
			}
		}
		if strings.Contains(enc, "br") || !strings.Contains(enc, "gzip") {
			t.Fatalf("%s accept-encoding %q", c.name, enc)
		}
	}
}

func TestKimiModelDiscoveryMalformedBaseUrlIsUnconfirmedAndFiltersK3(t *testing.T) {
	fake := func(context.Context, string, string, time.Duration) ([]string, bool) {
		return []string{"k3", "kimi-k2.6"}, true
	}
	got, ok := kimiFetchModels(context.Background(), fake, "https://api.moonshot.ai/v1", "test-key", "https://[api.kimi.com/coding", 0)
	if !ok || !reflect.DeepEqual(got, []string{"kimi-k2.6"}) {
		t.Fatalf("models %v %v", got, ok)
	}
}

func TestKimiConfirmedCodingEndpointKeepsK3AndGainsV1(t *testing.T) {
	var seen string
	fake := func(_ context.Context, _ string, base string, _ time.Duration) ([]string, bool) {
		seen = base
		return []string{"k3", "kimi-k2.6"}, true
	}
	got, ok := kimiFetchModels(context.Background(), fake, "https://api.moonshot.ai/v1", "k", "https://api.kimi.com/coding/", 0)
	if !ok || !reflect.DeepEqual(got, []string{"k3", "kimi-k2.6"}) || seen != "https://api.kimi.com/coding/v1" {
		t.Fatalf("models %v %v base %s", got, ok, seen)
	}
	for raw, want := range map[string]bool{
		"https://api.kimi.com/coding/v1": true, "https://api.kimi.com:443/coding": true, "http://api.kimi.com/coding": false,
		"https://user@api.kimi.com/coding": false, "https://api.kimi.com/coding?x=1": false, "https://api.kimi.com/coding#f": false,
		"https://api.kimi.com:8443/coding": false, "https://api.kimi.com/other": false,
	} {
		if isConfirmedKimiCodingURL(raw) != want {
			t.Fatalf("%s confirmed != %v", raw, want)
		}
	}
}
