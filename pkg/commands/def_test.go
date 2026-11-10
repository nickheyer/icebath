package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/i18n"
)

func freshLanguageCache(t *testing.T) {
	t.Helper()
	i18n.ResetLanguageCache()
	t.Cleanup(i18n.ResetLanguageCache)
}

func TestDescribeFallsBackToEnglishFieldWhenNoCatalogEntry(t *testing.T) {
	freshLanguageCache(t)
	cmd := &Def{Name: "no-such-command-zz", Description: "Does a thing", Category: "Info"}
	if got := cmd.Describe(context.Background()); got != "Does a thing" {
		t.Fatalf("describe %q", got)
	}
}

func TestDescribeUsesCatalogEntryWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "en.yaml"), []byte("slash:\n  help:\n    description: 'Localized help'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_BUNDLED_LOCALES", dir)
	freshLanguageCache(t)
	var help *Def
	for _, d := range All(Default) {
		if d.Name == "help" {
			help = d
		}
	}
	if help == nil {
		t.Fatal("no help command in the registry")
	}
	if got := help.Describe(context.Background()); got != "Localized help" {
		t.Fatalf("describe %q", got)
	}
}
