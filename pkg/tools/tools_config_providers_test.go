package tools

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Picker row of a web backend with no keys and the extra markers in order
func webTestRow(backend string, extra ...any) *pyjson.Object {
	row := pyjson.NewObject()
	row.Set("name", backend)
	row.Set("web_backend", backend)
	row.Set("env_vars", []any{})
	for i := 0; i+1 < len(extra); i += 2 {
		row.Set(extra[i].(string), extra[i+1])
	}
	return row
}

// The autouse fixture of the web picker cases, no web credentials and a blank provider env lookup
func noWebEnv(t *testing.T) {
	t.Helper()
	unsetEnv(t, "EXA_API_KEY", "PARALLEL_API_KEY", "TAVILY_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL")
	swapHook(t, &webTierProviderEnv, func(context.Context, *Env, string) (string, error) { return "", nil })
}

func TestWebSelectionReportsTheBackendItWrites(t *testing.T) {
	for _, c := range []struct {
		managed bool
		backend string
	}{{true, "firecrawl"}, {false, "perplexity"}, {false, "exa"}} {
		t.Run(c.backend, func(t *testing.T) {
			s, out := newTestToolsSetup(t, "")
			swapHook(t, &nousProviderGateFn, func(*ToolsSetup, *pyjson.Object, config.Tree, string, bool) bool { return true })
			row := pyjson.NewObject()
			row.Set("name", "Test provider")
			row.Set("web_backend", c.backend)
			row.Set("env_vars", []any{})
			if c.managed {
				row.Set("managed_nous_feature", "web")
			}
			cfg := config.Tree{}
			if err := s.configureProvider(row, cfg, true, false); err != nil {
				t.Fatal(err)
			}
			selected, _ := config.CfgGet(cfg, "web", "backend")
			want := c.backend
			if c.managed {
				want = "nous"
			}
			if selected != want {
				t.Fatalf("web.backend %v, want %s", selected, want)
			}
			if !slices.Contains(successLines(out.String()), "  Web backend set to: "+want) {
				t.Fatalf("success lines %q", successLines(out.String()))
			}
		})
	}
}

func TestRowActiveIffItServesACapability(t *testing.T) {
	noWebEnv(t)
	s, _ := newTestToolsSetup(t, "")
	cases := []struct {
		web              map[string]any
		active, inactive []string
	}{
		{map[string]any{"search_backend": "firecrawl", "extract_backend": "tavily", "backend": "firecrawl"}, []string{"firecrawl", "tavily"}, []string{"ddgs"}},
		{map[string]any{"search_backend": "searxng", "extract_backend": "tavily", "backend": "firecrawl"}, []string{"searxng", "tavily"}, []string{"firecrawl"}},
		{map[string]any{"search_backend": "", "extract_backend": nil, "backend": "searxng"}, []string{"searxng"}, []string{"firecrawl"}},
		{map[string]any{"search_backend": " FireCrawl ", "backend": "tavily"}, []string{"firecrawl", "tavily"}, nil},
	}
	for _, c := range cases {
		cfg := config.Tree{"web": c.web}
		var missing, lit []string
		for _, b := range c.active {
			if ok, err := s.IsProviderActive(webTestRow(b), cfg, false); err != nil || !ok {
				missing = append(missing, b)
			}
		}
		for _, b := range c.inactive {
			if ok, err := s.IsProviderActive(webTestRow(b), cfg, false); err != nil || ok {
				lit = append(lit, b)
			}
		}
		if len(missing) > 0 || len(lit) > 0 {
			t.Fatalf("web %v: inactive serving rows %v, active idle rows %v", c.web, missing, lit)
		}
		tiered := config.Tree{"web": map[string]any{"search_backend": "parallel", "provider_tier": map[string]any{"parallel": "paid"}}}
		if ok, err := s.IsProviderActive(webTestRow("parallel", "web_tier", "paid"), tiered, false); err != nil || !ok {
			t.Fatalf("paid parallel row inactive %v", err)
		}
		if ok, err := s.IsProviderActive(webTestRow("parallel", "web_tier", "free"), tiered, false); err != nil || ok {
			t.Fatalf("free parallel row active %v", err)
		}
	}
}

func TestManagedRowActiveForANousCapabilityPin(t *testing.T) {
	noWebEnv(t)
	s, _ := newTestToolsSetup(t, "")
	swapHook(t, &ToolsConfigHooks.NousFeatures, func(context.Context, string, map[string]any, bool) NousToolFeatures {
		return NousToolFeatures{Features: map[string]NousToolFeature{"web": {ManagedByNous: true}}}
	})
	nous := webTestRow("firecrawl", "managed_nous_feature", "web")
	split := config.Tree{"web": map[string]any{"search_backend": "firecrawl", "extract_backend": "nous"}}
	if ok, err := s.IsProviderActive(nous, split, false); err != nil || !ok {
		t.Fatalf("managed row inactive on an extract pin %v", err)
	}
	if ok, err := s.IsProviderActive(webTestRow("firecrawl"), split, false); err != nil || !ok {
		t.Fatalf("byok row inactive while serving search %v", err)
	}
	vendors := config.Tree{"web": map[string]any{"search_backend": "firecrawl", "extract_backend": "tavily"}}
	if ok, err := s.IsProviderActive(nous, vendors, false); err != nil || ok {
		t.Fatalf("managed row active on a vendor only config %v", err)
	}
}

// Web picker rows by name over an empty web config, the catalog rows then the registry rows
func webRowsByName(t *testing.T, s *ToolsSetup) map[string]*pyjson.Object {
	t.Helper()
	rows, err := s.VisibleProviders(catalogCategory("web"), config.Tree{"web": map[string]any{}}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*pyjson.Object{}
	for _, row := range rows {
		out[rowText(row, "name")] = row
	}
	return out
}

func TestFirecrawlRowsSplitByTheCredentialSet(t *testing.T) {
	for _, c := range []struct {
		env    map[string]string
		active []string
	}{
		{map[string]string{"FIRECRAWL_API_KEY": "fc-x"}, []string{"Firecrawl"}},
		{map[string]string{"FIRECRAWL_API_URL": "http://localhost:3002"}, []string{"Firecrawl Self-Hosted"}},
		{map[string]string{}, []string{"Firecrawl"}},
	} {
		t.Run(c.active[0], func(t *testing.T) {
			noWebEnv(t)
			s, _ := newTestToolsSetup(t, "")
			writeHomeEnv(t, s.home(), c.env)
			rows := webRowsByName(t, s)
			cfg := config.Tree{"web": map[string]any{"backend": "firecrawl"}}
			var lit []string
			for _, name := range []string{"Firecrawl", "Firecrawl Self-Hosted"} {
				row, ok := rows[name]
				if !ok {
					t.Fatalf("no %s row in %v", name, sortedRowNames(rows))
				}
				active, err := s.IsProviderActive(row, cfg, false)
				if err != nil {
					t.Fatal(err)
				}
				if active {
					lit = append(lit, name)
				}
			}
			if !slices.Equal(lit, c.active) {
				t.Fatalf("active rows %v, want %v", lit, c.active)
			}
		})
	}
}

func sortedRowNames(rows map[string]*pyjson.Object) []string {
	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestKeylessRowReadinessFollowsRegistryAvailability(t *testing.T) {
	noWebEnv(t)
	s, _ := newTestToolsSetup(t, "")
	rows := webRowsByName(t, s)
	var native *pyjson.Object
	var free []*pyjson.Object
	for _, name := range sortedRowNames(rows) {
		row := rows[name]
		if rowGet(row, "web_backend") == "openai-native" {
			native = row
		}
		if rowGet(row, "web_tier") == "free" {
			free = append(free, row)
		}
	}
	if native == nil || len(free) == 0 {
		t.Fatalf("rows %v", sortedRowNames(rows))
	}
	cfg := config.Tree{"web": map[string]any{}}
	swapHook(t, &hasCodexCredentials, func(*Env) bool { return false })
	if got, err := s.ProviderReadinessStatus(native, cfg, nil, nil); err != nil || got != "needs_auth" {
		t.Fatalf("native readiness %q %v", got, err)
	}
	statuses := map[string]bool{}
	for _, row := range free {
		got, err := s.ProviderReadinessStatus(row, cfg, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		statuses[got] = true
	}
	if len(statuses) != 1 || !statuses["ready"] {
		t.Fatalf("free row statuses %v", statuses)
	}
	swapHook(t, &hasCodexCredentials, func(*Env) bool { return true })
	if got, err := s.ProviderReadinessStatus(native, cfg, nil, nil); err != nil || got != "ready" {
		t.Fatalf("native readiness after login %q %v", got, err)
	}
}
