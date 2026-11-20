package provider

import (
	"context"
	"testing"
)

func TestRegistryInferenceBaseURLsFollowRegistryOrder(t *testing.T) {
	if got, ok := RegistryInferenceBaseURL("openai-api"); !ok || got != "https://api.openai.com/v1" {
		t.Fatalf("openai-api %q %v", got, ok)
	}
	if got, ok := RegistryInferenceBaseURL("vertex"); !ok || got != "" {
		t.Fatalf("vertex computes its endpoint per request %q %v", got, ok)
	}
	if got, ok := RegistryInferenceBaseURL("no-such-provider"); ok || got != "" {
		t.Fatalf("unregistered id %q %v", got, ok)
	}
	items := providerRegistry().items(context.Background())
	urls := RegistryInferenceBaseURLs()
	if len(urls) == 0 || len(urls) != len(items) {
		t.Fatalf("%d urls for %d registry rows", len(urls), len(items))
	}
	for i, it := range items {
		want := it.Row.BaseURL
		if it.Row.AuthType == AuthTypeVertex {
			want = ""
		}
		if urls[i] != want {
			t.Fatalf("%s at %d got %q want %q", it.Key, i, urls[i], want)
		}
	}
}
