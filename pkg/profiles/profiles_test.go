package profiles

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Temp user home with the default home under it and caches cleared
func profileEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	ResetFileCache()
	home.ResetKeyCache()
	return tmp
}

// Named profile directory carrying a config.yaml identity marker
func makeProfile(t *testing.T, tmp, name string) string {
	t.Helper()
	dir := filepath.Join(tmp, ".icebath", "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func isValueError(err error) bool {
	var ve *ValueError
	return errors.As(err, &ve)
}

func TestNormalizeProfileNameTitleCaseNormalized(t *testing.T) {
	for in, want := range map[string]string{"Jules": "jules", "  Librarian ": "librarian"} {
		if got, err := NormalizeProfileName(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
}

func TestNormalizeProfileNameNonStringNameRejected(t *testing.T) {
	for _, v := range []any{0, nil, 42} {
		if _, err := NormalizeProfileNameValue(v); !isValueError(err) {
			t.Fatalf("%v accepted: %v", v, err)
		}
	}
	if _, err := NormalizeProfileNameValue(0); err == nil || err.Error() != "profile name must be a string, got int: 0" {
		t.Fatalf("message %v", err)
	}
}

func TestNormalizeProfileNameLiteralZeroStringIsAValidExplicitName(t *testing.T) {
	if got, err := NormalizeProfileName("0"); err != nil || got != "0" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestNormalizeProfileNameDefaultCaseAndBlankAndUnicodeFolds(t *testing.T) {
	for in, want := range map[string]string{"DEFAULT": "default", "\x1cDeFault\x1f": "default", "İX": "i̇x"} {
		if got, err := NormalizeProfileName(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
	if _, err := NormalizeProfileName("   "); !isValueError(err) || err.Error() != "profile name cannot be empty" {
		t.Fatalf("blank %v", err)
	}
}

func TestValidateProfileNameValidNamesAccepted(t *testing.T) {
	for _, n := range []string{"coder", "work-bot", "a1", "my_agent"} {
		t.Run(n, func(t *testing.T) {
			if err := ValidateProfileName(n); err != nil {
				t.Fatalf("%s rejected: %v", n, err)
			}
		})
	}
}

func TestValidateProfileNameInvalidNamesRejected(t *testing.T) {
	for _, n := range []string{"UPPER", "has space", ".hidden", "-leading"} {
		t.Run(n, func(t *testing.T) {
			if err := ValidateProfileName(n); !isValueError(err) {
				t.Fatalf("%s accepted", n)
			}
		})
	}
}

func TestValidateProfileNameReservedDefaultAndSuggestion(t *testing.T) {
	if err := ValidateProfileName("default"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProfileName("coder\n"); err != nil {
		t.Fatalf("re.match dollar before a final newline %v", err)
	}
	if err := ValidateProfileName("sudo"); !isValueError(err) {
		t.Fatal("reserved accepted")
	}
	if err := ValidateProfileName("My Work"); err == nil || !strings.Contains(err.Error(), "(for example: my-work). Then run `icebath profile create my-work`.") {
		t.Fatalf("suggestion %v", err)
	}
}

func TestGetProfileDirDefaultReturnsHermesHome(t *testing.T) {
	tmp := profileEnv(t)
	if got, err := GetProfileDir("default"); err != nil || got != filepath.Join(tmp, ".icebath") {
		t.Fatalf("default %q %v", got, err)
	}
}

func TestGetProfileDirTraversalAndInvalidNamesRejected(t *testing.T) {
	profileEnv(t)
	for _, n := range []string{"..", "../outside", "../../tmp", "a/b", `a\b`, ".hidden", "has space"} {
		t.Run(n, func(t *testing.T) {
			if _, err := GetProfileDir(n); !isValueError(err) {
				t.Fatalf("%q accepted: %v", n, err)
			}
		})
	}
}

func TestGetProfileDirProfileExistsFalseForInvalidNames(t *testing.T) {
	profileEnv(t)
	for _, n := range []string{"..", "../outside", "a/b"} {
		t.Run(n, func(t *testing.T) {
			if ProfileExists(n) {
				t.Fatalf("%q exists", n)
			}
		})
	}
}

func TestGetActiveProfileNameCustomPathReturnsDefault(t *testing.T) {
	tmp := profileEnv(t)
	custom := filepath.Join(tmp, "some", "other", "path")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, custom)
	if got := GetActiveProfileName(context.Background()); got != "default" {
		t.Fatalf("custom path %q", got)
	}
}

func TestCurrentProfileNamePinOnlyOutsideOverride(t *testing.T) {
	tmp := profileEnv(t)
	dir := makeProfile(t, tmp, "coder")
	t.Setenv("ICEBATH_PROFILE_NAME", "pinned")
	if got := CurrentProfileName(context.Background()); got != "pinned" {
		t.Fatalf("pin %q", got)
	}
	if got := CurrentProfileName(home.WithOverride(context.Background(), dir)); got != "coder" {
		t.Fatalf("override %q", got)
	}
}

// Replaces the which or where probe for the test, counting its calls
func fakeWhich(t *testing.T, stdout string, code int, err error) *int {
	t.Helper()
	calls := 0
	old := runWhich
	runWhich = func(string) (string, int, error) { calls++; return stdout, code, err }
	t.Cleanup(func() { runWhich = old })
	return &calls
}

func TestAliasCollisionTraversalAliasRejectedBeforePathLookup(t *testing.T) {
	profileEnv(t)
	calls := fakeWhich(t, "", 1, nil)
	msg, err := CheckAliasCollision("../../.bashrc")
	if err != nil || msg == "" || !strings.Contains(strings.ToLower(msg), "invalid alias name") {
		t.Fatalf("%q %v", msg, err)
	}
	if *calls != 0 {
		t.Fatalf("which ran %d times", *calls)
	}
}

func TestAnAliasNamedLikeAServerSubcommandIsAllowedAsOnMain(t *testing.T) {
	for _, n := range []string{"dashboard", "serve", "webapp"} {
		t.Run(n, func(t *testing.T) {
			profileEnv(t)
			fakeWhich(t, "", 1, nil)
			if msg, err := CheckAliasCollision(n); msg != "" || err != nil {
				t.Fatalf("%s: %q %v", n, msg, err)
			}
		})
	}
}

func TestAliasCollisionSubcommandExistingCommandAndLaunchFailures(t *testing.T) {
	profileEnv(t)
	fakeWhich(t, "", 1, nil)
	if msg, _ := CheckAliasCollision("gateway"); msg != "'gateway' conflicts with an icebath subcommand" {
		t.Fatalf("subcommand %q", msg)
	}
	fakeWhich(t, "  /usr/bin/foo\r\n/opt/foo\n", 0, nil)
	if msg, _ := CheckAliasCollision("foo"); msg != "'foo' conflicts with an existing command (/usr/bin/foo)" {
		t.Fatalf("existing %q", msg)
	}
	for _, skip := range []error{exec.ErrNotFound, &pysubprocess.TimeoutExpired{Cmd: []string{"which", "foo"}, Timeout: 5}} {
		fakeWhich(t, "", -1, skip)
		if msg, err := CheckAliasCollision("foo"); msg != "" || err != nil {
			t.Fatalf("%v: %q %v", skip, msg, err)
		}
	}
	fakeWhich(t, "", -1, fs.ErrPermission)
	if _, err := CheckAliasCollision("foo"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("permission error swallowed %v", err)
	}
	fakeWhich(t, " \n", 0, nil)
	if _, err := CheckAliasCollision("foo"); err == nil || err.Error() != "list index out of range" {
		t.Fatalf("empty which output %v", err)
	}
}

func TestAliasCollisionWindowsChecksBatExtension(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows platform test")
	}
	tmp := profileEnv(t)
	dir := filepath.Join(tmp, ".local", "bin")
	_ = os.MkdirAll(dir, 0o755)
	bat := filepath.Join(dir, "mybot.bat")
	_ = os.WriteFile(bat, []byte("@echo off\r\nicebath -p mybot %*\r\n"), 0o644)
	fakeWhich(t, bat, 0, nil)
	if msg, err := CheckAliasCollision("mybot"); msg != "" || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
}

func TestWrapperScriptCreatesShOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix platform test")
	}
	profileEnv(t)
	old := lookPath
	lookPath = func(string) (string, error) { return "/opt/icebath/bin/icebath", nil }
	t.Cleanup(func() { lookPath = old })
	path, err := CreateWrapperScript("mybot", "")
	if err != nil || path == "" || filepath.Base(path) != "mybot" {
		t.Fatalf("%q %v", path, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "#!/bin/sh") || !strings.Contains(string(data), "exec /opt/icebath/bin/icebath -p mybot") {
		t.Fatalf("content %q", data)
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestWrapperScriptRemoveFindsBatOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows platform test")
	}
	profileEnv(t)
	path, err := CreateWrapperScript("mybot", "")
	if err != nil || path == "" {
		t.Fatalf("%q %v", path, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "@echo off\r\r\nicebath -p mybot %*\r\r\n" {
		t.Fatalf("text mode bytes %q", data)
	}
	if removed, _ := RemoveWrapperScript("mybot"); !removed {
		t.Fatal("not removed")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("still exists")
	}
}

func TestWrapperScriptSecurityCreateWrapperRejectsTraversal(t *testing.T) {
	tmp := profileEnv(t)
	sentinel := filepath.Join(tmp, ".bashrc")
	_ = os.WriteFile(sentinel, []byte("keep"), 0o644)
	if _, err := CreateWrapperScript("../../.bashrc", "coder"); !isValueError(err) || !strings.Contains(err.Error(), "Invalid alias name") {
		t.Fatalf("traversal %v", err)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != "keep" {
		t.Fatal("sentinel changed")
	}
}

func TestWrapperScriptSecurityCreateWrapperRejectsAbsolutePath(t *testing.T) {
	tmp := profileEnv(t)
	target := filepath.Join(tmp, "abs-wrapper")
	if _, err := CreateWrapperScript(target, ""); !isValueError(err) || !strings.Contains(err.Error(), "Invalid alias name") {
		t.Fatalf("absolute %v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("absolute wrapper written")
	}
}

func TestFindAliasForProfileProfileNamedAlias(t *testing.T) {
	profileEnv(t)
	if _, err := CreateWrapperScript("steve", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := FindAliasForProfile("steve"); err != nil || got != "steve" {
		t.Fatalf("named alias %q %v", got, err)
	}
	if _, err := CreateWrapperScript("qiaobusi", "steve"); err != nil {
		t.Fatal(err)
	}
	if got, _ := FindAliasForProfile("Steve"); got != "qiaobusi" {
		t.Fatalf("custom alias %q", got)
	}
}

func TestFindAliasForProfileIgnoresUnrelatedFiles(t *testing.T) {
	profileEnv(t)
	dir, err := WrapperDir()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pip"), []byte("#!/bin/sh\nexec python -m pip \"$@\"\n"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "blob"), []byte{0xff, 0xfe, 'i', 'c', 'e'}, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "steve."), []byte("#!/bin/sh\nexec icebath -p steve \"$@\"\n"), 0o755)
	if got, err := FindAliasForProfile("steve"); err != nil || got != "" {
		t.Fatalf("unrelated %q %v", got, err)
	}
}

// Seeds profile.yaml with a hand curated description
func seedMeta(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "coder")
	_ = os.MkdirAll(dir, 0o755)
	d, auto := "Curated by hand", false
	if err := WriteProfileMeta(context.Background(), dir, MetaUpdate{Description: &d, DescriptionAuto: &auto}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Runs a meta write whose serialization fails
func interruptedWrite(t *testing.T, dir string) {
	defer config.SwapSafeDump(func(any) ([]byte, error) { return nil, errors.New("simulated interruption mid-write") })()
	auto := true
	if err := WriteProfileMeta(context.Background(), dir, MetaUpdate{DescriptionAuto: &auto}); err == nil {
		t.Fatal("write succeeded")
	}
}

func TestWriteProfileMetaDurabilityFailedWriteLeavesExistingFileIntact(t *testing.T) {
	dir := seedMeta(t)
	path := filepath.Join(dir, "profile.yaml")
	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), "Curated by hand") {
		t.Fatalf("seed %q", before)
	}
	interruptedWrite(t, dir)
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("changed %q", after)
	}
}

func TestWriteProfileMetaDurabilityFailedWriteDoesNotSilentlyDropUnspecifiedFields(t *testing.T) {
	dir := seedMeta(t)
	interruptedWrite(t, dir)
	auto := true
	if err := WriteProfileMeta(context.Background(), dir, MetaUpdate{DescriptionAuto: &auto}); err != nil {
		t.Fatal(err)
	}
	meta := ReadProfileMeta(dir)
	if meta.Description != "Curated by hand" || !meta.DescriptionAuto {
		t.Fatalf("%+v", meta)
	}
}

func TestWriteProfileMetaDurabilityEmojiDescriptionIsWrittenAsRealUtf8(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wizard")
	_ = os.MkdirAll(dir, 0o755)
	d := "Code wizard \U0001F9D9 ✨"
	if err := WriteProfileMeta(context.Background(), dir, MetaUpdate{Description: &d}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "profile.yaml"))
	if strings.Contains(string(raw), `\U`) || !strings.Contains(string(raw), "\U0001F9D9") {
		t.Fatalf("raw %q", raw)
	}
	if got := ReadProfileMeta(dir).Description; got != d {
		t.Fatalf("read %q", got)
	}
}

func TestWriteProfileMetaDurabilitySymlinkedProfileYamlSurvivesTheWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "linked")
	realDir := filepath.Join(tmp, "dotfiles")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.MkdirAll(realDir, 0o755)
	real := filepath.Join(realDir, "profile.yaml")
	_ = os.WriteFile(real, []byte("description: from dotfiles\n"), 0o644)
	if err := os.Symlink(real, filepath.Join(dir, "profile.yaml")); err != nil {
		t.Fatal(err)
	}
	d := "updated"
	if err := WriteProfileMeta(context.Background(), dir, MetaUpdate{Description: &d}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(dir, "profile.yaml")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link detached")
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), "updated") {
		t.Fatalf("target %q", data)
	}
	for _, d := range []string{dir, realDir} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("temp left %s", e.Name())
			}
		}
	}
}

func TestResolveProfileEnvSpellingResolutionMatrixPreservesConfiguredSpelling(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "configured-root")
	custom := filepath.Join(tmp, "custom-icebath")
	for _, d := range []string{filepath.Join(root, "profiles", "beta"), filepath.Join(root, "profiles", "coder"), filepath.Join(custom, "profiles", "beta")} {
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "config.yaml"), []byte("{}\n"), 0o644)
	}
	cases := []struct{ env, profile, want string }{
		{root, "coder", filepath.Join(root, "profiles", "coder")},
		{filepath.Join(root, "profiles", "alpha"), "beta", filepath.Join(root, "profiles", "beta")},
		{filepath.Join(root, "profiles", "alpha"), "default", root},
		{custom, "beta", filepath.Join(custom, "profiles", "beta")},
	}
	for _, c := range cases {
		t.Setenv(home.EnvHome, c.env)
		if got, err := ResolveProfileEnv(c.profile); err != nil || got != c.want {
			t.Fatalf("%v: %q %v", c, got, err)
		}
	}
}

func TestResolveProfileEnvSpellingMissingNamedProfileStillRaises(t *testing.T) {
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "configured-root"))
	if _, err := ResolveProfileEnv("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%v", err)
	}
}

func TestResolveProfileEnvSpellingUnsetEnvFallsBackToDefaultRoot(t *testing.T) {
	t.Cleanup(home.PatchUserHome(t.TempDir()))
	t.Setenv(home.EnvHome, "")
	if err := os.Unsetenv(home.EnvHome); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveProfileEnv("default"); err != nil || got != DefaultHome() {
		t.Fatalf("%q %v want %q", got, err, DefaultHome())
	}
}

func TestFormatProfileLabelShapes(t *testing.T) {
	for _, c := range [][3]string{{"default", "Harumesu", "Harumesu (default)"}, {"default", "", "default"}, {"worker", "worker", "worker"}, {"worker", "  ", "worker"}} {
		if got := FormatProfileLabel(c[0], c[1]); got != c[2] {
			t.Fatalf("%v: %q", c, got)
		}
	}
}

func TestListProfileNamesSkipsGhostsAndTombstones(t *testing.T) {
	tmp := profileEnv(t)
	makeProfile(t, tmp, "alpha")
	gone := makeProfile(t, tmp, "gone")
	_ = os.MkdirAll(filepath.Join(tmp, ".icebath", "profiles", "ghost"), 0o755)
	if err := home.MarkProfileDeleted(gone); err != nil {
		t.Fatal(err)
	}
	got := ListProfileNames()
	if strings.Join(got, ",") != "default,alpha" {
		t.Fatalf("%v", got)
	}
}

func TestPrivatePathPredicates(t *testing.T) {
	for _, parts := range [][]string{{"auth.json"}, {"Platforms", "Pairing", "x"}, {"config.yaml.bak-20240101t000000z"}, {".ssh", "id"}, {"ſlack_tokens.json"}} {
		if !ProfilePathIsPrivate(parts) {
			t.Fatalf("%v public", parts)
		}
	}
	for _, parts := range [][]string{{"skills"}, {"paıring"}, {"config.yaml"}} {
		if ProfilePathIsPrivate(parts) {
			t.Fatalf("%v private", parts)
		}
	}
	if !ProfilePathContainsPrivateStore([]string{"platforms"}) || ProfilePathContainsPrivateStore([]string{"platforms", "pairing"}) {
		t.Fatal("ancestor check")
	}
}

func TestCachedProfileReadReusesUntilSignatureChanges(t *testing.T) {
	ResetFileCache()
	path := filepath.Join(t.TempDir(), "x.yaml")
	calls := 0
	read := func() int { calls++; return calls }
	if cachedProfileRead(path, "k", read, nil) != 1 || cachedProfileRead(path, "k", read, nil) != 2 {
		t.Fatal("missing file cached")
	}
	_ = os.WriteFile(path, []byte("a"), 0o644)
	first := cachedProfileRead(path, "k", read, nil)
	if cachedProfileRead(path, "k", read, nil) != first {
		t.Fatal("not reused")
	}
	_ = os.WriteFile(path+".new", []byte("bb"), 0o644)
	_ = os.Rename(path+".new", path)
	if cachedProfileRead(path, "k", read, nil) == first {
		t.Fatal("stale after replace")
	}
}
