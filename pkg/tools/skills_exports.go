package tools

import (
	"net/http"
	"time"
)

// Caps text at limit characters with the elision marker, the exported face for pkg/skills
func Elide(text string, limit int) string { return elide(text, limit) }

// Bash executable the local backend runs, the exported face for pkg/skills
func FindBash() (string, error) { return findBash() }

// Reports a literal parent directory component, the exported face for pkg/skills
func HasTraversalComponent(p string) bool { return hasTraversalComponent(p) }

// Fuzzy find and replace shared with the patch tool, the exported face for pkg/skills
func FuzzyFindAndReplace(content, oldString, newString string, replaceAll bool, correctionNotes *[]string) (string, int, string, string) {
	return fuzzyFindAndReplace(content, oldString, newString, replaceAll, correctionNotes, nil)
}

// Did you mean hint for a plain no match error, the exported face for pkg/skills
func FormatNoMatchHint(errText string, matchCount int, oldString, content string) string {
	return formatNoMatchHint(errText, matchCount, oldString, content)
}

// Session context variable of the turn, the process env otherwise, the exported face for pkg/skills
func SessionEnvValue(env *Env, name string) string { return sessionEnvValue(env, name) }

// HOME a tool subprocess gets under the home mode, empty when it keeps its own, the exported face for pkg/skills
func SubprocessHome(env *Env) string { return subprocessHome(env, map[string]string{}) }

// Expands environment references like os.path.expandvars, the exported face for pkg/skills
func OSPathExpandVars(p string) string { return osPathExpandVars(p) }

// Lines of difflib.unified_diff over lines that keep their terminators, the exported face for pkg/skills
func UnifiedDiff(a, b []string, fromfile, tofile string, n int) []string {
	return unifiedDiff(a, b, fromfile, tofile, n)
}

// Reports whether a URL targets a public host, the exported face for pkg/skills
func IsSafeURL(env *Env, raw string) bool { return isSafeURL(env, raw) }

// HTTP client that vets every connect address against SSRF rules, the exported face for pkg/skills
func NewSSRFSafeClient(env *Env, timeout time.Duration, followRedirects bool) *http.Client {
	return newSSRFSafeClient(env, timeout, followRedirects, nil)
}

// Reentrant cross process file lock holder, the exported face for pkg/skills
type FileLockHolder = fileLockHolder

// Takes the advisory lock file within the timeout and returns its release, the exported face for pkg/skills
func AcquireFileLock(lockPath string, holder *FileLockHolder, timeout time.Duration, msg string) (func(), error) {
	return acquireFileLock(lockPath, holder, timeout, msg)
}
