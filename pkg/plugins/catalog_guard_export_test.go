package plugins

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Exported guards refuse kill listed names and entries the platform or version gate rejects, passing the rest
func TestCatalogGuardExportsRefuseRemovedAndIncompatibleEntries(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	savedURL, savedClient, savedVersion := LiveCatalogURL, liveCatalogClient, RunningVersion
	t.Cleanup(func() { LiveCatalogURL, liveCatalogClient, RunningVersion = savedURL, savedClient, savedVersion })
	LiveCatalogURL = "http://127.0.0.1:9/nope"
	liveCatalogClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	ctx := context.Background()
	var opErr *PluginOperationError
	err := RaiseIfRemoved(ctx, "fresh-plugin", "or-picker")
	if !errors.As(err, &opErr) || opErr.FailureClass != "removed_from_catalog" ||
		!strings.HasPrefix(opErr.Msg, "Plugin 'or-picker' was removed from the Icebath plugin catalog and is blocked from installation: Folded into openrouter-picker.") ||
		!strings.HasSuffix(opErr.Msg, "(removed 2026-09-28)") {
		t.Fatalf("kill listed candidate answered %v", err)
	}
	if err := RaiseIfRemoved(ctx, "fresh-plugin"); err != nil {
		t.Fatalf("a candidate off the kill list answered %v", err)
	}
	RunningVersion = "0.30.0"
	err = RefuseIncompatibleCatalogEntry(ctx, &PluginCatalogEntry{Name: "needs-new", RequiresIcebath: ">=99.0"})
	if !errors.As(err, &opErr) || opErr.FailureClass != "incompatible" ||
		!strings.HasPrefix(opErr.Msg, "needs-new needs Icebath >=99.0; this is 0.30.0. Update Icebath (`") ||
		!strings.HasSuffix(opErr.Msg, "`) and install again.") {
		t.Fatalf("version gated entry answered %v", err)
	}
	err = RefuseIncompatibleCatalogEntry(ctx, &PluginCatalogEntry{Name: "elsewhere", Platforms: []string{"no-such-os"}})
	if !errors.As(err, &opErr) || opErr.FailureClass != "incompatible" ||
		!strings.HasPrefix(opErr.Msg, "Plugin 'elsewhere' is unavailable on ") || !strings.HasSuffix(opErr.Msg, "; supported platforms: no-such-os.") {
		t.Fatalf("platform gated entry answered %v", err)
	}
	if err := RefuseIncompatibleCatalogEntry(ctx, &PluginCatalogEntry{Name: "fine", RequiresIcebath: ">=0.1"}); err != nil {
		t.Fatalf("a compatible entry answered %v", err)
	}
}
