package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Elevation fact, whoami and the native writer faked like the Python _fake_elevated fixture, returning the grants and command runs
func fakeElevated(t *testing.T, tmp string, elevated bool) (*[]string, *[][]string) {
	t.Helper()
	aclSeams(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	var granted []string
	var runs [][]string
	whoamiSID = func() (string, error) {
		runs = append(runs, []string{"whoami", "/user", "/fo", "csv", "/nh"})
		return "S-1-5-21-1-2-3-1001", nil
	}
	runACLCommand = func(argv []string) error { runs = append(runs, argv); return nil }
	processIsElevated = func() bool { return elevated }
	grantNative = func(p string, _ bool, _ string) (bool, error) {
		real, _ := pypath.Realpath(p, pypath.Lax)
		granted = append(granted, real)
		return false, nil
	}
	return &granted, &runs
}

func TestRepairRunsOnlyFromAnElevatedUpdate(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "home"), 0o777); err != nil {
		t.Fatal(err)
	}
	granted, runs := fakeElevated(t, tmp, false)
	ctx := context.Background()
	no, yes := false, true
	if err := RepairWindowsHomeACL(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := RepairWindowsHomeACL(ctx, &no); err != nil {
		t.Fatal(err)
	}
	if len(*granted) != 0 || len(*runs) != 0 {
		t.Fatalf("unelevated repair granted %v ran %v", *granted, *runs)
	}
	if err := RepairWindowsHomeACL(ctx, &yes); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(tmp, "home")}; !reflect.DeepEqual(*granted, want) {
		t.Fatalf("granted %v, want %v", *granted, want)
	}
}

func TestRepairNeverRecursesThroughALinkInsideTheHome(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(h, "cache"), 0o777); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "Program Files", "X")
	if err := os.MkdirAll(filepath.Join(outside, "bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	symlinkOrFail(t, outside, filepath.Join(h, "cache", "images"))
	granted, runs := fakeElevated(t, tmp, true)
	if err := RepairWindowsHomeACL(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, argv := range *runs {
		if len(argv) > 0 && argv[0] == "icacls" && slices.Contains(argv, "/T") {
			t.Fatalf("recursive icacls ran: %v", *runs)
		}
	}
	if want := []string{h, filepath.Join(h, "cache")}; !reflect.DeepEqual(*granted, want) {
		t.Fatalf("granted %v, want %v", *granted, want)
	}
}

func TestRepairStaysOwedWhenNothingCouldBeGranted(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "home"), 0o777); err != nil {
		t.Fatal(err)
	}
	fakeElevated(t, tmp, true)
	grantNative = func(string, bool, string) (bool, error) { return false, errors.New("access denied") }
	if err := RepairWindowsHomeACL(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "could not restore user access") {
		t.Fatalf("repair = %v", err)
	}
}

func TestRepairWalksBelowAnOperatorLinkedHome(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real-home")
	if err := os.MkdirAll(filepath.Join(real, "state"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeACLFile(t, filepath.Join(real, "state", "s.json"), "{}")
	outside := filepath.Join(tmp, "outside")
	if err := os.Mkdir(outside, 0o777); err != nil {
		t.Fatal(err)
	}
	symlinkOrFail(t, outside, filepath.Join(real, "jn"))
	granted, _ := fakeElevated(t, tmp, true)
	symlinkOrFail(t, real, filepath.Join(tmp, "home"))
	if err := RepairWindowsHomeACL(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(slices.Values(*granted))
	want := slices.Sorted(slices.Values([]string{real, filepath.Join(real, "state"), filepath.Join(real, "state", "s.json")}))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("granted %v, want %v", got, want)
	}
}

func TestRepairThatRepairedNothingStaysOwed(t *testing.T) {
	tmp := t.TempDir()
	fakeElevated(t, tmp, true)
	symlinkOrFail(t, filepath.Join(tmp, "gone"), filepath.Join(tmp, "home"))
	if err := RepairWindowsHomeACL(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "nothing was repaired") {
		t.Fatalf("repair = %v", err)
	}
}

func TestRepairWarnsOnAPartialFailure(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.MkdirAll(filepath.Join(h, "sessions"), 0o777); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(h, "sessions", "a.json")
	writeACLFile(t, bad, "{}")
	fakeElevated(t, tmp, true)
	grantNative = func(p string, _ bool, _ string) (bool, error) {
		if p == bad {
			return false, errors.New("Access is denied")
		}
		return false, nil
	}
	warnings := captureWarnings(t)
	if err := RepairWindowsHomeACL(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	want := "Restoring user access under " + h + " failed for 1 entry, e.g. " + bad + ": "
	if !strings.Contains(warnings.String(), want) {
		t.Fatalf("warning %q missing %q", warnings.String(), want)
	}
}
