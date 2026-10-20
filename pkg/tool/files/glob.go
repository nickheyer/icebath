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
"pattern":{"type":"string","description":"Glob such as *.go or src/**/*.ts, double star spans directories"},
"path":{"type":"string","description":"Directory to search, the working directory when omitted"}},
"required":["pattern"],"additionalProperties":false}`

type globber struct{ Options }

// Tool that lists files matching a pattern, newest first
func Glob(o Options) tool.Tool { return globber{o} }

func (g globber) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        GlobName,
		Description: fmt.Sprintf("Finds files whose path matches a glob pattern and lists them newest first, up to %d entries. Ignored directories: %s.", g.maxResults(), strings.Join(g.ignore(), ", ")),
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
	dir, err := g.resolve(in.Path)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return tool.Errorf("%s is not a directory", dir), nil
	}
	var matches []found
	err = g.walk(dir, func(p string, d fs.DirEntry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		subject := p
		if !filepath.IsAbs(in.Pattern) {
			subject, _ = filepath.Rel(dir, p)
		}
		if !glob.Match(in.Pattern, filepath.ToSlash(subject)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		matches = append(matches, found{path: p, mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	if len(matches) == 0 {
		return tool.Text("No files found"), nil
	}
	byNewest(matches)
	shown := matches
	if len(shown) > g.maxResults() {
		shown = shown[:g.maxResults()]
	}
	var sb strings.Builder
	for _, m := range shown {
		sb.WriteString(m.path)
		sb.WriteByte('\n')
	}
	if len(shown) < len(matches) {
		fmt.Fprintf(&sb, "[showing %d of %d matches, narrow the pattern]", len(shown), len(matches))
	}
	return tool.Text(strings.TrimSuffix(sb.String(), "\n")), nil
}
