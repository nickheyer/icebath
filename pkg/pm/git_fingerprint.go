package pm

import (
	"os"
	"strings"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Cheap checkout fingerprint read from git metadata without spawning git, false on any OSError
func ReadGitRevisionFingerprint(repoRoot string) (string, bool) {
	fp, err := gitRevisionFingerprint(repoRoot)
	if err != nil {
		if errorutil.IsSystem(err) {
			return "", false
		}
		panic(err)
	}
	return fp, true
}

// Fingerprint as git ref and sha, the unresolved ref marker or the detached head value
func gitRevisionFingerprint(repoRoot string) (string, error) {
	gitDir := pathutil.Join(repoRoot, ".git")
	fi, err := pathutil.StatIfExists(gitDir)
	if err != nil {
		return "", err
	}
	if fi != nil && fi.Mode().IsRegular() {
		text, err := readGitText(gitDir)
		if err != nil {
			return "", err
		}
		for _, line := range textutil.Lines(text) {
			key, value, _ := strings.Cut(line, ":")
			if strings.TrimSpace(key) == "gitdir" && strings.TrimSpace(value) != "" {
				if gitDir, err = pathutil.Resolve(pathutil.Join(repoRoot, strings.TrimSpace(value))); err != nil {
					return "", err
				}
				break
			}
		}
	}
	commonDir := gitDir
	commondirFile := pathutil.Join(gitDir, "commondir")
	fi, err = pathutil.StatIfExists(commondirFile)
	if err != nil {
		return "", err
	}
	if fi != nil {
		resolved, err := readCommonDir(gitDir, commondirFile)
		if err != nil && !errorutil.IsSystem(err) {
			return "", err
		}
		if err == nil && resolved != "" {
			commonDir = resolved
		}
	}
	text, err := readGitText(pathutil.Join(gitDir, "HEAD"))
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(text)
	if !strings.HasPrefix(head, "ref:") {
		return "git:HEAD:" + head, nil
	}
	_, rest, _ := strings.Cut(head, ":")
	ref := strings.TrimSpace(rest)
	for _, candidate := range []string{gitDir, commonDir} {
		refFile := pathutil.Join(candidate, ref)
		fi, err := pathutil.StatIfExists(refFile)
		if err != nil {
			return "", err
		}
		if fi != nil {
			sha, err := readGitText(refFile)
			if err != nil {
				return "", err
			}
			return "git:" + ref + ":" + strings.TrimSpace(sha), nil
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
	rel := strings.TrimSpace(text)
	if rel == "" {
		return "", nil
	}
	return pathutil.Resolve(pathutil.Join(gitDir, rel))
}

// Sha a packed-refs file records for a ref, empty when unreadable or absent
func readPackedRef(commonDir, ref string) string {
	text, err := readGitText(pathutil.Join(commonDir, "packed-refs"))
	if err != nil {
		return ""
	}
	for _, line := range textutil.Lines(text) {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && strings.TrimSpace(name) == ref {
			return strings.TrimSpace(sha)
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
	return charsetutil.DecodeReplace(data, "utf-8-sig")
}
