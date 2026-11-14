package tools

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestEmptyPathIsAMissNotAmbient(t *testing.T) {
	res := locateCommand("python3", &commandLookupContext{Path: strPtr("")}, nil)
	if res.Kind != "missing" || len(res.Command()) != 0 || len(res.PresentCandidates()) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestAbsentAndNoneBothMeanAmbient(t *testing.T) {
	if (&commandLookupContext{}).Path != nil || (&commandLookupContext{}).effectivePath() != nil {
		t.Fatal("unset path is not ambient")
	}
	if p := (&commandLookupContext{Path: strPtr("/x")}).effectivePath(); p == nil || *p != "/x" {
		t.Fatalf("explicit path %v", p)
	}
}

func TestOneCandidatePerKnownDirRegardlessOfPathext(t *testing.T) {
	tmp := t.TempDir()
	a, b := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	res := locateCommand("nothing-here", &commandLookupContext{Path: strPtr("")}, []string{a, b})
	var sources []string
	for _, c := range res.Candidates {
		sources = append(sources, c.Source)
	}
	if want := []string{"PATH", "known_dir:" + a, "known_dir:" + b}; !slices.Equal(sources, want) {
		t.Fatalf("sources %v want %v", sources, want)
	}
}

func TestKnownDirTablesMatchTheHostOS(t *testing.T) {
	var every []string
	for _, table := range [][]string{homebrewDirs(), userLocalBin(), rustToolDirs(), nodeToolDirs(), icebathVendoredDirs(), windowsUserProgramDirs()} {
		every = append(every, table...)
	}
	if len(every) == 0 {
		t.Fatal("at least one table applies on every host")
	}
	for _, d := range every {
		if runtime.GOOS == "windows" && strings.HasPrefix(d, "/") {
			t.Fatalf("posix dir %q on windows", d)
		}
		if runtime.GOOS != "windows" && strings.Contains(d, "%") {
			t.Fatalf("windows dir %q on %s", d, runtime.GOOS)
		}
	}
	if runtime.GOOS != "windows" && (len(homebrewDirs()) > 0) != (runtime.GOOS == "darwin") {
		t.Fatalf("homebrew dirs %v on %s", homebrewDirs(), runtime.GOOS)
	}
	if (len(windowsUserProgramDirs()) > 0) != (runtime.GOOS == "windows") || (len(visualStudioInstallerDirs()) > 0) != (runtime.GOOS == "windows") {
		t.Fatalf("windows tables %v %v on %s", windowsUserProgramDirs(), visualStudioInstallerDirs(), runtime.GOOS)
	}
	if (len(macApplicationDirs()) > 0) != (runtime.GOOS == "darwin") {
		t.Fatalf("mac application dirs %v on %s", macApplicationDirs(), runtime.GOOS)
	}
	if (len(flatpakExportDirs()) > 0) != (runtime.GOOS == "linux") || (len(snapBinDirs()) > 0) != (runtime.GOOS == "linux") {
		t.Fatalf("linux tables %v %v on %s", flatpakExportDirs(), snapBinDirs(), runtime.GOOS)
	}
	if got := rustToolDirs()[0]; got != map[bool]string{true: "%USERPROFILE%/.cargo/bin", false: "~/.cargo/bin"}[runtime.GOOS == "windows"] {
		t.Fatalf("rust dir %q", got)
	}
}

func TestObservationNotChecked(t *testing.T) {
	o := observationNotChecked[string]()
	if o.State != checkNotChecked || o.Value != nil || o.Detail != "" {
		t.Fatalf("%+v", o)
	}
	v := true
	if p := (observation[bool]{State: checkPresent, Value: &v}); p.State != "present" || !*p.Value {
		t.Fatalf("%+v", p)
	}
	for _, s := range []checkState{checkAbsent, checkUnavailable, checkError} {
		if s == "" {
			t.Fatal("empty state")
		}
	}
}
