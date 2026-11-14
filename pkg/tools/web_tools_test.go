package tools

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestHasXAICredentialsExportMatchesTheProbe(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	env := &Env{Home: t.TempDir(), Config: config.Tree{}}
	if HasXAICredentials(env) != hasXAICredentials(env) {
		t.Fatal("xai credential probe diverges")
	}
}
