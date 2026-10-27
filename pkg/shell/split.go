package shell

import (
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// One simple command a line runs, as written
type Command struct {
	Text string
	// Runs commands the line does not spell out, through substitution, eval or source
	Hidden bool
}

// Builtins that run text as commands
var hiding = map[string]bool{"eval": true, "source": true, ".": true}

// Simple commands of line in the order written, those inside substitutions and compound bodies included
func Commands(line string) ([]Command, error) {
	prog, err := Parse(line)
	if err != nil {
		return nil, err
	}
	var out []Command
	syntax.Walk(prog, func(n syntax.Node) bool {
		st, ok := n.(*syntax.Stmt)
		if !ok || !simple(st.Cmd) {
			return true
		}
		out = append(out, Command{Text: span(line, st), Hidden: hides(st)})
		return true
	})
	return out, nil
}

// Reports whether the command runs as one unit rather than holding statements of its own
func simple(cm syntax.Command) bool {
	switch cm.(type) {
	case *syntax.CallExpr, *syntax.DeclClause, *syntax.LetClause, *syntax.ArithmCmd, *syntax.TestClause:
		return true
	}
	return false
}

// Source text of the statement from its first word or redirect to its last, heredoc bodies left out
func span(src string, st *syntax.Stmt) string {
	start, end := st.Cmd.Pos().Offset(), st.Cmd.End().Offset()
	for _, rd := range st.Redirs {
		start = min(start, rd.Pos().Offset())
		end = max(end, rd.Word.End().Offset())
	}
	return strings.TrimSpace(src[start:end])
}

// Reports whether the statement substitutes command output or names a builtin that runs text as commands
func hides(st *syntax.Stmt) bool {
	if call, ok := st.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 && hiding[call.Args[0].Lit()] {
		return true
	}
	hidden := false
	syntax.Walk(st, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			hidden = true
		}
		return !hidden
	})
	return hidden
}

// Words of a line as the shell splits them, quotes removed and expansions kept as written, nothing run
func Words(line string) ([]string, error) {
	var out []string
	for w, err := range syntax.NewParser(syntax.Variant(syntax.LangBash)).WordsSeq(strings.NewReader(line)) {
		if err != nil {
			return nil, err
		}
		text, err := unquote(line, w)
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

// Line that the shell splits back into exactly these words
func Join(words []string) (string, error) {
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		q, err := syntax.Quote(w, syntax.LangBash)
		if err != nil {
			return "", err
		}
		quoted = append(quoted, q)
	}
	return strings.Join(quoted, " "), nil
}

// Text of one word with its quoting removed, every expansion copied from the source untouched
func unquote(src string, w *syntax.Word) (string, error) {
	cfg := &expand.Config{Env: expand.ListEnviron()}
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(unescape(p.Value))
		case *syntax.SglQuoted:
			if !p.Dollar {
				sb.WriteString(p.Value)
				continue
			}
			text, err := expand.Literal(cfg, &syntax.Word{Parts: []syntax.WordPart{p}})
			if err != nil {
				return "", err
			}
			sb.WriteString(text)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					sb.WriteString(src[inner.Pos().Offset():inner.End().Offset()])
					continue
				}
				text, err := expand.Literal(cfg, &syntax.Word{Parts: []syntax.WordPart{&syntax.DblQuoted{Parts: []syntax.WordPart{lit}}}})
				if err != nil {
					return "", err
				}
				sb.WriteString(text)
			}
		default:
			sb.WriteString(src[part.Pos().Offset():part.End().Offset()])
		}
	}
	return sb.String(), nil
}

// Unquoted literal with each backslash removed and the character after it kept, a backslash newline dropped
func unescape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}
