package provider

import (
	"strings"
	"testing"
)

func TestAuxProbeModeRepeatProbesStayResolvableAndNeverCache(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		auxIsolateClientCache(t)
		ctx := auxMainTestHome(t, "")
		ShutdownCachedClients(ctx)
		auxFakeResolveProviderClient(t, func(string, string, ProviderClientOptions) (AuxClient, string) {
			probe := &auxProbeClient{apiKey: "k", baseURL: "https://x.invalid/v1"}
			if wrap {
				return NewCodexAuxiliaryClient(probe, "v/m"), "v/m"
			}
			return probe, "v/m"
		})
		for i := 0; i < 3; i++ {
			client, _, err := auxGetCachedClient(AuxProbeMode(ctx), auxCachedClientRequest{Provider: "probe-guard-test", Model: "vendor/model"})
			if err != nil || client == nil {
				t.Fatalf("wrap %v probe %d: %v %v", wrap, i, client, err)
			}
		}
		auxClientCacheMu.Lock()
		for k := range auxClientCache {
			if !k.Async && k.Model == "vendor/model" {
				auxClientCacheMu.Unlock()
				t.Fatalf("wrap %v: probe landed under the runtime key %+v", wrap, k)
			}
		}
		auxClientCacheMu.Unlock()
	}
}

func TestAuxProbeModeProbeStubRaisesOnRuntimeUse(t *testing.T) {
	_, err := (&auxProbeClient{}).CreateChatCompletion(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "availability checks only") {
		t.Fatalf("probe client must fail loudly on runtime use, got %v", err)
	}
}
