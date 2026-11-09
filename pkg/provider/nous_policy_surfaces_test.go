package provider

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

var nousPolicyCurated = []string{"vendor/allowed", "vendor/blocked"}

// Org policy seam admitting only allowed, nil for an ungoverned org
func fakeNousPolicy(t *testing.T, allowed map[string]bool) {
	t.Helper()
	prev := nousPolicyAllowedIDsFn
	nousPolicyAllowedIDsFn = func(context.Context, bool) (map[string]bool, bool) { return allowed, allowed != nil }
	t.Cleanup(func() { nousPolicyAllowedIDsFn = prev })
}

// Model ids the post login picker received after a stubbed Nous device login
func runNousPolicyLogin(t *testing.T, allowed map[string]bool) ([]string, string) {
	t.Helper()
	resetNousSeams(t)
	h := newTestHome(t)
	patchLoginInternals(t, "")
	nousLoginCuratedIDsFn = func(context.Context) []string { return slices.Clone(nousPolicyCurated) }
	nousLoginPolicyAllowedFn = func(context.Context) (map[string]bool, bool) { return allowed, allowed != nil }
	var seen []string
	scriptNousPicker(func(p ModelSelectionPrompt) (string, bool) {
		seen = append([]string(nil), p.ModelIDs...)
		return "", false
	})
	var out bytes.Buffer
	if err := LoginNous(testEnv(t, h), &out, NousLoginArgs{NoBrowser: true}); err != nil {
		t.Fatal(err)
	}
	return seen, out.String()
}

func TestLoginNousHiddenModelIsNotOffered(t *testing.T) {
	if got, _ := runNousPolicyLogin(t, map[string]bool{"vendor/allowed": true}); !slices.Equal(got, []string{"vendor/allowed"}) {
		t.Fatalf("offered %v", got)
	}
}

func TestLoginNousUnrestrictedOrgSeesTheFullCuratedList(t *testing.T) {
	if got, _ := runNousPolicyLogin(t, nil); !slices.Equal(got, nousPolicyCurated) {
		t.Fatalf("offered %v", got)
	}
}

// Nous row of the model switch picker with a signed in Nous store and the portal stubbed
func nousPolicyPickerRow(t *testing.T, allowed map[string]bool, pricing PricingTable) map[string]any {
	t.Helper()
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{"access_token": "tok"}}})
	fakeNousPolicy(t, allowed)
	prevCurated, prevPricing, prevTier, prevUnion := curatedNousModelIDsFn, pickerNousPricingFn, checkNousFreeTierFn, unionPortalPaidFn
	t.Cleanup(func() {
		curatedNousModelIDsFn, pickerNousPricingFn, checkNousFreeTierFn, unionPortalPaidFn = prevCurated, prevPricing, prevTier, prevUnion
	})
	curatedNousModelIDsFn = func(context.Context) []string { return slices.Clone(nousPolicyCurated) }
	pickerNousPricingFn = func(context.Context) PricingTable { return pricing }
	checkNousFreeTierFn = func(context.Context, bool, bool) bool { return false }
	unionPortalPaidFn = func(_ context.Context, ids []string, pricing PricingTable, _ string, _ bool) ([]string, PricingTable) {
		return slices.Clone(ids), pricing
	}
	maxModels := 10
	rows, err := ListAuthenticatedProviders(context.Background(), AuthenticatedProvidersOptions{MaxModels: &maxModels})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["slug"] == "nous" {
			return row
		}
	}
	t.Fatal("nous row should be listed")
	return nil
}

func TestModelSwitchPickerHiddenModelIsFiltered(t *testing.T) {
	models := rowModels(nousPolicyPickerRow(t, map[string]bool{"vendor/allowed": true}, PricingTable{}))
	if slices.Contains(models, "vendor/blocked") || !slices.Contains(models, "vendor/allowed") {
		t.Fatalf("models %v", models)
	}
}

func TestModelSwitchPickerUnrestrictedOrgKeepsBoth(t *testing.T) {
	models := rowModels(nousPolicyPickerRow(t, nil, PricingTable{}))
	if !slices.Contains(models, "vendor/allowed") || !slices.Contains(models, "vendor/blocked") {
		t.Fatalf("models %v", models)
	}
}

func TestModelSwitchPickerFilterSurvivesAFailedRecommendationFetch(t *testing.T) {
	models := rowModels(nousPolicyPickerRow(t, map[string]bool{"vendor/allowed": true}, nil))
	if slices.Contains(models, "vendor/blocked") {
		t.Fatalf("models %v", models)
	}
}

// Fast catalog pick for nous with the catalog ids served by the pricing fetch
func nousPolicyCatalog(t *testing.T, catalog []string) {
	t.Helper()
	prevFetch, prevCreds, prevBackground := auxFetchModelsWithPricingFn, auxResolveAPIKeyProviderCredentialsFn, resolveNousRuntimeCredentialsBackgroundFn
	t.Cleanup(func() {
		auxFetchModelsWithPricingFn, auxResolveAPIKeyProviderCredentialsFn, resolveNousRuntimeCredentialsBackgroundFn = prevFetch, prevCreds, prevBackground
	})
	resolveNousRuntimeCredentialsBackgroundFn = func(context.Context) (string, string, bool) {
		return "sk-nous", "https://inference.example.com", true
	}
	auxResolveAPIKeyProviderCredentialsFn = func(_ context.Context, provider string) (apiKeyCreds, error) {
		return apiKeyCreds{}, errors.New("not an api key provider: " + provider)
	}
	auxFetchModelsWithPricingFn = func(PricingFetchOptions) PricingTable {
		out := PricingTable{}
		for _, id := range catalog {
			out[id] = map[string]any{}
		}
		return out
	}
}

func TestAuxiliaryFastModelHiddenModelIsNotSelected(t *testing.T) {
	newTestHome(t)
	fakeNousPolicy(t, map[string]bool{"vendor/allowed": true})
	prevFamilies, prevExclude := auxFastModelFamilies, auxFastModelExclude
	t.Cleanup(func() { auxFastModelFamilies, auxFastModelExclude = prevFamilies, prevExclude })
	auxFastModelFamilies, auxFastModelExclude = []string{"vendor/"}, nil
	nousPolicyCatalog(t, []string{"vendor/blocked", "vendor/allowed"})
	if got := auxFastModelFromCatalog(context.Background(), "nous"); got != "vendor/allowed" {
		t.Fatalf("picked %q", got)
	}
}

func TestPolicyNoticeIsShownLoginPrintsIt(t *testing.T) {
	prev := nousPolicyPresentFn
	nousPolicyPresentFn = func(context.Context) (bool, bool) { return true, true }
	t.Cleanup(func() { nousPolicyPresentFn = prev })
	_, out := runNousPolicyLogin(t, map[string]bool{"vendor/allowed": true})
	notice := nousPolicyNotice(PoolEnv{}, true)
	if notice == "" || !strings.Contains(out, notice) {
		t.Fatalf("notice %q missing from %q", notice, out)
	}
}

// Nous aux model ladder with the org policy, a catalog without a fast tier and a profile recommendation
func nousAuxFallback(t *testing.T, allowed map[string]bool, recommended string) string {
	t.Helper()
	newTestHome(t)
	snapshotProviderRegistry(t)
	fakeNousPolicy(t, allowed)
	nousPolicyCatalog(t, []string{"vendor/allowed-large"})
	base, _ := GetProviderProfile(context.Background(), "nous")
	base.DefaultAuxModel = ""
	base.ResolveAuxModelHook = func(context.Context, bool) string { return recommended }
	registerPluginForTest(t, base)
	return auxAuxModelForProvider(context.Background(), "nous", true)
}

func TestAuxFallbackRespectsPolicyBlockedRecommendationIsNotUsed(t *testing.T) {
	if got := nousAuxFallback(t, map[string]bool{"vendor/allowed-large": true}, "vendor/blocked-haiku"); got != "" {
		t.Fatalf("picked %q", got)
	}
}

func TestAuxFallbackRespectsPolicyAllowedRecommendationStillUsed(t *testing.T) {
	if got := nousAuxFallback(t, map[string]bool{"vendor/allowed-large": true, "vendor/ok-haiku": true}, "vendor/ok-haiku"); got != "vendor/ok-haiku" {
		t.Fatalf("picked %q", got)
	}
}

func TestAuxFallbackRespectsPolicyUngovernedOrgIsUnaffected(t *testing.T) {
	if got := nousAuxFallback(t, nil, "vendor/anything"); got != "vendor/anything" {
		t.Fatalf("picked %q", got)
	}
}
