package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const grepSchema = `{"type":"object","properties":{
"pattern":{"type":"string","description":"Regular expression in RE2 syntax"},
"path":{"type":"string","description":"File or directory to search, the working directory when omitted"},
"glob":{"type":"string","description":"Only search files matching this glob, a bare name pattern like *.go matches at any depth"},
"output_mode":{"type":"string","enum":["files_with_matches","content","count"],"description":"files_with_matches lists paths, content shows matching lines, count shows matches per file"},
"-i":{"type":"boolean","description":"Match case insensitively"},
"-n":{"type":"boolean","description":"Prefix content lines with line numbers, on by default"},
"-A":{"type":"integer","minimum":0,"description":"Lines of context after each match in content mode"},
"-B":{"type":"integer","minimum":0,"description":"Lines of context before each match in content mode"},
"-C":{"type":"integer","minimum":0,"description":"Lines of context around each match in content mode"},
"head_limit":{"type":"integer","minimum":1,"description":"Stop after this many matches or entries, context lines not counted"},
"multiline":{"type":"boolean","description":"Let the pattern span lines, matching the whole file at once with anchors still matching at line ends"}},
"required":["pattern"],"additionalProperties":false}`

type grepper struct{ Options }

// Tool that searches file contents with a regular expression
func Grep(o Options) tool.Tool { return grepper{o} }

func (g grepper) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        GrepName,
		Description: fmt.Sprintf("Searches file contents with a regular expression. Lists matching files newest first by default, or shows matching lines with context, or counts matches per file. Binary files and the directories %s are skipped. Output is cut at %d bytes.", strings.Join(g.ignore(), ", "), g.maxOutput()),
		Parameters:  json.RawMessage(grepSchema),
	}
}

func (grepper) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: true, Idempotent: true}
}

type grepArgs struct {
	Pattern     string `json:"pattern"`
	Path        string `json:"path"`
	Glob        string `json:"glob"`
	OutputMode  string `json:"output_mode"`
	Insensitive bool   `json:"-i"`
	Numbers     *bool  `json:"-n"`
	After       int    `json:"-A"`
	Before      int    `json:"-B"`
	Around      int    `json:"-C"`
	HeadLimit   int    `json:"head_limit"`
	Multiline   bool   `json:"multiline"`
}

// One matching or context line
type Hit struct {
	Line    int
	Text    string
	Context bool
}

// Matches found in one file
type FileHits struct {
	Path    string
	ModTime time.Time
	Count   int
	Hits    []Hit
}

// Structured outcome of Grep
type GrepData struct {
	Pattern string
	Mode    string
	Files   []FileHits
	// Matches across every file in the requested mode
	Total int
	// Files skipped because they could not be read
	Skipped int
	// Files skipped because they were too large to match as a whole
	Oversize int
}

func (g grepper) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in grepArgs
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if in.Pattern == "" {
		return tool.Error("pattern is required"), nil
	}
	expr := in.Pattern
	if in.Insensitive {
		expr = "(?i)" + expr
	}
	if in.Multiline {
		expr = "(?ms)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return tool.Errorf("invalid pattern: %v", err), nil
	}
	if in.Around > 0 {
		in.After, in.Before = max(in.After, in.Around), max(in.Before, in.Around)
	}
	mode := in.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	if mode != "files_with_matches" && mode != "content" && mode != "count" {
		return tool.Errorf("unknown output_mode %q", mode), nil
	}
	if in.Path == "" {
		in.Path = "."
	}
	target, err := g.Resolve(in.Path)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	st, err := os.Stat(target)
	if err != nil {
		return tool.Errorf("%s does not exist", target), nil
	}
	data := GrepData{Pattern: in.Pattern, Mode: mode}
	visit := func(p string, info fs.FileInfo) {
		if in.Glob != "" && !matchGlob(in.Glob, target, p) {
			return
		}
		fh, ok, err := g.scan(p, re, in, mode)
		var tooBig oversize
		switch {
		case errorsAs(err, &tooBig):
			data.Oversize++
			return
		case err != nil:
			data.Skipped++
			return
		}
		if ok {
			fh.Path, fh.ModTime = p, info.ModTime()
			data.Files = append(data.Files, fh)
		}
	}
	if st.IsDir() {
		err = g.walk(target, false, func(p string, d fs.DirEntry) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			info, err := d.Info()
			if err != nil {
				data.Skipped++
				return nil
			}
			visit(p, info)
			return nil
		})
		if err != nil {
			return tool.Error(err.Error()), nil
		}
	} else {
		visit(target, st)
	}
	byNewestHits(data.Files)
	return tool.Text(g.render(&data, in)).With(data), nil
}

// Error for a file too large to match as a whole
type oversize struct{ path string }

func (o oversize) Error() string { return o.path + " exceeds the multiline size limit" }

func errorsAs(err error, target *oversize) bool {
	o, ok := err.(oversize)
	if ok {
		*target = o
	}
	return ok
}

// Matches a bare name pattern against the base name and a path pattern against the relative path
func matchGlob(pattern, root, p string) bool {
	patterns := expandBraces(pattern)
	if !strings.Contains(pattern, "/") {
		return matchAny(patterns, filepath.Base(p))
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		rel = filepath.Base(p)
	}
	return matchAny(patterns, filepath.ToSlash(rel))
}

// Searches one file, returning its hits and whether it matched at all
func (g grepper) scan(p string, re *regexp.Regexp, in grepArgs, mode string) (FileHits, bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return FileHits{}, false, err
	}
	defer f.Close()
	head, err := peek(f)
	if err != nil {
		return FileHits{}, false, err
	}
	if isBinary(head) {
		return FileHits{}, false, nil
	}
	if in.Multiline {
		return g.scanWhole(f, re, in, mode)
	}
	var fh FileHits
	var before []Hit
	pending := 0
	n := 0
	err = lines(f, func(line string) bool {
		n++
		if re.MatchString(line) {
			fh.Count++
			if mode != "content" {
				return mode == "count"
			}
			fh.Hits = append(fh.Hits, before...)
			before = before[:0]
			fh.Hits = append(fh.Hits, Hit{Line: n, Text: line})
			pending = in.After
			return true
		}
		if mode != "content" {
			return true
		}
		if pending > 0 {
			fh.Hits = append(fh.Hits, Hit{Line: n, Text: line, Context: true})
			pending--
			return true
		}
		if in.Before > 0 {
			before = append(before, Hit{Line: n, Text: line, Context: true})
			if len(before) > in.Before {
				before = before[1:]
			}
		}
		return true
	})
	if err != nil {
		return FileHits{}, false, err
	}
	return fh, fh.Count > 0, nil
}

// Matches the pattern across the whole file and reports the lines each match spans
func (g grepper) scanWhole(f *os.File, re *regexp.Regexp, in grepArgs, mode string) (FileHits, bool, error) {
	st, err := f.Stat()
	if err != nil {
		return FileHits{}, false, err
	}
	if st.Size() > int64(g.maxFileBytes()) {
		return FileHits{}, false, oversize{path: f.Name()}
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return FileHits{}, false, err
	}
	content := string(data)
	locs := re.FindAllStringIndex(content, -1)
	fh := FileHits{Count: len(locs)}
	if mode == "content" {
		ls := strings.Split(content, "\n")
		seen := map[int]bool{}
		for _, loc := range locs {
			start := strings.Count(content[:loc[0]], "\n")
			end := start + strings.Count(content[loc[0]:loc[1]], "\n")
			for i := max(start-in.Before, 0); i <= min(end+in.After, len(ls)-1); i++ {
				if seen[i] {
					continue
				}
				seen[i] = true
				fh.Hits = append(fh.Hits, Hit{Line: i + 1, Text: ls[i], Context: i < start || i > end})
			}
		}
	}
	return fh, len(locs) > 0, nil
}

// Orders newest first, then by path for equal times
func byNewestHits(files []FileHits) {
	fs := make([]found, len(files))
	byPath := make(map[string]FileHits, len(files))
	for i, fh := range files {
		fs[i] = found{path: fh.Path, mod: fh.ModTime}
		byPath[fh.Path] = fh
	}
	byNewest(fs)
	for i, f := range fs {
		files[i] = byPath[f.path]
	}
}

type outLine struct {
	text  string
	sep   bool
	match bool
}

const separator = "--"

// Formats the hits in the requested mode, applying head_limit to matches and the output byte budget
func (g grepper) render(data *GrepData, in grepArgs) string {
	if len(data.Files) == 0 {
		return g.footer("No matches found", data)
	}
	var out []outLine
	switch data.Mode {
	case "files_with_matches":
		for _, fh := range data.Files {
			out = append(out, outLine{text: fh.Path, match: true})
		}
	case "count":
		for _, fh := range data.Files {
			out = append(out, outLine{text: fmt.Sprintf("%s:%d", fh.Path, fh.Count), match: true})
		}
	case "content":
		numbers := in.Numbers == nil || *in.Numbers
		for _, fh := range data.Files {
			last := 0
			for _, h := range fh.Hits {
				if last != 0 && h.Line > last+1 {
					out = append(out, outLine{text: separator, sep: true})
				}
				last = h.Line
				out = append(out, outLine{text: g.formatHit(fh.Path, h, numbers), match: !h.Context})
			}
		}
	}
	total := 0
	for _, l := range out {
		if l.match {
			total++
		}
	}
	data.Total = total
	limit := g.maxResults()
	if in.HeadLimit > 0 {
		limit = in.HeadLimit
	}
	cut := ""
	kept, shown := headLimit(out, limit)
	if shown < total {
		out = kept
		cut = fmt.Sprintf("[showing %d of %d, raise head_limit or narrow the search]", shown, total)
	}
	texts := make([]string, len(out))
	for i, l := range out {
		texts[i] = l.text
	}
	text := strings.Join(texts, "\n")
	if len(text) > g.maxOutput() {
		text = text[:g.maxOutput()]
		if i := strings.LastIndexByte(text, '\n'); i > 0 {
			text = text[:i]
		}
		cut = fmt.Sprintf("[output cut at %d bytes, narrow the search]", g.maxOutput())
	}
	if cut != "" {
		text += "\n" + cut
	}
	return g.footer(text, data)
}

// Keeps lines up to the limit of matches, context included, and reports how many matches were shown
func headLimit(out []outLine, limit int) ([]outLine, int) {
	shown := 0
	for i, l := range out {
		if !l.match {
			continue
		}
		if shown == limit {
			kept := out[:i]
			for len(kept) > 0 && (kept[len(kept)-1].sep || !kept[len(kept)-1].match && trailingContext(kept)) {
				kept = kept[:len(kept)-1]
			}
			return kept, shown
		}
		shown++
	}
	return out, shown
}

// Reports whether the kept lines end in context that follows no match within the same group
func trailingContext(kept []outLine) bool {
	for i := len(kept) - 1; i >= 0; i-- {
		if kept[i].sep {
			return true
		}
		if kept[i].match {
			return false
		}
	}
	return true
}

func (g grepper) formatHit(path string, h Hit, numbers bool) string {
	sep := ":"
	if h.Context {
		sep = "-"
	}
	if numbers {
		return fmt.Sprintf("%s%s%d%s%s", path, sep, h.Line, sep, g.clip(h.Text))
	}
	return path + sep + g.clip(h.Text)
}

func (g grepper) footer(text string, data *GrepData) string {
	if data.Skipped > 0 {
		text = fmt.Sprintf("%s\n[%d files could not be read]", text, data.Skipped)
	}
	if data.Oversize > 0 {
		text = fmt.Sprintf("%s\n[%d files over the %d byte multiline limit were skipped]", text, data.Oversize, g.maxFileBytes())
	}
	return text
}
