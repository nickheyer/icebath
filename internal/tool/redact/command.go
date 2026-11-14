package redact

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

var (
	errNoClosingQuote = errors.New("no closing quotation")
	errNoEscaped      = errors.New("no escaped character")
)

var envDumpCommands = map[string]bool{"env": true, "printenv": true, "set": true, "export": true, "declare": true}

var fileReadCommands = map[string]bool{
	"cat": true, "head": true, "tail": true, "type": true, "bat": true, "less": true, "more": true, "nl": true,
	"zcat": true, "tac": true, "view": true, "batcat": true, "grep": true, "awk": true, "sed": true,
}

var patternFirstCommands = map[string]bool{"grep": true, "awk": true, "sed": true}

var envFileBasenames = map[string]bool{
	".env": true, ".env.local": true, ".env.development": true, ".env.production": true, ".env.test": true,
	".env.staging": true, ".envrc": true,
}

var shellRCBasenames = map[string]bool{
	".bashrc": true, ".bash_profile": true, ".bash_login": true, ".profile": true,
	".zshrc": true, ".zprofile": true, ".zlogin": true, ".zshenv": true,
}

func commandSegments(command string) []string {
	var segments []string
	var buf strings.Builder
	var quote byte
	flush := func() {
		if seg := pyStrip(buf.String()); seg != "" {
			segments = append(segments, seg)
		}
		buf.Reset()
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote != 0:
			buf.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			buf.WriteByte(c)
		case c == '|' || c == ';' || c == '&':
			flush()
		default:
			buf.WriteByte(c)
		}
	}
	flush()
	return segments
}

func isShlexSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func shlexSplit(s string) ([]string, error) {
	var out []string
	var tok strings.Builder
	state, escaped := byte(' '), byte('a')
	quoted := false
	finish := func() {
		if tok.Len() > 0 || quoted {
			out = append(out, tok.String())
		}
		tok.Reset()
		quoted = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case ' ':
			switch {
			case isShlexSpace(c):
			case c == '\\':
				escaped, state = 'a', '\\'
			case c == '\'' || c == '"':
				state = c
			default:
				tok.WriteByte(c)
				state = 'a'
			}
		case '\'', '"':
			quoted = true
			switch {
			case c == state:
				state = 'a'
			case c == '\\' && state == '"':
				escaped, state = state, '\\'
			default:
				tok.WriteByte(c)
			}
		case '\\':
			if (escaped == '\'' || escaped == '"') && c != '\\' && c != escaped {
				tok.WriteByte('\\')
			}
			tok.WriteByte(c)
			state = escaped
		case 'a':
			switch {
			case isShlexSpace(c):
				state = ' '
				finish()
			case c == '\'' || c == '"':
				state = c
			case c == '\\':
				escaped, state = 'a', '\\'
			default:
				tok.WriteByte(c)
			}
		}
	}
	switch state {
	case '\'', '"':
		return nil, errNoClosingQuote
	case '\\':
		return nil, errNoEscaped
	case 'a':
		finish()
	}
	return out, nil
}

func IsEnvDumpCommand(command string) bool {
	if command == "" {
		return false
	}
	for _, seg := range commandSegments(command) {
		tokens, err := shlexSplit(seg)
		if err != nil {
			tokens = pyFields(seg)
		}
		if len(tokens) > 0 && envDumpCommands[tokens[0]] {
			return true
		}
	}
	return false
}

func (r *Redactor) ReadsSecretFile(ctx context.Context, command string) bool {
	if command == "" {
		return false
	}
	for _, seg := range commandSegments(command) {
		tokens := pyFields(seg)
		if len(tokens) == 0 {
			continue
		}
		reader := tokens[0]
		if i := strings.LastIndexByte(reader, '/'); i >= 0 {
			reader = reader[i+1:]
		}
		reader = pyLower(reader)
		if !fileReadCommands[reader] {
			continue
		}
		var positional []string
		for _, arg := range tokens[1:] {
			if !strings.HasPrefix(arg, "-") {
				positional = append(positional, arg)
			}
		}
		if patternFirstCommands[reader] && len(positional) > 0 {
			positional = positional[1:]
		}
		for _, arg := range positional {
			if r.IsSecretFile(ctx, arg) {
				return true
			}
		}
	}
	return false
}

func (r *Redactor) IsSecretFile(ctx context.Context, arg string) bool {
	path := strings.ReplaceAll(strings.Trim(arg, `"'`), `\`, "/")
	inHome := false
	for _, prefix := range []string{"$ICEBATH_HOME/", "${ICEBATH_HOME}/"} {
		if strings.HasPrefix(path, prefix) {
			path = path[len(prefix):]
			inHome = true
			break
		}
	}
	for _, prefix := range []string{"$HOME/", "${HOME}/"} {
		if strings.HasPrefix(path, prefix) {
			path = path[len(prefix):]
			break
		}
	}
	if strings.Contains(path, "$") {
		return false
	}
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, pyLower(part))
		}
	}
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if envFileBasenames[last] || shellRCBasenames[last] {
		return true
	}
	if last != "config.yaml" && !strings.HasPrefix(last, "config.yaml.good.") && !strings.HasPrefix(last, "config.yaml.corrupt.") {
		return false
	}
	return inHome || slices.Contains(parts[:len(parts)-1], ".icebath") || r.underHome(ctx, path)
}

func (r *Redactor) underHome(ctx context.Context, path string) bool {
	target, err := r.homes.Key(path)
	if err != nil {
		return false
	}
	var dirs []string
	if active, err := r.homes.Home(ctx); err == nil {
		dirs = append(dirs, active)
	}
	if process, err := home.ProcessHome(); err == nil {
		if root, err := home.RootOf(process); err == nil {
			dirs = append(dirs, root)
		}
	}
	for _, dir := range dirs {
		base, err := r.homes.Key(dir)
		if err != nil {
			continue
		}
		if target == base || strings.HasPrefix(target, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
