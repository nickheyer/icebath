package i18n

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

//go:embed locales/*.yaml
var bundledFS embed.FS

// SupportedLanguages are the bundled catalogs
var SupportedLanguages = []string{"en", "zh", "zh-hant", "ja", "de", "es", "fr", "tr", "uk", "af", "ko", "it", "ga", "pt", "ru", "hu", "ar"}

const DefaultLanguage = "en"

var languageAliases = map[string]string{
	"english": "en", "en-us": "en", "en-gb": "en",
	"chinese": "zh", "mandarin": "zh", "zh-cn": "zh", "zh-hans": "zh", "zh-sg": "zh",
	"traditional-chinese": "zh-hant", "traditional_chinese": "zh-hant",
	"zh-tw": "zh-hant", "zh-hk": "zh-hant", "zh-mo": "zh-hant",
	"japanese": "ja", "jp": "ja", "ja-jp": "ja",
	"german": "de", "deutsch": "de", "de-de": "de", "de-at": "de", "de-ch": "de",
	"spanish": "es", "español": "es", "espanol": "es", "es-es": "es", "es-mx": "es", "es-ar": "es",
	"french": "fr", "français": "fr", "france": "fr", "fr-fr": "fr", "fr-be": "fr", "fr-ca": "fr", "fr-ch": "fr",
	"ukrainian": "uk", "ukrainisch": "uk", "українська": "uk", "uk-ua": "uk", "ua": "uk",
	"turkish": "tr", "türkçe": "tr", "tr-tr": "tr",
	"afrikaans": "af", "af-za": "af",
	"korean": "ko", "한국어": "ko", "ko-kr": "ko",
	"italian": "it", "italiano": "it", "it-it": "it", "it-ch": "it",
	"irish": "ga", "gaeilge": "ga", "ga-ie": "ga",
	"portuguese": "pt", "português": "pt", "portugues": "pt",
	"pt-pt": "pt", "pt-br": "pt", "brazilian": "pt", "brasileiro": "pt",
	"russian": "ru", "русский": "ru", "ru-ru": "ru",
	"hungarian": "hu", "magyar": "hu", "hu-hu": "hu",
	"arabic": "ar", "العربية": "ar",
	"ar-sa": "ar", "ar-eg": "ar", "ar-ae": "ar", "ar-ma": "ar", "ar-dz": "ar",
}

// SecretReader resolves a per profile env value through the secret scope carried by ctx
type SecretReader func(ctx context.Context, name string) (string, bool, error)

// ErrUnscopedSecret marks a secret read refused because multiplexing is on with no scope bound
var ErrUnscopedSecret = errors.New("secret read with no profile scope while multiplexing")

var (
	catalogMu      sync.Mutex
	catalogCache   = map[[2]string]map[string]string{}
	supportedCache = map[string][]string{}
	bundledSurfMu  sync.Mutex
	bundledSurf    = map[string]map[langSurface]bool{}
	configLangMu   sync.Mutex
	configLangLRU  []configLangEntry
	secretReader   SecretReader = envSecretReader
	localesDirFn                = localesDir
	configLangFn                = configLanguage
)

type configLangEntry struct {
	home string
	lang string
	ok   bool
}

// SetSecretReader installs the scoped secret resolver the language lookup reads ICEBATH_LANGUAGE through
func SetSecretReader(r SecretReader) {
	if r == nil {
		r = envSecretReader
	}
	secretReader = r
}

func envSecretReader(_ context.Context, name string) (string, bool, error) {
	v, ok := os.LookupEnv(name)
	return v, ok, nil
}

// BundledLocales is the bundled locale tree and the path label it is reported under
type BundledLocales struct {
	FS    fs.FS
	Label string
}

// Bundled locale tree from ICEBATH_BUNDLED_LOCALES when it is a directory, else the embedded catalogs
func localesDir() BundledLocales {
	override := strings.TrimSpace(os.Getenv("ICEBATH_BUNDLED_LOCALES"))
	if override != "" {
		if info, err := os.Stat(override); err == nil && info.IsDir() {
			return BundledLocales{FS: os.DirFS(override), Label: override}
		}
		slog.Warn(fmt.Sprintf("ICEBATH_BUNDLED_LOCALES points to a non-directory path (%s); falling back to bundled/source locale resolution", override))
	}
	sub, _ := fs.Sub(bundledFS, "locales")
	return BundledLocales{FS: sub, Label: "locales"}
}

// LocalesDir reports the bundled locale tree in effect
func LocalesDir() BundledLocales { return localesDirFn() }

func currentHome(ctx context.Context) string { return home.Dir(ctx) }

// SupportedLanguagesFor lists every language a layer supplies for the home, en first then sorted
func SupportedLanguagesFor(ctx context.Context, h string) []string {
	if h == "" {
		h = currentHome(ctx)
	}
	catalogMu.Lock()
	if cached, ok := supportedCache[h]; ok {
		catalogMu.Unlock()
		return cached
	}
	catalogMu.Unlock()
	set := map[string]bool{}
	for _, l := range SupportedLanguages {
		set[l] = true
	}
	for _, l := range LayeredLanguages(h) {
		set[l] = true
	}
	delete(set, DefaultLanguage)
	result := append([]string{DefaultLanguage}, sortedKeys(set)...)
	catalogMu.Lock()
	supportedCache[h] = result
	catalogMu.Unlock()
	return result
}

// Language and surface pairs of the bundled tree, cached per tree label
func bundledSurfaces(b BundledLocales) map[langSurface]bool {
	bundledSurfMu.Lock()
	defer bundledSurfMu.Unlock()
	if cached, ok := bundledSurf[b.Label]; ok {
		return cached
	}
	found := map[langSurface]bool{}
	for _, f := range scanLocaleFS(b.FS, b.Label) {
		found[langSurface{f.Lang, f.Surface}] = true
	}
	if len(bundledSurf) >= 4 {
		bundledSurf = map[string]map[langSurface]bool{}
	}
	bundledSurf[b.Label] = found
	return found
}

// LanguageSurfaces maps each language to the surfaces some layer has strings for, in Surfaces order
func LanguageSurfaces(ctx context.Context, h string) map[string][]string {
	if h == "" {
		h = currentHome(ctx)
	}
	pairs := map[langSurface]bool{}
	for k := range bundledSurfaces(LocalesDir()) {
		pairs[k] = true
	}
	candidates := map[langSurface]bool{}
	for k := range overlaySurfaces(h) {
		candidates[k] = true
	}
	for k := range packSurfaces() {
		candidates[k] = true
	}
	for k := range candidates {
		if len(OverlayLayer(h, k.lang, k.surface)) > 0 || len(PackLayer(k.lang, k.surface)) > 0 {
			pairs[k] = true
		}
	}
	found := map[string]map[string]bool{}
	for k := range pairs {
		if found[k.lang] == nil {
			found[k.lang] = map[string]bool{}
		}
		found[k.lang][k.surface] = true
	}
	out := map[string][]string{}
	for lang, surfaces := range found {
		ordered := []string{}
		for _, s := range Surfaces {
			if surfaces[s] {
				ordered = append(ordered, s)
			}
		}
		out[lang] = ordered
	}
	return out
}

// ResolveLanguageID maps a code, alias or regional tag to a supported id, empty when no layer supplies it
func ResolveLanguageID(ctx context.Context, value any, h string) string {
	key := NormalizeLanguageID(value)
	if key == "" {
		return ""
	}
	supported := map[string]bool{}
	for _, l := range SupportedLanguagesFor(ctx, h) {
		supported[l] = true
	}
	if supported[key] {
		return key
	}
	if alias, ok := languageAliases[key]; ok && supported[alias] {
		return alias
	}
	base := strings.SplitN(key, "-", 2)[0]
	if supported[base] {
		return base
	}
	return ""
}

func normalizeLang(ctx context.Context, value any, h string) string {
	if id := ResolveLanguageID(ctx, value, h); id != "" {
		return id
	}
	return DefaultLanguage
}

// One bundled catalog flattened, empty and logged on any failure
func loadBundled(name string) map[string]string {
	b := LocalesDir()
	file := name + ".yaml"
	path := filepath.Join(b.Label, file)
	info, err := fs.Stat(b.FS, file)
	if err != nil || !info.Mode().IsRegular() {
		slog.Debug(fmt.Sprintf("i18n catalog missing for %s at %s", name, path))
		return map[string]string{}
	}
	flat, err := parseLocaleFS(b.FS, file, path)
	if err != nil {
		slog.Warn(fmt.Sprintf("Failed to load i18n catalog %s: %v", path, err))
		return map[string]string{}
	}
	return flat
}

// LoadBundled returns one bundled catalog flattened
func LoadBundled(name string) map[string]string { return loadBundled(name) }

// Merged catalog for a language in a home, packs over overlay over bundled, cached per pair
func loadCatalog(ctx context.Context, lang, h string) map[string]string {
	if h == "" {
		h = currentHome(ctx)
	}
	key := [2]string{h, lang}
	catalogMu.Lock()
	if cached, ok := catalogCache[key]; ok {
		catalogMu.Unlock()
		return cached
	}
	catalogMu.Unlock()
	merged := loadBundled(lang)
	for k, v := range OverlayLayer(h, lang, CoreSurface) {
		merged[k] = v
	}
	for k, v := range PackLayer(lang, CoreSurface) {
		merged[k] = v
	}
	catalogMu.Lock()
	catalogCache[key] = merged
	catalogMu.Unlock()
	return merged
}

// SurfaceCatalog is what a renderer receives for one surface, the core surface omitting bundled strings
func SurfaceCatalog(ctx context.Context, lang, surface string) map[string]string {
	h := currentHome(ctx)
	lang = normalizeLang(ctx, lang, h)
	merged := map[string]string{}
	if surface != CoreSurface {
		for k, v := range loadBundled(lang + "." + surface) {
			merged[k] = v
		}
	}
	for k, v := range LayerSurfaceCatalog(h, lang, surface) {
		merged[k] = v
	}
	return merged
}

// display.language of the home config read once per home through an eight entry cache
func configLanguageCached(ctx context.Context, h string) (string, bool) {
	configLangMu.Lock()
	for i, e := range configLangLRU {
		if e.home == h {
			configLangLRU = append(append([]configLangEntry{e}, configLangLRU[:i]...), configLangLRU[i+1:]...)
			configLangMu.Unlock()
			return e.lang, e.ok
		}
	}
	configLangMu.Unlock()
	lang, ok := configLangFn(ctx, h)
	configLangMu.Lock()
	configLangLRU = append([]configLangEntry{{home: h, lang: lang, ok: ok}}, configLangLRU...)
	if len(configLangLRU) > 8 {
		configLangLRU = configLangLRU[:8]
	}
	configLangMu.Unlock()
	return lang, ok
}

// Reads display.language from the effective config, absent when unset, falsy or unreadable
func configLanguage(ctx context.Context, h string) (string, bool) {
	cfg, err := config.LoadReadonly(home.WithOverride(ctx, h), h, os.LookupEnv)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read display.language from config: %v", err))
		return "", false
	}
	display, ok := cfg["display"]
	if !ok || !pyrepr.Truthy(display) {
		return "", false
	}
	dm, ok := display.(map[string]any)
	if !ok {
		slog.Debug(fmt.Sprintf("Could not read display.language from config: '%s' object has no attribute 'get'", pyrepr.TypeName(display)))
		return "", false
	}
	lang, ok := dm["language"]
	if !ok || !pyrepr.Truthy(lang) {
		return "", false
	}
	return normalizeLang(ctx, lang, h), true
}

// ResetLanguageCache drops resolved languages, merged catalogs and every layer view
func ResetLanguageCache() {
	configLangMu.Lock()
	configLangLRU = nil
	configLangMu.Unlock()
	bundledSurfMu.Lock()
	bundledSurf = map[string]map[langSurface]bool{}
	bundledSurfMu.Unlock()
	catalogMu.Lock()
	catalogCache = map[[2]string]map[string]string{}
	supportedCache = map[string][]string{}
	catalogMu.Unlock()
	ClearLayerCache()
}

// Env language through the secret scope, an unscoped refusal reading the process environment instead
func resolveLanguage(ctx context.Context, h string) string {
	envLang, _, err := secretReader(ctx, "ICEBATH_LANGUAGE")
	if err != nil {
		if !errors.Is(err, ErrUnscopedSecret) {
			panic(err)
		}
		envLang = os.Getenv("ICEBATH_LANGUAGE")
	}
	if envLang != "" {
		return normalizeLang(ctx, envLang, h)
	}
	if lang, ok := configLanguageCached(ctx, h); ok && lang != "" {
		return lang
	}
	return DefaultLanguage
}

// GetLanguage resolves the active language, env over config over the default
func GetLanguage(ctx context.Context) string {
	return resolveLanguage(ctx, currentHome(ctx))
}

// T translates a dotted key into the explicit or active language with English then key fallback
func T(ctx context.Context, key, lang string, kwargs map[string]any) string {
	h := currentHome(ctx)
	target := ""
	if lang != "" {
		target = normalizeLang(ctx, lang, h)
	} else {
		target = resolveLanguage(ctx, h)
	}
	value, ok := loadCatalog(ctx, target, h)[key]
	if !ok && target != DefaultLanguage {
		value, ok = loadCatalog(ctx, DefaultLanguage, h)[key]
	}
	if !ok {
		slog.Debug(fmt.Sprintf("i18n miss: key=%s lang=%s", pyrepr.StrRepr(key), pyrepr.StrRepr(target)))
		value = key
	}
	if len(kwargs) == 0 {
		return value
	}
	out, err := PyFormat(value, kwargs)
	if err != nil {
		var fe *FormatError
		if errors.As(err, &fe) && (fe.Class == "KeyError" || fe.Class == "IndexError" || fe.Class == "ValueError") {
			slog.Warn(fmt.Sprintf("i18n format failed for key=%s lang=%s kwargs=%s: %s", pyrepr.StrRepr(key), pyrepr.StrRepr(target), pyrepr.Repr(kwargs), fe.Msg))
			return value
		}
		panic(err)
	}
	return out
}

// LanguageOption is one picker row for a supported language
type LanguageOption struct {
	ID       string   `json:"id"`
	Endonym  string   `json:"endonym"`
	RTL      bool     `json:"rtl"`
	Source   string   `json:"source"`
	Surfaces []string `json:"surfaces"`
}

type bundledLanguage struct {
	Endonym string
	RTL     bool
}

// BundledLanguageInfo maps each bundled id to its endonym and script direction
var BundledLanguageInfo = map[string]bundledLanguage{
	"af": {"Afrikaans", false}, "ar": {"العربية", true}, "de": {"Deutsch", false}, "en": {"English", false},
	"es": {"Español", false}, "fr": {"Français", false}, "ga": {"Gaeilge", false}, "hu": {"Magyar", false},
	"it": {"Italiano", false}, "ja": {"日本語", false}, "ko": {"한국어", false}, "pt": {"Português", false},
	"ru": {"Русский", false}, "tr": {"Türkçe", false}, "uk": {"Українська", false}, "zh": {"简体中文", false},
	"zh-hant": {"繁體中文", false},
}

const (
	BundledSource = "bundled"
	OverlaySource = "overlay"
)

// DescribeLanguage builds one picker row, bundled table over pack metadata over the bare id
func DescribeLanguage(lang string, pack *PackInfo, overlay bool, surfaces []string) LanguageOption {
	opt := LanguageOption{ID: lang, Surfaces: append([]string{}, surfaces...)}
	bundled, isBundled := BundledLanguageInfo[lang]
	switch {
	case isBundled:
		opt.Endonym, opt.RTL = bundled.Endonym, bundled.RTL
	case pack != nil:
		opt.Endonym, opt.RTL = pack.Endonym, pack.RTL
		if opt.Endonym == "" {
			opt.Endonym = lang
		}
	default:
		opt.Endonym = lang
	}
	switch {
	case pack != nil:
		opt.Source = pack.Source
		if opt.Source == "" {
			opt.Source = "plugin"
		}
	case overlay && !isBundled:
		opt.Source = OverlaySource
	default:
		opt.Source = BundledSource
	}
	return opt
}

// LanguageOptions lists every supported language for the current home, en first then sorted by id
func LanguageOptions(ctx context.Context) []LanguageOption {
	h := currentHome(ctx)
	overlay := map[string]bool{}
	for _, l := range OverlayLanguages(h) {
		overlay[l] = true
	}
	surfaces := LanguageSurfaces(ctx, h)
	langs := SupportedLanguagesFor(ctx, "")
	out := make([]LanguageOption, 0, len(langs))
	for _, lang := range langs {
		out = append(out, DescribeLanguage(lang, PackInfoFor(lang), overlay[lang], surfaces[lang]))
	}
	return out
}
