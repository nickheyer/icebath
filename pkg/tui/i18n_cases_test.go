package tui

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
	})
	t.Run("the generated keys are the entries of the catalog", func(t *testing.T) {
		keyed := map[string]bool{}
		for _, key := range txt.All() {
			keyed[key.Name()] = true
			_, isString := enCatalog.Strings[key.Name()]
			_, isFormat := enCatalog.Formats[key.Name()]
			if !isString && !isFormat {
				t.Errorf("key %s names no catalog entry, run go generate ./pkg/ui/txt", key.Name())
			}
		}
		for _, entries := range []map[string]string{enCatalog.Strings, enCatalog.Formats} {
			for name := range entries {
				if !keyed[name] {
					t.Errorf("catalog entry %s has no key, run go generate ./pkg/ui/txt", name)
				}
			}
		}
	})
	t.Run("the lists drawn from a group of the catalog hold entries", func(t *testing.T) {
		if len(composerHints()) == 0 || len(fortunes()) == 0 || len(legendaryFortunes()) == 0 {
			t.Fatalf("composer hints %d, fortunes %d, legendary fortunes %d", len(composerHints()), len(fortunes()), len(legendaryFortunes()))
		}
	})
	t.Run("flattenKeys emits sorted dotted leaves", func(t *testing.T) {
		if _, ok := enCatalog.Strings["status.ready"]; !ok {
			t.Fatal("status.ready missing")
		}
	})
}

func TestTUII18nRuntime(t *testing.T) {
	t.Run("resolves active \u2192 en, and a name the catalog lacks to its fallback", func(t *testing.T) {
		resetLocale(t)
		if tr(txt.StatusReady) != enCatalog.Strings["status.ready"] {
			t.Fatal("english")
		}
		applyLocale("pl", map[string]string{"status.ready": "gotowy"})
		if catalog.lang != "pl" || tr(txt.StatusReady) != "gotowy" || tr(txt.StatusRunning) != enCatalog.Strings["status.running"] || trOr("nope.missing", "fallback") != "fallback" {
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
		for _, name := range formats[:3] {
			key, known := txt.Named(name)
			applyLocale("pl", map[string]string{name: "X {0} Y"})
			if got := tr(key, "a"); !known || got != "X a Y" {
				t.Fatalf("%s got %q", name, got)
			}
		}
	})
	t.Run("drops pack keys that en does not know", func(t *testing.T) {
		resetLocale(t)
		applyLocale("pl", map[string]string{"status.bogus": "x", "status.ready": "ok"})
		if _, kept := catalog.pack["status.bogus"]; kept || tr(txt.StatusReady) != "ok" {
			t.Fatal("pack filtering")
		}
	})
	t.Run("capitals of titles follow the casing rules of the active language", func(t *testing.T) {
		resetLocale(t)
		applyLocale("tr", nil)
		if got := lang.Upper("kimlik"); got != "KİMLİK" {
			t.Fatalf("turkish %q", got)
		}
		applyLocale("de", nil)
		if got := lang.Upper("id"); got != "ID" {
			t.Fatalf("german %q", got)
		}
	})
}

func TestTUII18nLoader(t *testing.T) {
	t.Run("normalizes display.language like the backend", func(t *testing.T) {
		if normalizeLanguageID(" PT_BR ") != "pt-br" || normalizeLanguageID("") != "en" {
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
		if catalog.lang != "pl" || tr(txt.StatusReady) != enCatalog.Strings["status.ready"] {
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
		if strings.Join(calls, "|") != "i18n.catalog pl tui" || tr(txt.StatusReady) != "gotowy" {
			t.Fatalf("calls %q", calls)
		}
		syncTUILocale(request, "pl")
		syncTUILocale(request, "en")
		if len(calls) != 1 || tr(txt.StatusReady) != enCatalog.Strings["status.ready"] {
			t.Fatalf("calls %q", calls)
		}
	})
}

// Under a pack that words every catalog key, each key legend, key description, palette title and slash command help comes from the pack
func TestRegisteredActionsAndCommandsFollowTheLanguage(t *testing.T) {
	resetLocale(t)
	a := testApp(t)
	pack := map[string]string{}
	for k := range enCatalog.Strings {
		pack[k] = "\u2039" + k + "\u203a"
	}
	for k := range enCatalog.Formats {
		pack[k] = "\u2039" + k + "\u203a"
	}
	applyLocale("zz", pack)
	packed := func(text string) bool { return strings.Contains(text, "\u2039") }
	for _, action := range a.keys.Actions() {
		if action.Label != "" && !packed(action.LabelText()) {
			t.Errorf("key %s shows its registered label %q in every language", action.ID, action.Label)
		}
		if action.Doc != "" && !packed(action.DocText()) {
			t.Errorf("key %s shows its registered description %q in every language", action.ID, action.Doc)
		}
	}
	for _, c := range a.wm.Commands() {
		if c.Title != "" && !packed(c.TitleText()) {
			t.Errorf("palette command %s shows its registered title %q in every language", c.ID, c.Title)
		}
	}
	for _, c := range a.localCommands {
		if !packed(c.helpText()) {
			t.Errorf("/%s shows its registered help %q in every language", c.name, c.help)
		}
	}
}

func TestEnglishLogicEntries(t *testing.T) {
	t.Run("status.sessionCount picks the singular for one and a pack replaces both forms", func(t *testing.T) {
		resetLocale(t)
		if tr(txt.StatusSessionCount, 1) != "1 more session" || tr(txt.StatusSessionCount, 3) != "3 more sessions" {
			t.Fatalf("english plural %q %q", tr(txt.StatusSessionCount, 1), tr(txt.StatusSessionCount, 3))
		}
		applyLocale("de", map[string]string{"status.sessionCount": "{0} Sitzung(en)"})
		if tr(txt.StatusSessionCount, 1) != "1 Sitzung(en)" {
			t.Fatal("pack form")
		}
	})
}
