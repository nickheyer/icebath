package discord

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/settings"
)

func TestDiscordCommandSyncFingerprintIncludesLanguage(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	t.Cleanup(i18n.ResetLanguageCache)
	language := func(lang string) {
		setSetting(t, settings.Language.Key, lang)
		i18n.ResetLanguageCache()
	}
	language("en")
	english := a.desiredCommandSyncFingerprint(context.Background())
	language("de")
	if a.desiredCommandSyncFingerprint(context.Background()) == english {
		t.Fatal("language change kept the fingerprint")
	}
	language("en")
	if a.desiredCommandSyncFingerprint(context.Background()) != english {
		t.Fatal("fingerprint not stable for the same language")
	}
}
