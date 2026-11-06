package pypath

import "testing"

func TestExpandvarsMatchesCPython(t *testing.T) {
	env := map[string]string{"HOME": "/h", "A": "x", "B-C": "dash"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	posix := map[string]string{
		"$HOME/a":       "/h/a",
		"${HOME}/a":     "/h/a",
		"$MISSING/a":    "$MISSING/a",
		"${A":           "${A",
		"${}":           "${}",
		"$A$A":          "xx",
		"no vars":       "no vars",
		"%A%":           "%A%",
		"$B-C":          "$B-C",
		"$é":            "$é",
		"${HOME}${A}/z": "/hx/z",
	}
	for in, want := range posix {
		if got := PosixExpandvars(in, lookup); got != want {
			t.Errorf("posix %q = %q, want %q", in, got, want)
		}
	}
	nt := map[string]string{
		`%HOME%\a`:    `/h\a`,
		`$HOME\a`:     `/h\a`,
		`${HOME}\a`:   `/h\a`,
		`%%`:          `%`,
		`$$`:          `$`,
		`'%HOME%'`:    `'%HOME%'`,
		`'%HOME%`:     `'%HOME%`,
		`%MISSING%\a`: `%MISSING%\a`,
		`%HOME`:       `%HOME`,
		`%`:           `%`,
		`$B-C`:        `dash`,
		`${A`:         `${A`,
	}
	for in, want := range nt {
		if got := NtExpandvars(in, lookup); got != want {
			t.Errorf("nt %q = %q, want %q", in, got, want)
		}
	}
}
