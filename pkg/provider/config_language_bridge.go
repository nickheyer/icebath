package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Display language layers of the active home, bundled catalogs, the locale overlay and discovered language packs
type languageCatalog struct{ ctx context.Context }

// Language catalog config set display.language validates against
func LanguageCatalog() config.LanguageCatalog { return languageCatalog{context.Background()} }

// Loads every plugin of the active home so its language packs register before the check
func (c languageCatalog) DiscoverPlugins() error {
	plugins.DiscoverPlugins(c.ctx, false)
	return nil
}

func (languageCatalog) ResetLanguageCache() { i18n.ResetLanguageCache() }

// Canonical id for a code, alias or regional tag, false when no layer supplies it
func (c languageCatalog) ResolveLanguageID(value string) (string, bool) {
	id := i18n.ResolveLanguageID(c.ctx, value, "")
	return id, id != ""
}

// Every language a layer supplies for the active home, en first
func (c languageCatalog) SupportedLanguages() []string { return i18n.SupportedLanguagesFor(c.ctx, "") }
