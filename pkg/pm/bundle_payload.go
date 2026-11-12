package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Snapshot dirs a sealed payload never reads, never packaged, compiled or baked
var inertSnapshotDirs = []string{
	"tests", "tests-js", "website", "evals", ".github", "nix", "docker",
	"apps", "ui-tui", "web", "scripts", "dev", "site", "packaging", "assets",
}

// Writes text with newline translated to the host line separator the way Path.write_text does
func writeTextFile(path, text string) error {
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	return os.WriteFile(path, []byte(text), 0o666)
}

// Archives a resolved git revision into destination without checkout metadata
func bundleSnapshot(ctx context.Context, source, revision, destination string, exclude []string) (err error) {
	repo, dest := resolvePath(source), resolvePath(destination)
	if repo == dest || isRelativeTo(repo, dest) {
		return &PyError{Class: "ValueError", Message: "the snapshot destination must not contain the source checkout"}
	}
	temp, err := os.MkdirTemp("", "icebath-archive-")
	if err != nil {
		return err
	}
	defer pyfs.CloseTempDir(temp, &err)
	archive := filepath.Join(temp, "source.tar")
	argv := []string{"git", "archive", "--format=tar", "--output", archive, revision, "--"}
	for _, name := range exclude {
		argv = append(argv, ":(exclude)"+name)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = repo, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &CalledProcessError{Argv: argv, Code: pyReturnCode(exitErr)}
		}
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		if err := pyfs.RmTree(dest, nil); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	return pyfs.ExtractTarData(archive, dest)
}

// Metadata path under the payload, refusing a symlink or a path resolving outside it
func payloadFile(root, relative string) (string, error) {
	path := filepath.Join(root, relative)
	if info, err := os.Lstat(path); (err == nil && info.Mode()&os.ModeSymlink != 0) || !isRelativeTo(resolvePath(path), resolvePath(root)) {
		return "", &PyError{Class: "ValueError", Message: fmt.Sprintf("metadata path escapes payload or is symlinked: %s", path)}
	}
	return path, nil
}

// Makes a packaged non secret record world readable outside Windows
func shareMetadata(path string) error {
	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o644)
	}
	return nil
}

// Removes the venv and PM runtime build locks from the payload
func discardBuildLocks(root string) error {
	for _, name := range []string{"venv", "pm-runtime"} {
		lock, err := payloadFile(root, name+"/.lock")
		if err != nil {
			return err
		}
		if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Records each staged tool's lock identity, env and tree digest into the payload facts
func recordTools(out, lockfile, target string, recorded *OrderedObject) error {
	store := filepath.Join(out, "tools")
	factsPath, err := payloadFile(out, "tools/facts.json")
	if err != nil {
		return err
	}
	facts, err := NewFacts(factsPath, false)
	if err != nil {
		return err
	}
	lock := NewLockfile(lockfile)
	for _, name := range recorded.Keys {
		entryName := pyrepr.Str(recorded.Values[name])
		entry := filepath.Join(store, entryName)
		version, _ := lock.Version(name)
		artifacts := lock.Artifacts(name, target)
		if !isDirectory(entry) || version == "" || len(artifacts) == 0 {
			return &PyError{Class: "ValueError", Message: "incomplete payload tool: " + name}
		}
		pkg, err := GetPackage(name)
		if err != nil {
			return err
		}
		env, err := PackageEnv(pkg, entry, target)
		if err != nil {
			return err
		}
		shas := make([]string, 0, len(artifacts))
		for _, artifact := range artifacts {
			sha, ok := artifact["sha256"]
			if !ok {
				return &PyError{Class: "KeyError", Message: pyrepr.StrRepr("sha256")}
			}
			shas = append(shas, pyrepr.Str(sha))
		}
		digest, err := TreeDigest(entry)
		if err != nil {
			return err
		}
		if err := facts.Record(name, version, entryName, env, store, FactRecord{Target: &target, Artifacts: shas, Digest: &digest}); err != nil {
			return err
		}
	}
	if len(recorded.Keys) > 0 {
		return shareMetadata(facts.Path)
	}
	return nil
}

// Rerecords final tool tree digests before the enclosing package is signed, returning the count
func rehashTools(root string) (int, error) {
	factsPath, err := payloadFile(root, "tools/facts.json")
	if err != nil {
		return 0, err
	}
	facts, err := NewFacts(factsPath, true)
	if err != nil {
		return 0, err
	}
	count, err := facts.RefreshDigests(filepath.Join(root, "tools"))
	if err != nil {
		return 0, err
	}
	return count, shareMetadata(facts.Path)
}

// Seals the resident PM runtime to a relative home and records its interpreter and site directory
func sealPMRuntime(ctx context.Context, root, python string) error {
	_, err := sealPMRuntimeMarker(root, python)
	return err
}

// Seals the resident PM runtime and returns the marker it records
func sealPMRuntimeMarker(root, python string) (*pyjson.Object, error) {
	root, python = resolvePath(root), resolvePath(python)
	if info, err := os.Stat(python); !isRelativeTo(python, root) || err != nil || !info.Mode().IsRegular() {
		return nil, &PyError{Class: "ValueError", Message: "PM interpreter must belong to the payload: " + python}
	}
	runtimeDir := filepath.Join(root, "pm-runtime")
	sites, err := pypath.Glob(runtimeDir, "lib/python*/site-packages")
	if err != nil {
		return nil, err
	}
	windowsSites, err := pypath.Glob(runtimeDir, "Lib/site-packages")
	if err != nil {
		return nil, err
	}
	sites = append(sites, windowsSites...)
	if len(sites) != 1 {
		return nil, &PyError{Class: "ValueError", Message: "PM dependency directory missing or ambiguous: " + runtimeDir}
	}
	relPython, err := filepath.Rel(runtimeDir, python)
	if err != nil {
		return nil, err
	}
	relSite, err := filepath.Rel(runtimeDir, sites[0])
	if err != nil {
		return nil, err
	}
	marker := pyjson.NewObject()
	marker.Set("python", filepath.ToSlash(relPython))
	marker.Set("sitePackages", filepath.ToSlash(relSite))
	cfg, err := payloadFile(root, "pm-runtime/pyvenv.cfg")
	if err != nil {
		return nil, err
	}
	markerPath, err := payloadFile(root, "pm-runtime/pm-runtime.json")
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		return nil, err
	}
	text, err := decodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	dropped := []string{"home", "executable", "base-executable", "base-prefix", "base-exec-prefix", "command"}
	home, err := filepath.Rel(runtimeDir, filepath.Dir(python))
	if err != nil {
		return nil, err
	}
	lines := []string{"home = " + home}
	for _, line := range pystr.Splitlines(text, false) {
		key, _, _ := strings.Cut(line, "=")
		if !slices.Contains(dropped, pystr.Strip(key)) {
			lines = append(lines, line)
		}
	}
	if err := writeTextFile(cfg, strings.Join(lines, "\n")+"\n"); err != nil {
		return nil, err
	}
	bindir := filepath.Join(runtimeDir, "bin")
	if runtime.GOOS == "windows" {
		bindir = filepath.Join(runtimeDir, "Scripts")
	}
	entries, err := os.ReadDir(bindir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		path := filepath.Join(bindir, entry.Name())
		if runtime.GOOS == "windows" || entry.Type()&os.ModeSymlink == 0 {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				if err := os.Remove(path); err != nil {
					return nil, err
				}
			}
		}
	}
	if _, err := relativizeBinLinks(root, filepath.Join(runtimeDir, "bin")); err != nil {
		return nil, err
	}
	encoded, err := pyjson.Dumps(marker, pyjson.Indent(2))
	if err != nil {
		return nil, err
	}
	if err := writeTextFile(markerPath, encoded+"\n"); err != nil {
		return nil, err
	}
	if err := shareMetadata(markerPath); err != nil {
		return nil, err
	}
	return marker, discardBuildLocks(root)
}

// Text of utf-8-sig bytes, a leading BOM dropped and invalid UTF-8 refused
func decodeUTF8Sig(raw []byte) (string, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if err := strictUTF8(raw); err != nil {
		return "", &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	return string(raw), nil
}

// Rewrites absolute dependency venv and PM runtime launcher links as payload relative ones
func relativizeLinks(out string) error {
	_, err := relativizeLinkCount(out)
	return err
}

// Relativizes the venv and PM runtime bin links and returns how many it rewrote
func relativizeLinkCount(out string) (int, error) {
	root := resolvePath(out)
	if err := discardBuildLocks(root); err != nil {
		return 0, err
	}
	total := 0
	for _, name := range []string{"venv", "pm-runtime"} {
		count, err := relativizeBinLinks(root, filepath.Join(root, name, "bin"))
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

// Rewrites absolute links in directory to relative ones inside root, store links rebased onto root tools
func relativizeBinLinks(root, directory string) (int, error) {
	count := 0
	if !isDirectory(directory) {
		return 0, nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		link := filepath.Join(directory, entry.Name())
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(link)
		if err != nil {
			return 0, err
		}
		if !filepath.IsAbs(target) {
			if !isRelativeTo(filepath.Join(directory, target), root) {
				return 0, &PyError{Class: "ValueError", Message: fmt.Sprintf("link escapes payload: %s -> %s", link, target)}
			}
			continue
		}
		resolved := resolvePath(target)
		if !isRelativeTo(resolved, root) {
			_, parts := pypath.PathParts(target)
			index := slices.Index(parts, "tools")
			if index < 0 {
				return 0, &PyError{Class: "ValueError", Message: fmt.Sprintf("link escapes payload: %s -> %s", link, target)}
			}
			resolved = resolvePath(filepath.Join(append([]string{root}, parts[index:]...)...))
		}
		if _, err := os.Stat(resolved); !isRelativeTo(resolved, root) || err != nil {
			return 0, &PyError{Class: "ValueError", Message: fmt.Sprintf("missing payload link target: %s -> %s", link, target)}
		}
		relative, err := filepath.Rel(directory, resolved)
		if err != nil {
			return 0, err
		}
		if relative != target {
			if err := os.Remove(link); err != nil {
				return 0, err
			}
			if err := os.Symlink(relative, link); err != nil {
				return 0, err
			}
			count++
		}
	}
	entries, err = os.ReadDir(directory)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		link := filepath.Join(directory, entry.Name())
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		if _, err := os.Stat(link); err != nil || !isRelativeTo(resolvePath(link), root) {
			return 0, &PyError{Class: "ValueError", Message: "invalid relative payload link: " + link}
		}
	}
	return count, nil
}

// Payload maintenance entry point that relocates links or rehashes tool facts, writing its report to stdout
func PayloadMain(stdout io.Writer, action, payload string) (int, error) {
	switch action {
	case "rehash":
		count, err := rehashTools(payload)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "rehashed %d payload tools\n", count)
	case "relocate":
		if err := relativizeLinks(payload); err != nil {
			return 0, err
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: icebath pm payload [-h] {relocate,rehash} payload\nicebath pm payload: error: argument action: invalid choice: %s (choose from 'relocate', 'rehash')\n", pyrepr.StrRepr(action))
		return 0, &SystemExit{Code: 2}
	}
	return 0, nil
}
