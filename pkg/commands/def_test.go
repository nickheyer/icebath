package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func TestRunHandlerDrivesTheBoundHandlerWithItsHost(t *testing.T) {
	r := NewRegistry()
	var got *Invocation
	failure := errors.New("handler failed")
	r.Register(Def{Name: "bound", Aliases: []string{"bd"}, Handler: func(ctx context.Context, inv *Invocation) error {
		got = inv
		if inv.Args == "fail" {
			return failure
		}
		return nil
	}})
	r.Register(Def{Name: "plain"})
	host := &struct{ turns []string }{}
	if err := RunHandler(context.Background(), r, "bd", "  audit 'the header' ", host); err != nil {
		t.Fatal(err)
	}
	if got.Name != "bound" || got.Def.Name != "bound" || got.Args != "audit 'the header'" || got.Raw != "/bd   audit 'the header'" ||
		!reflect.DeepEqual(got.Argv, []string{"audit", "the header"}) || got.Host != host {
		t.Fatalf("invocation %+v", got)
	}
	if err := RunHandler(context.Background(), r, "/BOUND", "", host); err != nil || got.Args != "" || got.Raw != "/BOUND" {
		t.Fatalf("bare invocation %+v %v", got, err)
	}
	if err := RunHandler(context.Background(), r, "bound", "fail", host); !errors.Is(err, failure) {
		t.Fatalf("handler error %v", err)
	}
	var unbound *UnboundError
	if err := RunHandler(context.Background(), r, "plain", "x", host); !errors.As(err, &unbound) || unbound.Name != "plain" ||
		err.Error() != "commands: /plain has no handler bound" {
		t.Fatalf("unbound %v", err)
	}
	var unknown *UnknownCommandError
	for _, name := range []string{"nope", "/a/b"} {
		if err := RunHandler(context.Background(), r, name, "", host); !errors.As(err, &unknown) {
			t.Fatalf("%s unknown %v", name, err)
		}
	}
	if unknown.Name != "a/b" || (&UnknownCommandError{Name: "nope"}).Error() != "commands: unknown command /nope" {
		t.Fatalf("unknown %+v", unknown)
	}
}
