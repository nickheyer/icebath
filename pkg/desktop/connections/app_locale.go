package connections

import "strings"

// chromiumLocales are the UI locales the Electron runtime shipped, the set its locale resolution chooses from
var chromiumLocales = map[string]bool{
	"af": true, "am": true, "ar": true, "bg": true, "bn": true, "ca": true, "cs": true, "da": true, "de": true, "el": true,
	"en-GB": true, "en-US": true, "es": true, "es-419": true, "et": true, "fa": true, "fi": true, "fil": true, "fr": true,
	"gu": true, "he": true, "hi": true, "hr": true, "hu": true, "id": true, "it": true, "ja": true, "kn": true, "ko": true,
	"lt": true, "lv": true, "ml": true, "mr": true, "ms": true, "nb": true, "nl": true, "pl": true, "pt-BR": true,
	"pt-PT": true, "ro": true, "ru": true, "sk": true, "sl": true, "sr": true, "sv": true, "sw": true, "ta": true,
	"te": true, "th": true, "tr": true, "uk": true, "ur": true, "vi": true, "zh-CN": true, "zh-TW": true,
}

var localeAliases = map[string]string{"en": "en-US", "iw": "he", "no": "nb", "pt": "pt-BR", "tl": "fil", "zh": "zh-CN"}

// canonicalLocale turns a POSIX or BCP 47 locale into a hyphenated tag without encoding or modifier
func canonicalLocale(raw string) string {
	value := strings.TrimSpace(raw)
	if i := strings.IndexAny(value, ".@"); i >= 0 {
		value = value[:i]
	}
	if value == "C" || value == "POSIX" {
		return "en-US-POSIX"
	}
	return strings.ReplaceAll(value, "_", "-")
}

// resolveChromiumLocale maps a locale onto a shipped UI locale with the region and alias fallbacks Chromium applies
func resolveChromiumLocale(locale string) (string, bool) {
	if chromiumLocales[locale] {
		return locale, true
	}
	lang, region, hasRegion := strings.Cut(locale, "-")
	if hasRegion && lang != "" {
		candidate := lang
		lowerLang, lowerRegion := strings.ToLower(lang), strings.ToLower(region)
		switch lowerLang {
		case "es":
			if lowerRegion != "es" {
				candidate += "-419"
			}
		case "pt":
			candidate += "-PT"
		case "zh":
			if lowerRegion == "hk" || lowerRegion == "mo" {
				candidate += "-TW"
			} else {
				candidate += "-CN"
			}
		case "en":
			switch lowerRegion {
			case "au", "ca", "in", "nz", "za":
				candidate += "-GB"
			default:
				candidate += "-US"
			}
		}
		if chromiumLocales[candidate] {
			return candidate, true
		}
	}
	if alias, ok := localeAliases[strings.ToLower(lang)]; ok && chromiumLocales[alias] {
		return alias, true
	}
	return "", false
}

// ApplicationLocale is the first preferred language that resolves to a shipped UI locale, en-US otherwise
func ApplicationLocale(preferred []string) string {
	for _, candidate := range preferred {
		if candidate = canonicalLocale(candidate); candidate == "" {
			continue
		}
		if resolved, ok := resolveChromiumLocale(candidate); ok {
			return resolved
		}
	}
	return "en-US"
}

// envPreferredLanguages lists the gettext LANGUAGE priority list then the configured LC_ALL, LC_MESSAGES or LANG locale
func envPreferredLanguages(getenv func(string) string) []string {
	var out []string
	for _, entry := range strings.Split(getenv("LANGUAGE"), ":") {
		if strings.TrimSpace(entry) != "" {
			out = append(out, entry)
		}
	}
	configured := "en_US_POSIX"
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := getenv(key); value != "" {
			configured = value
			break
		}
	}
	return append(out, configured)
}

// parseAppleLanguages reads the property list array that defaults prints for AppleLanguages
func parseAppleLanguages(out string) []string {
	var langs []string
	for _, line := range strings.Split(out, "\n") {
		entry := strings.Trim(strings.TrimSuffix(strings.TrimSpace(line), ","), `"`)
		if entry != "" && entry != "(" && entry != ")" {
			langs = append(langs, entry)
		}
	}
	return langs
}
