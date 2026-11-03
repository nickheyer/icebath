package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/glob"
)

// Seconds an inline shell command of a skill may run
const inlineTimeout = 2 * time.Minute

// Characters of inline command output kept inline before it goes to a file
const inlineCap = 30000

// What a skill is rendered with
type Render struct {
	// Argument text as typed after the skill name
	Args string
	// Variables substituted for ${NAME}, the session, project, skill and plugin ones
	Vars map[string]string
	// Runs an inline command and returns its output, nil when inline commands are disabled by policy
	Shell func(ctx context.Context, command, shell string) (string, error)
	// Directory relative file references resolve against
	Dir string
	// Scratch directory holding command output past the inline cap
	Scratch string
}

// Result of a render, the text and the files it referenced
type Rendered struct {
	Text string
	// Files named by @ references, as absolute paths
	Files []string
	// Argument text appended because no placeholder took it
	Appended bool
}

var (
	indexedArg = regexp.MustCompile(`\$ARGUMENTS\[(\d+)\]|\$(\d+)`)
	inlineCmd  = regexp.MustCompile("(?m)(^|[ \t])!`([^`]*)`")
	fencedCmd  = regexp.MustCompile("(?ms)^```![ \t]*\n(.*?)\n```[ \t]*$")
	envRefRe   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.]*)\}`)
	fileRef    = regexp.MustCompile(`(^|\s)@([^\s@,;:'"<>()]+)`)
)

// Renders the body of a skill, arguments first, then inline commands, then variables, file references collected
func (s *Skill) Render(ctx context.Context, r Render) (Rendered, error) {
	text, appended := substituteArgs(s.Body, r.Args, s.Arguments)
	var err error
	if text, err = runInline(ctx, text, s, r); err != nil {
		return Rendered{}, err
	}
	text = substituteEnv(text, r.Vars)
	out := Rendered{Text: text, Appended: appended}
	for _, m := range fileRef.FindAllStringSubmatch(text, -1) {
		ref := m[2]
		if strings.HasPrefix(ref, "agent-") || strings.Contains(ref, "://") {
			continue
		}
		path := expandHome(ref)
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.Dir, path)
		}
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			out.Files = append(out.Files, path)
		}
	}
	return out, nil
}

// Splits argument text shell style, quotes grouping words
func SplitArgs(text string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	escaped := false
	has := false
	flush := func() {
		if has {
			out = append(out, cur.String())
		}
		cur.Reset()
		has = false
	}
	for _, r := range text {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			has = true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			has = true
		case r == '"' || r == '\'':
			quote = r
			has = true
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	flush()
	return out
}

// Replaces the argument placeholders, appending the arguments when none took them
func substituteArgs(body, args string, names []string) (string, bool) {
	args = strings.TrimSpace(args)
	words := SplitArgs(args)
	received := false
	escapable := func(after string) bool {
		if strings.HasPrefix(after, "ARGUMENTS") || len(after) > 0 && after[0] >= '0' && after[0] <= '9' {
			return true
		}
		for _, n := range names {
			if strings.HasPrefix(after, n) {
				return true
			}
		}
		return false
	}
	var b strings.Builder
	i := 0
	for i < len(body) {
		c := body[i]
		if c == '\\' && i+1 < len(body) && body[i+1] == '$' && escapable(body[i+2:]) {
			b.WriteByte('$')
			i += 2
			continue
		}
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		rest := body[i:]
		if m := indexedArg.FindStringSubmatchIndex(rest); m != nil && m[0] == 0 {
			idx := ""
			if m[2] >= 0 {
				idx = rest[m[2]:m[3]]
			} else {
				idx = rest[m[4]:m[5]]
			}
			n := 0
			for _, d := range idx {
				n = n*10 + int(d-'0')
			}
			if n < len(words) {
				b.WriteString(words[n])
				received = true
			} else {
				b.WriteString(rest[:m[1]])
			}
			i += m[1]
			continue
		}
		if strings.HasPrefix(rest, "$ARGUMENTS") {
			b.WriteString(args)
			received = true
			i += len("$ARGUMENTS")
			continue
		}
		matched := false
		for n, name := range names {
			if strings.HasPrefix(rest, "$"+name) && !wordByte(rest, 1+len(name)) {
				if n < len(words) {
					b.WriteString(words[n])
				}
				received = true
				i += 1 + len(name)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		b.WriteByte(c)
		i++
	}
	out := b.String()
	if args != "" && !received {
		out = strings.TrimRight(out, "\n") + "\n\nARGUMENTS: " + args
		return out, true
	}
	return out, false
}

// Reports whether the byte at the offset continues a word
func wordByte(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	c := s[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// Replaces ${NAME} with the value of each variable, unknown names left as they are
func substituteEnv(text string, vars map[string]string) string {
	if len(vars) == 0 {
		return text
	}
	return envRefRe.ReplaceAllStringFunc(text, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// Runs every inline and fenced command once over the original text, output replacing it
func runInline(ctx context.Context, text string, s *Skill, r Render) (string, error) {
	if !inlineCmd.MatchString(text) && !fencedCmd.MatchString(text) {
		return text, nil
	}
	if r.Shell == nil {
		replace := func(string) string { return "[shell command execution disabled by policy]" }
		text = fencedCmd.ReplaceAllStringFunc(text, replace)
		return inlineCmd.ReplaceAllStringFunc(text, func(m string) string {
			lead := inlineCmd.FindStringSubmatch(m)[1]
			return lead + replace(m)
		}), nil
	}
	var failed error
	run := func(command string) string {
		if failed != nil {
			return ""
		}
		cctx, cancel := context.WithTimeout(ctx, inlineTimeout)
		defer cancel()
		out, err := r.Shell(cctx, command, s.Shell)
		if err != nil {
			failed = fmt.Errorf("shell command failed for pattern %q: %w", command, err)
			return ""
		}
		if len(out) > inlineCap && r.Scratch != "" {
			path := filepath.Join(r.Scratch, fmt.Sprintf("skill-%s-%d.txt", SafeFile(s.Short), time.Now().UnixNano()))
			if os.MkdirAll(r.Scratch, 0o700) == nil && os.WriteFile(path, []byte(out), 0o600) == nil {
				return fmt.Sprintf("[output of %d characters saved to %s, it begins]\n%s", len(out), path, out[:inlineCap])
			}
		}
		return out
	}
	text = fencedCmd.ReplaceAllStringFunc(text, func(m string) string {
		return run(fencedCmd.FindStringSubmatch(m)[1])
	})
	text = inlineCmd.ReplaceAllStringFunc(text, func(m string) string {
		parts := inlineCmd.FindStringSubmatch(m)
		return parts[1] + run(parts[2])
	})
	if failed != nil {
		return "", failed
	}
	return text, nil
}

// Name with path separators and spaces turned into dashes, for a file
func SafeFile(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ' ' || r == ':' {
			return '-'
		}
		return r
	}, name)
}

// Reports whether a path under the directory matches any of the globs, patterns relative to the directory
func matchesAny(patterns []string, dir, path string) bool {
	rel := path
	if filepath.IsAbs(path) {
		r, err := filepath.Rel(dir, path)
		if err != nil || strings.HasPrefix(r, "..") {
			return false
		}
		rel = r
	}
	rel = filepath.ToSlash(rel)
	for _, p := range patterns {
		p = strings.TrimPrefix(filepath.ToSlash(p), "./")
		if glob.Match(p, rel) || strings.HasPrefix(p, "**/") && glob.Match(strings.TrimPrefix(p, "**/"), rel) {
			return true
		}
		if !strings.Contains(p, "/") && glob.Match(p, filepath.Base(rel)) {
			return true
		}
	}
	return false
}

// Variables every skill and command sees, sorted for a stable listing
func TextVars(session, project, effort string, s *Skill) map[string]string {
	vars := map[string]string{}
	if session != "" {
		vars["CLAUDE_SESSION_ID"] = session
		vars["ICEBATH_SESSION_ID"] = session
	}
	if project != "" {
		vars["CLAUDE_PROJECT_DIR"] = project
		vars["ICEBATH_PROJECT_DIR"] = project
	}
	if effort != "" {
		vars["CLAUDE_EFFORT"] = effort
	}
	if s != nil {
		vars["CLAUDE_SKILL_DIR"] = s.Dir
		vars["ICEBATH_SKILL_DIR"] = s.Dir
		if s.Plugin != nil {
			for k, v := range s.Plugin.textVars(project) {
				vars[k] = v
			}
		}
	}
	return vars
}

// Rules of a skill with its variables substituted, so Bash patterns naming the skill directory match
func (s *Skill) AllowedRules(vars map[string]string) []string {
	out := make([]string, 0, len(s.AllowedTools))
	for _, r := range s.AllowedTools {
		out = append(out, substituteEnv(r, vars))
	}
	return out
}

// Error for a skill the model asked for that only a person may run
var ErrModelInvocation = errors.New("this skill is run only by the person, through its slash command, and its steps are not to be reproduced another way")
