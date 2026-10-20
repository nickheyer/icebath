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

	"github.com/nickheyer/icebath/pkg/internal/glob"
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
"head_limit":{"type":"integer","minimum":1,"description":"Stop after this many output lines or entries"},
"multiline":{"type":"boolean","description":"Let the pattern span lines, matching the whole file at once"}},
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

type hit struct {
	line int
	text string
	ctx  bool
}

type fileHits struct {
	found
	hits  []hit
	count int
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
		expr = "(?s)" + expr
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
	target, err := g.resolve(in.Path)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	st, err := os.Stat(target)
	if err != nil {
		return tool.Errorf("%s does not exist", target), nil
	}
	var files []fileHits
	skipped := 0
	visit := func(p string, info fs.FileInfo) {
		if in.Glob != "" && !matchGlob(in.Glob, target, p) {
			return
		}
		fh, ok, err := g.scan(p, re, in, mode)
		if err != nil {
			skipped++
			return
		}
		if ok {
			fh.found = found{path: p, mod: info.ModTime()}
			files = append(files, fh)
		}
	}
	if st.IsDir() {
		err = g.walk(target, func(p string, d fs.DirEntry) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			info, err := d.Info()
			if err != nil {
				skipped++
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
	return tool.Text(g.render(files, mode, in, skipped)), nil
}

// Matches a bare name pattern against the base name and a path pattern against the relative path
func matchGlob(pattern, root, p string) bool {
	if !strings.Contains(pattern, "/") {
		return glob.Match(pattern, filepath.Base(p))
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return glob.Match(pattern, filepath.ToSlash(rel))
}

// Searches one file, returning its hits and whether it matched at all
func (g grepper) scan(p string, re *regexp.Regexp, in grepArgs, mode string) (fileHits, bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return fileHits{}, false, err
	}
	defer f.Close()
	head, err := peek(f)
	if err != nil {
		return fileHits{}, false, err
	}
	if isBinary(head) {
		return fileHits{}, false, nil
	}
	if in.Multiline {
		return g.scanWhole(f, re, in, mode)
	}
	var fh fileHits
	var before []hit
	pending := 0
	n := 0
	err = lines(f, func(line string) bool {
		n++
		if re.MatchString(line) {
			fh.count++
			if mode != "content" {
				return mode == "count"
			}
			fh.hits = append(fh.hits, before...)
			before = before[:0]
			fh.hits = append(fh.hits, hit{line: n, text: line})
			pending = in.After
			return true
		}
		if mode != "content" {
			return true
		}
		if pending > 0 {
			fh.hits = append(fh.hits, hit{line: n, text: line, ctx: true})
			pending--
			return true
		}
		if in.Before > 0 {
			before = append(before, hit{line: n, text: line, ctx: true})
			if len(before) > in.Before {
				before = before[1:]
			}
		}
		return true
	})
	if err != nil {
		return fileHits{}, false, err
	}
	return fh, fh.count > 0, nil
}

// Matches the pattern across the whole file and reports the line where each match starts
func (g grepper) scanWhole(f *os.File, re *regexp.Regexp, in grepArgs, mode string) (fileHits, bool, error) {
	st, err := f.Stat()
	if err != nil {
		return fileHits{}, false, err
	}
	if st.Size() > int64(g.maxFileBytes()) {
		return fileHits{}, false, fmt.Errorf("%s exceeds the multiline size limit", f.Name())
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		return fileHits{}, false, err
	}
	content := string(data)
	locs := re.FindAllStringIndex(content, -1)
	fh := fileHits{count: len(locs)}
	if mode == "content" {
		ls := strings.Split(content, "\n")
		for _, loc := range locs {
			start := strings.Count(content[:loc[0]], "\n")
			end := start + strings.Count(content[loc[0]:loc[1]], "\n")
			for i := max(start-in.Before, 0); i <= min(end+in.After, len(ls)-1); i++ {
				fh.hits = append(fh.hits, hit{line: i + 1, text: ls[i], ctx: i < start || i > end})
			}
		}
	}
	return fh, len(locs) > 0, nil
}

// Formats the hits in the requested mode, applying head_limit and the output byte budget
func (g grepper) render(files []fileHits, mode string, in grepArgs, skipped int) string {
	if len(files) == 0 {
		return g.footer("No matches found", skipped)
	}
	var out []string
	total := 0
	switch mode {
	case "files_with_matches":
		fs := make([]found, len(files))
		for i, fh := range files {
			fs[i] = fh.found
		}
		byNewest(fs)
		for _, f := range fs {
			out = append(out, f.path)
		}
		total = len(out)
	case "count":
		for _, fh := range files {
			out = append(out, fmt.Sprintf("%s:%d", fh.path, fh.count))
		}
		total = len(out)
	case "content":
		numbers := in.Numbers == nil || *in.Numbers
		for _, fh := range files {
			last := 0
			for _, h := range fh.hits {
				if last != 0 && h.line > last+1 {
					out = append(out, separator)
				}
				last = h.line
				out = append(out, g.formatHit(fh.path, h, numbers))
				total++
			}
		}
	}
	limit := g.maxResults()
	if in.HeadLimit > 0 {
		limit = in.HeadLimit
	}
	cut := ""
	if kept, shown := headLimit(out, limit); shown < total {
		out = kept
		cut = fmt.Sprintf("[showing %d of %d, raise head_limit or narrow the search]", shown, total)
	}
	text := strings.Join(out, "\n")
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
	return g.footer(text, skipped)
}

const separator = "--"

// Keeps the first limit entries, separators not counted, and reports how many entries remain
func headLimit(out []string, limit int) ([]string, int) {
	shown := 0
	for i, l := range out {
		if l == separator {
			continue
		}
		if shown == limit {
			kept := out[:i]
			for len(kept) > 0 && kept[len(kept)-1] == separator {
				kept = kept[:len(kept)-1]
			}
			return kept, shown
		}
		shown++
	}
	return out, shown
}

func (g grepper) formatHit(path string, h hit, numbers bool) string {
	sep := ":"
	if h.ctx {
		sep = "-"
	}
	if numbers {
		return fmt.Sprintf("%s%s%d%s%s", path, sep, h.line, sep, g.clip(h.text))
	}
	return path + sep + g.clip(h.text)
}

func (g grepper) footer(text string, skipped int) string {
	if skipped > 0 {
		return fmt.Sprintf("%s\n[%d files could not be read]", text, skipped)
	}
	return text
}
