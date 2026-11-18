package commands

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
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

func TestQuickCommandEntryRaisesTheTypeErrorsOfAMalformedValue(t *testing.T) {
	for _, c := range []struct {
		quick any
		msg   string
	}{
		{nil, "argument of type 'NoneType' is not a container or iterable"},
		{5, "argument of type 'int' is not a container or iterable"},
		{2.5, "argument of type 'float' is not a container or iterable"},
		{true, "argument of type 'bool' is not a container or iterable"},
		{[]any{"zzz"}, "list indices must be integers or slices, not str"},
		{"xzzzx", "string indices must be integers, not 'str'"},
	} {
		_, found, err := QuickCommandEntry(config.Tree{"quick_commands": c.quick}, "zzz")
		var raised *RaisedError
		if found || !errors.As(err, &raised) || raised.Msg != c.msg || err.Error() != c.msg {
			t.Fatalf("%v gave %v %v", c.quick, found, err)
		}
	}
	for _, quick := range []any{[]any{"other", 7}, "abc", map[string]any{"other": map[string]any{}}} {
		if entry, found, err := QuickCommandEntry(config.Tree{"quick_commands": quick}, "zzz"); found || entry != nil || err != nil {
			t.Fatalf("%v gave %v %v %v", quick, entry, found, err)
		}
	}
	if _, found, err := QuickCommandEntry(config.Tree{}, "zzz"); found || err != nil {
		t.Fatalf("absent quick_commands gave %v %v", found, err)
	}
	entry, found, err := QuickCommandEntry(config.Tree{"quick_commands": map[string]any{"zzz": "echo"}}, "zzz")
	if !found || entry != "echo" || err != nil {
		t.Fatalf("mapping entry %v %v %v", entry, found, err)
	}
}
