package browser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	sqliteutil "github.com/nickheyer/icebath/pkg/utils/sqlite"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

var snapshotIgnores = []string{
	"*Cache*",
	"Extensions",
	"Extension*",
	"Local Extension Settings",
	"Service Worker",
	"IndexedDB", "Crash Reports", "Crashpad", "BrowserMetrics*", "Snapshots",
	"OptimizationGuide*", "optimization_guide_model_store", "Safe Browsing", "SafetyTips",
	"OnDeviceHeadSuggestModel", "segmentation_platform", "Sync Data", "Shared Dictionary",
	"History*",
	"Favicons*",
	"Singleton*",
	"RunningChromeVersion", "SingletonSocket", "*.tmp",
	"*-journal",
	"*-wal",
	"*-shm",
	"BrowserMetrics-spare.pma",
}

var authRefreshProfileFiles = []string{"Cookies", "Network/Cookies", "Login Data", "Login Data For Account", "Web Data", "Preferences"}

var sqliteAuthDBs = []string{"Cookies", "Login Data", "Login Data For Account", "Web Data"}

const (
	authDBLocked = "SQLite backup made no progress within five seconds — " +
		"a running browser holds a write lock on it"
	snapshotDoneMarker  = ".icebath-snapshot-complete"
	profileLockedPrefix = "[profile-locked] "
)

// Seconds one session cookie database backup may take, read at every copy
var authBackupDeadlineS = 5.0

const optionalAuthBackupDeadlineS = 0.5

// Module attributes of the real profile snapshot tests patch
var (
	connectRealProfileCopyDir   = realProfileCopyDir
	connectSnapshotRealProfile  = snapshotRealProfile
	connectCleanupSnapshots     = cleanupRealProfileSnapshots
	connectProfileIsLocked      = profileIsLocked
	connectRealProfileAutoclose = realProfileAutoclose
	connectMirrorProfileAuth    = mirrorProfileAuth
	connectResolveSourceProfile = resolveSourceProfile
	connectCopyAuthFile         = copyAuthFile
	connectCloseBrowserHolding  = closeBrowserHoldingProfile
	connectCopyTree             = fsutil.CopyTree
	connectOpenFile             = os.Open
	connectProcesses            = listProfileProcesses
)

func connectErrText(err error) string {
	if errorutil.IsSystem(err) {
		return err.Error()
	}
	return state.ErrText(err)
}

// The snapshot dir Icebath owns for a browser's real profile
func realProfileCopyDir(env *tools.Env, browser string) string {
	return pathutil.Join(cfgOf(env).homeDir(), "browser-profile", browser)
}

// Profile dir the browser last used per Local State, Default when unreadable or missing on disk
func lastUsedProfile(src string) string {
	var last any = "Default"
	if raw, err := os.ReadFile(pathutil.Join(src, "Local State")); err == nil {
		if parsed, err := jsonutil.Decode(strings.ToValidUTF8(string(textutil.TrimBOM(raw)), string(utf8.RuneError))); err == nil {
			if obj, ok := jsonutil.AsObject(parsed); ok {
				profile := anyGet(obj, "profile")
				if !anyutil.IsTrue(profile) {
					profile = jsonutil.NewObject()
				}
				if prof, ok := jsonutil.AsObject(profile); ok {
					last = anyutil.Or(anyGet(prof, "last_used"), "Default")
				}
			}
		}
	}
	if name, ok := last.(string); ok && isDirPath(pathutil.Join(src, name)) {
		return name
	}
	return "Default"
}

// Applies the owner only secret store policy to a snapshot dir, or to everything inside it
func secureSnapshot(env *tools.Env, path string, contents bool) {
	h := cfgOf(env).homeDir()
	ctx := context.Background()
	if !contents {
		config.SecureDir(ctx, h, path)
		return
	}
	filepath.WalkDir(path, func(entry string, d fs.DirEntry, err error) error {
		if err != nil || entry == path {
			return nil
		}
		if isDirPath(entry) {
			config.SecureDir(ctx, h, entry)
		} else {
			config.SecureFile(ctx, h, entry)
		}
		return nil
	})
}

type backupTimeout struct{ msg string }

func (e *backupTimeout) Error() string { return e.msg }

func openSnapshotDB(dsn string) (*sql.DB, *sql.Conn, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

// Online backup of one auth database bounded by a deadline that only a stalled copy trips
func backupAuthDB(srcFile, dstFile string, deadline time.Duration) error {
	deadlineAt := time.Now().Add(deadline)
	lastRemaining, seen := 0, false
	progress := func(status, remaining, total int) error {
		before := total
		if seen {
			before = lastRemaining
		}
		lastRemaining, seen = remaining, true
		if status != 101 && !time.Now().Before(deadlineAt) {
			if remaining < before {
				limit := strconv.FormatFloat(authBackupDeadlineS, 'g', 6, 64)
				return &backupTimeout{"SQLite backup exceeded " + limit + "s while still making progress"}
			}
			return &backupTimeout{authDBLocked}
		}
		return nil
	}
	srcURI, err := state.ReadOnlyDBURI(srcFile)
	if err != nil {
		return err
	}
	srcDB, source, err := openSnapshotDB(srcURI + "&_pragma=busy_timeout(0)")
	if err != nil {
		return err
	}
	defer srcDB.Close()
	defer source.Close()
	dstAbs, err := filepath.Abs(dstFile)
	if err != nil {
		return err
	}
	dstURI, err := pathutil.FileURL(dstAbs)
	if err != nil {
		return err
	}
	dstDB, out, err := openSnapshotDB(dstURI + "?_pragma=busy_timeout(0)")
	if err != nil {
		return err
	}
	defer dstDB.Close()
	defer out.Close()
	return sqliteutil.Backup(source, out, 256, progress, 100*time.Millisecond)
}

// Copies auth state, the reason a file could not be snapshotted returned and a database never raw copied
func copyAuthFile(srcFile, dstFile string, deadlineS *float64) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dstFile), 0o777); err != nil {
		return "", err
	}
	var err error
	if hasToken(sqliteAuthDBs, pathutil.Name(srcFile)) {
		seconds := authBackupDeadlineS
		if deadlineS != nil {
			seconds = *deadlineS
		}
		err = backupAuthDB(srcFile, dstFile, time.Duration(seconds*float64(time.Second)))
	} else {
		err = fsutil.CopyFile(srcFile, dstFile)
	}
	if err == nil {
		return "", nil
	}
	pkgLog().Debug(fmt.Sprintf("real-profile: could not copy %s: %s", srcFile, connectErrText(err)))
	if reason := connectErrText(err); reason != "" {
		return reason, nil
	}
	return errorutil.Kind(err), nil
}

type authFailure struct{ rel, reason string }

// Mirrors the source profile's auth files into the copy's Default, returning the session cookie databases that failed
func mirrorProfileAuth(src, dst, sourceProfile string) ([]authFailure, error) {
	failed := []authFailure{}
	for _, rel := range authRefreshProfileFiles {
		s := pathutil.Join(src, sourceProfile, rel)
		if !isFilePath(s) {
			continue
		}
		base := pathutil.Name(rel)
		sessionDB := base == "Cookies"
		var deadline *float64
		if !sessionDB {
			optional := optionalAuthBackupDeadlineS
			deadline = &optional
		}
		reason, err := connectCopyAuthFile(s, pathutil.Join(dst, "Default", rel), deadline)
		if err != nil {
			return nil, err
		}
		if reason == "" || !hasToken(sqliteAuthDBs, base) {
			continue
		}
		if sessionDB {
			failed = append(failed, authFailure{rel, reason})
			continue
		}
		why := reason
		if reason == authDBLocked {
			why = "the running browser holds a write lock on it"
		}
		pkgLog().Info(fmt.Sprintf("real-profile snapshot: skipped %s (%s); session cookies still copied", rel, why))
	}
	return failed, nil
}

// Message naming which session cookie databases could not be snapshotted and why
func unavailableAuthDBsError(browser string, failed []authFailure) string {
	names := make([]string, 0, len(failed))
	details := make([]string, 0, len(failed))
	allLocked := true
	for _, f := range failed {
		names = append(names, f.rel)
		details = append(details, f.rel+": "+f.reason)
		allLocked = allLocked && f.reason == authDBLocked
	}
	if allLocked {
		return browser + " is running and holds the profile's " + strings.Join(names, ", ") + " with a write lock, so their " +
			"SQLite backup made no progress within five seconds. Icebath does not fall back to a " +
			"raw file copy (it could lose committed logins). Fully quit " +
			browser + " (including any background instance) and retry, or turn " +
			"browser.use_real_profile off."
	}
	return "could not read the '" + browser + "' profile's login data (" + strings.Join(details, "; ") + "). " +
		"Close " + browser + " and retry, or turn browser.use_real_profile off."
}

// True when the active profile's cookie database cannot be opened for a permission reason
func profileIsLocked(src, sourceProfile string) bool {
	db := firstPresent([]string{pathutil.Join(src, sourceProfile, pathutil.Join("Network", "Cookies")), pathutil.Join(src, sourceProfile, "Cookies")})
	if db == "" {
		return false
	}
	f, err := connectOpenFile(db)
	if err != nil {
		return errors.Is(err, fs.ErrPermission)
	}
	f.Close()
	return false
}

// Pinned source profile dir name from browser.real_profile_pin, empty when unset
func realProfilePin(env *tools.Env) string {
	return strings.TrimSpace(settings.BrowserRealProfilePin.Get(cfgOf(env).ctx()))
}

// The pinned profile when set, else the last used one, a pin missing on disk failing closed
func resolveSourceProfile(env *tools.Env, src string) (string, string) {
	pin := realProfilePin(env)
	if pin == "" {
		return lastUsedProfile(src), ""
	}
	if isDirPath(pathutil.Join(src, pin)) {
		return pin, ""
	}
	return "", "browser.real_profile_pin is set to '" + pin + "' but that profile directory does not " +
		"exist under " + anyutil.Quote(src) + ". Profile directories are named like 'Default' or 'Profile 2' " +
		"— list them with: ls " + anyutil.Quote(src) + ". Fix the pin, or remove it to fall back to the " +
		"last-used profile."
}

// Whether browser.real_profile_autoclose consent is on
func realProfileAutoclose(env *tools.Env) bool {
	return settings.BrowserRealProfileAutoclose.Get(cfgOf(env).ctx())
}

// One listed process with the facts the profile holder search reads
type profileProcess struct {
	PID     int
	Name    string
	Cmdline []string
}

// Every process the OS lists with its name and argv, unreadable facts left empty
func listProfileProcesses() []profileProcess {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	out := make([]profileProcess, 0, len(table))
	for _, p := range table {
		name, _ := proc.Name(p.PID)
		cmdline, _ := proc.Cmdline(p.PID)
		out = append(out, profileProcess{PID: p.PID, Name: name, Cmdline: cmdline})
	}
	return out
}

var profileBrowserBins = []string{
	"chrome", "chrome.exe", "chromium", "chromium.exe", "chrome_crashpad",
	"brave", "brave.exe", "msedge", "msedge.exe", "google chrome"}

// Chromium family processes whose argv references exactly this user data dir
func processesHoldingProfile(src string) []profileProcess {
	norm := pathutil.FoldCase(filepath.Clean(src))
	out := []profileProcess{}
	for _, p := range connectProcesses() {
		name := strings.ToLower(p.Name)
		joined := strings.Join(p.Cmdline, " ")
		argv0 := ""
		if len(p.Cmdline) > 0 {
			argv0 = strings.ToLower(p.Cmdline[0])
		}
		isBrowser := false
		for _, b := range profileBrowserBins {
			if strings.Contains(name, b) || strings.Contains(argv0, b) {
				isBrowser = true
				break
			}
		}
		if !isBrowser {
			continue
		}
		if strings.Contains(pathutil.FoldCase(filepath.Clean(joined)), norm) ||
			strings.Contains(strings.ToLower(joined), strings.ToLower("--user-data-dir="+src)) {
			out = append(out, p)
		}
	}
	return out
}

// Waits until every pid is gone or the timeout passes, returning the ones still alive
func waitProcesses(pids []int, timeout time.Duration) []int {
	alive := func() ([]int, bool) {
		left := []int{}
		for _, pid := range pids {
			if proc.Exists(pid) {
				left = append(left, pid)
			}
		}
		return left, len(left) == 0
	}
	return syncutil.PollUntil(context.Background(), timeout, 40*time.Millisecond, alive)
}

// Terminates the browser process tree holding a user data dir and waits for the profile lock to release
func closeBrowserHoldingProfile(env *tools.Env, src string, timeout float64) (bool, string) {
	procs := processesHoldingProfile(src)
	if len(procs) == 0 {
		return false, "no matching browser process found holding the profile."
	}
	targets := []int{}
	for _, p := range procs {
		targets = append(targets, p.PID)
	}
	if table, err := proc.Snapshot(); err == nil {
		for _, p := range procs {
			targets = append(targets, table.Descendants(p.PID)...)
		}
	}
	for _, pid := range targets {
		proc.Terminate(pid)
	}
	alive := waitProcesses(targets, time.Duration(min(timeout, 8.0)*float64(time.Second)))
	for _, pid := range alive {
		proc.KillPID(pid)
	}
	waitProcesses(alive, 3*time.Second)
	sourceProfile, _ := connectResolveSourceProfile(env, src)
	if sourceProfile == "" {
		sourceProfile = lastUsedProfile(src)
	}
	released := syncutil.PollUntil(context.Background(), time.Duration(timeout*float64(time.Second)), 500*time.Millisecond, func() (bool, bool) {
		free := !connectProfileIsLocked(src, sourceProfile)
		return free, free
	})
	if released {
		return true, "closed the browser and the profile lock released."
	}
	return false, "closed the browser processes but the profile is still locked — " +
		"another instance may have relaunched (background/tray mode)."
}

// Copies Local State into the snapshot and rewrites it for the single Default profile of the copy
func syncLocalState(src, dst, sourceProfile string) {
	lsSrc, lsDst := pathutil.Join(src, "Local State"), pathutil.Join(dst, "Local State")
	if isFilePath(lsSrc) {
		if err := fsutil.CopyFile(lsSrc, lsDst); err != nil {
			pkgLog().Debug(fmt.Sprintf("real-profile snapshot: skipped Local State: %s", connectErrText(err)))
		}
	}
	err := func() error {
		raw, err := os.ReadFile(lsDst)
		if err != nil {
			return err
		}
		text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
		if err != nil {
			return err
		}
		parsed, err := jsonutil.Decode(text)
		if err != nil {
			return err
		}
		stateObj, ok := jsonutil.AsObject(parsed)
		if !ok {
			panic(fmt.Errorf("'%s' object has no attribute 'get'", anyutil.Kind(parsed)))
		}
		if prof, ok := jsonutil.AsObject(anyGet(stateObj, "profile")); ok {
			if cache, ok := jsonutil.AsObject(anyGet(prof, "info_cache")); ok {
				entry := anyGet(cache, sourceProfile)
				if !anyutil.IsTrue(entry) {
					entry = anyGet(cache, "Default")
				}
				if anyutil.IsTrue(entry) {
					prof.Set("info_cache", jsonutil.ObjectOf("Default", entry))
				}
			}
			prof.Set("last_used", "Default")
			prof.Set("last_active_profiles", []any{"Default"})
		}
		out, err := jsonutil.Encode(stateObj, jsonutil.Spaced(), jsonutil.ASCII())
		if err != nil {
			return err
		}
		return os.WriteFile(lsDst, []byte(out), 0o666)
	}()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("real-profile snapshot: could not normalize Local State: %s", connectErrText(err)))
	}
}

// Fail closed message for a locked profile, offering the close only when autoclose consent arms it
func lockedProfileError(env *tools.Env, browser string) string {
	if connectRealProfileAutoclose(env) {
		return profileLockedPrefix + browser + " is running and has its profile locked, so its login data can't be copied " +
			"yet. Icebath can close it for you (this quits the browser — you'll lose unsaved " +
			"tabs). Ask the user to confirm, then close it and retry; if it's still locked after " +
			"that, they must fully quit it (including any background/tray instance)."
	}
	return profileLockedPrefix + browser + " is running and has its profile locked, so its login data can't be copied. " +
		"Fully quit the browser (including any background/tray instance) and retry, or turn " +
		"browser.use_real_profile off. (Enable browser.real_profile_autoclose to let Icebath " +
		"offer to close it for you.)"
}

func countJoined(err error) int {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return 1
	}
	n := 0
	for _, inner := range joined.Unwrap() {
		n += countJoined(inner)
	}
	return n
}

// Fresh copy of the active profile dir into the copy's Default without caches and the auth databases
func copyProfileTree(src, dst, sourceProfile string) error {
	dstDefault := pathutil.Join(dst, "Default")
	os.RemoveAll(dstDefault)
	patterns := fsutil.IgnorePatterns(append(append([]string{}, snapshotIgnores...), sqliteAuthDBs...)...)
	ignore := func(dir string, names []string) map[string]bool {
		skipped := patterns(dir, names)
		for _, name := range names {
			entry := filepath.Join(dir, name)
			if info, err := os.Lstat(entry); err == nil && info.Mode()&os.ModeSymlink != 0 {
				if _, err := os.Stat(entry); err != nil {
					skipped[name] = true
				}
			}
		}
		return skipped
	}
	err := connectCopyTree(pathutil.Join(src, sourceProfile), dstDefault, fsutil.CopyTreeOptions{DirsExistOK: true, Ignore: ignore})
	if err == nil {
		return nil
	}
	if _, perFile := err.(interface{ Unwrap() []error }); !perFile {
		return err
	}
	pkgLog().Info(fmt.Sprintf("real-profile snapshot: %d file(s) skipped copying %s/%s", countJoined(err), src, sourceProfile))
	return nil
}

// Removes live instance leftovers that make the browser think the copy is already open
func dropSingletonLeftovers(dst string) {
	for _, leftover := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		os.Remove(pathutil.Join(dst, leftover))
	}
}

// Source profile name a completed snapshot was taken from, empty without a readable marker
func snapshotSource(marker string) string {
	raw, err := os.ReadFile(marker)
	if err != nil {
		return ""
	}
	text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(text)
}

// Serves the last completed snapshot as it is while the live profile is locked
func reuseSnapshot(browser, dst string) (string, string) {
	dropSingletonLeftovers(dst)
	pkgLog().Info(fmt.Sprintf("real-profile: %s holds its profile lock; reusing the last snapshot in %s "+
		"(logins as of that copy)", browser, dst))
	return dst, ""
}

// Snapshots a browser's active real profile into the Icebath copy dir, returning the copy dir or the error text
func snapshotRealProfile(env *tools.Env, browser, src string) (string, string) {
	if src == "" {
		src = connectRealProfileDataDir(browser, "")
	}
	if src == "" || !isDirPath(src) {
		shown := "None"
		if src != "" {
			shown = anyutil.Quote(src)
		}
		return "", "profile directory for '" + browser + "' was not found (" + shown + "). " +
			"Launch that browser at least once, or turn browser.use_real_profile off."
	}
	sourceProfile, resolveErr := connectResolveSourceProfile(env, src)
	if resolveErr != "" || sourceProfile == "" {
		return "", resolveErr
	}
	dst := connectRealProfileCopyDir(env, browser)
	marker := pathutil.Join(dst, snapshotDoneMarker)
	populated := isFilePath(marker)
	sameSource := populated && snapshotSource(marker) == sourceProfile
	if connectProfileIsLocked(src, sourceProfile) {
		if sameSource {
			return reuseSnapshot(browser, dst)
		}
		return "", lockedProfileError(env, browser)
	}
	failedDBs, err := func() ([]authFailure, error) {
		if populated && !sameSource {
			if err := os.Remove(marker); err != nil {
				return nil, err
			}
		}
		if err := os.MkdirAll(dst, 0o777); err != nil {
			return nil, err
		}
		for _, path := range []string{filepath.Dir(dst), dst} {
			if path != "" {
				secureSnapshot(env, path, false)
			}
		}
		syncLocalState(src, dst, sourceProfile)
		if !populated {
			if err := copyProfileTree(src, dst, sourceProfile); err != nil {
				return nil, err
			}
		}
		failed, err := connectMirrorProfileAuth(src, dst, sourceProfile)
		if err != nil || len(failed) > 0 {
			return failed, err
		}
		dropSingletonLeftovers(dst)
		if err := os.WriteFile(marker, []byte(sourceProfile), 0o666); err != nil {
			pkgLog().Debug(fmt.Sprintf("real-profile snapshot: could not write done marker: %s", connectErrText(err)))
		}
		secureSnapshot(env, dst, true)
		return nil, nil
	}()
	if err != nil {
		return "", "could not snapshot the '" + browser + "' profile into " + dst + ": " + connectErrText(err)
	}
	if len(failedDBs) > 0 {
		return "", unavailableAuthDBsError(browser, failedDBs)
	}
	return dst, ""
}

// Deletes the whole real profile snapshot store when consent is off
func cleanupRealProfileSnapshots(env *tools.Env) {
	root := pathutil.Join(cfgOf(env).homeDir(), "browser-profile")
	if isDirPath(root) {
		os.RemoveAll(root)
		pkgLog().Info(fmt.Sprintf("real-profile: removed snapshot store %s (consent off)", root))
	}
}
