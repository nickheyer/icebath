package config

import (
	"fmt"
	"strings"
)

// Config key holding the display language
const DisplayLanguageKey = "display.language"

// Language layers the display language check consults, implemented by the i18n and plugin owners
type LanguageCatalog interface {
	DiscoverPlugins() error
	ResetLanguageCache()
	ResolveLanguageID(value string) (string, bool)
	SupportedLanguages() []string
}

// Canonical language id after plugin discovery, false when no layer supplies it
func ResolveDisplayLanguage(value string, cat LanguageCatalog) (string, bool) {
	_ = cat.DiscoverPlugins()
	cat.ResetLanguageCache()
	return cat.ResolveLanguageID(value)
}

// Refusal for an unsupported display language, empty when the value is blank or resolves
func DisplayLanguageError(value string, cat LanguageCatalog) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	if _, ok := ResolveDisplayLanguage(value, cat); ok {
		return ""
	}
	return fmt.Sprintf("✗ Unknown language %s for %s. Available: %s.\n  Install a language pack plugin "+
		"(icebath plugins install <pack>) or drop <ICEBATH_HOME>/locales/<id>.yaml to add one.",
		PyRepr(value), DisplayLanguageKey, strings.Join(cat.SupportedLanguages(), ", "))
}
