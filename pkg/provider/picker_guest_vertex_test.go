package provider

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestVertexHasCuratedModelList(t *testing.T) {
	models := staticCatalogModels("vertex")
	if len(models) == 0 {
		t.Fatal("the curated table must have a non-empty vertex row")
	}
	for _, m := range models {
		if !strings.HasPrefix(m, "google/") {
			t.Fatalf("%q lacks the google/ publisher prefix", m)
		}
	}
}

// Picker home with real overlays and curated catalogs and the vertex credential gate fixed
func vertexPickerHome(t *testing.T, hasCreds bool) context.Context {
	t.Helper()
	overlays := icebathOverlays
	ctx, _ := pickerTestHome(t, nil)
	providerModelIDsFn, icebathOverlays = ProviderModelIDs, overlays
	saved := pickerVertexCredentialsFn
	pickerVertexCredentialsFn = func(context.Context) bool { return hasCreds }
	t.Cleanup(func() { pickerVertexCredentialsFn = saved })
	return ctx
}

func TestVertexAppearsWhenCredentialsConfigured(t *testing.T) {
	ctx := vertexPickerHome(t, true)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", MaxModels: intPtr(50)})
	vertex := pickerRow(rows, "vertex")
	if vertex == nil {
		t.Fatal("vertex should appear when credentials are configured")
	}
	models := rowModels(vertex)
	if len(models) == 0 {
		t.Fatal("vertex row must carry the curated model list")
	}
	curated := staticCatalogModels("vertex")
	for _, m := range models {
		if !containsString(curated, m) {
			t.Fatalf("%q is not curated", m)
		}
	}
}

func TestVertexHiddenWithoutCredentials(t *testing.T) {
	ctx := vertexPickerHome(t, false)
	rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: "openrouter", MaxModels: intPtr(50)})
	if pickerRow(rows, "vertex") != nil {
		t.Fatal("vertex should not appear without credentials")
	}
}

// Guest that claimed the welcome trial as the only Nous state, served on the local Desktop backend, offline
func guestPickerHome(t *testing.T) (context.Context, string) {
	t.Helper()
	pickerTestHome(t, nil)
	h := home.Dir(context.Background())
	t.Setenv("ICEBATH_SHARED_AUTH_DIR", filepath.Join(t.TempDir(), "shared-store"))
	for _, v := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NOUS_API_KEY", "LM_API_KEY", "LM_BASE_URL"} {
		t.Setenv(v, "")
	}
	writeAuthStore(t, h, map[string]any{"active_provider": "nous", "providers": map[string]any{"nous": map[string]any{
		"auth_method": "anonymous", "account_tier": "anonymous", "anon_token": "anon_t", "access_token": "aaa.bbb.ccc",
		"expires_at": "2030-01-01T00:00:00+00:00", "inference_base_url": "https://welcome-api.nousresearch.com/v1",
		WelcomeClaimFlag: true}}})
	curatedNousModelIDsFn = func(context.Context) []string { return []string{"anthropic/claude-x", "openai/gpt-y"} }
	saved := pickerNousModelIDsFn
	pickerNousModelIDsFn = func(context.Context, map[string][]string, bool) []string {
		t.Error("guest must not fetch the Portal catalog")
		return nil
	}
	t.Cleanup(func() { pickerNousModelIDsFn = saved })
	return tools.WithServingLocalDesktop(context.Background(), true), h
}

func nousRows(rows []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if strings.ToLower(fmt.Sprint(r["slug"])) == "nous" {
			out = append(out, r)
		}
	}
	return out
}

func guestPickerRows(t *testing.T, ctx context.Context) []map[string]any {
	t.Helper()
	rows, err := ListPickerProviders(ctx, PickerProvidersOptions{CurrentProvider: "nous", MaxModels: intPtr(50),
		CurrentModel: "nous/welcome"})
	if err != nil {
		t.Fatal(err)
	}
	return nousRows(rows)
}

func TestGuestIdentityShowsFreeTierRowWithOnlyWelcomeModel(t *testing.T) {
	ctx, _ := guestPickerHome(t)
	rows := guestPickerRows(t, ctx)
	if len(rows) != 1 {
		t.Fatalf("nous rows %v", rows)
	}
	row := rows[0]
	if !strings.Contains(fmt.Sprint(row["name"]), "free tier") {
		t.Fatalf("name %v", row["name"])
	}
	if got := rowModels(row); len(got) != 1 || got[0] != "nous/welcome" {
		t.Fatalf("models %v", got)
	}
	if row["total_models"] != 1 {
		t.Fatalf("total_models %v", row["total_models"])
	}
	rendered := strings.ToLower(fmt.Sprint(row))
	if strings.Contains(rendered, "guest") || strings.Contains(rendered, "anonymous") {
		t.Fatalf("row names the guest identity %s", rendered)
	}
}

func TestGuestIdentityWithGuestOffHidesTheNousRow(t *testing.T) {
	ctx, h := guestPickerHome(t)
	writeConfigYAML(t, h, "nous:\n  guest: false\n")
	env := modelsPoolEnv(ctx)
	if !hasGuest(env) || profileFreeInferenceActive(env) {
		t.Fatal("guest off must keep the guest but end free inference")
	}
	if rows := guestPickerRows(t, ctx); len(rows) != 0 {
		t.Fatalf("nous rows %v", rows)
	}
}

func TestDesktopPickerPayloadNeverLocksTheFreeTierRow(t *testing.T) {
	ctx, _ := guestPickerHome(t)
	saved := inventoryPrewarmFn
	inventoryPrewarmFn = func(context.Context, []map[string]any, string, string) <-chan struct{} { return nil }
	t.Cleanup(func() { inventoryPrewarmFn = saved })
	pc, err := loadPickerContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := BuildModelOptionsPayload(ctx, pc, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := nousRows(payloadRows(payload))
	if len(rows) != 1 {
		t.Fatalf("nous rows %v", rows)
	}
	row := rows[0]
	if row["free_tier_row"] != true {
		t.Fatalf("free_tier_row %v", row["free_tier_row"])
	}
	if got := rowModels(row); len(got) != 1 || got[0] != "nous/welcome" {
		t.Fatalf("models %v", got)
	}
	if um, ok := row["unavailable_models"].([]any); ok && len(um) != 0 {
		t.Fatalf("unavailable %v", um)
	}
	if pyTruthy(row["free_tier_pending"]) || pyTruthy(row["pricing_pending"]) {
		t.Fatalf("pending flags %v %v", row["free_tier_pending"], row["pricing_pending"])
	}
}
