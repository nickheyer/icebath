package browser

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool result note when the real profile cannot load and a fresh signed out browser serves instead
func realProfileFallbackNote(err string) string {
	return err + " Using a fresh browser without the user's logins for this session instead; " +
		"tell the user if the task needs their accounts."
}

// Launcher key of the current profile's real profile browser, one per profile home
func realProfileKey(env *tools.Env) string { return "icebath-real-profile@" + cfgOf(env).homeKey() }

const realProfilePrefix = "browser.use_real_profile is on, but "

// Seconds a call waits for another call preparing the same profile's browser
var realProfileLockTimeoutS = 30.0

var (
	realProfileLocks      = map[string]chan struct{}{}
	realProfileLocksGuard sync.Mutex
)

var realProfileExtraFlags = []string{"--disable-hang-monitor", "--disable-popup-blocking", "--disable-prompt-on-repost"}

// Module attributes of the real profile launch hermes tests patch
var (
	realProfileLaunchChromium = launchChromium
	realProfileEnsureScreen   = ensureScreenForHeadedChromium
	realProfileLightpanda     = usingLightpandaEngine
	realProfileConsent        = func(env *tools.Env) bool { return cfgOf(env).useRealProfile() }
)

// Why the default browser's profile cannot be used, empty when it can
func unsupportedReason(browser string) string {
	if browser == "" {
		return realProfilePrefix + "your default browser is not a supported Chromium browser (Chrome, Edge, Brave, " +
			"Brave Origin, Chromium). Real-profile browsing requires a Chromium default; set one or turn the toggle off."
	}
	if browser == UnsupportedChannel {
		return realProfilePrefix + "your default browser is a pre-release Chromium channel (Beta / Dev / Canary), which " +
			"real-profile browsing does not support. Set your default to a " +
			"stable Chrome / Edge / Brave / Brave Origin / Chromium, or turn the toggle off."
	}
	return ""
}

// Snapshot failure text, a locked profile naming the approved close command
func snapshotError(err string) string {
	if err != "" && strings.HasPrefix(err, profileLockedPrefix) {
		return err[len(profileLockedPrefix):] + " To close it (only after the user approves — it " +
			"quits their browser and loses unsaved tabs), run: `icebath browser close-profile`; the next " +
			"browser session then uses the profile."
	}
	return realProfilePrefix + err
}

// Launch lock of the current profile home, one per home so a slow launch never makes another profile wait
func realProfileHomeLock(env *tools.Env) chan struct{} {
	realProfileLocksGuard.Lock()
	defer realProfileLocksGuard.Unlock()
	key := cfgOf(env).homeKey()
	lock, ok := realProfileLocks[key]
	if !ok {
		lock = make(chan struct{}, 1)
		realProfileLocks[key] = lock
	}
	return lock
}

// Stops this profile's real profile browser if this process launched it
func terminateRealProfileBrowser(env *tools.Env) {
	stopBrowser(tools.WithEnvHome(context.Background(), env), realProfileKey(env))
}

// Consent off stops the copy browser and deletes the snapshot store of cookie and login copies
func realProfileConsentRevoked(env *tools.Env) (string, string) {
	terminateRealProfileBrowser(env)
	connectCleanupSnapshots(env)
	return "", ""
}

func launchRealProfile(env *tools.Env, browserName string) (string, string) {
	realBinary := connectRealChromiumExecutable(browserName, "")
	if realBinary == "" {
		return "", realProfilePrefix + "the real browser binary for '" + browserName + "' could not be found. Reinstall it or turn the toggle off."
	}
	snapshotFailure := []string{}
	overlay := func() string {
		copied, err := connectSnapshotRealProfile(env, browserName, "")
		if err != "" || copied == "" {
			snapshotFailure = append(snapshotFailure, snapshotError(err))
			return snapshotFailure[0]
		}
		return ""
	}
	realProfileEnsureScreen(env)
	timeout := 30 * time.Second
	launched, err := realProfileLaunchChromium(tools.WithEnvHome(context.Background(), env), env, realProfileKey(env), LaunchOptions{
		Executable: realBinary, UserDataDir: connectRealProfileCopyDir(env, browserName), Ephemeral: false,
		ExtraArgs: realProfileExtraFlags, Timeout: &timeout, SandboxBypass: false, Prepare: overlay})
	if launched == nil {
		if len(snapshotFailure) > 0 {
			return "", snapshotFailure[0]
		}
		return "", realProfilePrefix + "the real-profile browser failed to start: " + err
	}
	cfgOf(env).logger().Info(fmt.Sprintf("real-profile browser ready for %s at %s (%s)", browserName, launched.HTTPURL, launched.UserDataDir))
	return launched.CDPURL, ""
}

// CDP URL of the consented real profile browser, else the reason it cannot load, both empty without consent
func realProfileCDP(env *tools.Env) (string, string) {
	if !realProfileConsent(env) {
		return realProfileConsentRevoked(env)
	}
	if realProfileLightpanda(env) {
		return "", realProfilePrefix + "browser.engine is set to 'lightpanda', which cannot load a real Chromium profile. " +
			"Set browser.engine to 'auto' to use real-profile browsing, or turn the toggle off."
	}
	lock := realProfileHomeLock(env)
	timer := time.NewTimer(time.Duration(realProfileLockTimeoutS * float64(time.Second)))
	defer timer.Stop()
	select {
	case lock <- struct{}{}:
	case <-timer.C:
		return "", realProfilePrefix + "the real-profile browser is already being prepared by another call that has not " +
			"finished. Retry after that call completes, or restart Icebath if it was abandoned."
	}
	defer func() { <-lock }()
	if running := getBrowser(realProfileKey(env)); running != nil {
		return running.CDPURL, ""
	}
	browserName := connectDetectDefaultChromium("")
	if unsupported := unsupportedReason(browserName); unsupported != "" {
		return "", unsupported
	}
	return launchRealProfile(env, browserName)
}
