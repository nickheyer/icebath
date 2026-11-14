package pm

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Install steps the closure tests observe, nil names and an unset sync standing for a step never reached
type closureSpy struct {
	names      []string
	verified   *bool
	synced     bool
	syncExtras []string
	activated  []bool
	fail       map[string]bool
	activate   func() []string
}

// Checkout with the upstream pyproject over the real lockfile and registry, the install steps recorded
func closureInstallSpy(t *testing.T) (*closureSpy, context.Context, *bytes.Buffer) {
	t.Helper()
	tmp := isolateHome(t)
	repo := filepath.Join(tmp, "checkout")
	peWrite(t, filepath.Join(repo, "pyproject.toml"), peRead(t, filepath.Join(peReferences(t, "pyproject.toml"), "pyproject.toml")))
	peSwap(t, &repoRoot, func() string { return repo })
	spy := &closureSpy{fail: map[string]bool{}, activate: func() []string { return []string{} }}
	peSwap(t, &cliInstallNames, func(_ context.Context, names []string, _ *string, verify bool) (int, error) {
		spy.names = append(append([]string{}, spy.names...), names...)
		spy.verified = &verify
		failed := 0
		for _, name := range dedupeStrings(names) {
			if spy.fail[name] {
				failed++
			}
		}
		return failed, nil
	})
	peSwap(t, &installSyncVenv, func(_ context.Context, opts SyncVenvOptions) error {
		spy.synced = true
		spy.syncExtras = append([]string{}, opts.Extras...)
		return nil
	})
	peSwap(t, &installActivate, func(_ context.Context, allowIncomplete bool) ([]string, error) {
		spy.activated = append(spy.activated, allowIncomplete)
		return spy.activate(), nil
	})
	out := &bytes.Buffer{}
	return spy, withSyncOutput(context.Background(), out), out
}

// Runs the install command and fails the test on an error escaping it
func closureInstall(t *testing.T, ctx context.Context, args InstallArgs) int {
	t.Helper()
	code, err := CmdInstall(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestDefaultClosureIncludesTheBootInterpreter(t *testing.T) {
	spy, ctx, _ := closureInstallSpy(t)
	if code := closureInstall(t, ctx, InstallArgs{}); code != 0 {
		t.Fatalf("install %d", code)
	}
	if !slices.Contains(spy.names, "python") || slices.Contains(spy.names, "venv") {
		t.Fatalf("names %q", spy.names)
	}
	for _, name := range spy.names {
		p, err := GetPackage(name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Base().Internal {
			t.Fatalf("internal %s installed", name)
		}
	}
	if !spy.synced || !reflect.DeepEqual(spy.syncExtras, []string{"all"}) || !reflect.DeepEqual(spy.activated, []bool{true}) {
		t.Fatalf("sync %v %q activated %v", spy.synced, spy.syncExtras, spy.activated)
	}
}

func TestExplicitNamesPassThroughUntouched(t *testing.T) {
	for _, names := range [][]string{{"npm", "ripgrep"}, {"dmgbuild"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			spy, ctx, _ := closureInstallSpy(t)
			if code := closureInstall(t, ctx, InstallArgs{Names: names}); code != 0 {
				t.Fatalf("install %d", code)
			}
			if !reflect.DeepEqual(spy.names, names) || spy.synced || len(spy.activated) != 0 {
				t.Fatalf("names %q synced %v activated %v", spy.names, spy.synced, spy.activated)
			}
		})
	}
}

func TestToolsOnlyPublishesToolsAndStopsBeforeTheVenv(t *testing.T) {
	spy, ctx, _ := closureInstallSpy(t)
	if code := closureInstall(t, ctx, InstallArgs{Extra: []string{}, ToolsOnly: true}); code != 0 {
		t.Fatalf("install %d", code)
	}
	if !slices.Contains(spy.names, "python") || slices.Contains(spy.names, "venv") || spy.synced || !reflect.DeepEqual(spy.activated, []bool{true}) {
		t.Fatalf("names %q synced %v activated %v", spy.names, spy.synced, spy.activated)
	}
}

func TestTrustRecordedSkipsTheByteCheckAndStillSyncs(t *testing.T) {
	spy, ctx, _ := closureInstallSpy(t)
	if code := closureInstall(t, ctx, InstallArgs{Extra: []string{}, TrustRecorded: true}); code != 0 {
		t.Fatalf("install %d", code)
	}
	if !slices.Contains(spy.names, "python") || spy.verified == nil || *spy.verified || !reflect.DeepEqual(spy.syncExtras, []string{"all"}) {
		t.Fatalf("names %q verified %v sync %q", spy.names, spy.verified, spy.syncExtras)
	}
}

func TestTrustRecordedRefusesANarrowedInstall(t *testing.T) {
	target := "linux-x64"
	for _, c := range []struct {
		args    InstallArgs
		message string
	}{
		{InstallArgs{Names: []string{"ripgrep"}, Extra: []string{}, TrustRecorded: true}, "--trust-recorded"},
		{InstallArgs{Extra: []string{"anthropic"}, TrustRecorded: true}, "--trust-recorded"},
		{InstallArgs{Extra: []string{}, Target: &target, TrustRecorded: true}, "--target"},
	} {
		t.Run(c.message+strings.Join(c.args.Names, ",")+strings.Join(c.args.Extra, ","), func(t *testing.T) {
			spy, ctx, out := closureInstallSpy(t)
			if code := closureInstall(t, ctx, c.args); code != 1 {
				t.Fatalf("install %d", code)
			}
			if !strings.Contains(out.String(), c.message) || spy.names != nil || spy.synced {
				t.Fatalf("out %q names %q synced %v", out.String(), spy.names, spy.synced)
			}
		})
	}
}

func TestAMissingToolBlocksTheVenvSync(t *testing.T) {
	spy, ctx, out := closureInstallSpy(t)
	spy.activate = func() []string { return []string{"git: not installed or outdated"} }
	if code := closureInstall(t, ctx, InstallArgs{}); code != 1 {
		t.Fatalf("install %d", code)
	}
	if spy.synced || !strings.Contains(out.String(), "git: not installed or outdated") {
		t.Fatalf("synced %v out %q", spy.synced, out.String())
	}
}

// Names a bare install requests after the recorded names are cleared
func closureBareInstall(t *testing.T, spy *closureSpy, ctx context.Context, args InstallArgs) []string {
	t.Helper()
	spy.names = nil
	if code := closureInstall(t, ctx, args); code != 0 {
		t.Fatalf("bare install %d", code)
	}
	return spy.names
}

func TestBrowserToolsAreADefaultTheUserCanDeclineAndRestore(t *testing.T) {
	spy, ctx, _ := closureInstallSpy(t)
	if !slices.Contains(closureBareInstall(t, spy, ctx, InstallArgs{}), "chromium") {
		t.Fatal("chromium is no default")
	}
	if slices.Contains(closureBareInstall(t, spy, ctx, InstallArgs{Without: []string{"chromium"}}), "chromium") {
		t.Fatal("--without chromium installed chromium")
	}
	if slices.Contains(closureBareInstall(t, spy, ctx, InstallArgs{}), "chromium") {
		t.Fatal("the recorded opt-out did not stick")
	}
	spy.names = nil
	if code := closureInstall(t, ctx, InstallArgs{Names: []string{"chromium"}}); code != 0 {
		t.Fatalf("opt back in %d", code)
	}
	if !slices.Contains(closureBareInstall(t, spy, ctx, InstallArgs{}), "chromium") {
		t.Fatal("naming chromium did not restore the default")
	}
}

func TestAFailedDefaultDownloadDoesNotFailTheInstall(t *testing.T) {
	spy, ctx, out := closureInstallSpy(t)
	spy.fail["chromium"] = true
	if !slices.Contains(closureBareInstall(t, spy, ctx, InstallArgs{}), "chromium") {
		t.Fatal("chromium is no default")
	}
	if !reflect.DeepEqual(spy.syncExtras, []string{"all"}) || !strings.Contains(out.String(), "icebath pm install chromium") {
		t.Fatalf("sync %q out %q", spy.syncExtras, out.String())
	}
}

func TestWithoutRefusesARequiredPackage(t *testing.T) {
	spy, ctx, out := closureInstallSpy(t)
	if code := closureInstall(t, ctx, InstallArgs{Without: []string{"git"}}); code != 1 {
		t.Fatalf("install %d", code)
	}
	if !strings.Contains(out.String(), "--without accepts only") || spy.names != nil {
		t.Fatalf("out %q names %q", out.String(), spy.names)
	}
}
