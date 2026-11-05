package tools

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Where a command lookup searches, a nil Path meaning the ambient PATH and an empty one searching nothing
type commandLookupContext struct {
	Path    *string
	PathExt *string
}

// Search path handed to which, nil for the ambient PATH whether the caller left it unset or passed none
func (c *commandLookupContext) effectivePath() *string {
	return c.Path
}

// State of one checked fact, consumers branch on it and never parse the detail
type checkState string

const (
	checkNotChecked  checkState = "not_checked"
	checkPresent     checkState = "present"
	checkAbsent      checkState = "absent"
	checkUnavailable checkState = "unavailable"
	checkError       checkState = "error"
)

// One checked fact with its state, the value when present and a human detail
type observation[T any] struct {
	State  checkState
	Value  *T
	Detail string
}

// Observation of a fact nobody probed
func observationNotChecked[T any]() observation[T] {
	return observation[T]{State: checkNotChecked}
}

// One place a lookup probed, recorded whether or not the command was there
type commandCandidate struct {
	Value   string
	Source  string
	Present bool
}

// Outcome of a lookup, every candidate in probe order with the kind of the first present one
type commandResolution struct {
	Kind       string
	Candidates []commandCandidate
}

// Argv of the first present candidate, empty when none was found
func (r commandResolution) Command() []string {
	for _, c := range r.Candidates {
		if c.Present {
			return []string{c.Value}
		}
	}
	return nil
}

// Label of the first present candidate, none when nothing was found
func (r commandResolution) Source() string {
	for _, c := range r.Candidates {
		if c.Present {
			return c.Source
		}
	}
	return "none"
}

// Candidates that were found, in probe order
func (r commandResolution) PresentCandidates() []commandCandidate {
	var out []commandCandidate
	for _, c := range r.Candidates {
		if c.Present {
			out = append(out, c)
		}
	}
	return out
}

// Reports whether any candidate was found
func (r commandResolution) Found() bool { return r.Kind != "missing" }

// Finds a command on PATH and then in the known directories, recording every candidate in probe order
func locateCommand(name string, ctx *commandLookupContext, knownDirs []string) commandResolution {
	if ctx == nil {
		ctx = &commandLookupContext{}
	}
	name = strings.TrimFunc(name, pyIsSpace)
	if strings.Contains(name, string(os.PathSeparator)) || (runtime.GOOS == "windows" && strings.Contains(name, "/")) {
		expanded := osPathExpandVars(osPathExpandUser(name))
		present := filepath.IsAbs(expanded) && commandIsExecutableFile(expanded)
		kind := "missing"
		if present {
			kind = "explicit_path"
		}
		return commandResolution{Kind: kind, Candidates: []commandCandidate{{Value: expanded, Source: "explicit", Present: present}}}
	}
	var candidates []commandCandidate
	hit, found := commandWhich(name, ctx)
	if !found {
		hit = name
	}
	candidates = append(candidates, commandCandidate{Value: hit, Source: "PATH", Present: found})
	for _, d := range knownDirs {
		base := osPathJoin(osPathExpandVars(osPathExpandUser(d)), name)
		candidate := commandCandidate{Value: base, Source: "known_dir:" + d}
		for _, v := range knownDirVariants(base) {
			if commandIsExecutableFile(v) {
				candidate.Value, candidate.Present = v, true
				break
			}
		}
		candidates = append(candidates, candidate)
	}
	kind := "missing"
	for _, c := range candidates {
		if c.Present {
			kind = "known_path"
			if c.Source == "PATH" {
				kind = "path_executable"
			}
			break
		}
	}
	return commandResolution{Kind: kind, Candidates: candidates}
}

// PATH hit for a bare name honouring an explicit search path and, on Windows, an explicit PATHEXT
func commandWhich(name string, ctx *commandLookupContext) (string, bool) {
	if ctx.Path != nil && *ctx.Path == "" {
		return "", false
	}
	if ctx.PathExt != nil && runtime.GOOS == "windows" {
		exts := []string{""}
		for _, e := range strings.Split(*ctx.PathExt, string(os.PathListSeparator)) {
			if e != "" {
				exts = append(exts, e)
			}
		}
		for _, ext := range exts {
			if hit, ok := shutilWhich(name+ext, ctx.effectivePath()); ok {
				return hit, true
			}
		}
		return "", false
	}
	return shutilWhich(name, ctx.effectivePath())
}

// Reports a regular file this process may execute
func commandIsExecutableFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && osAccessExecutable(p)
}

// A Windows bare name in a known directory under each PATHEXT suffix, the plain name alone elsewhere
func knownDirVariants(base string) []string {
	if runtime.GOOS != "windows" {
		return []string{base}
	}
	source, ok := os.LookupEnv("PATHEXT")
	if !ok {
		source = ".EXE;.CMD;.BAT"
	}
	variants := []string{base}
	for _, e := range strings.Split(source, string(os.PathListSeparator)) {
		if e != "" {
			variants = append(variants, base+e)
		}
	}
	return variants
}

// Search path CPython uses when PATH is unset, the confstr value where libc has one
func shutilDefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		return `.;C:\bin`
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return "/bin:/usr/bin"
}

// Executable a command names on a search path the way shutil.which finds it, false when absent
func shutilWhich(cmd string, path *string) (string, bool) {
	dirname, base := osPathSplit(cmd)
	var dirs []string
	if dirname != "" {
		dirs = []string{dirname}
	} else {
		var search string
		if path != nil {
			search = *path
		} else if v, ok := os.LookupEnv("PATH"); ok {
			search = v
		} else {
			search = shutilDefaultPath()
		}
		if search == "" {
			return "", false
		}
		dirs = strings.Split(search, string(os.PathListSeparator))
		if runtime.GOOS == "windows" {
			if _, set := os.LookupEnv("NoDefaultCurrentDirectoryInExePath"); !set || strings.Contains(base, `\`) {
				dirs = append([]string{"."}, dirs...)
			}
		}
	}
	files := []string{base}
	if runtime.GOOS == "windows" {
		source := os.Getenv("PATHEXT")
		if source == "" {
			source = ".COM;.EXE;.BAT;.CMD;.VBS;.JS;.WS;.MSC"
		}
		var exts []string
		for _, e := range strings.Split(source, string(os.PathListSeparator)) {
			if e != "" {
				exts = append(exts, strings.TrimRight(e, "."))
			}
		}
		files = nil
		upper := strings.ToUpper(base)
		for _, e := range exts {
			if strings.HasSuffix(upper, strings.ToUpper(e)) {
				files = []string{base}
				break
			}
		}
		for _, e := range exts {
			files = append(files, base+e)
		}
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		norm := osPathNormcase(dir)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		for _, f := range files {
			name := osPathJoin(dir, f)
			if st, err := os.Stat(name); err == nil && !st.IsDir() && osAccessExecutable(name) {
				return name, true
			}
		}
	}
	return "", false
}

// Reports what os.access with X_OK answers, existence alone on Windows
func osAccessExecutable(p string) bool {
	if runtime.GOOS == "windows" {
		_, err := os.Lstat(p)
		return err == nil
	}
	return dirSearchable(p)
}

// Directory and final component of a path split the way os.path.split does
func osPathSplit(p string) (string, string) {
	if runtime.GOOS != "windows" {
		i := strings.LastIndex(p, "/") + 1
		head, tail := p[:i], p[i:]
		if head != "" && strings.Trim(head, "/") != "" {
			head = strings.TrimRight(head, "/")
		}
		return head, tail
	}
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	i := strings.LastIndexAny(rest, `\/`) + 1
	head, tail := rest[:i], rest[i:]
	if trimmed := strings.TrimRight(head, `\/`); trimmed != "" {
		head = trimmed
	}
	return vol + head, tail
}

// Two path parts joined the way os.path.join does on this platform
func osPathJoin(dir, name string) string {
	if runtime.GOOS == "windows" {
		return ntJoin(dir, name)
	}
	if strings.HasPrefix(name, "/") {
		return name
	}
	if dir == "" || strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// Path case folded and slashes turned to backslashes on Windows, unchanged elsewhere
func osPathNormcase(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	return strings.ToLower(strings.ReplaceAll(p, "/", `\`))
}

// Leading tilde expanded the way os.path.expanduser does on this platform, unchanged when the home is unknown
func osPathExpandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	if runtime.GOOS == "windows" {
		i := 1
		for i < len(p) && p[i] != '\\' && p[i] != '/' {
			i++
		}
		userhome, ok := os.LookupEnv("USERPROFILE")
		if !ok {
			homepath, ok := os.LookupEnv("HOMEPATH")
			if !ok {
				return p
			}
			userhome = ntJoin(os.Getenv("HOMEDRIVE"), homepath)
		}
		if i != 1 {
			target := p[1:i]
			if current := os.Getenv("USERNAME"); target != current {
				if current != ntBasename(userhome) {
					return p
				}
				userhome = ntJoin(ntDirname(userhome), target)
			}
		}
		return userhome + p[i:]
	}
	i := strings.Index(p[1:], "/") + 1
	if i == 0 {
		i = len(p)
	}
	var userhome string
	if i == 1 {
		if h, ok := os.LookupEnv("HOME"); ok {
			userhome = h
		} else {
			u, err := user.LookupId(strconv.Itoa(os.Getuid()))
			if err != nil {
				return p
			}
			userhome = u.HomeDir
		}
	} else {
		u, err := user.Lookup(p[1:i])
		if err != nil {
			return p
		}
		userhome = u.HomeDir
	}
	if out := strings.TrimRight(userhome, "/") + p[i:]; out != "" {
		return out
	}
	return "/"
}

var (
	posixVarRe = regexp.MustCompile(`\$(\w+|\{[^}]*\}?)`)
	ntVarRe    = regexp.MustCompile(`'[^']*'?|%(%|[^%]*%?)|\$(\$|[-\w]+|\{[^}]*\}?)`)
)

// Environment references expanded the way os.path.expandvars does on this platform, unknown ones left unchanged
func osPathExpandVars(p string) string {
	if runtime.GOOS != "windows" {
		if !strings.Contains(p, "$") {
			return p
		}
		return replaceSubmatchFunc(posixVarRe, p, func(m []string) string {
			name := m[1]
			if strings.HasPrefix(name, "{") {
				if !strings.HasSuffix(name, "}") || len(name) < 2 {
					return m[0]
				}
				name = name[1 : len(name)-1]
			}
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			return m[0]
		})
	}
	if !strings.Contains(p, "$") && !strings.Contains(p, "%") {
		return p
	}
	return replaceSubmatchFunc(ntVarRe, p, func(m []string) string {
		var name string
		switch {
		case m[2] != "" || strings.HasPrefix(m[0], "$"):
			name = m[2]
			if name == "$" {
				return name
			}
			if strings.HasPrefix(name, "{") {
				if !strings.HasSuffix(name, "}") || len(name) < 2 {
					return m[0]
				}
				name = name[1 : len(name)-1]
			}
		case strings.HasPrefix(m[0], "%"):
			name = m[1]
			if name == "%" {
				return name
			}
			if !strings.HasSuffix(name, "%") {
				return m[0]
			}
			name = name[:len(name)-1]
		default:
			return m[0]
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m[0]
	})
}

// Every match of the pattern replaced by the callback result over its submatches
func replaceSubmatchFunc(re *regexp.Regexp, s string, fn func([]string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		groups := make([]string, len(loc)/2)
		for g := range groups {
			if loc[2*g] >= 0 {
				groups[g] = s[loc[2*g]:loc[2*g+1]]
			}
		}
		b.WriteString(fn(groups))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Windows path joined with one more component the way ntpath.join does
func ntJoin(a, b string) string {
	drive, root, rest := ntSplitRoot(a)
	bDrive, bRoot, bRest := ntSplitRoot(b)
	switch {
	case bRoot != "":
		if bDrive != "" || drive == "" {
			drive = bDrive
		}
		root, rest = bRoot, bRest
	case bDrive != "" && bDrive != drive && !strings.EqualFold(bDrive, drive):
		drive, root, rest = bDrive, bRoot, bRest
	default:
		if bDrive != "" {
			drive = bDrive
		}
		if rest != "" && !strings.ContainsRune(`\/`, rune(rest[len(rest)-1])) {
			rest += `\`
		}
		rest += bRest
	}
	if rest != "" && root == "" && drive != "" && !strings.ContainsRune(`:\/`, rune(drive[len(drive)-1])) {
		return drive + `\` + rest
	}
	return drive + root + rest
}

// Final component of a Windows path the way ntpath.basename splits it
func ntBasename(p string) string {
	_, tail := osPathSplit(p)
	return tail
}

// Directory part of a Windows path the way ntpath.dirname splits it
func ntDirname(p string) string {
	head, _ := osPathSplit(p)
	return head
}
