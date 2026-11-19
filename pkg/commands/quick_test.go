package commands

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Context over a temp home with the language left to the English default
func quickTestContext(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_LANGUAGE", "")
	os.Unsetenv("ICEBATH_LANGUAGE")
	i18n.ResetLanguageCache()
	t.Cleanup(i18n.ResetLanguageCache)
	return context.Background()
}

// Tool env of the temp home the test context set
func quickToolEnv() *tools.Env { return &tools.Env{Home: os.Getenv("ICEBATH_HOME")} }

// Overrides a quick command timeout for one test
func setQuickTimeout(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}
