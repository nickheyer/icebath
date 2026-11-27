package speech

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/proc"
)

// A program of yours that went quiet past its timeout
type TimeoutError struct {
	Command string
	Timeout time.Duration
}

// The seconds written as a fraction, a whole number keeping its .0
func (e *TimeoutError) Error() string {
	seconds := strconv.FormatFloat(e.Timeout.Seconds(), 'f', -1, 64)
	if !strings.Contains(seconds, ".") {
		seconds += ".0"
	}
	return fmt.Sprintf("Command '%s' timed out after %s seconds", e.Command, seconds)
}

// A program of yours that exited with a failure
type ExitError struct {
	Command        string
	Code           int
	Stdout, Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", e.Command, e.Code)
}

// What a failed program said, its error output and output labelled, else that it said nothing
func (e *ExitError) Detail() string {
	var parts []string
	for _, s := range [][2]string{{"stderr", e.Stderr}, {"stdout", e.Stdout}} {
		if strings.TrimSpace(s[1]) != "" {
			parts = append(parts, s[0]+": "+strings.TrimSpace(s[1]))
		}
	}
	if len(parts) == 0 {
		return "no command output"
	}
	return strings.Join(parts, "; ")
}

// The quote a template is inside at a byte position: ' or ", empty outside quotes
func quoteAt(template string, position int) string {
	quote, escaped := "", false
	for i := 0; i < position && i < len(template); i++ {
		c := template[i]
		switch {
		case quote == "'":
			if c == '\'' {
				quote = ""
			}
		case quote == `"`:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				quote = ""
			}
		case c == '\'' || c == '"':
			quote = string(c)
		case c == '\\':
			i++
		}
	}
	return quote
}

// Windows command line quoting of one argument
func windowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteRune(r)
			slashes = 0
			continue
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteRune(r)
			continue
		}
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

var safeWord = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// POSIX shell quoting of one word
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// A value quoted for where it sits in a command: escaped inside quotes, quoted for the shell outside them
func quoteValue(value, quote string) string {
	switch quote {
	case "'":
		return strings.ReplaceAll(value, "'", `'\''`)
	case `"`:
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(value)
	}
	if runtime.GOOS == "windows" {
		return windowsArg(value)
	}
	return ShellQuote(value)
}

// A command template with each {name} or {{name}} placeholder put in quoted for where it sits, ${name} left to the shell, and the
// other doubled braces made single
func Render(template string, values map[string]string) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, regexp.QuoteMeta(name))
	}
	slices.Sort(names)
	alternatives := strings.Join(names, "|")
	re := regexp2.MustCompile(`(?<!\$)(?:\{\{(?<double>`+alternatives+`)\}\}|\{(?<single>`+alternatives+`)\})`, regexp2.None)
	runes := []rune(template)
	var out strings.Builder
	var tokens [][2]string
	last := 0
	m, _ := re.FindStringMatch(template)
	for m != nil {
		name := m.GroupByName("double").String()
		if name == "" {
			name = m.GroupByName("single").String()
		}
		token := fmt.Sprintf("__ICEBATH_CMD_PLACEHOLDER_%d__", len(tokens))
		tokens = append(tokens, [2]string{token, quoteValue(values[name], quoteAt(template, len(string(runes[:m.RuneIndex]))))})
		out.WriteString(string(runes[last:m.RuneIndex]))
		out.WriteString(token)
		last = m.RuneIndex + m.RuneLength
		m, _ = re.FindNextMatch(m)
	}
	out.WriteString(string(runes[last:]))
	rendered := strings.ReplaceAll(strings.ReplaceAll(out.String(), "{{", "{"), "}}", "}")
	for _, t := range tokens {
		rendered = strings.ReplaceAll(rendered, t[0], t[1])
	}
	return rendered
}

// The shell of this machine running a command line
func shell(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		cmd := exec.Command(cmpOr(os.Getenv("COMSPEC"), "cmd.exe"))
		cmd.SysProcAttr = commandLine("/d /s /c \"" + command + "\"")
		return cmd
	}
	return exec.Command("/bin/sh", "-c", command)
}

// Output written so far, its arrival resetting the idle clock
type output struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	poke chan struct{}
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.buf.Write(p)
	o.mu.Unlock()
	select {
	case o.poke <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (o *output) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.ToValidUTF8(o.buf.String(), "�")
}

// Runs a command line of yours through the shell, in a group of its own that dies with icebath, without the credentials programs may not
// see but with the variables it passes through; its whole tree killed once it writes nothing for the timeout
func (j *Job) Shell(ctx context.Context, command string, timeout time.Duration, passthrough []string, purpose string) (string, string, error) {
	extra := map[string]string{}
	for _, name := range passthrough {
		if value, ok := j.Config.LookupEnv(name); ok {
			extra[name] = value
		}
	}
	cmd := shell(command)
	cmd.Env = terminal.Environ(j.Speech.terminal.ChildEnv(j.Config, extra))
	poke := make(chan struct{}, 1)
	stdout, stderr := &output{poke: poke}, &output{poke: poke}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, nil
	cmd.WaitDelay = 10 * time.Second
	terminal.NewSession(cmd)
	proc.HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	p := terminal.Watch(cmd)
	p.Own(j.Speech.terminal.Supervisor)
	defer p.Release()
	terminal.RegisterChild(j.Config, p.Pid(), purpose)
	done := make(chan struct{})
	go func() {
		p.Wait(0)
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-done:
			code, _ := p.Poll()
			if code != 0 {
				return stdout.text(), stderr.text(), &ExitError{Command: command, Code: code, Stdout: stdout.text(), Stderr: stderr.text()}
			}
			return stdout.text(), stderr.text(), nil
		case <-poke:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			p.KillTree()
			<-done
			return stdout.text(), stderr.text(), &TimeoutError{Command: command, Timeout: timeout}
		case <-ctx.Done():
			p.KillTree()
			<-done
			return stdout.text(), stderr.text(), ctx.Err()
		}
	}
}
