package tools

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Picker row of a web backend with no keys and the extra markers in order
func webTestRow(backend string, extra ...any) *jsonutil.Object {
	row := jsonutil.NewObject()
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
		backend string
	}{{"firecrawl"}, {"perplexity"}, {"exa"}} {
		t.Run(c.backend, func(t *testing.T) {
			s, out := newTestToolsSetup(t, "")
			row := jsonutil.NewObject()
			row.Set("name", "Test provider")
			row.Set("web_backend", c.backend)
			row.Set("env_vars", []any{})
			cfg := config.Tree{}
			if err := s.configureProvider(row, cfg, false); err != nil {
				t.Fatal(err)
			}
			selected, _ := config.CfgGet(cfg, "web", "backend")
			want := c.backend
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
			if ok, err := s.IsProviderActive(webTestRow(b), cfg); err != nil || !ok {
				missing = append(missing, b)
			}
		}
		for _, b := range c.inactive {
			if ok, err := s.IsProviderActive(webTestRow(b), cfg); err != nil || ok {
				lit = append(lit, b)
			}
		}
		if len(missing) > 0 || len(lit) > 0 {
			t.Fatalf("web %v: inactive serving rows %v, active idle rows %v", c.web, missing, lit)
		}
		tiered := config.Tree{"web": map[string]any{"search_backend": "parallel", "provider_tier": map[string]any{"parallel": "paid"}}}
		if ok, err := s.IsProviderActive(webTestRow("parallel", "web_tier", "paid"), tiered); err != nil || !ok {
			t.Fatalf("paid parallel row inactive %v", err)
		}
		if ok, err := s.IsProviderActive(webTestRow("parallel", "web_tier", "free"), tiered); err != nil || ok {
			t.Fatalf("free parallel row active %v", err)
		}
	}
}

// Web picker rows by name over an empty web config, the catalog rows then the registry rows
func webRowsByName(t *testing.T, s *ToolsSetup) map[string]*jsonutil.Object {
	t.Helper()
	rows, err := s.VisibleProviders(catalogCategory("web"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*jsonutil.Object{}
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
				active, err := s.IsProviderActive(row, cfg)
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

func sortedRowNames(rows map[string]*jsonutil.Object) []string {
	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
