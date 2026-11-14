package pm

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestExtraExpandsSelfReferencesAndRejectsUnknown(t *testing.T) {
	tmp := t.TempDir()
	peWrite(t, filepath.Join(tmp, "pyproject.toml"), "[project]\nname = \"demo\"\nversion = \"0\"\n"+
		"[project.optional-dependencies]\n"+
		"a = [\"pkg-a==1.0\"]\nb = [\"demo[a]\", \"pkg-b==2.0\"]\n")
	got, err := extraRequirements(tmp, []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"pkg-a==1.0", "pkg-b==2.0"}) {
		t.Fatalf("requirements %q", got)
	}
	_, err = extraRequirements(tmp, []string{"missing"})
	var py *PyError
	if !errors.As(err, &py) || py.Class != "ValueError" || !strings.Contains(py.Message, "not an extra") {
		t.Fatalf("missing extra %#v", err)
	}
}
