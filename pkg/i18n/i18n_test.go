package i18n

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
)

const approvalKey = "approval.denied"

// Raw catalog flattened keeping every leaf, the shape the parity tests compare
func rawCatalog(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("locales", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loadYAML11(data)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	var walk func(node any, prefix string)
	walk = func(node any, prefix string) {
		m, ok := node.(pyMap)
		if !ok {
			out[prefix] = node
			return
		}
		for _, p := range m {
			key := pyrepr.Str(p.Key)
			if prefix != "" {
				key = prefix + "." + key
			}
			walk(p.Value, key)
		}
	}
	if doc != nil {
		walk(doc, "")
	}
	return out
}

// Fresh home with caches and the pack registry cleared before and after the test
func tempHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(h, "locales"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_LANGUAGE", "")
	os.Unsetenv("ICEBATH_LANGUAGE")
	ResetRegistryForTests()
	ResetLanguageCache()
	t.Cleanup(func() {
		ResetRegistryForTests()
		ResetLanguageCache()
	})
	return h
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func en(key string) string { return T(context.Background(), key, "en", nil) }

func TestCatalogKeysMatchEnglish(t *testing.T) {
	enKeys := rawCatalog(t, "en")
	for _, lang := range SupportedLanguages {
		if lang == "en" {
			continue
		}
		got := rawCatalog(t, lang)
		var missing, extra []string
		for k := range enKeys {
			if _, ok := got[k]; !ok {
				missing = append(missing, k)
			}
		}
		for k := range got {
			if _, ok := enKeys[k]; !ok {
				extra = append(extra, k)
			}
		}
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s.yaml missing %v extra %v", lang, missing, extra)
		}
	}
}

func TestCatalogPlaceholdersMatchEnglish(t *testing.T) {
	re := regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	set := func(v any) map[string]bool {
		s, _ := v.(string)
		out := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			out[m[1]] = true
		}
		return out
	}
	enFlat := rawCatalog(t, "en")
	for _, lang := range SupportedLanguages {
		flat := rawCatalog(t, lang)
		for key, v := range enFlat {
			if !reflect.DeepEqual(set(v), set(flat[key])) {
				t.Errorf("%s.yaml key=%q placeholders %v don't match English %v", lang, key, set(flat[key]), set(v))
			}
		}
	}
}

func TestCatalogValuesHaveNoEscapedNewlines(t *testing.T) {
	for _, lang := range SupportedLanguages {
		for key, v := range rawCatalog(t, lang) {
			if s, ok := v.(string); ok && strings.Contains(s, `\n`) {
				t.Errorf("%s.yaml has literal \\n in %s", lang, key)
			}
		}
	}
}

func TestDefaultWhenNothingSet(t *testing.T) {
	tempHome(t)
	prev := configLangFn
	configLangFn = func(context.Context, string) (string, bool) { return "", false }
	t.Cleanup(func() { configLangFn = prev })
	if got := GetLanguage(context.Background()); got != "en" {
		t.Fatalf("got %q", got)
	}
}

func TestLanguageIsPerProfileUnderMultiplex(t *testing.T) {
	tempHome(t)
	root := t.TempDir()
	defHome, profB := filepath.Join(root, "default"), filepath.Join(root, "b")
	writeFile(t, filepath.Join(defHome, "config.yaml"), "display:\n  language: fr\n")
	writeFile(t, filepath.Join(profB, "config.yaml"), "display:\n  language: de\n")
	t.Setenv("ICEBATH_LANGUAGE", "zh")
	t.Setenv("ICEBATH_HOME", defHome)
	ResetLanguageCache()
	SetSecretReader(func(context.Context, string) (string, bool, error) { return "", false, nil })
	t.Cleanup(func() { SetSecretReader(nil) })
	ctx := context.Background()
	if got := GetLanguage(ctx); got != "fr" {
		t.Fatalf("scoped miss got %q", got)
	}
	if got := GetLanguage(home.WithOverride(ctx, profB)); got != "de" {
		t.Fatalf("second profile got %q", got)
	}
}

func TestUnscopedSecretReadsProcessEnvironment(t *testing.T) {
	tempHome(t)
	t.Setenv("ICEBATH_LANGUAGE", "ja")
	SetSecretReader(func(context.Context, string) (string, bool, error) { return "", false, ErrUnscopedSecret })
	t.Cleanup(func() { SetSecretReader(nil) })
	if got := GetLanguage(context.Background()); got != "ja" {
		t.Fatalf("got %q", got)
	}
}

func TestTMissingKeyInNonEnglishFallsBackToEnglish(t *testing.T) {
	tempHome(t)
	dir := filepath.Join(t.TempDir(), "locales")
	writeFile(t, filepath.Join(dir, "en.yaml"), "foo: English Foo\n")
	writeFile(t, filepath.Join(dir, "zh.yaml"), "# intentionally empty\n")
	prev := localesDirFn
	localesDirFn = func() BundledLocales { return BundledLocales{FS: os.DirFS(dir), Label: dir} }
	t.Cleanup(func() { localesDirFn = prev; ResetLanguageCache() })
	ResetLanguageCache()
	if got := T(context.Background(), "foo", "zh", nil); got != "English Foo" {
		t.Fatalf("got %q", got)
	}
}

func TestLocalesDirEnvOverrideIgnoredWhenMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("ICEBATH_BUNDLED_LOCALES", missing)
	got := localesDir()
	if got.Label == missing || filepath.Base(got.Label) != "locales" {
		t.Fatalf("got %q", got.Label)
	}
	t.Setenv("ICEBATH_BUNDLED_LOCALES", "locales")
	if got := localesDir(); got.Label != "locales" {
		t.Fatalf("existing override got %q", got.Label)
	}
}

func TestTFormatsKwargsAndKeepsTemplateOnFailure(t *testing.T) {
	tempHome(t)
	ctx := context.Background()
	if got := T(ctx, "no.such.key", "en", nil); got != "no.such.key" {
		t.Fatalf("bare key got %q", got)
	}
	tmpl := en("cli.shared.n_more")
	if got := T(ctx, "cli.shared.n_more", "en", map[string]any{"count": "3"}); got != strings.ReplaceAll(tmpl, "{count}", "3") {
		t.Fatalf("formatted got %q from %q", got, tmpl)
	}
	if got := T(ctx, "cli.shared.n_more", "en", map[string]any{"other": 1}); got != tmpl {
		t.Fatalf("missing kwarg got %q", got)
	}
}

func TestUserOverlayOverridesBundledAndIsPartial(t *testing.T) {
	h := tempHome(t)
	writeFile(t, filepath.Join(h, "locales", "de.yaml"), "approval:\n  denied: Überschrieben\n")
	ResetLanguageCache()
	ctx := context.Background()
	if got := T(ctx, approvalKey, "de", nil); got != "Überschrieben" {
		t.Fatalf("got %q", got)
	}
	bundled := LoadBundled("de")
	for _, k := range sortedMapKeys(bundled) {
		if k != approvalKey && bundled[k] != en(k) {
			if got := T(ctx, k, "de", nil); got != bundled[k] {
				t.Fatalf("%s got %q", k, got)
			}
			return
		}
	}
	t.Fatal("no distinct German key")
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstOtherKey() string {
	for _, k := range sortedMapKeys(LoadBundled("en")) {
		if k != approvalKey {
			return k
		}
	}
	return ""
}

func TestOverlayOnlyLanguageIsSupportedAndFallsBackToEnglish(t *testing.T) {
	h := tempHome(t)
	writeFile(t, filepath.Join(h, "locales", "eo.yaml"), "approval:\n  denied: Esperanto titolo\n")
	ResetLanguageCache()
	ctx := context.Background()
	langs := SupportedLanguagesFor(ctx, "")
	if langs[0] != "en" || !contains(langs, "eo") {
		t.Fatalf("supported %v", langs)
	}
	if got := T(ctx, approvalKey, "eo", nil); got != "Esperanto titolo" {
		t.Fatalf("got %q", got)
	}
	other := firstOtherKey()
	if got := T(ctx, other, "eo", nil); got != en(other) {
		t.Fatalf("fallback got %q", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestOverlayIsProfileScopedAcrossTwoHomes(t *testing.T) {
	tempHome(t)
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "a"), filepath.Join(root, "b")
	writeFile(t, filepath.Join(homeA, "locales", "de.yaml"), "approval:\n  denied: Nur A\n")
	os.MkdirAll(homeB, 0o755)
	t.Setenv("ICEBATH_HOME", homeA)
	ResetLanguageCache()
	ctx := context.Background()
	bundled := LoadBundled("de")[approvalKey]
	if got := T(ctx, approvalKey, "de", nil); got != "Nur A" {
		t.Fatalf("A got %q", got)
	}
	ctxB := home.WithOverride(ctx, homeB)
	if got := T(ctxB, approvalKey, "de", nil); got != bundled {
		t.Fatalf("B got %q", got)
	}
	if contains(SupportedLanguagesFor(ctxB, ""), "eo") {
		t.Fatal("eo leaked into B")
	}
	if got := T(ctx, approvalKey, "de", nil); got != "Nur A" {
		t.Fatalf("A again got %q", got)
	}
}

func TestPackRegistersLanguageAndDisplayLanguageResolves(t *testing.T) {
	h := tempHome(t)
	ctx := context.Background()
	if contains(SupportedLanguagesFor(ctx, ""), "pl") {
		t.Fatal("pl before registration")
	}
	src := "plugin:icebath-lang-pl"
	for surface, msgs := range map[string]map[string]any{
		"core": {"approval": map[string]any{"denied": "Zatwierdzenie"}}, "tui": {"status": map[string]any{"ready": "Gotowy"}},
		"desktop": {"settings": map[string]any{"title": "Ustawienia"}}} {
		flat, err := LoadLocaleSource(msgs)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RegisterPack("pl", surface, flat, src, "", false); err != nil {
			t.Fatal(err)
		}
	}
	if !contains(SupportedLanguagesFor(ctx, ""), "pl") || T(ctx, approvalKey, "pl", nil) != "Zatwierdzenie" {
		t.Fatal("pack language not served")
	}
	other := firstOtherKey()
	if got := T(ctx, other, "pl", nil); got != en(other) {
		t.Fatalf("partial pack fallback got %q", got)
	}
	for surface, want := range map[string]map[string]string{"tui": {"status.ready": "Gotowy"}, "desktop": {"settings.title": "Ustawienia"}, "core": {"approval.denied": "Zatwierdzenie"}} {
		if got := SurfaceCatalog(ctx, "pl", surface); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s surface got %v", surface, got)
		}
	}
	if got := GetLanguage(ctx); got != "en" {
		t.Fatalf("language before config got %q", got)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "display:\n  language: pl\n")
	ResetLanguageCache()
	if got := GetLanguage(ctx); got != "pl" || T(ctx, approvalKey, "", nil) != "Zatwierdzenie" {
		t.Fatalf("language after config got %q", got)
	}
	want := LanguageOption{ID: "pl", Endonym: "pl", RTL: false, Source: src, Surfaces: []string{"core", "tui", "desktop"}}
	if got := optionFor(ctx, "pl"); !reflect.DeepEqual(got, want) {
		t.Fatalf("option got %+v", got)
	}
}

func optionFor(ctx context.Context, id string) LanguageOption {
	for _, o := range LanguageOptions(ctx) {
		if o.ID == id {
			return o
		}
	}
	return LanguageOption{}
}

func TestPackWithMetadata(t *testing.T) {
	tempHome(t)
	if _, err := RegisterPack("pl", "core", map[string]string{approvalKey: "Zatwierdzenie"}, "plugin:icebath-lang-pl", "Polski", false); err != nil {
		t.Fatal(err)
	}
	opt := optionFor(context.Background(), "pl")
	if opt.Endonym != "Polski" || opt.Source != "plugin:icebath-lang-pl" {
		t.Fatalf("got %+v", opt)
	}
}

func TestPackOverridesOverlayAndUnloadDropsIt(t *testing.T) {
	h := tempHome(t)
	writeFile(t, filepath.Join(h, "locales", "de.yaml"), "approval:\n  denied: Overlay\n")
	ResetLanguageCache()
	ctx := context.Background()
	if got := T(ctx, approvalKey, "de", nil); got != "Overlay" {
		t.Fatalf("got %q", got)
	}
	entry, err := RegisterPack("de", "core", map[string]string{approvalKey: "Pack"}, "plugin:icebath-lang-de", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := T(ctx, approvalKey, "de", nil); got != "Pack" {
		t.Fatalf("got %q", got)
	}
	if !UnregisterPack(entry) || UnregisterPack(entry) {
		t.Fatal("unregister result")
	}
	if got := T(ctx, approvalKey, "de", nil); got != "Overlay" || len(RegisteredPacks()) != 0 {
		t.Fatalf("after unload got %q", got)
	}
}

func TestPackOnlyLanguageDisappearsOnUnload(t *testing.T) {
	tempHome(t)
	ctx := context.Background()
	entry, _ := RegisterPack("pl", "core", map[string]string{approvalKey: "Zatwierdzenie"}, "plugin:icebath-lang-pl", "", false)
	if !contains(SupportedLanguagesFor(ctx, ""), "pl") {
		t.Fatal("pl missing")
	}
	UnregisterPack(entry)
	if contains(SupportedLanguagesFor(ctx, ""), "pl") || T(ctx, approvalKey, "pl", nil) != en(approvalKey) {
		t.Fatal("pl survived unload")
	}
}

func TestLaterPackWinsOverEarlierPack(t *testing.T) {
	tempHome(t)
	RegisterPack("pl", "core", map[string]string{approvalKey: "First", "approval.cancelled": "Only first"}, "plugin:aaa-lang-pl", "", false)
	RegisterPack("pl", "core", map[string]string{approvalKey: "Second"}, "plugin:zzz-lang-pl", "", false)
	ctx := context.Background()
	if T(ctx, approvalKey, "pl", nil) != "Second" || T(ctx, "approval.cancelled", "pl", nil) != "Only first" {
		t.Fatal("pack order")
	}
}

func TestRegisterPackAcceptsDictsAndRejectsBadIDs(t *testing.T) {
	h := tempHome(t)
	flat, _ := LoadLocaleSource(map[string]any{"approval": map[string]any{"denied": "Título"}})
	entry, err := RegisterPack("PT_BR", "core", flat, "plugin:inline-pack", "Português (Brasil)", false)
	if err != nil || entry.Lang+"."+entry.Surface != "pt-br.core" {
		t.Fatalf("entry %+v err %v", entry, err)
	}
	if got := T(context.Background(), approvalKey, "pt-BR", nil); got != "Título" {
		t.Fatalf("got %q", got)
	}
	if _, err := RegisterPack("not a lang", "core", map[string]string{"a": "b"}, "x", "", false); err == nil {
		t.Fatal("bad id accepted")
	}
	if _, err := RegisterPack("pl", "web", map[string]string{"a": "b"}, "x", "", false); err == nil {
		t.Fatal("bad surface accepted")
	}
	if _, err := LoadLocaleSource(filepath.Join(h, "missing.yaml")); !os.IsNotExist(err) {
		t.Fatalf("missing file err %v", err)
	}
}

func TestBundledTableMatchesBundledCatalogs(t *testing.T) {
	if len(BundledLanguageInfo) != len(SupportedLanguages) {
		t.Fatal("table size")
	}
	for _, lang := range SupportedLanguages {
		info, ok := BundledLanguageInfo[lang]
		if !ok || strings.TrimSpace(info.Endonym) == "" || info.Endonym == lang {
			t.Fatalf("%s endonym %q", lang, info.Endonym)
		}
	}
	if !BundledLanguageInfo["ar"].RTL {
		t.Fatal("ar not rtl")
	}
}

func TestLanguageOptionsAreEnFirstSortedAndSourced(t *testing.T) {
	tempHome(t)
	opts := LanguageOptions(context.Background())
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = o.ID
		if o.Source != "bundled" {
			t.Fatalf("%s source %s", o.ID, o.Source)
		}
	}
	rest := append([]string{}, ids[1:]...)
	sort.Strings(rest)
	if ids[0] != "en" || !reflect.DeepEqual(rest, ids[1:]) || len(ids) != len(SupportedLanguages) {
		t.Fatalf("ids %v", ids)
	}
	raw, _ := json.Marshal(opts[0])
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if len(fields) != 5 {
		t.Fatalf("fields %v", fields)
	}
}

func TestDescribeLanguagePrecedence(t *testing.T) {
	pack := &PackInfo{Endonym: "Polski", RTL: false, Source: "plugin:icebath-lang-pl"}
	got := DescribeLanguage("pl", pack, false, []string{"core", "desktop"})
	if !reflect.DeepEqual(got, LanguageOption{ID: "pl", Endonym: "Polski", Source: "plugin:icebath-lang-pl", Surfaces: []string{"core", "desktop"}}) {
		t.Fatalf("pl got %+v", got)
	}
	if DescribeLanguage("ar", &PackInfo{Endonym: "Other", Source: pack.Source}, false, nil).Endonym != "العربية" {
		t.Fatal("ar endonym")
	}
	if !DescribeLanguage("ar", &PackInfo{Endonym: "Polski", Source: pack.Source}, false, nil).RTL {
		t.Fatal("ar rtl")
	}
	if got := DescribeLanguage("eo", nil, true, nil); !reflect.DeepEqual(got, LanguageOption{ID: "eo", Endonym: "eo", Source: "overlay", Surfaces: []string{}}) {
		t.Fatalf("eo got %+v", got)
	}
	if DescribeLanguage("de", nil, true, nil).Source != "bundled" {
		t.Fatal("de source")
	}
}

func TestLanguageSurfacesReportEachLayer(t *testing.T) {
	h := tempHome(t)
	ctx := context.Background()
	s := LanguageSurfaces(ctx, "")
	if !reflect.DeepEqual(s["ko"], []string{"core", "tui"}) || !reflect.DeepEqual(s["en"], []string{"core"}) {
		t.Fatalf("bundled surfaces ko %v en %v", s["ko"], s["en"])
	}
	writeFile(t, filepath.Join(h, "locales", "ko.desktop.yaml"), "settings:\n  title: 설정\n")
	RegisterPack("pl", "desktop", map[string]string{"settings.title": "Ustawienia"}, "plugin:icebath-lang-pl", "", false)
	ResetLanguageCache()
	s = LanguageSurfaces(ctx, "")
	if !reflect.DeepEqual(s["ko"], []string{"core", "tui", "desktop"}) || !reflect.DeepEqual(s["pl"], []string{"desktop"}) {
		t.Fatalf("layered surfaces ko %v pl %v", s["ko"], s["pl"])
	}
	if optionFor(ctx, "ko").Surfaces[2] != "desktop" || !reflect.DeepEqual(optionFor(ctx, "ja").Surfaces, []string{"core", "tui"}) {
		t.Fatal("option surfaces")
	}
	writeFile(t, filepath.Join(h, "locales", "ja.desktop.yaml"), "# nothing translated yet\n")
	writeFile(t, filepath.Join(h, "locales", "de.desktop.yaml"), "settings: [unclosed\n")
	RegisterPack("uk", "desktop", map[string]string{}, "plugin:icebath-lang-uk", "", false)
	ResetLanguageCache()
	s = LanguageSurfaces(ctx, "")
	for _, lang := range []string{"ja", "de", "uk"} {
		if contains(s[lang], "desktop") {
			t.Fatalf("%s advertises desktop", lang)
		}
	}
}

func TestBundledTUIPackMirrorsTUIKeyExport(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("locales", "_keys.tui.json"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		var asMap map[string]any
		if err := json.Unmarshal(data, &asMap); err != nil {
			t.Fatal(err)
		}
		for k := range asMap {
			keys = append(keys, k)
		}
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	for _, lang := range SupportedLanguages {
		if lang == "en" {
			continue
		}
		pack, err := ParseLocaleFile(filepath.Join("locales", lang+".tui.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for k := range want {
			if _, ok := pack[k]; !ok {
				t.Fatalf("%s.tui.yaml missing %s", lang, k)
			}
		}
		for k := range pack {
			if !want[k] {
				t.Fatalf("%s.tui.yaml extra %s", lang, k)
			}
		}
	}
}

func TestTUISurfaceServesBundledPackUnderOverlay(t *testing.T) {
	h := tempHome(t)
	ctx := context.Background()
	bundled, err := ParseLocaleFile(filepath.Join("locales", "de.tui.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	key := sortedMapKeys(bundled)[0]
	served := SurfaceCatalog(ctx, "de", "tui")
	if served[key] != bundled[key] || len(served) < len(bundled) {
		t.Fatal("bundled tui not served")
	}
	quoted, _ := json.Marshal(key)
	writeFile(t, filepath.Join(h, "locales", "de.tui.yaml"), string(quoted)+": Overlay\n")
	ResetLanguageCache()
	served = SurfaceCatalog(ctx, "de", "tui")
	if served[key] != "Overlay" || len(served) != len(bundled) {
		t.Fatalf("overlay got %q len %d", served[key], len(served))
	}
}

func TestCoreSurfaceNeverShipsBundledStrings(t *testing.T) {
	tempHome(t)
	if got := SurfaceCatalog(context.Background(), "de", CoreSurface); len(got) != 0 {
		t.Fatalf("got %d keys", len(got))
	}
}
