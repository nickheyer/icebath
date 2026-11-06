package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Temp user home with the root icebath home inside and quiet gateway hooks
func profileTestHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	saved := profiles.GatewayStatusHooks
	t.Cleanup(func() { profiles.GatewayStatusHooks = saved })
	h := &profiles.GatewayStatusHooks
	h.ProfileRunning = func(string) bool { return false }
	h.ServesNamedProfile = func(string) bool { return false }
	h.NotifyProfilesChanged = func(string) {}
	h.RecordedServedProfiles = func() ([]string, bool, error) { return nil, false, nil }
	h.ExclusiveMaintenance = func([]string) (func(), error) { return func() {}, nil }
	h.PidExists = func(int) bool { return false }
	h.ProcessStartTime = func(int) (int64, bool) { return 0, false }
	h.TerminatePid = func(int, bool, *int64) error { return nil }
	h.RecordedHomeConflicts = func(map[string]any, string) bool { return false }
	return tmp
}

func runProfileCmd(a ProfileArgs) (string, string, error) {
	var out, errOut bytes.Buffer
	err := CmdProfile(context.Background(), a, &out, &errOut, strings.NewReader(""))
	return out.String(), errOut.String(), err
}

func exitCodeOf(err error) int {
	var exit *ExitError
	switch {
	case errors.As(err, &exit):
		return exit.Code
	case err != nil:
		return -1
	}
	return 0
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Install preview of a plan requiring FOO_TOKEN against a profile whose .env holds envBytes
func renderFooPlan(t *testing.T, envBytes []byte) string {
	t.Helper()
	t.Setenv("FOO_TOKEN", "")
	os.Unsetenv("FOO_TOKEN")
	profile := filepath.Join(t.TempDir(), "profile")
	writeTestFile(t, filepath.Join(profile, ".env"), envBytes)
	plan := &manage.InstallPlan{
		Manifest: &profiles.DistributionManifest{Name: "demo", Version: "1.0.0",
			EnvRequires: []profiles.EnvRequirement{{Name: "FOO_TOKEN", Required: true}}},
		Provenance: "local", TargetDir: profile,
	}
	var out bytes.Buffer
	renderDistributionPlan(&profileIO{ctx: context.Background(), out: &out, err: &out}, plan)
	return out.String()
}

func TestBOMPrefixedEnvFirstKeyIsDetectedAsSet(t *testing.T) {
	out := renderFooPlan(t, []byte("\xef\xbb\xbfFOO_TOKEN=abc\nBAR=1\n"))
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "FOO_TOKEN") {
			if !strings.Contains(line, "✓ set") {
				t.Fatalf("BOM must not hide the first key: %q", line)
			}
			return
		}
	}
	t.Fatalf("no FOO_TOKEN line in %q", out)
}

func TestNonUTF8EnvDoesNotAbortThePreview(t *testing.T) {
	out := renderFooPlan(t, []byte("FOO_TOKEN=\xff\xfe bad\n"))
	if !strings.Contains(out, "FOO_TOKEN") {
		t.Fatalf("preview lost the env line: %q", out)
	}
}

func TestProfileAliasRemovesAnOrphanAlias(t *testing.T) {
	tmp := profileTestHome(t)
	writeTestFile(t, filepath.Join(tmp, ".icebath", "profiles", "keep", "config.yaml"), []byte("model: m\n"))
	wrapper, err := profiles.CreateWrapperScript("petcare", "")
	if err != nil || wrapper == "" {
		t.Fatalf("wrapper %q %v", wrapper, err)
	}
	out, _, err := runProfileCmd(ProfileArgs{Action: "alias", ProfileName: "petcare", Remove: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(wrapper); !os.IsNotExist(statErr) {
		t.Fatalf("orphan wrapper still present: %v", statErr)
	}
	if !strings.Contains(out, "Removed alias 'petcare'") {
		t.Fatalf("output %q", out)
	}
}

func TestRemoveRefusesUnknownProfileAndNeverUnlinksAForeignFile(t *testing.T) {
	tmp := profileTestHome(t)
	foreign := filepath.Join(tmp, ".local", "bin", "notes")
	writeTestFile(t, foreign, []byte("just my notes\n"))
	bot, err := profiles.CreateWrapperScript("bot", "work")
	if err != nil || bot == "" {
		t.Fatalf("wrapper %q %v", bot, err)
	}
	if _, _, err := runProfileCmd(ProfileArgs{Action: "alias", ProfileName: "notes", Remove: true}); exitCodeOf(err) != 1 {
		t.Fatalf("exit %v", err)
	}
	if data, _ := os.ReadFile(foreign); string(data) != "just my notes\n" {
		t.Fatalf("foreign file changed: %q", data)
	}
	if _, _, err := runProfileCmd(ProfileArgs{Action: "alias", ProfileName: "ghost", Remove: true, AliasName: "bot"}); exitCodeOf(err) != 1 {
		t.Fatalf("--name removal of a missing profile exit %v", err)
	}
	if _, err := os.Stat(bot); err != nil {
		t.Fatalf("custom alias removed: %v", err)
	}
	if _, _, err := runProfileCmd(ProfileArgs{Action: "alias", ProfileName: "ghost"}); exitCodeOf(err) != 1 {
		t.Fatalf("alias creation for a missing profile exit %v", err)
	}
}

func TestEveryProfileSubcommandHasADispatchEntry(t *testing.T) {
	c := NewCLI()
	profile := c.find("profile")
	if profile == nil || len(profile.Commands()) == 0 {
		t.Fatal("the profile parser exposes no subcommands")
	}
	var parsed, dispatched []string
	for _, sub := range profile.Commands() {
		parsed = append(parsed, sub.Name())
	}
	for action := range profileActions {
		if action != "" {
			dispatched = append(dispatched, action)
		}
	}
	sort.Strings(parsed)
	sort.Strings(dispatched)
	if strings.Join(parsed, " ") != strings.Join(dispatched, " ") {
		t.Fatalf("parsed %v dispatched %v", parsed, dispatched)
	}
	for _, path := range c.Unbound() {
		if path == "profile" || strings.HasPrefix(path, "profile ") {
			t.Errorf("unbound %s", path)
		}
	}
}

func TestPurgeIdentityExitsNonzeroWhenSettlementStaysPending(t *testing.T) {
	profileTestHome(t)
	saved := purgeIdentityFn
	t.Cleanup(func() { purgeIdentityFn = saved })
	purgeIdentityFn = func(context.Context, string) (bool, error) { return false, nil }
	_, errOut, err := runProfileCmd(ProfileArgs{Action: "purge-identity", ProfileName: "gone"})
	if code := exitCodeOf(err); code == 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut, "icebath profile purge-identity gone") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestMigrateIdentityExitsNonzeroWhenMigrationStaysPending(t *testing.T) {
	profileTestHome(t)
	saved := migrateIdentityFn
	t.Cleanup(func() { migrateIdentityFn = saved })
	migrateIdentityFn = func(context.Context, string, string) (bool, error) { return false, nil }
	_, errOut, err := runProfileCmd(ProfileArgs{Action: "migrate-identity", OldName: "old", NewName: "new"})
	if code := exitCodeOf(err); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut, "icebath profile migrate-identity old new") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestPurgeIdentityRefusesASameNameProfileCreatedAfterTheDelete(t *testing.T) {
	profileTestHome(t)
	if _, err := manage.CreateProfile(context.Background(), "gone", manage.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runProfileCmd(ProfileArgs{Action: "purge-identity", ProfileName: "gone"})
	if code := exitCodeOf(err); code != 1 {
		t.Fatalf("exit %d %v", code, err)
	}
	if !strings.Contains(out, "Error: Profile 'gone' exists; purge-identity only settles") {
		t.Fatalf("output %q", out)
	}
}

func TestProfileUseDefaultClearsAMissingStickyProfile(t *testing.T) {
	tmp := profileTestHome(t)
	sticky := filepath.Join(tmp, ".icebath", "active_profile")
	writeTestFile(t, sticky, []byte("ray"))
	out, _, err := runProfileCmd(ProfileArgs{Action: "use", ProfileName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(sticky); !os.IsNotExist(statErr) {
		t.Fatalf("active_profile still present: %v", statErr)
	}
	if !strings.Contains(out, "Switched to: default (~/.icebath)") {
		t.Fatalf("output %q", out)
	}
}

func TestProfileDescribeArgumentErrors(t *testing.T) {
	cases := []struct {
		args ProfileArgs
		want string
	}{
		{ProfileArgs{AllMissing: true}, "profile describe: --all requires --auto"},
		{ProfileArgs{AllMissing: true, Auto: true, ProfileName: "x"}, "profile describe: --all is mutually exclusive with a profile name / --text"},
		{ProfileArgs{}, "profile describe: profile name is required (or --all --auto)"},
		{ProfileArgs{ProfileName: "x", Text: "t", Auto: true}, "profile describe: --text is mutually exclusive with --auto"},
	}
	for _, c := range cases {
		c.args.Action = "describe"
		_, errOut, err := runProfileCmd(c.args)
		if exitCodeOf(err) != 2 || strings.TrimSpace(errOut) != c.want {
			t.Errorf("%+v: exit %v stderr %q", c.args, err, errOut)
		}
	}
}

func TestProfileDescribeTextThenShow(t *testing.T) {
	tmp := profileTestHome(t)
	writeTestFile(t, filepath.Join(tmp, ".icebath", "profiles", "coder", "config.yaml"), []byte("{}\n"))
	out, _, err := runProfileCmd(ProfileArgs{Action: "describe", ProfileName: "coder", Text: "Writes Go"})
	if err != nil || out != "Description updated for 'coder'.\n" {
		t.Fatalf("set %q %v", out, err)
	}
	out, _, err = runProfileCmd(ProfileArgs{Action: "describe", ProfileName: "coder"})
	if err != nil || out != "Writes Go\n" {
		t.Fatalf("show %q %v", out, err)
	}
	_, errOut, err := runProfileCmd(ProfileArgs{Action: "describe", ProfileName: "ghost"})
	if exitCodeOf(err) != 1 || errOut != "Error: profile 'ghost' not found\n" {
		t.Fatalf("missing %q %v", errOut, err)
	}
}

func TestCLIExportRejectsBadProfileNameWithoutTraceback(t *testing.T) {
	profileTestHome(t)
	out, _, err := runProfileCmd(ProfileArgs{Action: "export", ProfileName: "bad//name"})
	if exitCodeOf(err) != 1 || !strings.Contains(out, "Error:") {
		t.Fatalf("exit %v output %q", err, out)
	}
}

func TestCLIExportDefaultDoesNotWriteIntoTheCurrentCheckout(t *testing.T) {
	tmp := profileTestHome(t)
	def := filepath.Join(tmp, ".icebath")
	writeTestFile(t, filepath.Join(def, "config.yaml"), []byte("model: test\n"))
	checkout := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	out, _, err := runProfileCmd(ProfileArgs{Action: "export", ProfileName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	exported, _ := filepath.Glob(filepath.Join(def, "profile-exports", "default-*.tar.gz"))
	if len(exported) != 1 || filepath.Dir(exported[0]) != filepath.Join(def, "profile-exports") {
		t.Fatalf("exports %v", exported)
	}
	if _, err := os.Stat(filepath.Join(checkout, "default.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("archive written into the checkout: %v", err)
	}
	if !strings.Contains(out, exported[0]) {
		t.Fatalf("output %q", out)
	}
}
