package profiles

import (
	"context"
	"errors"
	"github.com/nickheyer/icebath/pkg/config"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Temp user home with the default home under it and caches cleared
func profileEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
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

func TestNormalizeProfileName(t *testing.T) {
	for in, want := range map[string]string{"Jules": "jules", "  Librarian ": "librarian", "0": "0", "DEFAULT": "default"} {
		if got, err := NormalizeProfileName(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
	for _, v := range []any{0, nil, 42} {
		if _, err := NormalizeProfileNameValue(v); !isValueError(err) {
			t.Fatalf("%v accepted: %v", v, err)
		}
	}
	if _, err := NormalizeProfileNameValue(0); err == nil || err.Error() != "profile name must be a string, got int: 0" {
		t.Fatalf("message %v", err)
	}
	if _, err := NormalizeProfileName("   "); !isValueError(err) {
		t.Fatal("blank accepted")
	}
}

func TestValidateProfileName(t *testing.T) {
	for _, n := range []string{"coder", "work-bot", "a1", "my_agent", "default"} {
		if err := ValidateProfileName(n); err != nil {
			t.Fatalf("%s rejected: %v", n, err)
		}
	}
	for _, n := range []string{"UPPER", "has space", ".hidden", "-leading", "sudo"} {
		if err := ValidateProfileName(n); !isValueError(err) {
			t.Fatalf("%s accepted", n)
		}
	}
	if err := ValidateProfileName("My Work"); err == nil || !strings.Contains(err.Error(), "(for example: my-work). Then run `icebath profile create my-work`.") {
		t.Fatalf("suggestion %v", err)
	}
}

func TestGetProfileDir(t *testing.T) {
	tmp := profileEnv(t)
	if got, err := GetProfileDir("default"); err != nil || got != filepath.Join(tmp, ".icebath") {
		t.Fatalf("default %q %v", got, err)
	}
	for _, n := range []string{"..", "../outside", "../../tmp", "a/b", `a\b`, ".hidden", "has space"} {
		if _, err := GetProfileDir(n); !isValueError(err) {
			t.Fatalf("%q accepted", n)
		}
	}
	for _, n := range []string{"..", "../outside", "a/b"} {
		if ProfileExists(n) {
			t.Fatalf("%q exists", n)
		}
	}
}

func TestSetActiveProfileToDefaultRemovesFile(t *testing.T) {
	tmp := profileEnv(t)
	makeProfile(t, tmp, "coder")
	if err := SetActiveProfile("coder"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(tmp, ".icebath", "active_profile")
	if GetActiveProfile("") != "coder" {
		t.Fatal("not active")
	}
	if err := SetActiveProfile("default"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); err == nil {
		t.Fatal("file kept")
	}
	if err := SetActiveProfile("ghost"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing profile accepted: %v", err)
	}
}

func TestGetActiveProfileName(t *testing.T) {
	tmp := profileEnv(t)
	dir := makeProfile(t, tmp, "coder")
	t.Setenv(home.EnvHome, dir)
	if got := GetActiveProfileName(context.Background()); got != "coder" {
		t.Fatalf("profile path %q", got)
	}
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

func TestAliasCollisionTraversalRejectedBeforePathLookup(t *testing.T) {
	profileEnv(t)
	called := false
	old := runWhich
	runWhich = func(string) (string, int, error) { called = true; return "", 1, nil }
	t.Cleanup(func() { runWhich = old })
	msg, err := CheckAliasCollision("../../.bashrc")
	if err != nil || !strings.Contains(strings.ToLower(msg), "invalid alias name") || called {
		t.Fatalf("%q %v called=%v", msg, err, called)
	}
}

func TestAliasNamedLikeServerSubcommandAllowed(t *testing.T) {
	profileEnv(t)
	old := runWhich
	runWhich = func(string) (string, int, error) { return "", 1, nil }
	t.Cleanup(func() { runWhich = old })
	for _, n := range []string{"dashboard", "serve", "webapp"} {
		if msg, err := CheckAliasCollision(n); msg != "" || err != nil {
			t.Fatalf("%s: %q %v", n, msg, err)
		}
	}
	if msg, _ := CheckAliasCollision("gateway"); msg != "'gateway' conflicts with an icebath subcommand" {
		t.Fatalf("subcommand %q", msg)
	}
	runWhich = func(string) (string, int, error) { return "/usr/bin/foo\n", 0, nil }
	if msg, _ := CheckAliasCollision("foo"); msg != "'foo' conflicts with an existing command (/usr/bin/foo)" {
		t.Fatalf("existing %q", msg)
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
	old := runWhich
	runWhich = func(string) (string, int, error) { return bat, 0, nil }
	t.Cleanup(func() { runWhich = old })
	if msg, err := CheckAliasCollision("mybot"); msg != "" || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
}

func TestCreateWrapperScriptOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix platform test")
	}
	profileEnv(t)
	old := lookPath
	lookPath = func(string) (string, error) { return "/opt/icebath/bin/icebath", nil }
	t.Cleanup(func() { lookPath = old })
	path, err := CreateWrapperScript("mybot", "")
	if err != nil || filepath.Base(path) != "mybot" {
		t.Fatalf("%q %v", path, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "#!/bin/sh") || !strings.Contains(string(data), "exec /opt/icebath/bin/icebath -p mybot") {
		t.Fatalf("content %q", data)
	}
	if st, _ := os.Stat(path); st.Mode().Perm()&0o111 != 0o111 {
		t.Fatalf("mode %v", st.Mode())
	}
	if removed, err := RemoveWrapperScript("mybot"); !removed || err != nil {
		t.Fatalf("remove %v %v", removed, err)
	}
}

func TestRemoveWrapperFindsBatOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows platform test")
	}
	profileEnv(t)
	path, err := CreateWrapperScript("mybot", "")
	if err != nil || path == "" {
		t.Fatalf("%q %v", path, err)
	}
	if removed, _ := RemoveWrapperScript("mybot"); !removed {
		t.Fatal("not removed")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("still exists")
	}
}

func TestCreateWrapperRejectsTraversalAndAbsolutePath(t *testing.T) {
	tmp := profileEnv(t)
	sentinel := filepath.Join(tmp, ".bashrc")
	_ = os.WriteFile(sentinel, []byte("keep"), 0o644)
	if _, err := CreateWrapperScript("../../.bashrc", "coder"); err == nil || !strings.Contains(err.Error(), "Invalid alias name") {
		t.Fatalf("traversal %v", err)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != "keep" {
		t.Fatal("sentinel changed")
	}
	target := filepath.Join(tmp, "abs-wrapper")
	if _, err := CreateWrapperScript(target, ""); err == nil || !strings.Contains(err.Error(), "Invalid alias name") {
		t.Fatalf("absolute %v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("absolute wrapper written")
	}
}

func TestFindAliasForProfile(t *testing.T) {
	profileEnv(t)
	if _, err := CreateWrapperScript("steve", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := FindAliasForProfile("steve"); got != "steve" {
		t.Fatalf("named alias %q", got)
	}
	if _, err := CreateWrapperScript("qiaobusi", "steve"); err != nil {
		t.Fatal(err)
	}
	if got, _ := FindAliasForProfile("Steve"); got != "qiaobusi" {
		t.Fatalf("custom alias %q", got)
	}
}

func TestFindAliasIgnoresUnrelatedFiles(t *testing.T) {
	profileEnv(t)
	dir, _ := WrapperDir()
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "pip"), []byte("#!/bin/sh\nexec python -m pip \"$@\"\n"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "blob"), []byte{0xff, 0xfe, 'i', 'c', 'e'}, 0o755)
	if got, _ := FindAliasForProfile("steve"); got != "" {
		t.Fatalf("unrelated %q", got)
	}
}

// Seeds profile.yaml with a hand curated description
func seedMeta(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "coder")
	_ = os.MkdirAll(dir, 0o755)
	d, auto := "Curated by hand", false
	if err := WriteProfileMeta(dir, MetaUpdate{Description: &d, DescriptionAuto: &auto}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Runs a meta write whose serialization fails
func interruptedWrite(t *testing.T, dir string) {
	defer config.SwapSafeDump(func(any) ([]byte, error) { return nil, errors.New("simulated interruption mid-write") })()
	auto := true
	if err := WriteProfileMeta(dir, MetaUpdate{DescriptionAuto: &auto}); err == nil {
		t.Fatal("write succeeded")
	}
}

func TestFailedMetaWriteLeavesExistingFileIntact(t *testing.T) {
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

func TestFailedMetaWriteDoesNotDropUnspecifiedFields(t *testing.T) {
	dir := seedMeta(t)
	interruptedWrite(t, dir)
	auto := true
	if err := WriteProfileMeta(dir, MetaUpdate{DescriptionAuto: &auto}); err != nil {
		t.Fatal(err)
	}
	meta := ReadProfileMeta(dir)
	if meta.Description != "Curated by hand" || !meta.DescriptionAuto {
		t.Fatalf("%+v", meta)
	}
}

func TestEmojiDescriptionWrittenAsRealUTF8(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wizard")
	_ = os.MkdirAll(dir, 0o755)
	d := "Code wizard \U0001F9D9 ✨"
	if err := WriteProfileMeta(dir, MetaUpdate{Description: &d}); err != nil {
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

func TestSymlinkedProfileYAMLSurvivesWrite(t *testing.T) {
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
	if err := WriteProfileMeta(dir, MetaUpdate{Description: &d}); err != nil {
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

func TestResolveProfileEnvMatrixPreservesConfiguredSpelling(t *testing.T) {
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

func TestResolveProfileEnvMissingNamedProfileRaises(t *testing.T) {
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "configured-root"))
	if _, err := ResolveProfileEnv("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%v", err)
	}
}

func TestResolveProfileEnvUnsetFallsBackToDefaultRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(home.EnvHome, "")
	if got, err := ResolveProfileEnv("default"); err != nil || got != DefaultHome() {
		t.Fatalf("%q %v want %q", got, err, DefaultHome())
	}
}

func TestDisplayNameMetaRoundTripPreservesOtherFields(t *testing.T) {
	tmp := profileEnv(t)
	def := filepath.Join(tmp, ".icebath")
	d, dn := "ops agent", "小助手"
	_ = WriteProfileMeta(def, MetaUpdate{Description: &d})
	_ = WriteProfileMeta(def, MetaUpdate{DisplayName: &dn})
	meta := ReadProfileMeta(def)
	if meta.DisplayName != dn || meta.Description != d {
		t.Fatalf("%+v", meta)
	}
}

func TestDisplayNameMissingFileDefaultsEmpty(t *testing.T) {
	tmp := profileEnv(t)
	if got := ReadProfileMeta(filepath.Join(tmp, ".icebath")).DisplayName; got != "" {
		t.Fatalf("%q", got)
	}
}

func TestBotTitleShapes(t *testing.T) {
	tmp := profileEnv(t)
	def := filepath.Join(tmp, ".icebath")
	path := filepath.Join(def, "profile.yaml")
	for text, want := range map[string]string{
		"ui_meta:\n  icebath-bots:\n    title: JordyV\n":         "JordyV",
		"display_name: JordieF\nui_meta:\n  user-sections: {}\n": "",
		"ui_meta: icebath-bots\n":                                "",
		"ui_meta:\n  icebath-bots: 7\n":                          "",
	} {
		_ = os.WriteFile(path, []byte(text), 0o644)
		ResetFileCache()
		if got := ReadProfileMeta(def).BotTitle; got != want {
			t.Fatalf("%q: %q", text, got)
		}
	}
}

func TestEmptyDisplayNameClearsKey(t *testing.T) {
	tmp := profileEnv(t)
	def := filepath.Join(tmp, ".icebath")
	dn, empty := "Harumesu", ""
	_ = WriteProfileMeta(def, MetaUpdate{DisplayName: &dn})
	_ = WriteProfileMeta(def, MetaUpdate{DisplayName: &empty})
	if m := loadYAMLDict(filepath.Join(def, "profile.yaml")); m == nil || m["display_name"] != nil {
		t.Fatalf("%+v", m)
	}
}

func TestDisplayNameSetter(t *testing.T) {
	tmp := profileEnv(t)
	if got, err := SetProfileDisplayName("default", "  Harumesu "); err != nil || got != "Harumesu" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := SetProfileDisplayName("default", strings.Repeat("x", 65)); !isValueError(err) {
		t.Fatalf("long %v", err)
	}
	if _, err := SetProfileDisplayName("ghost", "Ghost"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing %v", err)
	}
	def := filepath.Join(tmp, ".icebath")
	if dir, _ := GetProfileDir("default"); dir != def || !ProfileExists("default") || ProfileExists("harumesu") {
		t.Fatal("display name became resolvable")
	}
	if got, _ := ResolveProfileEnv("default"); got != def {
		t.Fatalf("%q", got)
	}
	if _, err := ResolveProfileEnv("harumesu"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%v", err)
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
