package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Runtime state files an import never overwrites, matched by basename in every profile
var importSkipNames = map[string]bool{"gateway_state.json": true, "gateway.pid": true, "cron.pid": true, "gateway.lock": true, "processes.json": true}

// Restored files tightened to owner only because zip members carry no mode bits
var secretFileNames = map[string]bool{".env": true, "auth.json": true, "state.db": true, "vault.key": true, "vault.json.enc": true}

// Reports whether err is one a damaged archive member raises while being read, OSError included
func isMemberReadError(err error) bool {
	var bad *pyzip.BadZipFile
	var zlibErr *pyzip.ZlibError
	var eof *pyzip.EOFError
	var lzmaErr *pyzip.LZMAError
	return isOSError(err) || errors.As(err, &bad) || errors.As(err, &zlibErr) || errors.As(err, &eof) || errors.As(err, &lzmaErr)
}

// Member restore of the import loops, a seam tests replace to fail one member while it is written
var extractMember = extractMemberAtomically

// Line read of the overwrite confirmation, a seam tests answer
var importInput = func(w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	return cliui.Default().Input("")
}

// Platform default home the gateway revive compares against, a seam tests point elsewhere
var platformDefaultHome = home.PlatformDefault

// Running check of the gateway service, a seam tests replace
var isServiceRunning = gateway.IsServiceRunning

// Gateway service start of the revive, a seam tests record
var ensureGatewayService = gateway.EnsureGatewayService

// Members whose data fails to decompress or fails its CRC, each as member then error
func findCorruptMembers(zr *pyzip.ZipReader, members []string) ([]string, error) {
	var bad []string
	buf := make([]byte, 1<<20)
	for _, member := range members {
		err := func() error {
			src, err := openMember(zr, member)
			if err != nil {
				return err
			}
			defer src.Close()
			for {
				n, err := src.Read(buf)
				if errors.Is(err, io.EOF) || (n == 0 && err == nil) {
					return nil
				}
				if err != nil {
					return err
				}
			}
		}()
		if err != nil {
			if !isMemberReadError(err) {
				return nil, err
			}
			bad = append(bad, member+": "+excText(err))
		}
	}
	return bad, nil
}

// Reports whether the import leaves a home relative member alone as runtime, PM or sidecar state
func importSkipped(rel string) bool {
	parts, err := pyfs.NormalizeArchiveParts(rel)
	if err != nil || len(parts) == 0 {
		return false
	}
	entry, ok := profileRootEntry(parts)
	return importSkipNames[parts[len(parts)-1]] || (ok && profiles.PMRuntimeRootDirs[entry]) || hasSidecarSuffix(rel)
}

// Reports whether a name ends with a SQLite WAL, SHM or journal suffix
func hasSidecarSuffix(rel string) bool {
	for _, s := range sqliteSidecarSuffixes {
		if strings.HasSuffix(rel, s) {
			return true
		}
	}
	return false
}

// Path a member restores to and whether the import skips it, external members never skipped
func importMemberRel(member, prefix string) (string, bool) {
	if rest, ok := strings.CutPrefix(member, externalPrefix); ok {
		return rest, false
	}
	return stripPrefix(member, prefix), importSkipped(stripPrefix(member, prefix))
}

// Member name with the detected archive prefix removed
func stripPrefix(member, prefix string) string {
	if prefix != "" && strings.HasPrefix(member, prefix) {
		return member[len(prefix):]
	}
	return member
}

// Namespace string value of an argparse dest, empty when absent
func nsString(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Restores a backup archive into the home, exit status one for damaged archives or incomplete restores
func RunImport(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	if err := config.RequireFileTooling(ctx, "`icebath import`"); err != nil {
		return 1, err
	}
	expanded, err := pypath.PathExpanduser(nsString(ns, "zipfile"))
	if err != nil {
		return 1, err
	}
	zipPath, err := pypath.Resolve(expanded)
	if err != nil {
		return 1, err
	}
	if st, err := os.Stat(zipPath); err != nil || !st.Mode().IsRegular() {
		fmt.Fprintf(w, "Error: File not found: %s\n", zipPath)
		return 1, nil
	}
	if !pyzip.IsZipfile(zipPath) {
		fmt.Fprintf(w, "Error: Not a valid zip file: %s\n", zipPath)
		return 1, nil
	}
	root := h
	zr, err := pyzip.OpenReader(zipPath)
	if err != nil {
		return 1, err
	}
	defer zr.Close()
	if ok, reason := validateBackupZip(zr); !ok {
		fmt.Fprintf(w, "Error: %s\n", reason)
		return 1, nil
	}
	prefix := detectPrefix(zr)
	var members []string
	for _, n := range zr.Namelist() {
		if !strings.HasSuffix(n, "/") {
			members = append(members, n)
		}
	}
	fileCount := len(members)
	fmt.Fprintf(w, "Backup contains %d files\n", fileCount)
	fmt.Fprintf(w, "Target: %s\n", home.Display(root))
	if prefix != "" {
		fmt.Fprintf(w, "Detected archive prefix: %s (will be stripped)\n", pyrepr.StrRepr(prefix))
	}
	_, cfgErr := os.Stat(pypath.PathJoin(root, "config.yaml"))
	_, envErr := os.Stat(pypath.PathJoin(root, ".env"))
	force, _ := ns["force"].(bool)
	if (cfgErr == nil || envErr == nil) && !force {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Warning: Target directory already has Icebath configuration.")
		fmt.Fprintln(w, "Importing will overwrite existing files with backup contents.")
		fmt.Fprintln(w)
		answer, err := importInput(w, "Continue? [y/N] ")
		if err != nil {
			if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
				fmt.Fprintln(w, "\nAborted.")
				return 1, nil
			}
			return 1, err
		}
		if a := pystr.Lower(pystr.Strip(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(w, "Aborted.")
			return 0, nil
		}
	}
	fmt.Fprintln(w, "\nChecking archive integrity ...")
	var checked []string
	for _, m := range members {
		if _, skipped := importMemberRel(m, prefix); !skipped {
			checked = append(checked, m)
		}
	}
	corrupt, err := findCorruptMembers(zr, checked)
	if err != nil {
		return 1, err
	}
	if len(corrupt) > 0 {
		printCapped(w, fmt.Sprintf("Error: backup archive is damaged (%d member(s) fail to "+
			"decompress or fail their CRC); nothing was restored:", len(corrupt)), corrupt, "  ")
		return 1, nil
	}
	fmt.Fprintf(w, "\nImporting %d files ...\n", fileCount)
	if err := home.PathMkdir(root, 0o777, true, true); err != nil {
		return 1, err
	}
	t0 := monotonic()
	res, err := importMembers(ctx, w, zr, members, prefix, root, fileCount)
	if err != nil {
		if isOwnershipConflict(err) {
			fmt.Fprintf(w, "\nImport refused; no files restored: %s\n", err.Error())
			return 1, nil
		}
		return 1, err
	}
	elapsed := monotonic().Sub(t0).Seconds()
	fmt.Fprintln(w)
	verdict := "complete"
	if len(res.errors) > 0 {
		verdict = "incomplete"
	}
	fmt.Fprintf(w, "Import %s: %d files restored in %.1fs\n", verdict, res.restored, elapsed)
	fmt.Fprintf(w, "  Target: %s\n", home.Display(root))
	if res.restoredExternal > 0 {
		fmt.Fprintf(w, "\n  Restored %d memory-provider file(s) to their original location(s) outside %s.\n", res.restoredExternal, home.Display(root))
	}
	if len(res.errors) > 0 {
		fmt.Fprintf(w, "\n  Warnings (%d files skipped):\n", len(res.errors))
		for _, e := range res.errors[:min(10, len(res.errors))] {
			fmt.Fprintln(w, e)
		}
		if len(res.errors) > 10 {
			fmt.Fprintf(w, "  ... and %d more\n", len(res.errors)-10)
		}
	}
	if len(res.dbShrunk) > 0 {
		fmt.Fprintln(w, "\n  ⚠ Session data replaced by older backup contents:")
		for _, s := range res.dbShrunk {
			fmt.Fprintf(w, "    %s: %s session(s) / %s message(s) -> %s / %s\n", s.rel, s.before[0], s.before[1], s.after[0], s.after[1])
		}
		fmt.Fprintf(w, "    Anything recorded after the backup was taken is not in it. %s\n", snapshotRecoveryHint())
	}
	if len(res.skippedRuntime) > 0 {
		fmt.Fprintf(w, "\n  Preserved %d runtime state file(s) (kept this machine's, not the backup's):\n", len(res.skippedRuntime))
		sorted := append([]string(nil), res.skippedRuntime...)
		sort.Strings(sorted)
		for _, rel := range sorted[:min(10, len(sorted))] {
			fmt.Fprintf(w, "    %s\n", rel)
		}
		if len(sorted) > 10 {
			fmt.Fprintf(w, "    ... and %d more\n", len(sorted)-10)
		}
	}
	restoredProfiles, err := restoreProfileAliases(w, root)
	if err != nil {
		return 1, err
	}
	fmt.Fprintln(w)
	if st, err := os.Stat(pypath.PathJoin(root, "icebath-agent")); err != nil || !st.IsDir() {
		fmt.Fprintln(w, "Note: The icebath-agent codebase was not included in the backup.")
		fmt.Fprintln(w, "  If this is a fresh install, run: icebath update")
	}
	if len(restoredProfiles) > 0 {
		fmt.Fprintln(w, "\nTo re-enable gateway services for profiles:")
		for _, p := range restoredProfiles {
			fmt.Fprintf(w, "  icebath -p %s gateway install\n", p.name)
		}
	}
	reviveGatewayAfterImport(ctx, w, root)
	if len(res.errors) > 0 {
		fmt.Fprintf(w, "Import incomplete: %d file(s) were not restored (see Warnings above). "+
			"Fix the cause and re-run the import.\n", len(res.errors))
		return 1, nil
	}
	fmt.Fprintln(w, "Done. Your Icebath configuration has been restored.")
	return 0, nil
}

// One profile alias the import tried to restore and whether its wrapper was written
type restoredProfile struct {
	name string
	ok   bool
}

// Recreates the wrapper scripts of restored profiles that carry a config or env file
func restoreProfileAliases(w io.Writer, root string) ([]restoredProfile, error) {
	profilesDir := pypath.PathJoin(root, "profiles")
	if st, err := os.Stat(profilesDir); err != nil || !st.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return nil, err
	}
	var restored []restoredProfile
	for _, entry := range entries {
		path := pypath.PathJoin(profilesDir, entry.Name())
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			continue
		}
		name := entry.Name()
		_, cfgErr := os.Stat(pypath.PathJoin(path, "config.yaml"))
		_, envErr := os.Stat(pypath.PathJoin(path, ".env"))
		if cfgErr != nil && envErr != nil {
			continue
		}
		collision, err := profiles.CheckAliasCollision(name)
		if err != nil {
			return nil, err
		}
		if collision != "" {
			fmt.Fprintf(w, "  Skipped alias '%s': %s\n", name, collision)
			restored = append(restored, restoredProfile{name, false})
			continue
		}
		wrapper, err := profiles.CreateWrapperScript(name, "")
		if err != nil {
			return nil, err
		}
		restored = append(restored, restoredProfile{name, wrapper != ""})
	}
	if len(restored) == 0 {
		return restored, nil
	}
	var created, skipped []string
	for _, p := range restored {
		if p.ok {
			created = append(created, p.name)
		} else {
			skipped = append(skipped, p.name)
		}
	}
	if len(created) > 0 {
		fmt.Fprintf(w, "\n  Profile aliases restored: %s\n", strings.Join(created, ", "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(w, "  Profile aliases skipped:  %s\n", strings.Join(skipped, ", "))
	}
	inPath, err := profiles.IsWrapperDirInPath()
	if err != nil {
		return nil, err
	}
	if !inPath {
		dir, err := profiles.WrapperDir()
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(w, "\n  Note: %s is not in your PATH.\n", dir)
		fmt.Fprintln(w, "  Add to your shell config (~/.bashrc or ~/.zshrc):")
		fmt.Fprintln(w, `    export PATH="$HOME/.local/bin:$PATH"`)
	}
	return restored, nil
}

// Session database the import replaced with one holding fewer messages
type shrunkDB struct {
	rel           string
	before, after [2]*big.Int
}

// Counts and problems of one member publishing pass
type importResult struct {
	restored, restoredExternal int
	errors, skippedRuntime     []string
	dbShrunk                   []shrunkDB
}

// Appends a path once, keeping first insertion order
func addHome(homes []string, seen map[string]bool, p string) []string {
	if seen[p] {
		return homes
	}
	seen[p] = true
	return append(homes, p)
}

// Reserves every profile the archive touches before publishing even the first member
func importMembers(ctx context.Context, w io.Writer, zr *pyzip.ZipReader, members []string, prefix, root string, fileCount int) (importResult, error) {
	rootRes, err := pypath.Resolve(root)
	if err != nil {
		return importResult{}, err
	}
	seen := map[string]bool{}
	homes := addHome(nil, seen, root)
	for _, member := range members {
		if strings.HasPrefix(member, externalPrefix) {
			continue
		}
		rel := stripPrefix(member, prefix)
		anchor, parts := pypath.PathParts(rel)
		if anchor != "" {
			parts = append([]string{anchor}, parts...)
		}
		if len(parts) >= 3 && parts[0] == "profiles" {
			profileHome := pypath.PathJoin(root, parts[0], parts[1])
			resolved, err := pypath.Resolve(profileHome)
			if err != nil {
				return importResult{}, err
			}
			if _, under := pypath.PathRelativeTo(resolved, rootRes); under {
				homes = addHome(homes, seen, profileHome)
			}
		}
		target := pypath.PathJoin(root, rel)
		if pypath.PathSuffix(target) == ".db" {
			resolved, err := pypath.Resolve(target)
			if err != nil {
				return importResult{}, err
			}
			if _, under := pypath.PathRelativeTo(resolved, rootRes); under {
				abs, err := pathAbsolute(target)
				if err != nil {
					return importResult{}, err
				}
				homes = addHome(homes, seen, pypath.PathParent(abs))
				homes = addHome(homes, seen, pypath.PathParent(resolved))
			}
		}
	}
	inner, release, err := gateway.ExclusiveMaintenance(ctx, homes, "")
	if err != nil {
		return importResult{}, err
	}
	defer release()
	return importMembersExclusive(inner, w, zr, members, prefix, root, fileCount)
}

// Publishes one external member under the user home, false with the error recorded when it is refused or fails
func importExternalMember(zr *pyzip.ZipReader, member, homeDir string, newMode uint32, newModeOK bool, errs *[]string) (bool, error) {
	target := pypath.PathJoin(homeDir, member[len(externalPrefix):])
	resolved, err := pypath.Resolve(target)
	if err != nil {
		return false, err
	}
	if _, under := pypath.PathRelativeTo(resolved, homeDir); !under {
		*errs = append(*errs, "  "+member+": path traversal blocked")
		return false, nil
	}
	err = home.PathMkdir(pypath.PathParent(target), 0o777, true, true)
	if err == nil {
		err = extractMember(zr, member, target, newMode, newModeOK)
	}
	if err != nil {
		if isMemberReadError(err) {
			*errs = append(*errs, "  "+member+": "+excText(err))
			return false, nil
		}
		return false, err
	}
	switch pypath.PathSuffix(target) {
	case ".json", ".env", ".conf":
		_ = os.Chmod(target, 0o600)
	default:
		if secretFileNames[pypath.PathName(target)] {
			_ = os.Chmod(target, 0o600)
		}
	}
	return true, nil
}

// Publishes every member while the profiles are reserved, recording skips, failures and shrunk session stores
func importMembersExclusive(ctx context.Context, w io.Writer, zr *pyzip.ZipReader, members []string, prefix, root string, fileCount int) (importResult, error) {
	var res importResult
	userHome, err := home.UserHome()
	if err != nil {
		return res, err
	}
	homeDir, err := pypath.Resolve(userHome)
	if err != nil {
		return res, err
	}
	newMode, newModeOK := defaultNewFileMode()
	rootRes, err := pypath.Resolve(root)
	if err != nil {
		return res, err
	}
	progress := func() {
		if res.restored%500 == 0 {
			fmt.Fprintf(w, "  %d/%d files ...\n", res.restored, fileCount)
		}
	}
	for _, member := range members {
		if rest, ok := strings.CutPrefix(member, externalPrefix); ok {
			if rest == "" {
				continue
			}
			ok, err := importExternalMember(zr, member, homeDir, newMode, newModeOK, &res.errors)
			if err != nil {
				return res, err
			}
			if ok {
				res.restored++
				res.restoredExternal++
			}
			progress()
			continue
		}
		rel := stripPrefix(member, prefix)
		if rel == "" {
			continue
		}
		parts, err := pyfs.NormalizeArchiveParts(rel)
		if err != nil {
			res.errors = append(res.errors, "  "+rel+": path traversal blocked")
			continue
		}
		entry, isRoot := profileRootEntry(parts)
		if importSkipNames[parts[len(parts)-1]] || (isRoot && profiles.PMRuntimeRootDirs[entry]) || hasSidecarSuffix(rel) {
			res.skippedRuntime = append(res.skippedRuntime, rel)
			continue
		}
		target := pypath.PathJoin(root, parts...)
		resolved, err := pypath.Resolve(target)
		if err != nil {
			return res, err
		}
		if _, under := pypath.PathRelativeTo(resolved, rootRes); !under {
			res.errors = append(res.errors, "  "+rel+": path traversal blocked")
			continue
		}
		err = func() error {
			if err := home.PathMkdir(pypath.PathParent(target), 0o777, true, true); err != nil {
				return err
			}
			if pypath.PathSuffix(target) == ".db" {
				before, err := countSessionRows(ctx, target)
				if err != nil {
					return err
				}
				if err := importDBMember(ctx, zr, member, target, newMode, newModeOK); err != nil {
					return err
				}
				after, err := countSessionRows(ctx, target)
				if err != nil {
					return err
				}
				if before != nil && after != nil && after[1].Cmp(before[1]) < 0 {
					res.dbShrunk = append(res.dbShrunk, shrunkDB{rel, *before, *after})
				}
			} else if err := extractMember(zr, member, target, newMode, newModeOK); err != nil {
				return err
			}
			if secretFileNames[pypath.PathName(target)] {
				if err := os.Chmod(target, 0o600); err != nil {
					return err
				}
			}
			res.restored++
			return nil
		}()
		if err != nil {
			if !isMemberReadError(err) {
				return res, err
			}
			res.errors = append(res.errors, "  "+rel+": "+excText(err))
		}
		progress()
	}
	return res, nil
}

// Reports whether two paths are equal as pathlib compares them
func pathEqual(a, b string) bool {
	return pypath.Normcase(pypath.PathStr(a)) == pypath.Normcase(pypath.PathStr(b))
}

// Starts an installed gateway service after a restore into the default home, printing the manual fallback on failure
func reviveGatewayAfterImport(ctx context.Context, w io.Writer, root string) {
	native := platformDefaultHome()
	if !pathEqual(root, native) {
		for _, marker := range []string{"config.yaml", ".env", "state.db"} {
			if _, err := os.Stat(pypath.PathJoin(native, marker)); err == nil {
				fmt.Fprintf(w, "\nRestored into a non-default home; leaving the gateway service alone to avoid clashing "+
					"with the install at %s.\nTo start a gateway for this home, run:  icebath gateway run\n", pypath.PathStr(native))
				return
			}
		}
	}
	running, err := isServiceRunning(ctx)
	if err == nil && !running {
		fmt.Fprintln(w)
		_, err = ensureGatewayService(ctx, w, gateway.ServiceSetupOptions{Context: "import"})
	}
	if err != nil {
		slog.Debug("gateway revive after import failed", "err", err)
		fmt.Fprintln(w, "\nStart the gateway to activate cron jobs and messaging:\n  icebath gateway run")
	}
}
