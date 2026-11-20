package provider

import (
	"context"
	"reflect"
	"testing"
)

// Resolves the provider client and returns the OpenAI wire client it built
func auxResolveOpenAIWireClient(t *testing.T, ctx context.Context, provider, model string) *auxOpenAIClient {
	t.Helper()
	client, _, err := ResolveProviderClient(ctx, provider, model, ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	oc, ok := client.(*auxOpenAIClient)
	if !ok {
		t.Fatalf("want the OpenAI wire client, got %T", client)
	}
	return oc
}

func TestApplyUserDefaultHeadersHelperNoneValuesSkipped(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: m\n  default_headers:\n    User-Agent: curl/8.7.1\n    X-Drop: null\n")
	merged := auxApplyUserDefaultHeaders(ctx, map[string]string{})
	if !reflect.DeepEqual(merged, map[string]string{"User-Agent": "curl/8.7.1"}) {
		t.Fatalf("merged %v", merged)
	}
}

func TestAuxClientHonorsUserDefaultHeadersCustomProviderOverridesSdkUserAgent(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "model:\n  default: my-custom-model\n  provider: custom\n  base_url: http://localhost:8080/v1\n  default_headers:\n    User-Agent: curl/8.7.1\n    X-Extra: \"1\"\n")
	oc := auxResolveOpenAIWireClient(t, ctx, "main", "my-custom-model")
	if oc.customHeaders["User-Agent"] != "curl/8.7.1" || oc.customHeaders["X-Extra"] != "1" {
		t.Fatalf("headers %v", oc.customHeaders)
	}
}

func TestAuxClientHonorsUserDefaultHeadersCustomProviderNoOverrideSendsNoUserAgent(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "model:\n  default: my-custom-model\n  provider: custom\n  base_url: http://localhost:8080/v1\n")
	oc := auxResolveOpenAIWireClient(t, ctx, "main", "my-custom-model")
	if _, ok := oc.customHeaders["User-Agent"]; ok {
		t.Fatalf("headers %v", oc.customHeaders)
	}
}

func TestAuxClientHonorsUserDefaultHeadersNamedCustomProviderHonorsOverride(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n  default_headers:\n    User-Agent: curl/8.7.1\ncustom_providers:\n  - name: my-gw\n    base_url: http://my-gw.local/v1\n    api_key: k\n")
	oc := auxResolveOpenAIWireClient(t, ctx, "my-gw", "test-model")
	if oc.customHeaders["User-Agent"] != "curl/8.7.1" {
		t.Fatalf("headers %v", oc.customHeaders)
	}
}
