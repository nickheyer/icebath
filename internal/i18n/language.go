// Message lookup over the embedded locale catalogs, the user overlay and plugin language packs
package i18n

import (
	"regexp"
	"strings"
)

const DefaultLanguage = "en"

const (
	sourceBundled = "bundled"
	sourceOverlay = "overlay"
	sourcePlugin  = "plugin"
)

type Surface string

const (
	Core    Surface = "core"
	TUI     Surface = "tui"
	Desktop Surface = "desktop"
)

func Surfaces() []Surface {
	return []Surface{Core, TUI, Desktop}
}

func (s Surface) Valid() bool {
	return s == Core || s == TUI || s == Desktop
}

const surfaceList = "core, tui, desktop"

var bundledLanguages = []string{
	"en", "zh", "zh-hant", "ja", "de", "es", "fr", "tr", "uk",
	"af", "ko", "it", "ga", "pt", "ru", "hu", "ar",
}

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

type identity struct {
	endonym string
	rtl     bool
}

var bundledIdentity = map[string]identity{
	"af":      {"Afrikaans", false},
	"ar":      {"العربية", true},
	"de":      {"Deutsch", false},
	"en":      {"English", false},
	"es":      {"Español", false},
	"fr":      {"Français", false},
	"ga":      {"Gaeilge", false},
	"hu":      {"Magyar", false},
	"it":      {"Italiano", false},
	"ja":      {"日本語", false},
	"ko":      {"한국어", false},
	"pt":      {"Português", false},
	"ru":      {"Русский", false},
	"tr":      {"Türkçe", false},
	"uk":      {"Українська", false},
	"zh":      {"简体中文", false},
	"zh-hant": {"繁體中文", false},
}

var (
	languageIDPattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})*$`)
	localeFilePattern = regexp.MustCompile(`^([a-z]{2,3}(?:-[a-z0-9]{2,8})*)(?:\.([a-z]+))?\.ya?ml$`)
)

func NormalizeID(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
}

func IsLanguageID(value string) bool {
	return languageIDPattern.MatchString(NormalizeID(value))
}

type Option struct {
	ID       string    `json:"id"`
	Endonym  string    `json:"endonym"`
	RTL      bool      `json:"rtl"`
	Source   string    `json:"source"`
	Surfaces []Surface `json:"surfaces"`
}

func describe(id string, pack *packInfo, overlay bool, surfaces []Surface) Option {
	opt := Option{ID: id, Surfaces: append(make([]Surface, 0, len(surfaces)), surfaces...)}
	info, bundled := bundledIdentity[id]
	switch {
	case bundled:
		opt.Endonym, opt.RTL = info.endonym, info.rtl
	case pack != nil:
		opt.Endonym, opt.RTL = pack.endonym, pack.rtl
		if opt.Endonym == "" {
			opt.Endonym = id
		}
	default:
		opt.Endonym = id
	}
	switch {
	case pack != nil:
		opt.Source = pack.source
		if opt.Source == "" {
			opt.Source = sourcePlugin
		}
	case overlay && !bundled:
		opt.Source = sourceOverlay
	default:
		opt.Source = sourceBundled
	}
	return opt
}
