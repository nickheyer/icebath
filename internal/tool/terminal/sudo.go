package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/secret"
	"github.com/nickheyer/icebath/internal/shell"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
	"github.com/nickheyer/icebath/internal/tool/env"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const sudoPromptTimeout = 45 * time.Second

type HumanWindows interface {
	Window(ctx context.Context, sessionKey string) func()
}

type SudoOptions struct {
	Secrets  *secret.Scope
	Homes    *home.Resolver
	Gate     *approval.Gate
	Redactor *redact.Redactor
	Windows  HumanWindows
}

type Sudo struct {
	o     SudoOptions
	mu    sync.Mutex
	cache map[string]string
}

var _ env.CommandPreparer = (*Sudo)(nil)

func NewSudo(o SudoOptions) (*Sudo, error) {
	switch {
	case o.Secrets == nil:
		return nil, &tool.UnfilledError{Name: "terminal.SudoOptions.Secrets"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "terminal.SudoOptions.Homes"}
	case o.Gate == nil:
		return nil, &tool.UnfilledError{Name: "terminal.SudoOptions.Gate"}
	case o.Redactor == nil:
		return nil, &tool.UnfilledError{Name: "terminal.SudoOptions.Redactor"}
	case o.Windows == nil:
		return nil, &tool.UnfilledError{Name: "terminal.HumanWindows"}
	}
	return &Sudo{o: o, cache: map[string]string{}}, nil
}

func (s *Sudo) String() string { return "terminal.Sudo" }

func (s *Sudo) GoString() string { return "terminal.Sudo" }

func (s *Sudo) Prepare(ctx context.Context, command string, nopasswd func(ctx context.Context) bool) (string, *string, error) {
	return s.transform(ctx, transformMacOSOpen(command, runtime.GOOS), nopasswd)
}

func (s *Sudo) RewriteCompoundBackground(command string) string {
	return rewriteCompoundBackground(command)
}

func (s *Sudo) scope(ctx context.Context) string {
	c := callFrom(ctx)
	switch {
	case c == nil:
		return ""
	case c.SessionKey != "":
		return "session:" + c.SessionKey
	case c.Surface != nil:
		return fmt.Sprintf("callback:%T:%p", c.Surface, c.Surface)
	}
	return ""
}

func (s *Sudo) cached(ctx context.Context) string {
	scope := s.scope(ctx)
	if scope == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[scope]
}

func (s *Sudo) store(ctx context.Context, password string) {
	scope := s.scope(ctx)
	if scope == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if password != "" {
		s.cache[scope] = password
	} else {
		delete(s.cache, scope)
	}
}

func (s *Sudo) configured(ctx context.Context) (string, bool) {
	if v, ok, err := s.o.Secrets.Lookup(ctx, "SUDO_PASSWORD"); err == nil {
		return v, ok
	}
	return os.LookupEnv("SUDO_PASSWORD")
}

func noSudoUser(c *tool.Call) bool {
	return c != nil && (c.Origin.DelegatedChild || c.Presence.SingleQuery)
}

func canPrompt(c *tool.Call) bool {
	return c != nil && (c.Presence.Interactive || c.Surface != nil) && !noSudoUser(c)
}

func (s *Sudo) transform(ctx context.Context, command string, nopasswd func(ctx context.Context) bool) (string, *string, error) {
	transformed, count := rewriteRealSudo(command)
	if count == 0 {
		return command, nil, nil
	}
	configured, has := s.configured(ctx)
	password := configured
	if !has {
		password = s.cached(ctx)
	}
	c := callFrom(ctx)
	if !has && password == "" && canPrompt(c) {
		if nopasswd != nil && nopasswd(ctx) {
			return command, nil, nil
		}
		prompted, err := s.prompt(ctx, c, command)
		if err != nil {
			return "", nil, err
		}
		password = prompted
		if password != "" {
			s.store(ctx, password)
		}
	}
	if has || password != "" {
		stdin := strings.Repeat(password+"\n", count)
		return transformed, &stdin, nil
	}
	return command, nil, nil
}

func (s *Sudo) prompt(ctx context.Context, c *tool.Call, command string) (string, error) {
	if c.Surface == nil {
		return "", &tool.UnfilledError{Name: "tool.Surface"}
	}
	password := ""
	s.o.Gate.HumanInput(ctx, c, "sudo", command, c.SessionKey, func() string {
		done := s.o.Windows.Window(ctx, c.SessionKey)
		answer, ok, err := c.Surface.PromptSecret(ctx, tool.SecretPrompt{Kind: "sudo_password",
			Prompt: s.o.Redactor.Approval(ctx, command, true, false), Timeout: sudoPromptTimeout})
		done()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return "timeout"
		case errors.Is(err, context.Canceled):
			return "cancelled"
		case err != nil:
			return "error"
		case !ok:
			return "cancelled"
		case answer == "":
			return "skipped"
		}
		password = answer
		return "provided"
	})
	return password, nil
}

var sudoHeadlessFailures = []string{"sudo: a password is required", "sudo: no tty present", "sudo: a terminal is required"}

var sudoWrongPasswordMarkers = []string{
	"sudo: authentication failed",
	"sudo: incorrect password attempt",
	"sudo: maximum 3 incorrect authentication attempts",
	"sudo: 3 incorrect password attempts",
}

func sudoWrongPassword(output string) bool {
	lowered := strings.ToLower(output)
	for _, m := range sudoWrongPasswordMarkers {
		if strings.Contains(lowered, m) {
			return true
		}
	}
	return false
}

func (s *Sudo) handleFailure(ctx context.Context, c *tool.Call, output string) (string, error) {
	isGateway := c != nil && c.Presence.Gateway
	noUser := noSudoUser(c)
	headless := false
	for _, f := range sudoHeadlessFailures {
		if strings.Contains(output, f) {
			headless = true
			break
		}
	}
	if !(isGateway || noUser) || !headless {
		return output, nil
	}
	h, err := s.o.Homes.Home(ctx)
	if err != nil {
		return "", err
	}
	shown, err := home.Display(h)
	if err != nil {
		return "", err
	}
	if noUser {
		return output + "\n\n\U0001F4A1 Tip: This session cannot prompt for a sudo password. " +
			"Add SUDO_PASSWORD to " + shown + "/.env on the agent machine, " +
			"or run the command without sudo.", nil
	}
	return output + "\n\n\U0001F4A1 Tip: To enable sudo over messaging, add SUDO_PASSWORD to " + shown + "/.env on the agent machine.", nil
}

func (s *Sudo) invalidateOnAuthFailure(ctx context.Context, command, output string) bool {
	if _, set := os.LookupEnv("SUDO_PASSWORD"); set {
		return false
	}
	if !sudoWrongPassword(output) || countRealSudo(command) == 0 || s.cached(ctx) == "" {
		return false
	}
	s.store(ctx, "")
	return true
}

func (s *Sudo) annotate(ctx context.Context, c *tool.Call, command, output string) (string, bool, bool, error) {
	output, err := s.handleFailure(ctx, c, output)
	if err != nil {
		return "", false, false, err
	}
	authFailed := sudoWrongPassword(output)
	cleared := s.invalidateOnAuthFailure(ctx, command, output)
	if cleared && canPrompt(c) {
		output += "\n\n" + string(rune(0x26A0)) + string(rune(0xFE0F)) + " Sudo authentication failed — cached password " +
			"cleared. You will be prompted again on the next sudo command."
	}
	return output, authFailed, cleared, nil
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func looksLikeEnvAssignment(token string) bool {
	if !strings.Contains(token, "=") || strings.HasPrefix(token, "=") {
		return false
	}
	name, _, _ := strings.Cut(token, "=")
	return envNameRe.MatchString(name)
}

const shellOps = ";|&()"

func readShellToken(cmd []rune, start int) int {
	n := len(cmd)
	i := start
	for i < n {
		c := cmd[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < n && cmd[j] != '\'' {
				j++
			}
			if j < n {
				j++
			}
			i = j
		case c == '"':
			j := i + 1
			for j < n && cmd[j] != '"' {
				if cmd[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				j++
			}
			if j < n {
				j++
			}
			i = j
		case c == '\\' && i+1 < n:
			i += 2
		case unicode.IsSpace(c) || strings.ContainsRune(shellOps, c):
			return i
		default:
			i++
		}
	}
	return i
}

type scanEvent struct {
	kind       string
	start, end int
	atStart    bool
}

func hasRunePrefix(r []rune, i int, prefix string) bool {
	p := []rune(prefix)
	if i+len(p) > len(r) {
		return false
	}
	for k, ch := range p {
		if r[i+k] != ch {
			return false
		}
	}
	return true
}

func scanShellRunes(cmd []rune, background bool) []scanEvent {
	var out []scanEvent
	n := len(cmd)
	atStart := true
	parens, braces := 0, 0
	twoChar := []string{"&&", "||", ";;"}
	if background {
		twoChar = []string{"&&", "||", "&>"}
	}
	for i := 0; i < n; {
		ch := cmd[i]
		grouped := parens > 0 || braces > 0
		wasStart := atStart
		kind, end := "", 0
		twoCharOp := false
		for _, op := range twoChar {
			if hasRunePrefix(cmd, i, op) {
				twoCharOp = true
				break
			}
		}
		switch {
		case unicode.IsSpace(ch):
			kind, end = "ws", i+1
			if grouped {
				kind = "skip"
			}
			atStart = atStart || ch == '\n'
		case ch == '#':
			end = n
			for k := i; k < n; k++ {
				if cmd[k] == '\n' {
					end = k
					break
				}
			}
			kind = "comment"
		case background && ch == '\\' && i+1 < n:
			kind, end = "escape", i+2
		case background && (ch == '\'' || ch == '"'):
			kind, end = "word", readShellToken(cmd, i)
		case background && (ch == '(' || ch == ')' || (ch == '{' && i+1 < n && unicode.IsSpace(cmd[i+1])) || (ch == '}' && braces > 0)):
			if ch == '(' {
				parens++
			} else if ch == ')' {
				parens = max(0, parens-1)
			}
			if ch == '{' {
				braces++
			} else if ch == '}' {
				braces--
			}
			kind, end = "op", i+1
		case background && grouped:
			kind, end = "skip", i+1
		case twoCharOp:
			kind, end, atStart = "op", i+2, true
		case strings.ContainsRune(shellOps, ch):
			kind, end, atStart = "op", i+1, ch != ')'
		default:
			end = readShellToken(cmd, i)
			kind = "word"
			atStart = atStart && looksLikeEnvAssignment(string(cmd[i:end]))
		}
		out = append(out, scanEvent{kind: kind, start: i, end: end, atStart: wasStart})
		i = end
	}
	return out
}

var (
	envValueOptions = map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true, "-a": true, "--argv0": true}
	envFlagOptions  = map[string]bool{"-i": true, "--ignore-environment": true, "-0": true, "--null": true, "-v": true, "--debug": true}
)

func envOptionAttached(word string) bool {
	for _, opt := range []string{"--unset", "--chdir", "--argv0"} {
		if strings.HasPrefix(word, opt+"=") {
			return true
		}
	}
	for _, opt := range []string{"-u", "-C", "-a"} {
		if strings.HasPrefix(word, opt) && len(word) > 2 {
			return true
		}
	}
	return false
}

func rewriteRealSudo(command string) (string, int) {
	r := []rune(command)
	var out strings.Builder
	count := 0
	inEnv, envOperand, envOptions := false, false, false
	for _, ev := range scanShellRunes(r, false) {
		text := string(r[ev.start:ev.end])
		out.WriteString(text)
		if ev.kind == "op" || (ev.kind == "ws" && text == "\n") {
			inEnv, envOperand, envOptions = false, false, false
		}
		if ev.kind != "word" || !(ev.atStart || inEnv) {
			continue
		}
		words, err := shell.Split(text)
		if err != nil {
			inEnv = false
			continue
		}
		if len(words) != 1 || strings.ContainsAny(text, "$`*?[]~<>{}") {
			inEnv = false
			continue
		}
		word := words[0]
		if inEnv {
			if envOperand {
				envOperand = false
				continue
			}
			if envOptions && (word == "-" || word == "--") {
				envOptions = false
				continue
			}
			if envOptions && envValueOptions[word] {
				envOperand = true
				continue
			}
			if envOptions && (envFlagOptions[word] || envOptionAttached(word)) {
				continue
			}
			if envOptions && strings.HasPrefix(word, "-") {
				inEnv = false
				continue
			}
			if looksLikeEnvAssignment(word) {
				envOptions = false
				continue
			}
		} else if looksLikeEnvAssignment(text) {
			continue
		}
		executable := word
		if i := strings.LastIndex(word, "/"); i >= 0 {
			executable = word[i+1:]
		}
		inEnv = executable == "env"
		envOptions = inEnv
		if executable == "sudo" {
			out.WriteString(" -S -p ''")
			count++
		}
	}
	return out.String(), count
}

func countRealSudo(command string) int {
	_, n := rewriteRealSudo(command)
	return n
}

func rewriteCompoundBackground(command string) string {
	r := []rune(command)
	chainEnd := -1
	type rewrite struct{ chainEnd, amp int }
	var rewrites []rewrite
	for _, ev := range scanShellRunes(r, true) {
		text := string(r[ev.start:ev.end])
		switch {
		case ev.kind == "op" && (text == "&&" || text == "||"):
			chainEnd = ev.end
		case (ev.kind == "ws" && text == "\n") || (ev.kind == "op" && (text == ";" || text == "|" || text == "}")):
			chainEnd = -1
		case ev.kind == "op" && text == "&":
			j := ev.start - 1
			for j >= 0 && unicode.IsSpace(r[j]) {
				j--
			}
			if j >= 0 && (r[j] == '<' || r[j] == '>') {
				continue
			}
			if chainEnd >= 0 {
				rewrites = append(rewrites, rewrite{chainEnd, ev.start})
			}
			chainEnd = -1
		}
	}
	result := r
	for i := len(rewrites) - 1; i >= 0; i-- {
		insert, amp := rewrites[i].chainEnd, rewrites[i].amp
		for insert < amp && unicode.IsSpace(result[insert]) {
			insert++
		}
		suffix := result[amp+1:]
		tail := strings.TrimLeft(string(suffix), " \t")
		needsSeparator := tail != "" && (!strings.ContainsRune(";\n&|)}", []rune(tail)[0]) || strings.HasPrefix(tail, "&>"))
		var b []rune
		b = append(b, result[:insert]...)
		b = append(b, []rune("{ ")...)
		b = append(b, result[insert:amp]...)
		b = append(b, []rune("& }")...)
		if needsSeparator {
			b = append(b, []rune(" ;")...)
		}
		b = append(b, suffix...)
		result = b
	}
	return string(result)
}
