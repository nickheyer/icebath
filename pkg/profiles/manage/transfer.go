package manage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reports whether a path lies inside a Git checkout by its own resolved ancestry, true when resolution fails
func insideGitCheckout(path string) bool {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	resolved = resolvePath(resolved)
	for cur := resolved; ; cur = filepath.Dir(cur) {
		if pathExists(filepath.Join(cur, ".git")) {
			return true
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}

// Account home and temp dir behind the export fallbacks, swapped by tests
var (
	userHomeDir = os.UserHomeDir
	tempDir     = os.TempDir
)

// Export directory that cannot become source tree input, failing closed when every candidate is a checkout
func profileExportDirectory() (string, error) {
	exportDir := filepath.Join(profiles.DefaultHome(), "profile-exports")
	if !insideGitCheckout(exportDir) {
		return exportDir, nil
	}
	uidSuffix := ""
	if uid := os.Getuid(); uid >= 0 {
		uidSuffix = fmt.Sprintf("-%d", uid)
	}
	accountHome, err := userHomeDir()
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{
		filepath.Join(accountHome, ".icebath-profile-exports"), filepath.Join(tempDir(), "icebath-profile-exports"+uidSuffix),
	} {
		if !insideGitCheckout(candidate) {
			return candidate, nil
		}
	}
	return "", &profiles.ValueError{Msg: "No safe automatic export destination: every candidate directory is " +
		"inside a Git checkout. Provide an explicit output path outside the " +
		"checkout (CLI: -o /path/outside/repo/profile.tar.gz)."}
}

// Managed archive path for an export with no explicit output, refusing a symlinked or foreign owned directory
func GetProfileExportPath(name, timestamp string) (string, error) {
	canon, err := profiles.CanonValid(name)
	if err != nil {
		return "", err
	}
	exportDir, err := profileExportDirectory()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(exportDir, 0o777); err != nil {
		return "", err
	}
	if st, err := os.Lstat(exportDir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", &profiles.ValueError{Msg: fmt.Sprintf("Export directory %s is a symlink; refusing to write "+
			"a profile archive through it. Provide an explicit output path.", exportDir)}
	}
	if owner, ok := fileOwnerUID(exportDir); ok && owner != os.Getuid() {
		return "", &profiles.ValueError{Msg: fmt.Sprintf("Export directory %s is owned by another user; "+
			"refusing to write a profile archive there. Provide an explicit output path.", exportDir)}
	}
	if timestamp == "" {
		timestamp = time.Now().Format("20060102-150405")
	}
	return filepath.Join(exportDir, canon+"-"+timestamp+".tar.gz"), nil
}

// Copytree ignore of the default profile export, a root allow list plus universal and credential exclusions
func defaultExportIgnore(rootDir string) pyfs.IgnoreFunc {
	return func(dir string, names []string) map[string]bool {
		ignored := nonExportableEntries(dir, names)
		for _, n := range names {
			if n == "package.json" || n == "package-lock.json" {
				ignored[n] = true
			}
		}
		for n := range exportCredentialEntries(dir, names) {
			ignored[n] = true
		}
		if filepath.Clean(dir) == filepath.Clean(rootDir) {
			for _, n := range names {
				if !profiles.DefaultExportIncludeRoot[n] {
					ignored[n] = true
				}
			}
		}
		return ignored
	}
}

var exportCredentialParts = func() [][]string {
	var out [][]string
	for _, p := range append([]string{"auth.json", ".env", "bot-desktop"}, profiles.OSCredentialStores...) {
		out = append(out, strings.Split(p, "/"))
	}
	return out
}()

// Entries of dir that are a credential store, matched on trailing path components at any depth
func exportCredentialEntries(dir string, names []string) map[string]bool {
	parts := pathParts(dir)
	out := map[string]bool{}
	for _, entry := range names {
		for _, store := range exportCredentialParts {
			if entry != store[len(store)-1] {
				continue
			}
			lead := store[:len(store)-1]
			start := len(parts) + 1 - len(store)
			if start < 0 {
				start = 0
			}
			if start > len(parts) {
				start = len(parts)
			}
			if equalParts(parts[start:], lead) {
				out[entry] = true
			}
		}
	}
	return out
}

// Components of a path like PurePath.parts, the root kept as its own first part
func pathParts(p string) []string {
	p = filepath.Clean(p)
	var parts []string
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	if strings.HasPrefix(rest, string(filepath.Separator)) {
		parts = append(parts, vol+string(filepath.Separator))
		rest = rest[1:]
	} else if vol != "" {
		parts = append(parts, vol)
	}
	for _, s := range strings.Split(rest, string(filepath.Separator)) {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	return parts
}

func equalParts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var exportRedactSuffixes = map[string]bool{
	".md": true, ".txt": true, ".yaml": true, ".yml": true, ".json": true, ".jsonl": true, ".toml": true, ".ini": true,
	".cfg": true, ".conf": true, ".py": true, ".sh": true, ".bash": true, ".zsh": true, ".js": true, ".ts": true,
	".tsx": true, ".jsx": true, ".css": true, ".html": true, ".xml": true, ".csv": true,
}

// Reports whether an exported file is text or config the secret scrub rewrites
func shouldRedactExportFile(path string) bool {
	name := filepath.Base(path)
	lower := strings.ToLower(name)
	return name == ".cursorrules" || strings.HasSuffix(lower, ".env.example") || strings.HasPrefix(lower, "config.yaml.") ||
		exportRedactSuffixes[strings.ToLower(pySuffix(name))]
}

// Suffix of a file name like PurePath.suffix
func pySuffix(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// Force redacts secret shaped strings in a staged export tree, materializing changed symlinked files
func scrubExportSecrets(staged string) error {
	var paths []string
	if err := walkNoFollow(staged, &paths); err != nil {
		return err
	}
	for _, path := range paths {
		lst, err := os.Lstat(path)
		if err != nil {
			continue
		}
		isLink := lst.Mode()&os.ModeSymlink != 0
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() || !shouldRedactExportFile(path) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil || !utf8.Valid(raw) {
			continue
		}
		text := readTextNewlines(strings.TrimPrefix(string(raw), "\ufeff"))
		redacted := tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true})
		if redacted == text {
			continue
		}
		if isLink {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		if err := os.WriteFile(path, []byte(textForWrite(redacted)), 0o666); err != nil {
			return err
		}
	}
	return nil
}

// Every path under root like Path.rglob, descending into real directories only
func walkNoFollow(root string, out *[]string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		*out = append(*out, p)
		if e.IsDir() {
			if err := walkNoFollow(p, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// Text file an export adds to the staged tree at an archive relative path, applied in order
type ExportExtraFile struct{ Rel, Content string }

// Exports a profile to a tar.gz without credentials and with staged text force redacted, returning the archive path
func ExportProfile(name, outputPath string, extraFiles []ExportExtraFile) (_ string, err error) {
	canon, profileDir, err := profiles.ExistingProfileDir(name)
	if err != nil {
		return "", err
	}
	base := strings.TrimSuffix(strings.TrimSuffix(pyPathString(outputPath), ".tar.gz"), ".tgz")
	ignoreCredentials := func(dir string, names []string) map[string]bool {
		ignored := nonExportableEntries(dir, names)
		for n := range exportCredentialEntries(dir, names) {
			ignored[n] = true
		}
		rel, _ := filepath.Rel(profileDir, dir)
		relParts := pathParts(rel)
		for _, n := range names {
			if profiles.ProfilePathIsPrivate(append(append([]string{}, relParts...), n)) {
				ignored[n] = true
			}
		}
		if filepath.Clean(dir) == filepath.Clean(profileDir) {
			for _, n := range names {
				if profiles.PMRuntimeRootDirs[n] {
					ignored[n] = true
				}
			}
		}
		return ignored
	}
	ignore := pyfs.IgnoreFunc(ignoreCredentials)
	if canon == "default" {
		ignore = defaultExportIgnore(profileDir)
	}
	tmp, err := os.MkdirTemp("", "tmp")
	if err != nil {
		return "", err
	}
	defer pyfs.CloseTempDir(tmp, &err)
	staged := filepath.Join(tmp, canon)
	if err := pyfs.CopyTree(profileDir, staged, true, false, ignore); err != nil {
		return "", err
	}
	for _, extra := range extraFiles {
		parts, err := pyfs.NormalizeArchiveParts(extra.Rel)
		if err != nil {
			return "", err
		}
		target := filepath.Join(append([]string{staged}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, []byte(textForWrite(extra.Content)), 0o666); err != nil {
			return "", err
		}
	}
	if err := scrubExportSecrets(staged); err != nil {
		return "", err
	}
	return pyfs.MakeTargz(base, tmp, canon)
}

// Removes a staged runtime tree of an imported archive, swapped by tests
var rmtreeStagedRuntime = func(p string) error { return pyfs.RmTree(p, nil) }

// Imports a profile archive with exactly one root directory, dropping foreign package manager runtime trees
func ImportProfile(archivePath, name string) (_ string, err error) {
	if !pathExists(archivePath) {
		return "", &profiles.NotFoundError{Msg: "Archive not found: " + archivePath}
	}
	topDirs, err := pyfs.ArchiveRootDirs(archivePath)
	if err != nil {
		return "", err
	}
	archiveRoot := ""
	if len(topDirs) == 1 {
		for d := range topDirs {
			archiveRoot = d
		}
	}
	inferred := name
	if inferred == "" {
		inferred = archiveRoot
	}
	if inferred == "" {
		return "", &profiles.ValueError{Msg: "Cannot determine profile name from archive. " +
			"Specify it explicitly: icebath profile import <archive> --name <name>"}
	}
	if archiveRoot == "" {
		return "", &profiles.ValueError{Msg: "Profile archive must contain exactly one top-level directory."}
	}
	canon, err := profiles.CanonValid(inferred)
	if err != nil {
		return "", err
	}
	if canon == "default" {
		return "", &profiles.ValueError{Msg: "Cannot import as 'default' — that is the built-in root profile (~/.icebath). " +
			"Specify a different name: icebath profile import <archive> --name <name>"}
	}
	profileDir, err := profiles.GetProfileDir(canon)
	if err != nil {
		return "", err
	}
	if pathExists(profileDir) {
		return "", profiles.ProfileExistsError(canon)
	}
	if err := os.MkdirAll(profiles.ProfilesRoot(), 0o777); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp("", "icebath_profile_import_")
	if err != nil {
		return "", err
	}
	defer pyfs.CloseTempDir(staging, &err)
	if err := pyfs.SafeExtractTargz(archivePath, staging); err != nil {
		return "", err
	}
	extracted := filepath.Join(staging, archiveRoot)
	if !isDir(extracted) {
		return "", &profiles.ValueError{Msg: "Profile archive root is missing or invalid: " + archiveRoot}
	}
	finalSource := extracted
	if archiveRoot != canon {
		finalSource = filepath.Join(staging, canon)
		if err := os.Rename(extracted, finalSource); err != nil {
			return "", err
		}
	}
	children, err := os.ReadDir(finalSource)
	if err != nil {
		return "", err
	}
	for _, child := range children {
		if !profiles.PMRuntimeRootDirs[child.Name()] {
			continue
		}
		p := filepath.Join(finalSource, child.Name())
		if isDir(p) {
			err = rmtreeStagedRuntime(p)
		} else {
			err = unlinkMissingOK(p)
		}
		if err != nil {
			return "", err
		}
	}
	if _, err := pyfs.Move(finalSource, profileDir); err != nil {
		return "", err
	}
	return profileDir, nil
}

// Path text like str(Path(p)), dropping empty and dot components while keeping parent references
func pyPathString(p string) string {
	parts := pathParts(p)
	if len(parts) == 0 {
		return "."
	}
	out := parts[0]
	for i, part := range parts[1:] {
		if i > 0 || !strings.HasSuffix(out, string(filepath.Separator)) {
			out += string(filepath.Separator)
		}
		out += part
	}
	return out
}

// Text with universal newlines like a Python text mode read
func readTextNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Text with newlines translated to the host line separator like a Python text mode write
func textForWrite(s string) string {
	if filepath.Separator == '\\' {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}
