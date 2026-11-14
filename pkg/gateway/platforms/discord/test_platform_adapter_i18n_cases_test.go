package discord

import (
	"context"
	"testing"
)

func TestDiscordCommandSyncFingerprintIncludesLanguage(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_LANGUAGE", "en")
	a := newTestAdapter(t, nil)
	english := a.desiredCommandSyncFingerprint(context.Background())
	t.Setenv("ICEBATH_LANGUAGE", "de")
	if a.desiredCommandSyncFingerprint(context.Background()) == english {
		t.Fatal("language change kept the fingerprint")
	}
	t.Setenv("ICEBATH_LANGUAGE", "en")
	if a.desiredCommandSyncFingerprint(context.Background()) != english {
		t.Fatal("fingerprint not stable for the same language")
	}
}
