package selfupdate

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Rows, guarded checks and directory creation print the bytes the Python primitives print
func TestDoctorReportPrimitives(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var b bytes.Buffer
	if !doctorCheckBool(&b, true, doctorRow{"git", "(2.4)"}, doctorRow{Text: "git not found"}, false) {
		t.Fatal("true cond reported false")
	}
	doctorCheckBool(&b, false, doctorRow{Text: "ok"}, doctorRow{"bad", "(why)"}, false)
	doctorCheckBool(&b, false, doctorRow{Text: "ok"}, doctorRow{Text: "worse"}, true)
	doctorSection(&b, "Title")
	doctorCheckInfo(&b, "hint")
	var issues []string
	doctorFailAndIssue(&b, "broken", "", "fix it", &issues)
	want := "  ✓ git (2.4)\n  ⚠ bad (why)\n  ✗ worse\n\n◆ Title\n    → hint\n  ✗ broken\n"
	if b.String() != want || len(issues) != 1 || issues[0] != "fix it" {
		t.Fatalf("rows %q issues %v", b.String(), issues)
	}

	b.Reset()
	f, err := doctorCheckGuarded(&b, "Relay plugin check failed: {e}", "", func(f *Finding) error {
		f.Issues = append(f.Issues, "kept")
		return errors.New("boom")
	})
	if err != nil || len(f.Issues) != 1 || b.String() != "  ⚠ Relay plugin check failed: boom\n" {
		t.Fatalf("guarded %v %v %q", f, err, b.String())
	}
	b.Reset()
	if f, err = doctorCheckGuarded(&b, "", "", func(*Finding) error { return errors.New("quiet") }); err != nil || f == nil || b.Len() != 0 {
		t.Fatalf("silent guard %v %v %q", f, err, b.String())
	}
	if f, err = doctorCheck(func(*Finding) error { return errors.New("raised") }); err == nil || f != nil {
		t.Fatalf("unguarded check swallowed %v", err)
	}
	var got []string
	report := func(_ io.Writer, text, detail string) { got = append(got, text, detail) }
	if err := doctorWarnOnError(&b, "x {e} {{y}}", "({e})", report, func() error { return errors.New("E") }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "x E {y}" || got[1] != "(E)" {
		t.Fatalf("report %q", got)
	}
	if err := doctorWarnOnError(&b, "{x}", "", report, func() error { return errors.New("E") }); err == nil || err.Error() != "'x'" {
		t.Fatalf("format error %v", err)
	}
}

// The format helper mirrors str.format with one keyword and its errors
func TestDoctorFormatError(t *testing.T) {
	e := errors.New("bad")
	for in, want := range map[string]string{"({e})": "(bad)", "{{e}} {e}": "{e} bad", "plain": "plain"} {
		if got, err := doctorFormatError(in, e); err != nil || got != want {
			t.Fatalf("%q gave %q %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"}": "Single '}' encountered in format string", "{e": "expected '}' before end of string",
		"{}": "Replacement index 0 out of range for positional args tuple", "{x}": "'x'"} {
		if _, err := doctorFormatError(in, e); err == nil || err.Error() != want {
			t.Fatalf("%q gave %v", in, err)
		}
	}
}

// Missing directories warn without fix and are created and counted with it
func TestDoctorEnsureDir(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	dir := filepath.Join(t.TempDir(), "a", "b")
	var b bytes.Buffer
	f := &Finding{}
	if err := doctorEnsureDir(&b, f, false, dir, "exists", "created", "missing"); err != nil || f.Fixed != 0 {
		t.Fatal(err)
	}
	if err := doctorEnsureDir(&b, f, true, dir, "exists", "created", "missing"); err != nil || f.Fixed != 1 {
		t.Fatal(err)
	}
	if err := doctorEnsureDir(&b, f, true, dir, "exists", "created", "missing"); err != nil || f.Fixed != 1 {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if want := "  ⚠ missing (will be created on first use)\n  ✓ created\n  ✓ exists\n"; b.String() != want {
		t.Fatalf("%q", b.String())
	}
	total := &Finding{Issues: []string{"a"}}
	total.Merge(&Finding{Issues: []string{"b"}, ManualIssues: []string{"m"}, Fixed: 2})
	if len(total.Issues) != 2 || len(total.ManualIssues) != 1 || total.Fixed != 2 {
		t.Fatalf("%+v", total)
	}
}
