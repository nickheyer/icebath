package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Operating system the desktop build and launch paths branch on
func desktopGOOS() string { return runtime.GOOS }

// Output stream of the desktop build and launch messages
var desktopOut io.Writer = os.Stdout

// Bundle or framework CFBundleIdentifier for local macOS signing, empty when unreadable
func desktopMacosBundleID(bundle string) string {
	info := filepath.Join(bundle, "Contents", "Info.plist")
	if _, err := os.Stat(info); err != nil && filepath.Ext(bundle) == ".framework" {
		candidates, _ := filepath.Glob(filepath.Join(bundle, "Versions", "*", "Resources", "Info.plist"))
		more, _ := filepath.Glob(filepath.Join(bundle, "Resources", "Info.plist"))
		if candidates = append(candidates, more...); len(candidates) > 0 {
			info = candidates[0]
		}
	}
	raw, err := os.ReadFile(info)
	if err != nil {
		return ""
	}
	data, err := pyplist.Load(raw)
	if err != nil {
		return ""
	}
	dict, ok := data.(*pyplist.Dict)
	if !ok {
		return ""
	}
	ident, _ := dict.Get("CFBundleIdentifier")
	if !pyrepr.Truthy(ident) {
		return ""
	}
	return pyplist.Str(ident)
}

// Configured desktop.macos_signing_identity on macOS, empty for ad hoc signing
func desktopMacosLocalSigningIdentity() string {
	if desktopGOOS() != "darwin" {
		return ""
	}
	cfg, err := config.Load(home.Dir(context.Background()), nil)
	if err != nil {
		fmt.Fprintf(desktopOut, "  (warning: could not load desktop.macos_signing_identity: %v; falling back to ad-hoc signing)\n", err)
		return ""
	}
	desktop, ok := cfg["desktop"].(map[string]any)
	if !ok {
		if tree, isTree := cfg["desktop"].(config.Tree); isTree {
			desktop, ok = tree, true
		}
	}
	if !ok {
		return ""
	}
	identity, ok := desktop["macos_signing_identity"].(string)
	if !ok {
		return ""
	}
	return pystr.Strip(identity)
}

// Strict deep codesign verification of an app bundle, its exit status
func codesignVerify(codesign, app string) (int, string, error) {
	cmd := exec.Command(codesign, "--verify", "--deep", "--strict", app)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(out), nil
	}
	return 0, string(out), err
}

// Signing identity of a bundle as team, identifier and strict verification
type macosSignature struct {
	Team, Identifier string
	Verified         bool
}

// Readable signature of an app bundle, nil when codesign cannot describe it
func macosSignatureSummary(codesign, app string) *macosSignature {
	cmd := exec.Command(codesign, "-dv", app)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil
	}
	output := stdout.String() + "\n" + stderr.String()
	field := func(key string) string {
		for _, line := range pystr.Splitlines(output, false) {
			if strings.HasPrefix(line, key+"=") {
				return pystr.Strip(line[len(key)+1:])
			}
		}
		return ""
	}
	team := field("TeamIdentifier")
	if team == "not set" {
		team = ""
	}
	code, _, err := codesignVerify(codesign, app)
	return &macosSignature{Team: team, Identifier: field("Identifier"), Verified: err == nil && code == 0}
}

// Reason to refuse replacing a publisher signed app with the rebuilt one, empty when allowed
func macosSigningDowngradeError(installed, rebuilt *macosSignature) string {
	if installed.Team == "" {
		return ""
	}
	if rebuilt == nil || rebuilt.Team == "" {
		return fmt.Sprintf("publisher-signed app (Team ID %s) would be replaced by a "+
			"locally signed or unreadable build; kept the existing app. To update it, "+
			"sign the rebuild with the publisher identity (CSC_LINK / "+
			"APPLE_SIGNING_IDENTITY) and update again.", installed.Team)
	}
	if rebuilt.Team != installed.Team {
		return fmt.Sprintf("publisher Team ID %s does not match rebuilt "+
			"%s; kept the existing app. Check the signing identity "+
			"in desktop.macos_signing_identity and update again.", installed.Team, rebuilt.Team)
	}
	if installed.Identifier != "" && rebuilt.Identifier != "" && installed.Identifier != rebuilt.Identifier {
		return fmt.Sprintf("bundle identifier %s does not match rebuilt "+
			"%s; kept the existing app. Align the build's "+
			"bundle identifier and update again.", pyrepr.Repr(installed.Identifier), pyrepr.Repr(rebuilt.Identifier))
	}
	if !rebuilt.Verified {
		return "rebuilt bundle failed strict signature verification; kept the existing " +
			"app. Re-run the build; if it persists, inspect with `codesign -vvv`."
	}
	return ""
}

// True for an intact Team ID signature the fixup must never replace with ad hoc
func desktopMacosHasValidRealSignature(app string) bool {
	codesign, ok := pyshutil.Which("codesign", nil)
	if !ok {
		return false
	}
	cmd := exec.Command(codesign, "-dv", app)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	output := stdout.String() + "\n" + stderr.String()
	if err != nil || !strings.Contains(output, "TeamIdentifier=") || strings.Contains(output, "TeamIdentifier=not set") {
		return false
	}
	code, _, err := codesignVerify(codesign, app)
	return err == nil && code == 0
}

// Signs a local build inside out with the repo entitlements, an identifier pinned requirement when ad hoc
func desktopMacosLocalCodesign(app, desktopDir, identity string) (bool, error) {
	codesign, ok := pyshutil.Which("codesign", nil)
	if !ok {
		return false, nil
	}
	entMain := filepath.Join(desktopDir, "electron", "entitlements.mac.plist")
	entInherit := filepath.Join(desktopDir, "electron", "entitlements.mac.inherit.plist")
	if !desktopPathExists(entMain) || !desktopPathExists(entInherit) {
		return false, desktopPyError{"FileNotFoundError", fmt.Sprintf("desktop entitlement plists missing under %s", filepath.Join(desktopDir, "electron"))}
	}
	signPath := func(path, entitlements, identifier string, runtime bool) error {
		args := []string{"--force", "--sign", identity, "--timestamp=none"}
		if runtime {
			args = append(args, "--options", "runtime")
		}
		if entitlements != "" && desktopPathExists(entitlements) {
			args = append(args, "--entitlements", entitlements)
		}
		if identifier != "" && identity == "-" {
			args = append(args, "--requirements", fmt.Sprintf(`=designated => identifier "%s"`, identifier))
		}
		args = append(args, path)
		out, err := exec.Command(codesign, args...).CombinedOutput()
		if exitErr, ok := err.(*exec.ExitError); ok {
			return &CalledProcessError{ReturnCode: exitErr.ExitCode(), Cmd: append([]string{codesign}, args...), Output: string(out)}
		}
		return err
	}
	depth := func(p string) int { return len(strings.Split(filepath.Clean(p), string(filepath.Separator))) }
	contents := filepath.Join(app, "Contents")
	var standalone []string
	_ = filepath.WalkDir(contents, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(app, filepath.Dir(path))
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasSuffix(part, ".app") {
				return nil
			}
		}
		name := d.Name()
		if name == "chrome_crashpad_handler" || name == "spawn-helper" || filepath.Ext(name) == ".node" || filepath.Ext(name) == ".dylib" {
			standalone = append(standalone, path)
		}
		return nil
	})
	sort.SliceStable(standalone, func(i, j int) bool { return depth(standalone[i]) > depth(standalone[j]) })
	for _, fp := range standalone {
		if err := signPath(fp, "", "", false); err != nil {
			return false, err
		}
	}
	var bundles []string
	frameworks := filepath.Join(contents, "Frameworks")
	if desktopPathExists(frameworks) {
		_ = filepath.WalkDir(frameworks, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && (filepath.Ext(path) == ".framework" || filepath.Ext(path) == ".app") {
				bundles = append(bundles, path)
			}
			return nil
		})
	}
	sort.SliceStable(bundles, func(i, j int) bool { return depth(bundles[i]) > depth(bundles[j]) })
	for _, bundle := range bundles {
		if err := signPath(bundle, entInherit, desktopMacosBundleID(bundle), true); err != nil {
			return false, err
		}
	}
	if err := signPath(app, entMain, desktopMacosBundleID(app), true); err != nil {
		return false, err
	}
	code, out, err := codesignVerify(codesign, app)
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, &CalledProcessError{ReturnCode: code, Cmd: []string{codesign, "--verify", "--deep", "--strict", app}, Output: out}
	}
	return true, nil
}

// True when a path exists
func desktopPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// True when a path is a directory
func desktopIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Legacy deep ad hoc re sign that never deletes the safeStorage keychain item
func macosLegacyAdhocResign(codesign, app string) bool {
	cmd := exec.Command(codesign, "--force", "--deep", "--sign", "-", app)
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			fmt.Fprintf(desktopOut, "  (warning: macOS relaunch fixup skipped: %v)\n", err)
			return false
		}
		fmt.Fprintf(desktopOut, "  (warning: legacy ad-hoc re-sign failed (exit %d); leaving safeStorage keychain item untouched)\n", exitErr.ExitCode())
		return false
	}
	code, _, err := codesignVerify(codesign, app)
	if err != nil {
		fmt.Fprintf(desktopOut, "  (warning: macOS relaunch fixup skipped: %v)\n", err)
		return false
	}
	if code != 0 {
		fmt.Fprintln(desktopOut, "  (warning: legacy ad-hoc re-sign did not pass strict verification; leaving safeStorage keychain item untouched)")
		return false
	}
	fmt.Fprintln(desktopOut, "  → macOS desktop re-signed (legacy ad-hoc); safeStorage keychain item left untouched")
	return true
}

// Re signs a locally built macOS app so self update keeps TCC grants, false when signing was refused
func desktopMacosRelaunchableFixup(desktopDir string, publisherSigningConfigured *bool, releaseDir string) bool {
	if desktopGOOS() != "darwin" {
		return true
	}
	publisher := os.Getenv("CSC_LINK") != "" || os.Getenv("APPLE_SIGNING_IDENTITY") != ""
	if publisherSigningConfigured != nil {
		publisher = *publisherSigningConfigured
	}
	if publisher {
		return true
	}
	searched := releaseDir
	if searched == "" {
		searched = filepath.Join(desktopDir, "release")
	}
	exe, ok := desktopPackagedExecutableIn(searched)
	if !ok {
		return true
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	if !strings.HasSuffix(app, ".app") || !desktopIsDir(app) {
		return true
	}
	codesign, found := pyshutil.Which("codesign", nil)
	if !found {
		return false
	}
	if desktopMacosHasValidRealSignature(app) {
		return true
	}
	_ = exec.Command("xattr", "-cr", app).Run()
	configured := desktopMacosLocalSigningIdentity()
	identity := configured
	if identity == "" {
		identity = "-"
	}
	replaces := app
	if releaseDir != "" {
		replaces = ""
		if rel, err := filepath.Rel(releaseDir, exe); err == nil && !strings.HasPrefix(rel, "..") {
			candidate := filepath.Join(desktopDir, "release", rel)
			if strings.HasSuffix(candidate, ".app") && desktopIsDir(candidate) {
				replaces = candidate
			}
		}
	}
	signed, err := desktopMacosLocalCodesign(app, desktopDir, identity)
	if err == nil && signed {
		label := "stable ad-hoc identity"
		if configured != "" {
			label = "keychain identity"
		}
		fmt.Fprintf(desktopOut, "  → macOS desktop signed with %s; TCC grants persist across rebuilds\n", label)
		return true
	}
	if err != nil {
		if configured != "" {
			var target *macosSignature
			if replaces != "" {
				target = macosSignatureSummary(codesign, replaces)
			}
			if target != nil && target.Team != "" {
				fmt.Fprintf(desktopOut, "  ✗ macOS signing identity %s did not produce a verified "+
					"signature, and the installed app is publisher-signed (Team ID "+
					"%s); keeping it (no ad-hoc fallback). Fix the identity "+
					"(e.g. `icebath desktop --setup-tcc-identity` or edit "+
					"desktop.macos_signing_identity in config.yaml) and update again.\n", pyrepr.Repr(configured), target.Team)
				return false
			}
			fmt.Fprintf(desktopOut, "  (warning: configured macOS signing identity failed: %s (%v); "+
				"falling back to identifier-pinned ad-hoc — TCC grants may need one "+
				"re-prompt; run the update from the logged-in session to keep the "+
				"certificate anchor)\n", pyrepr.Repr(identity), err)
			adhoc, adhocErr := desktopMacosLocalCodesign(app, desktopDir, "-")
			if adhocErr == nil && adhoc {
				fmt.Fprintln(desktopOut, "  → macOS desktop signed with stable ad-hoc identity; TCC grants persist across rebuilds")
				return true
			}
			if adhocErr != nil {
				err = adhocErr
			}
		}
		fmt.Fprintf(desktopOut, "  (warning: stable macOS signing failed (%v); using legacy ad-hoc sign)\n", err)
	}
	return macosLegacyAdhocResign(codesign, app)
}

// SHA 256 hex digest of an app bundle's app.asar, empty when absent or unreadable
func appAsarHash(appPath string) string {
	asar := filepath.Join(appPath, "Contents", "Resources", "app.asar")
	info, err := os.Stat(asar)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(asar)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Moves a staged bundle into place keeping the old one until the new one landed
func swapInNewMacosBundle(tmp, target, old string) error {
	movedOld := false
	if desktopPathExists(target) {
		if err := desktopRename(target, old); err != nil {
			_ = pyfs.RmTree(tmp, nil)
			return err
		}
		movedOld = true
	}
	if installErr := desktopRename(tmp, target); installErr != nil {
		var rollbackErr error
		if movedOld {
			rollbackErr = desktopRename(old, target)
		}
		_ = pyfs.RmTree(tmp, nil)
		if rollbackErr != nil {
			return desktopOSError(fmt.Sprintf("installing the staged bundle failed and rollback remains at %s: %s", old, pypath.OSErrorText(rollbackErr)))
		}
		return installErr
	}
	_ = pyfs.RmTree(old, nil)
	return nil
}

// Bundles of every live desktop process, which are never swapped under
var runningMacosAppBundles = func() map[string]bool {
	bundles := map[string]bool{}
	pids, err := proc.PIDs()
	if err != nil {
		return bundles
	}
	for _, pid := range pids {
		exe, err := proc.Exe(pid)
		if err != nil || !strings.HasSuffix(exe, "/Contents/MacOS/Icebath") {
			continue
		}
		if resolved, err := pypath.Resolve(exe); err == nil {
			bundles[filepath.Dir(filepath.Dir(filepath.Dir(resolved)))] = true
		}
	}
	return bundles
}

// Copies a bundle with ditto so signature, xattrs and links stay intact
var stageMacosBundleCopy = func(src, dst string) error {
	argv := []string{"/usr/bin/ditto", src, dst}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return &CalledProcessError{ReturnCode: exitErr.ExitCode(), Cmd: argv, Output: string(out)}
	}
	return err
}

// Copies the rebuilt macOS bundle over stale or missing installed copies, the installed ones and problems
func installRebuiltDesktopApp(desktopDir string, candidates []string) ([]string, []string) {
	if desktopGOOS() != "darwin" {
		return nil, nil
	}
	exe, ok := desktopPackagedExecutable(desktopDir)
	if !ok {
		return nil, nil
	}
	return installRebuiltMacosBundles(filepath.Dir(filepath.Dir(filepath.Dir(exe))), candidates, runningMacosAppBundles())
}

// Installs the rebuilt bundle over stale installed copies, reports each outcome and records the owned set
func refreshInstalledDesktopApps(ctx context.Context, desktopDir string) {
	if !ownsInstalledDesktopApps() {
		return
	}
	owned := installedDesktopApps()
	missing := map[string]bool{}
	for _, app := range owned {
		if !desktopPathExists(app) {
			missing[app] = true
		}
	}
	installed, problems := installRebuiltDesktopApp(desktopDir, owned)
	for _, app := range installed {
		if missing[app] {
			fmt.Fprintf(desktopOut, "  ✓ Reinstalled the Desktop app at %s: it had been removed, so Finder, the Dock and Spotlight could not find Icebath\n", app)
		} else {
			fmt.Fprintf(desktopOut, "  ✓ Installed the rebuilt Desktop app at %s\n", app)
		}
	}
	for _, problem := range problems {
		fmt.Fprintf(desktopOut, "  ⚠ %s\n", problem)
	}
	apps := make([]any, len(owned))
	for i, app := range owned {
		apps[i] = app
	}
	recorded, has := config.ReadJSONOrEmpty(desktopInstallRecord())["apps"]
	if !has {
		recorded = []any{}
	}
	if pyrepr.Equal(recorded, apps) {
		return
	}
	if err := config.AtomicJSONWrite(ctx, desktopInstallRecord(), map[string]any{"apps": apps}); err != nil {
		fmt.Fprintf(desktopOut, "  ⚠ Could not record the installed Desktop app (%s)\n", pypath.OSErrorText(err))
	}
}

// Executable a desktop launch starts, the installed app once it matches the checkout build
func installedDesktopLaunchTarget(ctx context.Context, desktopDir, packagedExecutable string) string {
	if desktopGOOS() != "darwin" {
		return packagedExecutable
	}
	refreshInstalledDesktopApps(ctx, desktopDir)
	rebuilt := appAsarHash(filepath.Dir(filepath.Dir(filepath.Dir(packagedExecutable))))
	for _, app := range installedDesktopApps() {
		if rebuilt != "" && appAsarHash(app) == rebuilt {
			return filepath.Join(app, "Contents", "MacOS", "Icebath")
		}
	}
	return packagedExecutable
}

// Stage and swap of the rebuilt bundle into each missing or differing candidate, refusing signing downgrades
func installRebuiltMacosBundles(rebuiltApp string, candidates []string, running map[string]bool) ([]string, []string) {
	rebuiltHash := appAsarHash(rebuiltApp)
	if rebuiltHash == "" {
		return nil, nil
	}
	codesign, hasCodesign := pyshutil.Which("codesign", nil)
	var rebuiltSig *macosSignature
	if hasCodesign {
		rebuiltSig = macosSignatureSummary(codesign, rebuiltApp)
	}
	installed, problems := []string{}, []string{}
	for _, app := range candidates {
		parent, name := filepath.Dir(app), filepath.Base(app)
		recoverInterruptedSwap(app, filepath.Join(parent, name+".icebath-update-old"))
		recoverInterruptedSwap(app, filepath.Join(parent, name+".old"))
		if desktopIsDir(app) && appAsarHash(app) == rebuiltHash {
			continue
		}
		resolved, err := pypath.Resolve(app)
		if err == nil && running[resolved] {
			problems = append(problems, fmt.Sprintf("%s is running and was not refreshed; quit Icebath Desktop and run "+
				"`icebath update` again (or update from inside the app)", app))
			continue
		}
		if hasCodesign {
			if installedSig := macosSignatureSummary(codesign, app); installedSig != nil {
				if downgrade := macosSigningDowngradeError(installedSig, rebuiltSig); downgrade != "" {
					problems = append(problems, fmt.Sprintf("%s not refreshed: %s", app, downgrade))
					continue
				}
			}
		}
		tmp := filepath.Join(parent, name+".icebath-update-new")
		old := filepath.Join(parent, name+".icebath-update-old")
		_ = pyfs.RmTree(tmp, nil)
		err = stageMacosBundleCopy(rebuiltApp, tmp)
		if err == nil {
			err = swapInNewMacosBundle(tmp, app, old)
		}
		if err != nil {
			_ = pyfs.RmTree(tmp, nil)
			kept := ""
			if desktopPathExists(app) {
				kept = "; the previous app was kept"
			}
			problems = append(problems, fmt.Sprintf("%s could not be installed (%s)%s", app, desktopErrText(err), kept))
			continue
		}
		installed = append(installed, app)
	}
	return installed, problems
}

// Python str of an OS or subprocess error raised by a desktop step
func desktopErrText(err error) string {
	if called, ok := err.(*CalledProcessError); ok {
		return fmt.Sprintf("Command %s returned non-zero exit status %d.", pyrepr.Repr(anyStrings(called.Cmd)), called.ReturnCode)
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Strings as a list of any for Python repr
func anyStrings(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Turns electron builder identity auto discovery off for a local packaged macOS rebuild, true when it did
func forceAdhocMacosSigning(env map[string]string, sourceMode bool) bool {
	if desktopGOOS() != "darwin" || sourceMode {
		return false
	}
	if _, pinned := env["CSC_IDENTITY_AUTO_DISCOVERY"]; env["CSC_LINK"] != "" || env["APPLE_SIGNING_IDENTITY"] != "" || pinned {
		return false
	}
	env["CSC_IDENTITY_AUTO_DISCOVERY"] = "false"
	return true
}

// Python exception of a desktop step carrying its class and message
type desktopPyError struct{ name, msg string }

func (e desktopPyError) Error() string         { return e.msg }
func (e desktopPyError) ExceptionName() string { return e.name }
