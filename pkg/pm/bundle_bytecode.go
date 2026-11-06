package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Payload root marker telling launchers the bytecode caches are baked beside the sources
const bakedBytecodeMarker = ".icebath-baked-pycache"

// Time compileall may spend on one import root before the bake fails
var bakeCompileTimeout = 30 * time.Minute

// Payload import roots in launcher order, repo then venv site then PM runtime site
func bakeImportRoots(root string) ([]string, error) {
	roots := []string{filepath.Join(root, "icebath-agent")}
	for _, sourceRoot := range append(slices.Clone(roots), filepath.Join(root, "venv"), filepath.Join(root, "pm-runtime")) {
		if !isDirectory(sourceRoot) {
			return nil, &PyError{Class: "FileNotFoundError", Message: "missing import root: " + sourceRoot}
		}
	}
	for _, layer := range []struct{ dir, label string }{{"venv", "staged venv"}, {"pm-runtime", "PM runtime"}} {
		posix, err := pypath.Glob(filepath.Join(root, layer.dir), "lib/python*/site-packages")
		if err != nil {
			return nil, err
		}
		windows, err := pypath.Glob(filepath.Join(root, layer.dir), "Lib/site-packages")
		if err != nil {
			return nil, err
		}
		sort.Strings(posix)
		sort.Strings(windows)
		sites := append(posix, windows...)
		if len(sites) != 1 {
			return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("expected exactly one %s site-packages, found %d", layer.label, len(sites))}
		}
		roots = append(roots, sites[0])
	}
	return roots, nil
}

// Visits every entry under root the way Path.rglob walks, never descending linked directories
func rglobEntries(root string, visit func(path string, info os.DirEntry)) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		visit(path, entry)
		if entry.IsDir() {
			rglobEntries(path, visit)
		}
	}
}

// Reports whether any component of path is a bytecode cache directory
func underPycache(path string) bool {
	_, parts := pypath.PathParts(path)
	return slices.Contains(parts, "__pycache__")
}

// Bytecode cache path importlib derives for a source under the interpreter cache tag
func cacheFromSource(path, cacheTag string) string {
	head, tail := filepath.Split(path)
	base, sep, rest := "", "", tail
	if i := strings.LastIndex(tail, "."); i >= 0 {
		base, sep, rest = tail[:i], ".", tail[i+1:]
	}
	stem := base
	if stem == "" {
		stem = rest
	}
	return filepath.Join(head, "__pycache__", stem+sep+cacheTag+".pyc")
}

// Python program run by the staged interpreter that reports its cache tag or which sources parse
const bakeProbeScript = `import sys
if sys.argv[1] == "tag":
    tag = sys.implementation.cache_tag
    if tag is None:
        raise NotImplementedError("sys.implementation.cache_tag is None")
    print(tag)
else:
    for path in sys.stdin.buffer.read().split(b"\0"):
        if not path:
            continue
        try:
            compile(open(path, "rb").read(), path.decode("utf-8", "surrogateescape"), "exec")
        except SyntaxError:
            print("syntax")
        else:
            print("ok")
`

// Runs the bake probe under the staged interpreter, returning its stdout lines
func runBakeProbe(ctx context.Context, python, mode string, stdin []byte) ([]string, error) {
	argv := []string{python, "-I", "-c", bakeProbeScript, mode}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stderr = bytes.NewReader(stdin), os.Stderr
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, &CalledProcessError{Argv: argv, Code: pyReturnCode(exitErr)}
		}
		return nil, err
	}
	return pystr.Splitlines(string(output), false), nil
}

// Parseable sources under root that lack a cached pyc, and the count of unparseable fixtures skipped
func bakeUncovered(ctx context.Context, root, python, cacheTag string) ([]string, int, error) {
	var candidates []string
	rglobEntries(root, func(path string, entry os.DirEntry) {
		if matched, _ := filepath.Match("*.py", entry.Name()); !matched || underPycache(path) {
			return
		}
		if pathExists(cacheFromSource(path, cacheTag)) {
			return
		}
		candidates = append(candidates, path)
	})
	if len(candidates) == 0 {
		return nil, 0, nil
	}
	var stdin bytes.Buffer
	for _, path := range candidates {
		stdin.WriteString(path)
		stdin.WriteByte(0)
	}
	verdicts, err := runBakeProbe(ctx, python, "parse", stdin.Bytes())
	if err != nil {
		return nil, 0, err
	}
	if len(verdicts) != len(candidates) {
		return nil, 0, &PyError{Class: "RuntimeError", Message: fmt.Sprintf("parse probe answered %d of %d sources", len(verdicts), len(candidates))}
	}
	var missing []string
	unparseable := 0
	for i, verdict := range verdicts {
		if verdict == "syntax" {
			unparseable++
		} else {
			missing = append(missing, candidates[i])
		}
	}
	return missing, unparseable, nil
}

// Compiles every payload module with the staged interpreter as unchecked hash pycs made owner writable
func bakeBytecode(ctx context.Context, out, python string) (map[string]int, error) {
	root := resolvePath(out)
	python = resolvePath(python)
	if !isRegularFile(python) {
		return nil, &PyError{Class: "FileNotFoundError", Message: "staged interpreter is missing: " + python}
	}
	roots, err := bakeImportRoots(root)
	if err != nil {
		return nil, err
	}
	tag, err := runBakeProbe(ctx, python, "tag", nil)
	if err != nil {
		return nil, err
	}
	if len(tag) != 1 {
		return nil, &PyError{Class: "RuntimeError", Message: "staged interpreter reported no cache tag"}
	}
	total := 0
	for _, sourceRoot := range roots {
		argv := []string{python, "-I", "-m", "compileall", "-q", "--invalidation-mode", "unchecked-hash", sourceRoot}
		compileCtx, cancel := context.WithTimeout(ctx, bakeCompileTimeout)
		cmd := exec.CommandContext(compileCtx, argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		timedOut := compileCtx.Err() == context.DeadlineExceeded
		cancel()
		if timedOut {
			return nil, &PyError{Class: "TimeoutExpired", Message: fmt.Sprintf("Command '%s' timed out after %d seconds", pyrepr.Repr(argv), int(bakeCompileTimeout.Seconds()))}
		}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, &CalledProcessError{Argv: argv, Code: pyReturnCode(exitErr)}
			}
			return nil, err
		}
		missing, _, err := bakeUncovered(ctx, sourceRoot, python, tag[0])
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			sample := make([]string, 0, 5)
			for _, path := range missing[:min(5, len(missing))] {
				rel, _ := filepath.Rel(root, path)
				sample = append(sample, rel)
			}
			return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("%d payload modules have no bytecode pyc (%s…): a fresh install would cold-compile them every launch", len(missing), strings.Join(sample, ", "))}
		}
		rglobEntries(sourceRoot, func(path string, entry os.DirEntry) {
			if matched, _ := filepath.Match("*.pyc", entry.Name()); matched && filepath.Base(filepath.Dir(path)) == "__pycache__" {
				total++
			}
		})
	}
	roots, err = bakeImportRoots(root)
	if err != nil {
		return nil, err
	}
	for _, sourceRoot := range roots {
		var caches []string
		rglobEntries(sourceRoot, func(path string, entry os.DirEntry) {
			if entry.Name() == "__pycache__" {
				caches = append(caches, path)
			}
		})
		for _, cache := range caches {
			entries, err := os.ReadDir(cache)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				pyc := filepath.Join(cache, entry.Name())
				if isRegularFile(pyc) {
					if err := os.Chmod(pyc, 0o644); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if err := writeTextFile(filepath.Join(root, bakedBytecodeMarker), "unchecked-hash\n"); err != nil {
		return nil, err
	}
	return map[string]int{"modules": total}, nil
}
