//go:build !windows

package browser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Stores or reads a marker in a real auth database so the snapshot exercises SQLite
func authDB(t *testing.T, path string, value string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if value != "" {
		for _, stmt := range []string{"create table if not exists marker(value)", "delete from marker"} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec("insert into marker values(?)", value); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	if err := db.QueryRowContext(context.Background(), "select value from marker").Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Minimal real looking Chromium user data dir with auth databases, caches and a live instance leftover
func makeProfile(t *testing.T, root string) string {
	t.Helper()
	for _, dir := range []string{"Default/Network", "Default/Cache/Cache_Data", "Code Cache/js", "Crashpad"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "Local State"), `{"os_crypt": {}}`)
	authDB(t, filepath.Join(root, "Default", "Cookies"), "sqlite-cookies")
	authDB(t, filepath.Join(root, "Default", "Network", "Cookies"), "sqlite-net-cookies")
	authDB(t, filepath.Join(root, "Default", "Login Data"), "sqlite-logins")
	writeFile(t, filepath.Join(root, "Default", "Preferences"), "{}")
	writeFile(t, filepath.Join(root, "Default", "Cache", "Cache_Data", "big"), strings.Repeat("x", 1000))
	writeFile(t, filepath.Join(root, "Code Cache", "js", "blob"), strings.Repeat("y", 1000))
	writeFile(t, filepath.Join(root, "Crashpad", "dump"), "z")
	if err := os.Symlink("dead-target-1", filepath.Join(root, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	return root
}

// Synthetic profile and a home whose config carries no pin, with the copy dir the snapshot lands in
func snapshotFixture(t *testing.T) (*tools.Env, string, string) {
	t.Helper()
	dir := t.TempDir()
	src := makeProfile(t, filepath.Join(dir, "real"))
	h := filepath.Join(dir, "hermes-home")
	t.Setenv("ICEBATH_HOME", h)
	return &tools.Env{Home: h}, src, filepath.Join(h, "browser-profile", "chrome")
}

func TestRealProfileResolversDataDirUnknownBrowserIsNone(t *testing.T) {
	if got := realProfileDataDir("firefox", "Windows"); got != "" {
		t.Fatalf("data dir %q", got)
	}
}

func TestSnapshotRealProfileFreshSnapshotCopiesAuthAndSkipsCaches(t *testing.T) {
	env, src, copyDir := snapshotFixture(t)
	dst, err := snapshotRealProfile(env, "chrome", src)
	if err != "" || dst != copyDir {
		t.Fatalf("snapshot %q %q", dst, err)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Cookies"), ""); got != "sqlite-cookies" {
		t.Fatalf("cookies marker %q", got)
	}
	for _, present := range []string{"Default/Network/Cookies", "Default/Login Data", "Local State"} {
		if !lexists(filepath.Join(copyDir, present)) {
			t.Fatalf("%s missing from the copy", present)
		}
	}
	for _, absent := range []string{"Default/Cache", "Code Cache", "Crashpad", "SingletonLock"} {
		if lexists(filepath.Join(copyDir, absent)) {
			t.Fatalf("%s reached the copy", absent)
		}
	}
}

func TestSnapshotRealProfileExistingSnapshotRefreshesAuthFilesOnly(t *testing.T) {
	env, src, copyDir := snapshotFixture(t)
	dst, err := snapshotRealProfile(env, "chrome", src)
	if err != "" {
		t.Fatal(err)
	}
	authDB(t, filepath.Join(src, "Default", "Cookies"), "sqlite-cookies-v2")
	history := filepath.Join(copyDir, "Default", "History")
	writeFile(t, history, "agent-session-history")
	dst2, err2 := snapshotRealProfile(env, "chrome", src)
	if err2 != "" || dst2 != dst {
		t.Fatalf("second snapshot %q %q", dst2, err2)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Cookies"), ""); got != "sqlite-cookies-v2" {
		t.Fatalf("cookies marker %q", got)
	}
	if raw, _ := os.ReadFile(history); string(raw) != "agent-session-history" {
		t.Fatalf("history %q", raw)
	}
}

func TestSnapshotRealProfileMissingSourceFailsClosed(t *testing.T) {
	dir := t.TempDir()
	env := &tools.Env{Home: filepath.Join(dir, "hh")}
	t.Setenv("ICEBATH_HOME", env.Home)
	dst, err := snapshotRealProfile(env, "chrome", filepath.Join(dir, "nope"))
	if dst != "" || err == "" {
		t.Fatalf("snapshot %q %q", dst, err)
	}
}

func TestSnapshotRealProfileSnapshotFilesAreOwnerOnly(t *testing.T) {
	env, src, _ := snapshotFixture(t)
	old := syscall.Umask(0o022)
	dst, err := snapshotRealProfile(env, "chrome", src)
	syscall.Umask(old)
	if err != "" || dst == "" {
		t.Fatalf("snapshot %q %q", dst, err)
	}
	offenders := []string{}
	filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
		if err == nil && path != dst && info.Mode().Perm()&0o077 != 0 {
			offenders = append(offenders, path+" "+info.Mode().Perm().String())
		}
		return nil
	})
	if len(offenders) != 0 {
		t.Fatalf("group/world-accessible snapshot entries: %v", offenders)
	}
}

func TestSnapshotRealProfileExistingLaxSnapshotHealsOnRefresh(t *testing.T) {
	env, src, _ := snapshotFixture(t)
	dst, err := snapshotRealProfile(env, "chrome", src)
	if err != "" || dst == "" {
		t.Fatalf("snapshot %q %q", dst, err)
	}
	cookies := filepath.Join(dst, "Default", "Cookies")
	if err := os.Chmod(cookies, 0o644); err != nil {
		t.Fatal(err)
	}
	dst2, err2 := snapshotRealProfile(env, "chrome", src)
	if err2 != "" || dst2 != dst {
		t.Fatalf("second snapshot %q %q", dst2, err2)
	}
	if info, err := os.Stat(cookies); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cookies mode %v %v", info.Mode().Perm(), err)
	}
}

func TestRealProfileCdpLaunchConsentOffIsNoop(t *testing.T) {
	env := testEnv(t)
	pin(t, &realProfileConsent, func(*tools.Env) bool { return false })
	if cdp, err := realProfileCDP(env); cdp != "" || err != "" {
		t.Fatalf("cdp %q err %q", cdp, err)
	}
}

func TestRealProfileCdpLaunchNonChromiumDefaultFailsClosed(t *testing.T) {
	env := testEnv(t)
	pin(t, &realProfileConsent, func(*tools.Env) bool { return true })
	pin(t, &realProfileLightpanda, func(*tools.Env) bool { return false })
	pin(t, &connectDetectDefaultChromium, func(string) string { return "" })
	if cdp, err := realProfileCDP(env); cdp != "" || err == "" {
		t.Fatalf("cdp %q err %q", cdp, err)
	}
}

func TestRealProfileCdpLaunchSnapshotFailureFailsClosed(t *testing.T) {
	env := testEnv(t)
	pin(t, &realProfileConsent, func(*tools.Env) bool { return true })
	pin(t, &realProfileLightpanda, func(*tools.Env) bool { return false })
	pin(t, &realProfileEnsureScreen, func(*tools.Env) {})
	pin(t, &connectDetectDefaultChromium, func(string) string { return "chrome" })
	pin(t, &connectRealChromiumExecutable, func(string, string) string { return "/usr/bin/chrome" })
	pin(t, &connectSnapshotRealProfile, func(*tools.Env, string, string) (string, string) { return "", "boom" })
	cdp, err := realProfileCDP(env)
	if cdp != "" || !strings.Contains(err, "boom") {
		t.Fatalf("cdp %q err %q", cdp, err)
	}
}

func pinRunStdout(t *testing.T, stdout string) {
	t.Helper()
	pin(t, &connectRunStdout, func([]string) (string, bool) { return stdout, true })
}

func TestChannelIdentityLinuxBetaNotNormalizedToStable(t *testing.T) {
	pinRunStdout(t, "google-chrome-beta.desktop\n")
	if got := detectDefaultLinux(); got != UnsupportedChannel {
		t.Fatalf("detected %q", got)
	}
}

func TestChannelIdentityLinuxStableStillResolves(t *testing.T) {
	pinRunStdout(t, "google-chrome.desktop\n")
	if got := detectDefaultLinux(); got != "chrome" {
		t.Fatalf("detected %q", got)
	}
}

func TestChannelIdentityLinuxFlatpakBetaNotStable(t *testing.T) {
	pinRunStdout(t, "com.google.chrome.beta.desktop\n")
	if got := detectDefaultLinux(); got != UnsupportedChannel {
		t.Fatalf("detected %q", got)
	}
}

func TestChannelIdentityDarwinCanaryNotNormalized(t *testing.T) {
	pin(t, &connectLaunchServicesHandler, func(string) string { return "com.google.chrome.canary" })
	pinRunStdout(t, "")
	if got := detectDefaultDarwin(); got != UnsupportedChannel {
		t.Fatalf("detected %q", got)
	}
}

func TestChannelIdentityWindowsProgidMaps(t *testing.T) {
	stable := map[string]string{}
	for _, entry := range windowsProgIDMap {
		stable[entry.fragment] = entry.browser
	}
	if stable["chromehtml"] != "chrome" || !hasToken(windowsChannelProgIDs, "chromebhtml") || !hasToken(windowsChannelProgIDs, "msedgebhtml") {
		t.Fatalf("ProgId tables %v %v", stable, windowsChannelProgIDs)
	}
	for _, channel := range windowsChannelProgIDs {
		for _, entry := range windowsProgIDMap {
			if strings.HasPrefix(channel, entry.fragment) {
				t.Fatalf("channel %s is a prefix hit for stable %s", channel, entry.fragment)
			}
		}
	}
}

func TestChannelIdentityChannelSentinelFailsClosedInCdp(t *testing.T) {
	env := testEnv(t)
	pin(t, &realProfileConsent, func(*tools.Env) bool { return true })
	pin(t, &realProfileLightpanda, func(*tools.Env) bool { return false })
	pin(t, &connectDetectDefaultChromium, func(string) string { return UnsupportedChannel })
	snapshots, launches := 0, 0
	pin(t, &connectSnapshotRealProfile, func(*tools.Env, string, string) (string, string) {
		snapshots++
		return "", ""
	})
	pin(t, &realProfileLaunchChromium, func(context.Context, *tools.Env, string, LaunchOptions) (*LaunchedBrowser, string) {
		launches++
		return nil, ""
	})
	cdp, err := realProfileCDP(env)
	if cdp != "" || err == "" || snapshots != 0 || launches != 0 {
		t.Fatalf("cdp %q err %q snapshots %d launches %d", cdp, err, snapshots, launches)
	}
}

func TestChannelIdentityDataDirRejectsSentinel(t *testing.T) {
	if got := realProfileDataDir(UnsupportedChannel, "Linux"); got != "" {
		t.Fatalf("data dir %q", got)
	}
	if got := realChromiumExecutable(UnsupportedChannel, "Linux"); got != "" {
		t.Fatalf("executable %q", got)
	}
}

func TestSnapshotIsCredentialStoreReadGuardBlocksSnapshot(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	cookies := filepath.Join(h, "browser-profile", "chrome", "Default", "Cookies")
	writeFile(t, cookies, "secret-cookie-db")
	t.Setenv("ICEBATH_HOME", h)
	if tools.ReadBlockError(&tools.Env{Home: h}, cookies) == "" {
		t.Fatal("the snapshot cookie database is readable by the agent")
	}
}

func TestSnapshotIsCredentialStoreReadGuardAllowsNormalFile(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	normal := filepath.Join(dir, "notes.txt")
	writeFile(t, normal, "hello")
	if msg := tools.ReadBlockError(&tools.Env{Home: h}, normal); msg != "" {
		t.Fatalf("a normal file is blocked: %s", msg)
	}
}

// Data dir where the signed in session lives in Profile 6 and Default holds tracking cookies only
func multiProfile(t *testing.T) (*tools.Env, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "real")
	for _, prof := range []string{"Default", "Profile 6"} {
		if err := os.MkdirAll(filepath.Join(root, prof, "Network"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "Local State"), `{"profile": {"last_used": "Profile 6"}}`)
	authDB(t, filepath.Join(root, "Default", "Cookies"), "default-tracking-only")
	authDB(t, filepath.Join(root, "Profile 6", "Cookies"), "PROFILE6-SESSION-AUTH")
	authDB(t, filepath.Join(root, "Profile 6", "Login Data"), "profile6-logins")
	writeFile(t, filepath.Join(root, "Profile 6", "Preferences"), "{}")
	h := filepath.Join(dir, "hh")
	t.Setenv("ICEBATH_HOME", h)
	return &tools.Env{Home: h}, root, filepath.Join(h, "browser-profile", "chrome")
}

func TestReviewBugFixesLastUsedProfileLandsInCopyDefault(t *testing.T) {
	env, src, copyDir := multiProfile(t)
	if _, err := snapshotRealProfile(env, "chrome", src); err != "" {
		t.Fatal(err)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Cookies"), ""); got != "PROFILE6-SESSION-AUTH" {
		t.Fatalf("cookies marker %q", got)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Login Data"), ""); got != "profile6-logins" {
		t.Fatalf("login data marker %q", got)
	}
}

func TestReviewBugFixesLastUsedFallsBackToDefault(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d")
	if err := os.MkdirAll(filepath.Join(root, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Local State"), `{"profile": {"last_used": "Profile 9"}}`)
	if got := lastUsedProfile(root); got != "Default" {
		t.Fatalf("last used %q", got)
	}
}

func TestReviewBugFixesRefreshRemirrorsLastUsed(t *testing.T) {
	env, src, copyDir := multiProfile(t)
	snapshotRealProfile(env, "chrome", src)
	authDB(t, filepath.Join(src, "Profile 6", "Cookies"), "PROFILE6-REFRESHED")
	if _, err := snapshotRealProfile(env, "chrome", src); err != "" {
		t.Fatal(err)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Cookies"), ""); got != "PROFILE6-REFRESHED" {
		t.Fatalf("cookies marker %q", got)
	}
}

func TestReviewBugFixesMacos26DetectReturnsChrome(t *testing.T) {
	pinRunStdout(t, `( { LSHandlerPreferredVersions = { LSHandlerRoleAll = "7559.97"; }; `+
		`LSHandlerRoleAll = "com.google.chrome"; LSHandlerURLScheme = https; } )`)
	if got := detectDefaultDarwin(); got != "chrome" {
		t.Fatalf("detected %q", got)
	}
}

func TestReviewBugFixesLightpandaEngineFailsActionably(t *testing.T) {
	env := testEnv(t)
	pin(t, &realProfileConsent, func(*tools.Env) bool { return true })
	pin(t, &realProfileLightpanda, func(*tools.Env) bool { return true })
	detections := 0
	pin(t, &connectDetectDefaultChromium, func(string) string {
		detections++
		return "chrome"
	})
	cdp, err := realProfileCDP(env)
	lowered := strings.ToLower(err)
	if cdp != "" || !strings.Contains(lowered, "lightpanda") || !strings.Contains(lowered, "browser.engine") || detections != 0 {
		t.Fatalf("cdp %q err %q detections %d", cdp, err, detections)
	}
}

// Data dir whose last used Profile 6 holds the session while Default is signed out
func roundThreeProfile(t *testing.T) (*tools.Env, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "real")
	for _, prof := range []string{"Default", "Profile 6"} {
		if err := os.MkdirAll(filepath.Join(root, prof, "Network"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "Local State"), `{"profile": {"last_used": "Profile 6"}}`)
	authDB(t, filepath.Join(root, "Default", "Cookies"), "default-signed-out")
	authDB(t, filepath.Join(root, "Profile 6", "Cookies"), "PROFILE6-SESSION")
	writeFile(t, filepath.Join(root, "Profile 6", "Preferences"), "{}")
	h := filepath.Join(dir, "hh")
	t.Setenv("ICEBATH_HOME", h)
	return &tools.Env{Home: h}, root, filepath.Join(h, "browser-profile", "chrome")
}

func pinLocked(t *testing.T, autoclose bool) {
	t.Helper()
	pin(t, &connectProfileIsLocked, func(string, string) bool { return true })
	pin(t, &connectRealProfileAutoclose, func(*tools.Env) bool { return autoclose })
}

func TestReviewRound3TornCopyIsRedoneNotOverlaid(t *testing.T) {
	env, src, _ := roundThreeProfile(t)
	dst := realProfileCopyDir(env, "chrome")
	writeFile(t, filepath.Join(dst, "Default", "Cookies"), "HALF-COPY-GARBAGE")
	if _, err := snapshotRealProfile(env, "chrome", src); err != "" {
		t.Fatal(err)
	}
	if got := authDB(t, filepath.Join(dst, "Default", "Cookies"), ""); got != "PROFILE6-SESSION" {
		t.Fatalf("cookies marker %q", got)
	}
	if !isFilePath(filepath.Join(dst, snapshotDoneMarker)) {
		t.Fatal("no done marker after a completed snapshot")
	}
}

func TestReviewRound3OnlyActiveProfileCopied(t *testing.T) {
	env, src, copyDir := roundThreeProfile(t)
	if err := os.Mkdir(filepath.Join(src, "Profile 3"), 0o755); err != nil {
		t.Fatal(err)
	}
	authDB(t, filepath.Join(src, "Profile 3", "Cookies"), "PROFILE3-SHOULD-NOT-COPY")
	if _, err := snapshotRealProfile(env, "chrome", src); err != "" {
		t.Fatal(err)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Cookies"), ""); got != "PROFILE6-SESSION" {
		t.Fatalf("cookies marker %q", got)
	}
	if lexists(filepath.Join(copyDir, "Profile 3")) || lexists(filepath.Join(copyDir, "Profile 6")) {
		t.Fatal("a profile other than the active one reached the copy")
	}
}

func TestReviewRound3CleanupRemovesStore(t *testing.T) {
	h := filepath.Join(t.TempDir(), "hh")
	env := &tools.Env{Home: h}
	writeFile(t, filepath.Join(h, "browser-profile", "chrome", "Default", "Cookies"), "secret")
	cleanupRealProfileSnapshots(env)
	if lexists(filepath.Join(h, "browser-profile")) {
		t.Fatal("the snapshot store survived consent off")
	}
}

func TestReviewRound3CleanupIdempotentWhenAbsent(t *testing.T) {
	h := filepath.Join(t.TempDir(), "hh")
	cleanupRealProfileSnapshots(&tools.Env{Home: h})
	if lexists(filepath.Join(h, "browser-profile")) {
		t.Fatal("cleanup created the snapshot store")
	}
}

func TestReviewRound3LockProbeFalseWhenReadable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Default", "Network", "Cookies"), "db")
	if profileIsLocked(dir, "Default") {
		t.Fatal("a readable cookie database reads as locked")
	}
}

func TestReviewRound3LockProbeFalseWhenNoCookieDb(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if profileIsLocked(dir, "Default") {
		t.Fatal("a profile without a cookie database reads as locked")
	}
}

func TestReviewRound3LockProbeTrueOnPermissionerror(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Default", "Cookies"), "db")
	pin(t, &connectOpenFile, func(path string) (*os.File, error) {
		if strings.HasSuffix(path, "Cookies") {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		return os.Open(path)
	})
	if !profileIsLocked(dir, "Default") {
		t.Fatal("a permission error on the cookie database does not read as locked")
	}
}

func TestReviewRound3SnapshotFailsFastWhenLocked(t *testing.T) {
	env, src, _ := roundThreeProfile(t)
	pinLocked(t, false)
	copies := 0
	realCopyTree := connectCopyTree
	pin(t, &connectCopyTree, func(src, dst string, symlinks, dirsExistOK bool, ignore func(string, []string) []string) error {
		copies++
		return realCopyTree(src, dst, symlinks, dirsExistOK, ignore)
	})
	dst, err := snapshotRealProfile(env, "chrome", src)
	if dst != "" || !strings.HasPrefix(err, profileLockedPrefix) || copies != 0 {
		t.Fatalf("snapshot %q %q copies %d", dst, err, copies)
	}
}

func TestReviewRound3SnapshotBlocksWhenLockedEvenWithAutoclose(t *testing.T) {
	env, src, _ := roundThreeProfile(t)
	pinLocked(t, true)
	killed := 0
	pin(t, &connectCloseBrowserHolding, func(*tools.Env, string, float64) (bool, string) {
		killed++
		return true, "x"
	})
	dst, err := snapshotRealProfile(env, "chrome", src)
	if dst != "" || !strings.HasPrefix(err, profileLockedPrefix) || killed != 0 {
		t.Fatalf("snapshot %q %q killed %d", dst, err, killed)
	}
}

func TestReviewRound3LockedProfileReusesCompletedSnapshot(t *testing.T) {
	env, src, copyDir := roundThreeProfile(t)
	first, err := snapshotRealProfile(env, "chrome", src)
	if err != "" {
		t.Fatal(err)
	}
	authDB(t, filepath.Join(src, "Profile 6", "Cookies"), "NEWER-SESSION")
	writeFile(t, filepath.Join(first, "SingletonLock"), "")
	pin(t, &connectProfileIsLocked, func(string, string) bool { return true })
	pin(t, &connectMirrorProfileAuth, func(string, string, string) ([]authFailure, error) {
		t.Error("must not touch the locked live profile")
		return nil, nil
	})
	dst, err := snapshotRealProfile(env, "chrome", src)
	if dst != first || err != "" {
		t.Fatalf("snapshot %q %q", dst, err)
	}
	if got := authDB(t, filepath.Join(copyDir, "Default", "Cookies"), ""); got != "PROFILE6-SESSION" {
		t.Fatalf("cookies marker %q", got)
	}
	if lexists(filepath.Join(dst, "SingletonLock")) {
		t.Fatal("a live instance leftover stayed in the reused copy")
	}
}

func TestReviewRound3LockedProfileRefusesSnapshotOfAnotherProfile(t *testing.T) {
	env, src, _ := roundThreeProfile(t)
	first, err := snapshotRealProfile(env, "chrome", src)
	if err != "" {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(first, snapshotDoneMarker), "Default")
	pinLocked(t, false)
	dst, err := snapshotRealProfile(env, "chrome", src)
	if dst != "" || !strings.HasPrefix(err, profileLockedPrefix) {
		t.Fatalf("snapshot %q %q", dst, err)
	}
}

func TestReviewRound3InterruptedProfileSwitchNeverReusesStaleMarker(t *testing.T) {
	env, src, _ := roundThreeProfile(t)
	if _, err := snapshotRealProfile(env, "chrome", src); err != "" {
		t.Fatal(err)
	}
	realResolve, realMirror := connectResolveSourceProfile, connectMirrorProfileAuth
	connectResolveSourceProfile = func(*tools.Env, string) (string, string) { return "Default", "" }
	connectMirrorProfileAuth = func(string, string, string) ([]authFailure, error) {
		return nil, &os.PathError{Op: "copy", Path: "Cookies", Err: syscall.EIO}
	}
	interrupted, _ := snapshotRealProfile(env, "chrome", src)
	connectResolveSourceProfile, connectMirrorProfileAuth = realResolve, realMirror
	if interrupted != "" {
		t.Fatalf("the interrupted switch reported a snapshot: %q", interrupted)
	}
	pinLocked(t, false)
	dst, err := snapshotRealProfile(env, "chrome", src)
	if dst != "" || !strings.HasPrefix(err, profileLockedPrefix) {
		t.Fatalf("snapshot %q %q", dst, err)
	}
}

func TestReviewRound3ProcessesHoldingProfileIdentityBinding(t *testing.T) {
	ud := filepath.Join(t.TempDir(), "ud")
	pin(t, &connectProcesses, func() []profileProcess {
		return []profileProcess{
			{PID: 1, Name: "chrome.exe", Cmdline: []string{"chrome.exe", "--user-data-dir=" + ud}},
			{PID: 2, Name: "chrome.exe", Cmdline: []string{"chrome.exe", `--user-data-dir=C:\Other`}},
			{PID: 3, Name: "python.exe", Cmdline: []string{"python.exe", "--user-data-dir=" + ud}},
		}
	})
	matched := processesHoldingProfile(ud)
	if len(matched) != 1 || matched[0].Name != "chrome.exe" || !strings.Contains(strings.Join(matched[0].Cmdline, " "), "--user-data-dir="+ud) {
		t.Fatalf("matched %+v", matched)
	}
}

func TestReviewRound3ConsentOffTriggersCleanup(t *testing.T) {
	env := testEnv(t)
	pin(t, &realProfileConsent, func(*tools.Env) bool { return false })
	cleanups := 0
	pin(t, &connectCleanupSnapshots, func(*tools.Env) { cleanups++ })
	if cdp, err := realProfileCDP(env); cdp != "" || err != "" || cleanups != 1 {
		t.Fatalf("cdp %q err %q cleanups %d", cdp, err, cleanups)
	}
}

func TestReviewRound3ReuseSkipsSnapshotOverlay(t *testing.T) {
	env := testEnv(t)
	dir := t.TempDir()
	key := realProfileKey(env)
	pid := int64(os.Getpid())
	launcherMu.Lock()
	browsers.set(key, &LaunchedBrowser{Key: key, PID: &pid, CDPURL: "ws://127.0.0.1:9251/devtools/browser/rp",
		HTTPURL: "http://127.0.0.1:9251", UserDataDir: dir, Owned: false, Ephemeral: false, HomeKey: "h"})
	launcherMu.Unlock()
	t.Cleanup(func() { launcherMu.Lock(); browsers.del(key); launcherMu.Unlock() })
	pin(t, &realProfileConsent, func(*tools.Env) bool { return true })
	pin(t, &realProfileLightpanda, func(*tools.Env) bool { return false })
	pin(t, &connectDetectDefaultChromium, func(string) string { return "chrome" })
	pin(t, &connectRealProfileCopyDir, func(*tools.Env, string) string { return dir })
	snapshots := 0
	pin(t, &connectSnapshotRealProfile, func(*tools.Env, string, string) (string, string) {
		snapshots++
		return "", ""
	})
	cdp, err := realProfileCDP(env)
	if cdp != "ws://127.0.0.1:9251/devtools/browser/rp" || err != "" || snapshots != 0 {
		t.Fatalf("cdp %q err %q snapshots %d", cdp, err, snapshots)
	}
}

func execSQL(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func queryInts(t *testing.T, path, query string) []int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func copyAuth(t *testing.T, src, dst string) string {
	t.Helper()
	reason, err := copyAuthFile(src, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestWindowsLockedProfileCopyLockedCookieDbCopiedViaBackup(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(root, "Default", "Network"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Local State"), `{"profile": {"last_used": "Default"}}`)
	writeFile(t, filepath.Join(root, "Default", "Preferences"), "{}")
	cookies := filepath.Join(root, "Default", "Cookies")
	holder, err := sql.Open("sqlite", cookies)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec("create table cookies(host_key, name)"); err != nil {
		t.Fatal(err)
	}
	for i := range 42 {
		if _, err := holder.Exec("insert into cookies values(?,?)", "nous.ai", "c"+strings.Repeat("x", i)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("insert into cookies values('u','uncommitted')"); err != nil {
		t.Fatal(err)
	}
	h := filepath.Join(dir, "hh")
	t.Setenv("ICEBATH_HOME", h)
	_, snapErr := snapshotRealProfile(&tools.Env{Home: h}, "chrome", root)
	tx.Rollback()
	if snapErr != "" {
		t.Fatal(snapErr)
	}
	copyDefault := filepath.Join(h, "browser-profile", "chrome", "Default")
	probe := filepath.Join(dir, "probe")
	raw, err := os.ReadFile(filepath.Join(copyDefault, "Cookies"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, probe, string(raw))
	if got := queryInts(t, probe, "select count(*) from cookies"); len(got) != 1 || got[0] != 42 {
		t.Fatalf("copied rows %v", got)
	}
	if lexists(filepath.Join(copyDefault, "Cookies-journal")) {
		t.Fatal("a stale journal sits next to the backed up database")
	}
}

func TestWindowsLockedProfileCopyCopyAuthFileBacksUpDb(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "Cookies"), filepath.Join(dir, "out", "Cookies")
	execSQL(t, src, "create table cookies(x)", "insert into cookies values(1)")
	if reason := copyAuth(t, src, dst); reason != "" {
		t.Fatal(reason)
	}
	if got := queryInts(t, dst, "select count(*) from cookies"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("copied rows %v", got)
	}
}

func TestWindowsLockedProfileCopyCopyAuthFileBoundsLocksWithoutOverwriting(t *testing.T) {
	for _, locked := range []string{"source", "destination"} {
		t.Run(locked, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "Cookies"), filepath.Join(dir, "out", "Cookies")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			execSQL(t, src, "create table cookies(x)", "insert into cookies values(7)")
			execSQL(t, dst, "create table cookies(x)", "insert into cookies values(99)")
			held := src
			if locked == "destination" {
				held = dst
			}
			holder, err := sql.Open("sqlite", held)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			conn, err := holder.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(context.Background(), "begin exclusive"); err != nil {
				t.Fatal(err)
			}
			reason := copyAuth(t, src, dst)
			conn.ExecContext(context.Background(), "rollback")
			conn.Close()
			if reason != authDBLocked {
				t.Fatalf("reason %q", reason)
			}
			if got := queryInts(t, dst, "select x from cookies"); len(got) != 1 || got[0] != 99 {
				t.Fatalf("destination overwritten under a lock: %v", got)
			}
			if reason := copyAuth(t, src, dst); reason != "" {
				t.Fatal(reason)
			}
			if got := queryInts(t, dst, "select x from cookies"); len(got) != 1 || got[0] != 7 {
				t.Fatalf("destination after the lock released: %v", got)
			}
		})
	}
}

func TestWindowsLockedProfileCopyCopyAuthFileSlowButProgressingBackupIsNotCalledLocked(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Web Data")
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("create table t(x)"); err != nil {
		t.Fatal(err)
	}
	blob := []byte(strings.Repeat("x", 4000))
	for range 600 {
		if _, err := db.Exec("insert into t values(?)", blob); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	pin(t, &authBackupDeadlineS, -1.0)
	reason := copyAuth(t, src, filepath.Join(dir, "out", "Web Data"))
	if reason == "" || reason == authDBLocked || strings.Contains(reason, "write lock") || !strings.Contains(reason, "exceeded") {
		t.Fatalf("reason %q", reason)
	}
}

func TestWindowsLockedProfileCopyCopyAuthFilePlainForNonDb(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "Preferences"), filepath.Join(dir, "out", "Preferences")
	writeFile(t, src, `{"k":1}`)
	if reason := copyAuth(t, src, dst); reason != "" {
		t.Fatal(reason)
	}
	if raw, _ := os.ReadFile(dst); string(raw) != `{"k":1}` {
		t.Fatalf("copied %q", raw)
	}
}

func TestWindowsLockedProfileCopyFailClosedWhenDbUnreadable(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "real")
	writeFile(t, filepath.Join(root, "Local State"), `{"profile": {"last_used": "Default"}}`)
	writeFile(t, filepath.Join(root, "Default", "Cookies"), "not-a-db")
	writeFile(t, filepath.Join(root, "Default", "Preferences"), "{}")
	h := filepath.Join(dir, "hh")
	t.Setenv("ICEBATH_HOME", h)
	pin(t, &connectCopyAuthFile, func(src, _ string, _ *float64) (string, error) {
		if hasToken(sqliteAuthDBs, filepath.Base(src)) {
			return "file is not a database", nil
		}
		return "", nil
	})
	dst, err := snapshotRealProfile(&tools.Env{Home: h}, "chrome", root)
	if dst != "" || err == "" {
		t.Fatalf("snapshot %q %q", dst, err)
	}
}
