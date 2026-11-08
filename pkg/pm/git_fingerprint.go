package pm

import (
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Cheap checkout fingerprint read from git metadata without spawning git, false on any OSError
func ReadGitRevisionFingerprint(repoRoot string) (string, bool) {
	fp, err := gitRevisionFingerprint(repoRoot)
	if err != nil {
		if pypath.IsOSError(err) {
			return "", false
		}
		panic(err)
	}
	return fp, true
}

// Fingerprint as git ref and sha, the unresolved ref marker or the detached head value
func gitRevisionFingerprint(repoRoot string) (string, error) {
	gitDir := pypath.PathJoin(repoRoot, ".git")
	fi, err := pypath.PathlibStat(gitDir)
	if err != nil {
		return "", err
	}
	if fi != nil && fi.Mode().IsRegular() {
		text, err := readGitText(gitDir)
		if err != nil {
			return "", err
		}
		for _, line := range pystr.Splitlines(text, false) {
			key, value, _ := strings.Cut(line, ":")
			if pystr.Strip(key) == "gitdir" && pystr.Strip(value) != "" {
				if gitDir, err = pypath.Resolve(pypath.PathJoin(repoRoot, pystr.Strip(value))); err != nil {
					return "", err
				}
				break
			}
		}
	}
	commonDir := gitDir
	commondirFile := pypath.PathJoin(gitDir, "commondir")
	fi, err = pypath.PathlibStat(commondirFile)
	if err != nil {
		return "", err
	}
	if fi != nil {
		resolved, err := readCommonDir(gitDir, commondirFile)
		if err != nil && !pypath.IsOSError(err) {
			return "", err
		}
		if err == nil && resolved != "" {
			commonDir = resolved
		}
	}
	text, err := readGitText(pypath.PathJoin(gitDir, "HEAD"))
	if err != nil {
		return "", err
	}
	head := pystr.Strip(text)
	if !strings.HasPrefix(head, "ref:") {
		return "git:HEAD:" + head, nil
	}
	_, rest, _ := strings.Cut(head, ":")
	ref := pystr.Strip(rest)
	for _, candidate := range []string{gitDir, commonDir} {
		refFile := pypath.PathJoin(candidate, ref)
		fi, err := pypath.PathlibStat(refFile)
		if err != nil {
			return "", err
		}
		if fi != nil {
			sha, err := readGitText(refFile)
			if err != nil {
				return "", err
			}
			return "git:" + ref + ":" + pystr.Strip(sha), nil
		}
	}
	if packed := readPackedRef(commonDir, ref); packed != "" {
		return "git:" + ref + ":" + packed, nil
	}
	return "git:" + ref + ":unresolved", nil
}

// Resolved common gitdir a worktree commondir file names, empty when the file is blank
func readCommonDir(gitDir, commondirFile string) (string, error) {
	text, err := readGitText(commondirFile)
	if err != nil {
		return "", err
	}
	rel := pystr.Strip(text)
	if rel == "" {
		return "", nil
	}
	return pypath.Resolve(pypath.PathJoin(gitDir, rel))
}

// Sha a packed-refs file records for a ref, empty when unreadable or absent
func readPackedRef(commonDir, ref string) string {
	text, err := readGitText(pypath.PathJoin(commonDir, "packed-refs"))
	if err != nil {
		return ""
	}
	for _, line := range pystr.Splitlines(text, false) {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && pystr.Strip(name) == ref {
			return pystr.Strip(sha)
		}
	}
	return ""
}

// File text decoded as utf-8-sig with undecodable bytes replaced
func readGitText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return pycodecs.DecodeReplace(data, "utf-8-sig")
}
