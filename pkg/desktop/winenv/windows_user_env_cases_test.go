package winenv

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func TestParseRegQueryValueExtractsARegSzValue(t *testing.T) {
	out := strings.Join([]string{"", `HKEY_CURRENT_USER\Environment`, `    ICEBATH_HOME    REG_SZ    F:\Hermes\data`, ""}, "\r\n")
	if v, ok := ParseRegQueryValue(out, "ICEBATH_HOME"); !ok || v != `F:\Hermes\data` {
		t.Fatalf("got %q %v", v, ok)
	}
}

func TestParseRegQueryValueMatchesTheNameCaseInsensitively(t *testing.T) {
	out := "HKEY_CURRENT_USER\\Environment\r\n    Icebath_Home    REG_EXPAND_SZ    %USERPROFILE%\\h\r\n"
	if v, ok := ParseRegQueryValue(out, "ICEBATH_HOME"); !ok || v != `%USERPROFILE%\h` {
		t.Fatalf("got %q %v", v, ok)
	}
}

func TestParseRegQueryValuePreservesSpacesInsideTheValue(t *testing.T) {
	out := "    ICEBATH_HOME    REG_SZ    C:\\Program Files\\Hermes\r\n"
	if v, ok := ParseRegQueryValue(out, "ICEBATH_HOME"); !ok || v != `C:\Program Files\Hermes` {
		t.Fatalf("got %q %v", v, ok)
	}
}

func TestParseRegQueryValueReturnsNullWhenTheValueLineIsAbsent(t *testing.T) {
	for _, out := range []string{"HKEY_CURRENT_USER\\Environment\r\n    Path    REG_SZ    C:\\x\r\n", "", "garbage"} {
		if v, ok := ParseRegQueryValue(out, "ICEBATH_HOME"); ok {
			t.Fatalf("%q gave %q", out, v)
		}
	}
}

func TestExpandWindowsEnvRefsExpandsVarCaseInsensitively(t *testing.T) {
	env := map[string]string{"USERPROFILE": `C:\Users\jeff`}
	if got := ExpandWindowsEnvRefs(`%UserProfile%\h`, env, []string{"USERPROFILE"}); got != `C:\Users\jeff\h` {
		t.Fatalf("got %q", got)
	}
}

func TestExpandWindowsEnvRefsLeavesLiteralPathsAndUnknownRefsIntact(t *testing.T) {
	if got := ExpandWindowsEnvRefs(`F:\Hermes\data`, map[string]string{}, nil); got != `F:\Hermes\data` {
		t.Fatalf("got %q", got)
	}
	if got := ExpandWindowsEnvRefs(`%NOPE%\x`, map[string]string{}, nil); got != `%NOPE%\x` {
		t.Fatalf("got %q", got)
	}
}

func TestReadWindowsUserEnvVarReturnsNullOffWindowsWithoutSpawning(t *testing.T) {
	spawned := false
	exec := func(string, []string) (string, error) {
		spawned = true
		return "", nil
	}
	if v, ok := ReadWindowsUserEnvVar("ICEBATH_HOME", Options{Platform: "linux", Exec: exec}); ok {
		t.Fatalf("got %q", v)
	}
	if spawned {
		t.Fatal("spawned reg off Windows")
	}
}

func TestReadWindowsUserEnvVarQueriesHkcuEnvironmentAndExpandsTheValue(t *testing.T) {
	var calls [][]any
	exec := func(cmd string, args []string) (string, error) {
		calls = append(calls, []any{cmd, args})
		return "HKEY_CURRENT_USER\\Environment\r\n    ICEBATH_HOME    REG_EXPAND_SZ    %DRIVE%\\Hermes\r\n", nil
	}
	v, ok := ReadWindowsUserEnvVar("ICEBATH_HOME", Options{Platform: "win32", Env: map[string]string{"DRIVE": "F:"}, EnvOrder: []string{"DRIVE"}, Exec: exec})
	if !ok || v != `F:\Hermes` {
		t.Fatalf("got %q %v", v, ok)
	}
	want := [][]any{{"reg", []string{"query", `HKCU\Environment`, "/v", "ICEBATH_HOME"}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
}

func TestReadWindowsUserEnvVarReturnsNullWhenRegExitsNonZeroValueMissing(t *testing.T) {
	exec := func(string, []string) (string, error) { return "", errors.New("reg exited 1") }
	if v, ok := ReadWindowsUserEnvVar("ICEBATH_HOME", Options{Platform: "win32", Env: map[string]string{}, Exec: exec}); ok {
		t.Fatalf("got %q", v)
	}
}

func TestReadWindowsUserEnvVarReturnsNullForAnEmptyValue(t *testing.T) {
	exec := func(string, []string) (string, error) { return "    ICEBATH_HOME    REG_SZ    \r\n", nil }
	if v, ok := ReadWindowsUserEnvVar("ICEBATH_HOME", Options{Platform: "win32", Env: map[string]string{}, Exec: exec}); ok {
		t.Fatalf("got %q", v)
	}
}
