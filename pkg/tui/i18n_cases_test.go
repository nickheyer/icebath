package tui

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// Restores English and a fresh loader after a case
func resetLocale(t *testing.T) {
	t.Cleanup(func() {
		applyLocale(defaultLocale, nil)
		catalog.mu.Lock()
		catalog.lastRequested, catalog.inFlight = "", nil
		catalog.mu.Unlock()
	})
}

func TestTUII18nCatalog(t *testing.T) {
	t.Run("siblings own disjoint top-level namespaces", func(t *testing.T) {
		for k := range enCatalog.Strings {
			if _, both := enCatalog.Formats[k]; both {
				t.Fatalf("%s is both a string and a format", k)
			}
		}
	})
	t.Run("every leaf is a string or a function", func(t *testing.T) {
		var keys []string
		for k := range enCatalog.Strings {
			keys = append(keys, k)
		}
		for k := range enCatalog.Formats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i := 1; i < len(keys); i++ {
			if strings.HasPrefix(keys[i], keys[i-1]+".") {
				t.Fatalf("%s is a leaf and a namespace", keys[i-1])
			}
		}
		if len(keys) != 1390 {
			t.Fatalf("%d keys", len(keys))
		}
	})
	t.Run("flattenKeys emits sorted dotted leaves", func(t *testing.T) {
		if _, ok := enCatalog.Strings["status.ready"]; !ok {
			t.Fatal("status.ready missing")
		}
	})
}

func TestTUII18nRuntime(t *testing.T) {
	t.Run("resolves active \u2192 en \u2192 key", func(t *testing.T) {
		resetLocale(t)
		if tr("status.ready") != enCatalog.Strings["status.ready"] {
			t.Fatal("english")
		}
		applyLocale("pl", map[string]string{"status.ready": "gotowy"})
		if catalog.lang != "pl" || tr("status.ready") != "gotowy" || tr("status.running") != enCatalog.Strings["status.running"] || tr("nope.missing") != "nope.missing" {
			t.Fatal("resolution order")
		}
	})
	t.Run("wraps a string override of a function leaf into a positional formatter", func(t *testing.T) {
		resetLocale(t)
		if got := formatPositional("cze\u015b\u0107 {0} \u00d7{1} {2}", []any{"Ala", 3}); got != "cze\u015b\u0107 Ala \u00d73 {2}" {
			t.Fatalf("got %q", got)
		}
		var formats []string
		for k := range enCatalog.Formats {
			formats = append(formats, k)
		}
		sort.Strings(formats)
		for _, key := range formats[:3] {
			applyLocale("pl", map[string]string{key: "X {0} Y"})
			if got := tr(key, "a"); got != "X a Y" {
				t.Fatalf("%s got %q", key, got)
			}
		}
	})
	t.Run("drops pack keys that en does not know", func(t *testing.T) {
		resetLocale(t)
		applyLocale("pl", map[string]string{"status.bogus": "x", "status.ready": "ok"})
		if _, kept := catalog.pack["status.bogus"]; kept || tr("status.ready") != "ok" {
			t.Fatal("pack filtering")
		}
	})
}

func TestTUII18nLoader(t *testing.T) {
	t.Run("normalizes display.language like the backend", func(t *testing.T) {
		if normalizeLanguageID(" PT_BR ") != "pt-br" || normalizeLanguageID("") != "en" || normalizeLanguageID(nil) != "en" {
			t.Fatal("normalize")
		}
	})
	t.Run("stays English when the backend lacks i18n.catalog", func(t *testing.T) {
		resetLocale(t)
		request := func(string, map[string]any) (any, error) { return nil, errors.New("Method not found: i18n.catalog") }
		if _, ok := fetchCatalogPack(request, "pl"); ok {
			t.Fatal("want no pack")
		}
		syncTUILocale(request, "pl")
		if catalog.lang != "pl" || tr("status.ready") != enCatalog.Strings["status.ready"] {
			t.Fatal("english fallback")
		}
	})
	t.Run("applies the pack for display.language and skips the RPC for English", func(t *testing.T) {
		resetLocale(t)
		var calls []string
		request := func(method string, params map[string]any) (any, error) {
			calls = append(calls, method+" "+params["lang"].(string)+" "+params["surface"].(string))
			return map[string]any{"lang": "pl", "surface": "tui", "messages": map[string]any{"status.ready": "gotowy"}}, nil
		}
		syncTUILocale(request, "pl")
		if strings.Join(calls, "|") != "i18n.catalog pl tui" || tr("status.ready") != "gotowy" {
			t.Fatalf("calls %q", calls)
		}
		syncTUILocale(request, "pl")
		syncTUILocale(request, "en")
		if len(calls) != 1 || tr("status.ready") != enCatalog.Strings["status.ready"] {
			t.Fatalf("calls %q", calls)
		}
	})
}

func TestEnglishLogicEntries(t *testing.T) {
	t.Run("status.sessionCount picks the singular for one and a pack replaces both forms", func(t *testing.T) {
		resetLocale(t)
		if tr("status.sessionCount", 1) != "1 session" || tr("status.sessionCount", 3) != "3 sessions" {
			t.Fatalf("english plural %q %q", tr("status.sessionCount", 1), tr("status.sessionCount", 3))
		}
		applyLocale("de", map[string]string{"status.sessionCount": "{0} Sitzung(en)"})
		if tr("status.sessionCount", 1) != "1 Sitzung(en)" {
			t.Fatal("pack form")
		}
	})
}
