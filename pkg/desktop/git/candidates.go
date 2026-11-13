package git

import (
	"sort"
	"strings"
	"unicode"
)

// Filesystem slice the Windows git and ssh candidate probes read
type CandidateFS interface {
	Exists(path string) bool
	ReadDir(dir string) ([]string, error)
}

// Windows environment the git candidates are rooted in
type WindowsGitEnv struct {
	LocalAppData    string
	ProgramFiles    string
	ProgramFilesX86 string
}

// WinJoin joins Windows path segments with backslashes the way path.win32.join normalizes them
func WinJoin(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return "."
	}
	joined := strings.ReplaceAll(strings.Join(kept, `\`), "/", `\`)
	prefix := ""
	if strings.HasPrefix(joined, `\\`) {
		prefix, joined = `\\`, joined[2:]
	}
	var segs []string
	for _, s := range strings.Split(joined, `\`) {
		switch s {
		case "", ".":
		case "..":
			if len(segs) > 0 && segs[len(segs)-1] != ".." && !strings.HasSuffix(segs[len(segs)-1], ":") {
				segs = segs[:len(segs)-1]
			} else if len(segs) == 0 || segs[len(segs)-1] == ".." {
				segs = append(segs, s)
			}
		default:
			segs = append(segs, s)
		}
	}
	out := prefix + strings.Join(segs, `\`)
	if strings.HasPrefix(strings.ReplaceAll(strings.Join(kept, `\`), "/", `\`), `\`) && prefix == "" {
		out = `\` + out
	}
	if len(segs) == 1 && strings.HasSuffix(segs[0], ":") {
		out += `\`
	}
	return out
}

// WinDir is the parent of a Windows path the way path.win32.dirname answers
func WinDir(p string) string {
	p = strings.TrimRight(p, `\/`)
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return "."
	}
	if i == 2 && len(p) > 1 && p[1] == ':' {
		return p[:3]
	}
	return p[:i]
}

// Orders install directory names newest first with numeric runs compared by value
func sortDescendingNumeric(entries []string) {
	sort.SliceStable(entries, func(i, j int) bool { return naturalCompare(entries[i], entries[j]) > 0 })
}

// Compares like localeCompare with numeric collation, digit runs by value and letters case insensitively
func naturalCompare(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if unicode.IsDigit(ra[i]) && unicode.IsDigit(rb[j]) {
			si := i
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return sign(len(na) - len(nb))
			}
			if na != nb {
				return strings.Compare(na, nb)
			}
			continue
		}
		ca, cb := unicode.ToLower(ra[i]), unicode.ToLower(rb[j])
		if ca != cb {
			return sign(int(ca) - int(cb))
		}
		i++
		j++
	}
	if d := (len(ra) - i) - (len(rb) - j); d != 0 {
		return sign(d)
	}
	return strings.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// UgitGitBinaries lists the git.exe of every UGit app version present, newest first
func UgitGitBinaries(localAppData string, fs CandidateFS) []string {
	root := WinJoin(localAppData, "UGit")
	entries, err := fs.ReadDir(root)
	if err != nil {
		return nil
	}
	var apps []string
	for _, e := range entries {
		if strings.HasPrefix(e, "app-") {
			apps = append(apps, e)
		}
	}
	sortDescendingNumeric(apps)
	var out []string
	for _, e := range apps {
		if p := WinJoin(root, e, "resources", "app", "git", "cmd", "git.exe"); fs.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// ManagedGitBinaries lists the git.exe of every Icebath managed git under the tools directory, newest first
func ManagedGitBinaries(localAppData string, fs CandidateFS) []string {
	root := WinJoin(localAppData, "icebath", "tools")
	entries, err := fs.ReadDir(root)
	if err != nil {
		return nil
	}
	var managed []string
	for _, e := range entries {
		if strings.HasPrefix(e, "git-") {
			managed = append(managed, e)
		}
	}
	sortDescendingNumeric(managed)
	var out []string
	for _, e := range managed {
		if p := WinJoin(root, e, "cmd", "git.exe"); fs.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// WindowsGitCandidates lists git.exe locations in resolution order, per user installs first
func WindowsGitCandidates(env WindowsGitEnv, fs CandidateFS) []string {
	var out []string
	if env.LocalAppData != "" {
		out = append(out, ManagedGitBinaries(env.LocalAppData, fs)...)
		out = append(out, WinJoin(env.LocalAppData, "icebath", "git", "cmd", "git.exe"))
		out = append(out, WinJoin(env.LocalAppData, "icebath", "git", "bin", "git.exe"))
		out = append(out, UgitGitBinaries(env.LocalAppData, fs)...)
	}
	out = append(out, WinJoin(env.ProgramFiles, "Git", "cmd", "git.exe"))
	out = append(out, WinJoin(env.ProgramFilesX86, "Git", "cmd", "git.exe"))
	if env.LocalAppData != "" {
		out = append(out, WinJoin(env.LocalAppData, "Programs", "Git", "cmd", "git.exe"))
	}
	return out
}
