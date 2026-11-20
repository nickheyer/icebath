package provider

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func resetPickerPrewarmGuard(t *testing.T) {
	t.Helper()
	pickerPrewarmDone.Store(false)
	t.Cleanup(func() { pickerPrewarmDone.Store(false) })
}

func TestPrewarmGuardIsOncePerProcess(t *testing.T) {
	pickerTestHome(t, nil)
	resetPickerPrewarmGuard(t)
	ctx := context.Background()
	first := PrewarmPickerCacheAsync(ctx)
	if first == nil {
		t.Fatal("first prewarm must start a worker")
	}
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("prewarm worker never finished")
	}
	if PrewarmPickerCacheAsync(ctx) != nil || PrewarmPickerCacheAsync(ctx) != nil {
		t.Fatal("repeat prewarm calls must be no ops")
	}
}

func TestPrewarmWarmsTheActiveCustomEndpointForTheNextOpen(t *testing.T) {
	const baseURL = "https://api.example-gateway.test/v1"
	var mu sync.Mutex
	ctx, calls := pickerTestHome(t, func(apiKey, url string) ([]string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return []string{"gateway-model-a", "gateway-model-b"}, true
	})
	resetPickerPrewarmGuard(t)
	writeConfigYAML(t, home.Dir(ctx), "model:\n  provider: custom:example-gateway\n  base_url: "+baseURL+"\n"+
		"custom_providers:\n- name: example-gateway\n  base_url: "+baseURL+"\n  api_key: sk-gateway-key\n")
	pc := invLoadContext(t, ctx)
	pc.CurrentModel = ""
	openPicker := func() map[string]any {
		rows := mustListProviders(t, ctx, AuthenticatedProvidersOptions{CurrentProvider: pc.CurrentProvider,
			CurrentBaseURL: pc.CurrentBaseURL, UserProviders: pc.UserProviders, CustomProviders: pc.CustomProviders,
			ExcludedProviders: pc.ExcludedProviders, SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true})
		for _, r := range rows {
			if r["api_url"] == baseURL {
				return r
			}
		}
		t.Fatalf("no row for %s", baseURL)
		return nil
	}
	callCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(*calls)
	}
	done := PrewarmPickerCacheAsync(ctx)
	if done == nil {
		t.Fatal("prewarm must start a worker")
	}
	<-done
	want := []string{"gateway-model-a", "gateway-model-b"}
	if row := openPicker(); callCount() != 1 || !reflect.DeepEqual(rowModels(row), want) {
		t.Fatalf("first open calls %d models %v", callCount(), row["models"])
	}
	if row := openPicker(); callCount() != 1 || !reflect.DeepEqual(rowModels(row), want) {
		t.Fatalf("second open calls %d models %v", callCount(), row["models"])
	}
}

func excludedCtx(excluded []string, currentProvider, currentModel string) PickerConfigContext {
	return PickerConfigContext{CurrentProvider: currentProvider, CurrentModel: currentModel, UserProviders: map[string]any{},
		CustomProviders: []any{}, ExcludedProviders: excluded}
}

func skeletonSlugs(t *testing.T, pc PickerConfigContext) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, r := range appendUnconfiguredRows(context.Background(), nil, pc, false) {
		out[strings.ToLower(pyStr(r["slug"]))] = true
	}
	return out
}

func TestUnconfiguredRowsHideExcludedProvider(t *testing.T) {
	invTestHome(t)
	baseline := skeletonSlugs(t, excludedCtx(nil, "", ""))
	if !baseline["openrouter"] {
		t.Fatal("sanity: openrouter should be a canonical skeleton row")
	}
	slugs := skeletonSlugs(t, excludedCtx([]string{"openrouter"}, "", ""))
	delete(baseline, "openrouter")
	if !reflect.DeepEqual(slugs, baseline) {
		t.Fatalf("only the excluded provider should be removed %v", slugs)
	}
}

func TestUnconfiguredRowsExclusionIsCaseInsensitive(t *testing.T) {
	invTestHome(t)
	if skeletonSlugs(t, excludedCtx([]string{"OpenRouter"}, "", ""))["openrouter"] {
		t.Fatal("excluded provider re-added")
	}
}

func TestUnconfiguredRowsHideExcludedProviderByAlias(t *testing.T) {
	invTestHome(t)
	baseline := skeletonSlugs(t, excludedCtx(nil, "", ""))
	canonical := map[string]bool{}
	for _, p := range CanonicalProviders() {
		canonical[p.Slug] = true
	}
	aliases := CatalogProviderAliases()
	names := make([]string, 0, len(aliases))
	for alias := range aliases {
		names = append(names, alias)
	}
	sort.Strings(names)
	targetSlug, targetAlias := "", ""
	for _, alias := range names {
		if canon := aliases[alias]; canon != "" && baseline[strings.ToLower(canon)] && canonical[canon] {
			targetSlug, targetAlias = canon, alias
			break
		}
	}
	if targetSlug == "" {
		t.Fatal("no aliased canonical provider present as an unconfigured row")
	}
	if skeletonSlugs(t, excludedCtx([]string{targetAlias}, "", ""))[strings.ToLower(targetSlug)] {
		t.Fatalf("excluding alias %q should hide canonical %q", targetAlias, targetSlug)
	}
}

func TestUnconfiguredRowsExcludeCurrentProviderMatchesCLI(t *testing.T) {
	invTestHome(t)
	if skeletonSlugs(t, excludedCtx([]string{"openrouter"}, "openrouter", "some-model"))["openrouter"] {
		t.Fatal("excluded current provider resurfaced as a skeleton row")
	}
}

func TestUnconfiguredRowsEmptyExcludedIsNoop(t *testing.T) {
	invTestHome(t)
	base := skeletonSlugs(t, excludedCtx(nil, "", ""))
	if empty := skeletonSlugs(t, excludedCtx([]string{}, "", "")); !reflect.DeepEqual(base, empty) {
		t.Fatalf("empty %v base %v", empty, base)
	}
}
