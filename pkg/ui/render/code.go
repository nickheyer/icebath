package render

import (
	"bytes"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Chroma style name that suits the theme
func CodeStyle(t theme.Theme) string {
	switch {
	case t.Plain:
		return "bw"
	case t.Name == "contrast":
		return "github-dark"
	case isLight(t):
		return "github"
	}
	return "catppuccin-mocha"
}

// Lexer for a language name or a file name, analysed from the text when neither is known
func lexerFor(lang, path, src string) chroma.Lexer {
	var l chroma.Lexer
	if lang != "" {
		l = lexers.Get(lang)
	}
	if l == nil && path != "" {
		l = lexers.Match(filepath.Base(path))
	}
	if l == nil && src != "" {
		l = lexers.Analyse(src)
	}
	if l == nil {
		l = lexers.Fallback
	}
	return chroma.Coalesce(l)
}

// Highlights source for the language or file name in the theme, one line per input line
func Code(src, lang, path string, t theme.Theme) string {
	src = strings.TrimRight(src, "\n")
	if src == "" {
		return ""
	}
	l := lexerFor(lang, path, src)
	formatter := formatters.Get("terminal16m")
	if t.Plain {
		formatter = formatters.Get("terminal16")
	}
	if formatter == nil {
		formatter = formatters.Fallback
	}
	style := styles.Get(CodeStyle(t))
	if style == nil {
		style = styles.Fallback
	}
	it, err := l.Tokenise(nil, src)
	if err != nil {
		return src
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, it); err != nil {
		return src
	}
	return strings.TrimRight(buf.String(), "\n")
}

// Language a lexer would call the file, empty when none matches
func Language(path string) string {
	l := lexers.Match(filepath.Base(path))
	if l == nil {
		return ""
	}
	return l.Config().Name
}
