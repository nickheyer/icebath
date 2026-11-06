package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Cwd override registered for the raw task id, empty when none is registered
func RegisteredTaskCwdOverride(env *Env, taskID string) string {
	return registeredTaskCwdOverride(env, taskID)
}

// Removes runtime owned PYTHONPATH entries in place, the error is a damaged runtime dependency record
func StripIcebathOwnedPythonpath(child map[string]string) error {
	return stripIcebathOwnedPythonpath(child)
}

// Reports whether two path spellings name the same components under host filesystem case rules
func SamePath(left, right string) bool {
	return samePathParts(left, right, pypath.Normcase)
}

// Compares pathlib parts of both spellings after the given normcase
func samePathParts(left, right string, normcase func(string) string) bool {
	la, lp := pypath.PathParts(left)
	ra, rp := pypath.PathParts(right)
	if normcase(la) != normcase(ra) || len(lp) != len(rp) {
		return false
	}
	for i := range lp {
		if normcase(lp[i]) != normcase(rp[i]) {
			return false
		}
	}
	return true
}

// Tree the running binary lives in, the install root the runtime venv belongs to
var runtimeRepoRootDir = pm.RepoRoot

// Source checkout the binary was compiled from, a rooted build path taking the current drive, empty when trimmed or absent
var runtimeSourceRootDir = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	if !filepath.IsAbs(file) {
		if !strings.HasPrefix(file, "/") && !strings.HasPrefix(file, `\`) {
			return ""
		}
		abs, err := filepath.Abs(file)
		if err != nil {
			return ""
		}
		file = abs
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return ""
	}
	return root
}

// Repo root spellings of the install tree and the source checkout computed once like the module level tuple
var runtimeRepoRootAliases = sync.OnceValue(func() []string {
	aliases := repoRootAliasesOf(runtimeRepoRootDir())
	if source := runtimeSourceRootDir(); source != "" {
		for _, alias := range repoRootAliasesOf(source) {
			if !slices.ContainsFunc(aliases, func(a string) bool { return SamePath(a, alias) }) {
				aliases = append(aliases, alias)
			}
		}
	}
	return aliases
})

// Aliases of one root spelling from its resolved and lexical absolute forms under the process home
func repoRootAliasesOf(lexical string) []string {
	resolved, err := pypath.Resolve(lexical)
	if err != nil {
		resolved = lexical
	}
	return buildIcebathRepoRootAliases(resolved, pathAbsolute(lexical), home.Process())
}

// Absolute spelling of a path without resolving links or dot dot components
func pathAbsolute(p string) string {
	if pypath.IsAbs(p) {
		return pypath.PathStr(p)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return pypath.PathStr(p)
	}
	return pypath.PathJoin(cwd, p)
}

// Exact repo root spellings launchers emit, home relative re homes and proven repo junctions included
func buildIcebathRepoRootAliases(resolvedRoot, lexicalRoot, configuredHome string) []string {
	candidates := []string{resolvedRoot, lexicalRoot}
	homes := []string{configuredHome}
	if pypath.PathName(pypath.PathParent(configuredHome)) == home.ProfilesDir {
		homes = append(homes, pypath.PathParent(pypath.PathParent(configuredHome)))
	}
	for _, h := range homes {
		resolvedHome, err := pypath.Resolve(h)
		if err != nil {
			continue
		}
		if rel, ok := pathPartsBelow(resolvedRoot, resolvedHome); ok {
			candidates = append(candidates, pypath.PathJoin(h, rel...))
		}
	}
	for _, h := range homes {
		repoCandidate := pypath.PathJoin(h, pypath.PathName(resolvedRoot))
		physical, err := pypath.Realpath(repoCandidate, pypath.StrictAll)
		if err != nil {
			continue
		}
		root, err := pypath.Realpath(resolvedRoot, pypath.StrictAll)
		if err == nil && physical == root {
			candidates = append(candidates, repoCandidate)
		}
	}
	var aliases []string
	for _, c := range candidates {
		dup := false
		for _, existing := range aliases {
			if SamePath(c, existing) {
				dup = true
				break
			}
		}
		if !dup {
			aliases = append(aliases, c)
		}
	}
	return aliases
}

// Components of path below base when the normcased commonpath of both is base itself
func pathPartsBelow(path, base string) ([]string, bool) {
	pa, pp := pypath.PathParts(path)
	ba, bp := pypath.PathParts(base)
	if runtime.GOOS != "windows" {
		pa, ba = posixCommonpathAnchor(pa), posixCommonpathAnchor(ba)
	}
	if pypath.Normcase(pa) != pypath.Normcase(ba) || len(pp) < len(bp) {
		return nil, false
	}
	for i := range bp {
		if pypath.Normcase(pp[i]) != pypath.Normcase(bp[i]) {
			return nil, false
		}
	}
	return pp[len(bp):], true
}

// Leading double slash collapses to one slash the way posixpath commonpath prefixes absolute paths
func posixCommonpathAnchor(anchor string) string {
	if anchor == "//" {
		return "/"
	}
	return anchor
}

// Regular file test that follows links and reads every failure as absent
func isRegularFileAt(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Producer owned runtime venv, the PM generation, a sealed payload venv, then a proven repo VIRTUAL_ENV
func ValidatedRuntimeVenv(child map[string]string) (string, bool, error) {
	ctx := context.Background()
	root, err := pypath.Resolve(runtimeRepoRootDir())
	if err != nil {
		return "", false, err
	}
	if isRegularFileAt(pm.EnvironmentRuntimeFactsPath(ctx, root)) {
		venv, err := pm.SelectedVenv(ctx, root)
		if err != nil {
			return "", false, err
		}
		return venv, true, nil
	}
	if shipped, err := pm.PayloadVenv(root); err == nil && shipped != "" && isRegularFileAt(filepath.Join(shipped, "pyvenv.cfg")) {
		return shipped, true, nil
	}
	virtualEnv := child["VIRTUAL_ENV"]
	if virtualEnv == "" {
		return "", false, nil
	}
	owned := false
	for _, alias := range runtimeRepoRootAliases() {
		if SamePath(virtualEnv, pypath.PathJoin(alias, "venv")) {
			owned = true
			break
		}
	}
	if !owned || !isRegularFileAt(filepath.Join(virtualEnv, "pyvenv.cfg")) {
		return "", false, nil
	}
	return pypath.PathStr(virtualEnv), true, nil
}
