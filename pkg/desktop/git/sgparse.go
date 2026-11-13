package git

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// One path of git status with its index and worktree codes
type FileStatus struct {
	Path       string
	Index      string
	WorkingDir string
}

// Part of simple-git status the review pane and coding rail read
type StatusResult struct {
	NotAdded   []string
	Conflicted []string
	Staged     []string
	Files      []FileStatus
	Ahead      int
	Behind     int
	Current    *string
	Tracking   *string
	Detached   bool
}

var statusStaged = setOf("A ", "AM", "D ", "M ", "MM")

var statusConflicts = setOf("AA", "AU", "DD", "DU", "UA", "UD", "UU")

var (
	aheadRe    = regexp.MustCompile(`ahead (\d+)`)
	behindRe   = regexp.MustCompile(`behind (\d+)`)
	trackingRe = regexp.MustCompile(`\.{3}(` + `[^\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*)`)
)

func isJSLineTerminator(r rune) bool { return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 }

// Shortest leading run ended by three dots, whitespace or the end, the branch name of a ## line
func statusCurrent(line string) *string {
	for i, r := range line {
		if isJSLineTerminator(r) {
			return nil
		}
		end := i + utf8.RuneLen(r)
		if end == len(line) || strings.HasPrefix(line[end:], "...") {
			s := line[:end]
			return &s
		}
		if next, _ := utf8.DecodeRuneInString(line[end:]); isJSSpace(next) {
			s := line[:end]
			return &s
		}
	}
	return nil
}

// Branch after an on word ended by three dots or the end, the unborn branch of a ## line
func statusOnEmptyBranch(line string) *string {
	for i, r := range line {
		if !isJSSpace(r) || !strings.HasPrefix(line[i+utf8.RuneLen(r):], "on") {
			continue
		}
		rest := line[i+utf8.RuneLen(r)+2:]
		sp, size := utf8.DecodeRuneInString(rest)
		if size == 0 || !isJSSpace(sp) {
			continue
		}
		name := rest[size:]
		for j, c := range name {
			if isJSSpace(c) {
				break
			}
			end := j + utf8.RuneLen(c)
			if end == len(name) || strings.HasPrefix(name[end:], "...") {
				s := name[:end]
				return &s
			}
		}
	}
	return nil
}

func applyBranchLine(result *StatusResult, line string) {
	if m := aheadRe.FindStringSubmatch(line); m != nil {
		result.Ahead, _ = strconv.Atoi(m[1])
	} else {
		result.Ahead = 0
	}
	if m := behindRe.FindStringSubmatch(line); m != nil {
		result.Behind, _ = strconv.Atoi(m[1])
	} else {
		result.Behind = 0
	}
	result.Current = statusCurrent(line)
	result.Tracking = nil
	if m := trackingRe.FindStringSubmatch(line); m != nil {
		t := m[1]
		result.Tracking = &t
	}
	if on := statusOnEmptyBranch(line); on != nil {
		result.Current = on
	}
	result.Detached = strings.Contains(line, "(no branch)")
}

// UTF-16 units from..to of s as JavaScript slice reads them, a split surrogate pair read as U+FFFD
func unitSlice(units []uint16, from, to int) string {
	from, to = min(from, len(units)), min(to, len(units))
	if from >= to {
		return ""
	}
	return string(utf16.Decode(units[from:to]))
}

// Rename target of a status path holding new and old names split by NUL, as simple-git reads it
func renameTarget(path string) string {
	if strings.ContainsFunc(path, isJSLineTerminator) {
		return path
	}
	for k := len(path) - 2; k >= 1; k-- {
		if path[k] == 0 {
			return path[:k]
		}
	}
	return path
}

func statusLine(result *StatusResult, lineStr string) {
	units := utf16.Encode([]rune(jsTrim(lineStr)))
	var index, workingDir, path string
	switch {
	case unitSlice(units, 2, 3) == " ":
		index, workingDir, path = unitSlice(units, 0, 1), unitSlice(units, 1, 2), unitSlice(units, 3, len(units))
	case unitSlice(units, 1, 2) == " ":
		index, workingDir, path = " ", unitSlice(units, 0, 1), unitSlice(units, 2, len(units))
	default:
		return
	}
	raw := index + workingDir
	switch {
	case raw == "##":
		applyBranchLine(result, path)
	case statusStaged[raw]:
		result.Staged = append(result.Staged, path)
	case raw == "??":
		result.NotAdded = append(result.NotAdded, path)
	case statusConflicts[raw]:
		result.Conflicted = append(result.Conflicted, path)
	}
	if raw != "##" && raw != "!!" {
		file := FileStatus{Path: path, Index: index, WorkingDir: workingDir}
		if index == "R" || workingDir == "R" {
			file.Path = renameTarget(path)
		}
		result.Files = append(result.Files, file)
	}
}

// Parses git status --porcelain -b -u --null output the way simple-git builds its StatusResult
func parseStatusSummary(text string) StatusResult {
	result := StatusResult{NotAdded: []string{}, Conflicted: []string{}, Staged: []string{}, Files: []FileStatus{}}
	lines := strings.Split(text, "\x00")
	for i := 0; i < len(lines); {
		line := jsTrim(lines[i])
		i++
		if line == "" {
			continue
		}
		if line[0] == 'R' {
			next := ""
			if i < len(lines) {
				next = lines[i]
			}
			i++
			line += "\x00" + next
		}
		statusLine(&result, line)
	}
	return result
}

// One file of a diff summary with its line counts
type DiffFile struct {
	File       string
	Insertions int
	Deletions  int
	Binary     bool
}

// Diff summary with its totals
type DiffResult struct {
	Insertions int
	Deletions  int
	Files      []DiffFile
}

type diffLineParser struct {
	re     *regexp.Regexp
	handle func(*DiffResult, []string)
}

func jsRe(src string) *regexp.Regexp {
	src = strings.ReplaceAll(src, `\s`, jsSpaceClass)
	src = strings.ReplaceAll(src, `(.`, `(`+jsDotClass)
	return regexp.MustCompile(src)
}

// parseInt of a decimal capture, zero when absent or not a number
func asNumber(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

var statDiffParsers = []diffLineParser{
	{jsRe(`^(.+)\s+\|\s+(\d+)(\s+[+\-]+)?$`), func(r *DiffResult, m []string) {
		r.Files = append(r.Files, DiffFile{File: jsTrim(m[1]), Insertions: strings.Count(m[3], "+"), Deletions: strings.Count(m[3], "-")})
	}},
	{jsRe(`^(.+) \|\s+Bin ([0-9.]+) -> ([0-9.]+) ([a-z]+)`), func(r *DiffResult, m []string) {
		r.Files = append(r.Files, DiffFile{File: jsTrim(m[1]), Binary: true})
	}},
	{jsRe(`^(.+)\s+\|\s+Bin\s*$`), func(r *DiffResult, m []string) {
		r.Files = append(r.Files, DiffFile{File: jsTrim(m[1]), Binary: true})
	}},
	{jsRe(`(\d+) files? changed\s*((?:, \d+ [^,]+){0,2})`), func(r *DiffResult, m []string) {
		r.Insertions, r.Deletions = 0, 0
		if ins := insertedRe.FindStringSubmatch(m[2]); ins != nil {
			r.Insertions = asNumber(ins[1])
		}
		if del := deletedRe.FindStringSubmatch(m[2]); del != nil {
			r.Deletions = asNumber(del[1])
		}
	}},
}

var (
	insertedRe = regexp.MustCompile(`(\d+) i`)
	deletedRe  = regexp.MustCompile(`(\d+) d`)
)

var numStatDiffParsers = []diffLineParser{
	{jsRe(`(\d+)\t(\d+)\t(.+)$`), func(r *DiffResult, m []string) {
		ins, del := asNumber(m[1]), asNumber(m[2])
		r.Insertions += ins
		r.Deletions += del
		r.Files = append(r.Files, DiffFile{File: m[3], Insertions: ins, Deletions: del})
	}},
	{jsRe(`-\t-\t(.+)$`), func(r *DiffResult, m []string) {
		r.Files = append(r.Files, DiffFile{File: m[1], Binary: true})
	}},
}

var nameOnlyDiffParsers = []diffLineParser{
	{jsRe(`(.+)$`), func(r *DiffResult, m []string) {
		r.Files = append(r.Files, DiffFile{File: m[1]})
	}},
}

var nameStatusDiffParsers = []diffLineParser{
	{jsRe(`([ACDMRTUXB])([0-9]{0,3})\t(.[^\t]*)(\t(.[^\t]*))?$`), func(r *DiffResult, m []string) {
		file := m[3]
		if m[4] != "" {
			file = m[5]
		}
		r.Files = append(r.Files, DiffFile{File: file})
	}},
}

var logFormatRe = regexp.MustCompile(`^--(stat|numstat|name-only|name-status)(=|$)`)

// Summary format a diff argv selects, empty when it names none
func logFormatFromArgs(args []string) string {
	for _, a := range args {
		if m := logFormatRe.FindStringSubmatch(a); m != nil {
			return "--" + m[1]
		}
	}
	return ""
}

// Parses diff summary output with the line parsers of the selected format
func parseDiffSummary(format, text string) DiffResult {
	parsers := statDiffParsers
	switch format {
	case "--numstat":
		parsers = numStatDiffParsers
	case "--name-only":
		parsers = nameOnlyDiffParsers
	case "--name-status":
		parsers = nameStatusDiffParsers
	}
	result := DiffResult{Files: []DiffFile{}}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		for _, p := range parsers {
			if m := p.re.FindStringSubmatch(line); m != nil {
				p.handle(&result, m)
				break
			}
		}
	}
	return result
}
