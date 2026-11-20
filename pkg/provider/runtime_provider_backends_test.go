package provider

import (
	"context"
	"testing"
)

func TestIsExternalProcessProviderExportMatchesRegisteredProcessProviders(t *testing.T) {
	snapshotProviderRegistry(t)
	registerPluginForTest(t, Profile{Name: "acme-acp", AuthType: AuthTypeExternalProcess, BaseURL: "acp://acme", ProcessCommand: "acme"})
	ctx := context.Background()
	if !IsExternalProcessProvider(ctx, " ACME-ACP ") || IsExternalProcessProvider(ctx, "openrouter") || IsExternalProcessProvider(ctx, "") {
		t.Fatal("external process provider")
	}
}
