package provider

import (
	"strings"
	"testing"
)

func TestMissingProviderCredentialsMessageComesFromTheRegistry(t *testing.T) {
	ctx := modelsTestHome(t)
	if got := MissingProviderCredentialsMessage(ctx, "llamacpp"); !strings.Contains(got, "no local llama.cpp server is running") {
		t.Fatalf("llamacpp %q", got)
	}
	if got := MissingProviderCredentialsMessage(ctx, "no-such-provider"); !strings.Contains(got, "Set the NO_SUCH_PROVIDER_API_KEY environment variable, or switch") {
		t.Fatalf("unknown %q", got)
	}
}
