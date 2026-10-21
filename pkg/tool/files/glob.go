package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/glob"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const globSchema = `{"type":"object","properties":{
"pattern":{"type":"string","description":"Glob such as *.go, src/**/*.ts or *.{js,ts}, double star spans directories and braces list alternatives"},
"path":{"type":"string","description":"Directory to search, the working directory when omitted"}},
"required":["pattern"],"additionalProperties":false}`

type globber struct{ Options }

// Tool that lists files and directories matching a pattern, newest first
func Glob(o Options) tool.Tool { return globber{o} }

func (g globber) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        GlobName,
		Description: fmt.Sprintf("Finds files and directories whose path matches a glob pattern and lists them newest first, up to %d entries, directories ending in a slash. Ignored directories: %s.", g.maxResults(), strings.Join(g.ignore(), ", ")),
		Parameters:  json.RawMessage(globSchema),
	}
}

func (globber) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: true, Idempotent: true}
}

func (g globber) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	if in.Pattern == "" {
		return tool.Error("pattern is required"), nil
	}
	if in.Path == "" {
		in.Path = "."
	}
	dir, err := g.Resolve(in.Path)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return tool.Errorf("%s is not a directory", dir), nil
	}
	patterns := expandBraces(in.Pattern)
	absolute := filepath.IsAbs(in.Pattern)
	var matches []Match
	err = g.walk(dir, true, func(p string, d fs.DirEntry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		subject := p
		if !absolute {
			subject, _ = filepath.Rel(dir, p)
		}
		if !matchAny(patterns, filepath.ToSlash(subject)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		matches = append(matches, Match{Path: p, Dir: d.IsDir(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	data := GlobData{Dir: dir, Pattern: in.Pattern, Total: len(matches)}
	if len(matches) == 0 {
		return tool.Text("No files found").With(data), nil
	}
	sortMatches(matches)
	shown := matches
	if len(shown) > g.maxResults() {
		shown = shown[:g.maxResults()]
	}
	data.Matches = shown
	var sb strings.Builder
	for _, m := range shown {
		sb.WriteString(m.Path)
		if m.Dir {
			sb.WriteString(string(filepath.Separator))
		}
		sb.WriteByte('\n')
	}
	if len(shown) < len(matches) {
		fmt.Fprintf(&sb, "[showing %d of %d matches, narrow the pattern]", len(shown), len(matches))
	}
	return tool.Text(strings.TrimSuffix(sb.String(), "\n")).With(data), nil
}

func matchAny(patterns []string, subject string) bool {
	for _, p := range patterns {
		if glob.Match(p, subject) {
			return true
		}
	}
	return false
}

// Orders newest first, then by path for equal times
func sortMatches(ms []Match) {
	fs := make([]found, len(ms))
	byPath := make(map[string]Match, len(ms))
	for i, m := range ms {
		fs[i] = found{path: m.Path, mod: m.ModTime}
		byPath[m.Path] = m
	}
	byNewest(fs)
	for i, f := range fs {
		ms[i] = byPath[f.path]
	}
}

// Expands every {a,b} group into the patterns it stands for, leaving unbalanced braces literal
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	depth := 0
	closeAt := -1
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeAt = i
			}
		}
		if closeAt >= 0 {
			break
		}
	}
	if closeAt < 0 {
		return []string{pattern}
	}
	prefix, body, suffix := pattern[:open], pattern[open+1:closeAt], pattern[closeAt+1:]
	var out []string
	for _, alt := range splitTopLevel(body) {
		for _, rest := range expandBraces(alt + suffix) {
			out = append(out, prefix+rest)
		}
	}
	return out
}

// Splits on commas that are not inside a nested brace group
func splitTopLevel(body string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, body[start:])
}
