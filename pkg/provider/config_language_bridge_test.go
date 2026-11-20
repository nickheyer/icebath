package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/plugins"
)

type languageHome struct {
	t      *testing.T
	h      string
	errOut interface{ String() string }
}

// Home whose display language is en with no bundled plugins and fresh pack and language caches
func newLanguageHome(t *testing.T) *languageHome {
	t.Helper()
	h, _, errOut := moaKeysHome(t, map[string]any{})
	root := filepath.Dir(h)
	writeLeftCoreFile(t, filepath.Join(h, "config.yaml"), "display:\n  language: en\n")
	bundled := filepath.Join(root, "bundled")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	t.Setenv(config.EnvManagedDir, filepath.Join(root, "no-managed"))
	config.InvalidateManagedCache()
	reset := func() {
		plugins.ResetPluginManagersForTests()
		i18n.ResetRegistryForTests()
		i18n.ResetLanguageCache()
	}
	reset()
	t.Cleanup(reset)
	return &languageHome{t: t, h: h, errOut: errOut}
}

func (l *languageHome) set(value string) error {
	src := moaCommandSources()
	src.Language = LanguageCatalog()
	return config.SetConfigValue(context.Background(), l.h, "display.language", value, false, src)
}

func (l *languageHome) saved() string {
	l.t.Helper()
	doc, err := config.ReadDocument(filepath.Join(l.h, "config.yaml"))
	if err != nil {
		l.t.Fatal(err)
	}
	v, _ := config.CfgGet(doc, "display", "language")
	s, _ := v.(string)
	return s
}

func TestBundledLanguageAndAliasAreAccepted(t *testing.T) {
	l := newLanguageHome(t)
	if err := l.set("de"); err != nil || l.saved() != "de" {
		t.Fatalf("set de: %v saved %q %s", err, l.saved(), l.errOut)
	}
	if err := l.set("zh-TW"); err != nil || l.saved() != "zh-TW" {
		t.Fatalf("set zh-TW: %v saved %q %s", err, l.saved(), l.errOut)
	}
	if got := i18n.GetLanguage(context.Background()); got != "zh-hant" {
		t.Fatalf("language %q", got)
	}
}

func TestUnknownLanguageIsRefusedWithTheAvailableList(t *testing.T) {
	l := newLanguageHome(t)
	err := l.set("klingon")
	var exit *config.ConfigExit
	if !errors.As(err, &exit) {
		t.Fatalf("klingon accepted: %v", err)
	}
	msg := l.errOut.String()
	if !strings.Contains(msg, "klingon") || !strings.Contains(msg, "Available:") || !strings.Contains(msg, "en, af") {
		t.Fatalf("refusal %q", msg)
	}
	if l.saved() != "en" {
		t.Fatalf("saved %q", l.saved())
	}
}

func TestPackLanguageIsAcceptedOnceThePackIsInstalled(t *testing.T) {
	l := newLanguageHome(t)
	plugin := filepath.Join(l.h, "plugins", "icebath-lang-pl")
	writeLeftCoreFile(t, filepath.Join(plugin, "plugin.yaml"),
		"description: Polish pack\nname: icebath-lang-pl\nprovides_locales:\n- pl\nversion: 1.0.0\n")
	writeLeftCoreFile(t, filepath.Join(plugin, "locales", "pl.yaml"), "approval:\n  denied: Odrzucono\n")
	writeLeftCoreFile(t, filepath.Join(l.h, "config.yaml"), "display:\n  language: en\nplugins:\n  enabled:\n  - icebath-lang-pl\n")
	config.ResetCache()
	if err := l.set("pl"); err != nil || l.saved() != "pl" {
		t.Fatalf("set pl: %v saved %q %s", err, l.saved(), l.errOut)
	}
	ctx := context.Background()
	if got := i18n.GetLanguage(ctx); got != "pl" {
		t.Fatalf("language %q", got)
	}
	if got := i18n.T(ctx, "approval.denied", "", nil); got != "Odrzucono" {
		t.Fatalf("approval.denied %q", got)
	}
}
