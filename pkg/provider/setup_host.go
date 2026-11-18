package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Kinds of question a hosted setup flow puts to its host
const (
	HostedChoice    = "choice"
	HostedChecklist = "checklist"
	HostedText      = "text"
	HostedSecret    = "secret"
	HostedCommand   = "command"
)

// HostedPrompt is one question a setup flow asks while a host answers in place of a terminal
type HostedPrompt struct {
	Kind        string
	Title       string
	Description string
	Rows        []string
	Answers     []string
	Headings    map[int]string
	Default     int
	Picked      []int
	Argv        []string
}

// HostedAnswer is the reply of the host, Cancelled when the user backed out of the question
type HostedAnswer struct {
	Cancelled bool
	Index     int
	Picked    []int
	Text      string
	Ran       bool
	Err       string
}

// SetupAsker puts one hosted prompt to the user and waits for the reply
type SetupAsker func(HostedPrompt) HostedAnswer

var (
	hostedMu  sync.Mutex
	hostedAsk SetupAsker
)

// Asker of the hosted flow running now, nil on a terminal
func hostedAsker() SetupAsker {
	hostedMu.Lock()
	defer hostedMu.Unlock()
	return hostedAsk
}

// Output of a hosted flow, written a whole line at a time with terminal commands named by their TUI counterpart
type hostedOutput struct {
	mu       sync.Mutex
	w        io.Writer
	partial  string
	recent   []string
	progress func(text string)
	shown    string
}

// Writer of a hosted flow that also shows the line a progress bar keeps rewriting
type progressWriter interface {
	io.Writer
	Progress(text string)
}

// Terminal commands a setup line names, a profile selector dropped since the TUI already runs in that profile
var (
	hostedSetupCommand = regexp.MustCompile(`icebath (?:-p [A-Za-z0-9_.-]+ )?setup( (?:model|tts|terminal|gateway|tools|agent|telemetry))?\b`)
	hostedModelCommand = regexp.MustCompile(`icebath (?:-p [A-Za-z0-9_.-]+ )?(?:auth add ([a-z0-9_-]+|<provider>)(?: --type oauth)?|(portal)|model)\b`)
)

// Line with every terminal command it names replaced by the TUI command that does the same
func tuiWording(line string) string {
	line = hostedModelCommand.ReplaceAllStringFunc(line, func(command string) string {
		m := hostedModelCommand.FindStringSubmatch(command)
		switch {
		case m[1] != "":
			return "/setup model " + m[1]
		case m[2] != "":
			return "/setup model nous"
		}
		return "/setup model"
	})
	return hostedSetupCommand.ReplaceAllString(line, "/setup$1")
}

func (o *hostedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.partial += string(p)
	for {
		line, rest, whole := strings.Cut(o.partial, "\n")
		if !whole {
			if at := strings.LastIndex(o.partial, "\r"); at >= 0 {
				o.partial = o.partial[at+1:]
			}
			if current := tuiWording(strings.TrimSpace(o.partial)); o.progress != nil && current != o.shown {
				o.shown = current
				o.progress(current)
			}
			return len(p), nil
		}
		o.partial = rest
		if at := strings.LastIndex(line, "\r"); at >= 0 {
			line = line[at+1:]
		}
		line = tuiWording(line)
		o.recent = append(o.recent, line)
		if len(o.recent) > 40 {
			o.recent = o.recent[len(o.recent)-40:]
		}
		if _, err := io.WriteString(o.w, line+"\n"); err != nil {
			return len(p), err
		}
	}
}

// Writes out an unfinished last line
func (o *hostedOutput) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.partial != "" {
		_, _ = io.WriteString(o.w, tuiWording(o.partial)+"\n")
		o.partial = ""
	}
	if o.progress != nil && o.shown != "" {
		o.shown = ""
		o.progress("")
	}
}

// Last whole lines printed, oldest first
func (o *hostedOutput) tail() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string{}, o.recent...)
}

var (
	hostedYesNo     = regexp.MustCompile(`^(.*?)\s*[\[(]\s*([Yy])\s*/\s*([Nn])\s*[\])]\s*:?\s*$`)
	hostedNumbered  = regexp.MustCompile(`^\s*(.*?)\s*\[\s*1\s*[-/]\s*\d+(?:\s*/\s*\d+)*\s*\]\s*(?:\((\d+)\))?\s*:?\s*$`)
	hostedMenuEntry = regexp.MustCompile(`^\s*(?:→\s*)?(\d+)\.\s+(.+?)\s*$`)
	hostedANSI      = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
)

// Typed question as the host sees it, yes or no prompts and numbered menus offered as lists
func hostedLine(trail *hostedOutput, prompt string, secret bool) HostedPrompt {
	title := strings.TrimSpace(hostedANSI.ReplaceAllString(prompt, ""))
	if secret {
		return HostedPrompt{Kind: HostedSecret, Title: title}
	}
	if m := hostedYesNo.FindStringSubmatch(title); m != nil {
		def := 0
		if m[3] == "N" {
			def = 1
		}
		return HostedPrompt{Kind: HostedChoice, Title: m[1], Rows: []string{"Yes", "No"}, Answers: []string{"y", "n"}, Default: def}
	}
	if m := hostedNumbered.FindStringSubmatch(title); m != nil {
		rows, answers := numberedMenu(trail.tail())
		def := 0
		for i, answer := range answers {
			if answer == m[2] {
				def = i
			}
		}
		if len(rows) > 1 {
			return HostedPrompt{Kind: HostedChoice, Title: m[1], Rows: rows, Answers: answers, Default: def}
		}
	}
	return HostedPrompt{Kind: HostedText, Title: title}
}

// Last run of lines numbered from one printed before a typed menu, with the number each row answers
func numberedMenu(lines []string) ([]string, []string) {
	var rows, answers []string
	for i := len(lines) - 1; i >= 0; i-- {
		m := hostedMenuEntry.FindStringSubmatch(hostedANSI.ReplaceAllString(lines[i], ""))
		if m == nil {
			if len(rows) > 0 {
				break
			}
			continue
		}
		rows, answers = append([]string{m[2]}, rows...), append([]string{m[1]}, answers...)
		if m[1] == "1" {
			return rows, answers
		}
	}
	return nil, nil
}

// Typed answer of a hosted line, an interrupt when the user backed out
func askHostedLine(ask SetupAsker, trail *hostedOutput, prompt string, secret bool) (string, error) {
	q := hostedLine(trail, prompt, secret)
	answer := ask(q)
	switch {
	case answer.Cancelled:
		return "", cliui.ErrInterrupt
	case q.Kind == HostedChoice:
		if answer.Index < 0 || answer.Index >= len(q.Answers) {
			return "", cliui.ErrInterrupt
		}
		return q.Answers[answer.Index], nil
	}
	return answer.Text, nil
}

// Plain row text of styled picker rows
func pickerRowTexts(items [][]PickerSegment) []string {
	rows := make([]string, len(items))
	for i, segs := range items {
		var b strings.Builder
		for _, s := range segs {
			b.WriteString(s.Text)
		}
		rows[i] = b.String()
	}
	return rows
}

// Chosen row index of a hosted list, the cancel value when the user backed out
func askHostedChoice(ask SetupAsker, title string, rows []string, def int, description string, headings map[int]string, cancel int) int {
	answer := ask(HostedPrompt{Kind: HostedChoice, Title: strings.TrimSpace(title), Rows: rows, Default: def, Description: description, Headings: headings})
	if answer.Cancelled || answer.Index < 0 || answer.Index >= len(rows) {
		return cancel
	}
	return answer.Index
}

// Ticked row indices of a hosted checklist, the preselection when the user backed out
func askHostedChecklist(ask SetupAsker, title string, rows []string, pre []int) []int {
	answer := ask(HostedPrompt{Kind: HostedChecklist, Title: strings.TrimSpace(title), Rows: rows, Picked: pre})
	if answer.Cancelled {
		return pre
	}
	picked := []int{}
	for _, i := range answer.Picked {
		if i >= 0 && i < len(rows) {
			picked = append(picked, i)
		}
	}
	sort.Ints(picked)
	return picked
}

// Whether a hosted run of a terminal program exited on its own, an error when it could not start
func askHostedCommand(ask SetupAsker, argv []string) (bool, error) {
	answer := ask(HostedPrompt{Kind: HostedCommand, Argv: argv, Title: strings.Join(argv, " ")})
	if answer.Err != "" {
		return false, errors.New(answer.Err)
	}
	return !answer.Cancelled && answer.Ran, nil
}

// Hosted console input asking the host for each line and refusing raw reads of the gateway input
type hostedInput struct {
	ask   SetupAsker
	trail *hostedOutput
}

func (hostedInput) Read([]byte) (int, error) { return 0, io.EOF }

// Answer the host gives to the question a console prompt asks, an interrupt when the user backed out
func (in hostedInput) AskLine(prompt string) (string, error) {
	return askHostedLine(in.ask, in.trail, prompt, false)
}

// Console of the hosted flow, a human answering its input and a terminal to menus asking to draw
func hostedConsole(out *hostedOutput, ask SetupAsker, tty bool) *cliui.Console {
	yes := func() bool { return tty }
	human := func() bool { return true }
	return &cliui.Console{In: hostedInput{ask: ask, trail: out}, Out: out, StdinTTY: human, StdoutTTY: yes,
		Getenv: func(name string) (string, bool) {
			if name == "TERM" {
				return "xterm-256color", true
			}
			if name == "NO_COLOR" {
				return "1", true
			}
			return "", false
		},
		MaskedSecretPrompt: func(prompt string) (string, error) { return askHostedLine(ask, out, prompt, true) }}
}

// Marker written through the captured standard output so a reader knows everything printed before it arrived
var stdoutSyncMark = []byte("\x00icebath-setup-sync\x00")

// Standard output of the process copied into the hosted output, with a wait for what was printed
type stdoutCapture struct {
	w      *os.File
	synced chan struct{}
	syncMu sync.Mutex
	errMu  sync.Mutex
	err    error
}

// Keeps the first failure of the capture
func (c *stdoutCapture) fail(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

// First failure copying or syncing standard output, nil when everything went through
func (c *stdoutCapture) failure() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Copies what the process prints on standard output into out until the returned stop runs
func captureStdout(out io.Writer) (*stdoutCapture, func(), error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	c := &stdoutCapture{w: w, synced: make(chan struct{})}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	write := func(p []byte) {
		_, err := out.Write(p)
		c.fail(err)
	}
	go func() {
		defer close(done)
		var pending []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			pending = append(pending, buf[:n]...)
			for {
				at := bytes.Index(pending, stdoutSyncMark)
				if at < 0 {
					break
				}
				write(pending[:at])
				pending = pending[at+len(stdoutSyncMark):]
				c.synced <- struct{}{}
			}
			if keep := len(stdoutSyncMark) - 1; err == nil && len(pending) > keep {
				write(pending[:len(pending)-keep])
				pending = pending[len(pending)-keep:]
			}
			if err != nil {
				write(pending)
				if !errors.Is(err, io.EOF) {
					c.fail(err)
				}
				return
			}
		}
	}()
	return c, func() {
		os.Stdout = prev
		c.fail(w.Close())
		<-done
		c.fail(r.Close())
	}, nil
}

// Waits until everything printed on standard output so far reached the hosted output
func (c *stdoutCapture) sync() {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if _, err := c.w.Write(stdoutSyncMark); err != nil {
		c.fail(err)
		return
	}
	<-c.synced
}

// HostSetupFlow runs fn with every setup prompt put to ask and every printed line written to out
func HostSetupFlow(asker SetupAsker, out io.Writer, fn func() error) (err error) {
	trail := &hostedOutput{w: out}
	defer trail.flush()
	printed := &hostedOutput{w: trail}
	if shows, ok := out.(progressWriter); ok {
		trail.progress, printed.progress = shows.Progress, shows.Progress
	}
	capture, restoreStdout, captureErr := captureStdout(printed)
	if captureErr != nil {
		return captureErr
	}
	ask := func(p HostedPrompt) HostedAnswer {
		capture.sync()
		printed.flush()
		trail.flush()
		return asker(p)
	}
	line := func(prompt string) (string, error) { return askHostedLine(ask, trail, prompt, false) }
	secret := func(prompt string) (string, error) { return askHostedLine(ask, trail, prompt, true) }
	outLine := func(_ io.Writer, prompt string) (string, error) { return line(prompt) }
	radiolist := func(title string, items [][]PickerSegment, opts PickerRadiolistOptions) (int, error) {
		return askHostedChoice(ask, title, pickerRowTexts(items), opts.Selected, opts.Description, opts.Headings, opts.CancelReturns), nil
	}
	choice := func(question string, choices []string, def int, description string) int {
		return askHostedChoice(ask, question, choices, def, description, nil, -1)
	}
	checklist := func(title string, items []string, pre []int) []int { return askHostedChecklist(ask, title, items, pre) }
	saved := struct {
		lineIn, pickerLine, authLine  func(string) (string, error)
		secretIn, authSecret, getpass func(string) (string, error)
		rawIn, pickerIn, authIn       func(io.Writer, string) (string, error)
		curses                        func(string, []string, int, string) (int, error)
		radio, pickerRadio            func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error)
		setupRadio                    func(string, []string, int, string) int
		setupCheck                    func(string, []string, map[int]bool) (map[int]bool, error)
		nousCheck                     func(string, []string, []int) ([]int, error)
		field                         func(string, string) (string, error)
		oauthPrompt                   func(string) (string, error)
		stdinTTY                      func() bool
		console                       func() *cliui.Console
		setupToken                    func(PoolEnv) (string, error)
		pkce, pluginOAuth, assignment io.Writer
	}{flowLineInputFn, pickerLineInputFn, authCmdLineInputFn, flowSecretInputFn, authCmdMaskedSecretPromptFn, secretGetpass,
		flowRawInputFn, pickerInputFn, authCmdInputFn, flowCursesChoiceFn, flowRadiolistFn, pickerRadiolistFn, setupRadiolist,
		setupChecklist, nousSubChecklistFn, secretFieldFn, oauthPromptFn, flowStdinIsTTYFn, pickerConsoleFn, anthRunOAuthSetupTokenFn,
		pkceOut, pluginOAuthOut, assignmentOut}
	menus := hostedConsole(trail, ask, true)
	plain := hostedConsole(trail, ask, false)
	flowLineInputFn, pickerLineInputFn, authCmdLineInputFn = line, line, line
	flowSecretInputFn, authCmdMaskedSecretPromptFn, secretGetpass = secret, secret, secret
	flowRawInputFn, pickerInputFn, authCmdInputFn = outLine, outLine, outLine
	flowCursesChoiceFn = func(question string, choices []string, def int, description string) (int, error) {
		return choice(question, choices, def, description), nil
	}
	flowRadiolistFn, pickerRadiolistFn = radiolist, radiolist
	setupRadiolist = choice
	setupChecklist = func(title string, items []string, pre map[int]bool) (map[int]bool, error) {
		chosen := map[int]bool{}
		for _, i := range checklist(title, items, setIndices(pre)) {
			chosen[i] = true
		}
		return chosen, nil
	}
	nousSubChecklistFn = func(title string, items []string, pre []int) ([]int, error) { return checklist(title, items, pre), nil }
	secretFieldFn = func(prompt, _ string) (string, error) { return secret(prompt) }
	oauthPromptFn = func(prompt string) (string, error) {
		text, err := line(prompt)
		return text + "\n", err
	}
	flowStdinIsTTYFn = func() bool { return true }
	pickerConsoleFn = func() *cliui.Console { return menus }
	anthRunOAuthSetupTokenFn = func(env PoolEnv) (string, error) { return hostedSetupToken(env, ask) }
	pkceOut, pluginOAuthOut, assignmentOut = trail, trail, trail
	openBrowser := openBrowserFn
	openBrowserFn = func(target string) error {
		if !canOpenGraphicalBrowser() {
			return errBrowserNotOpened
		}
		return openBrowser(target)
	}
	restoreOverrides := installHostedOverrides(ask)
	restoreAWS := installHostedAWSPrompts(secret)
	restoreDefault := cliui.SwapDefault(plain)
	restoreConsole := WithSetupConsole(plain)
	hostedMu.Lock()
	hostedAsk = ask
	hostedMu.Unlock()
	defer func() {
		hostedMu.Lock()
		hostedAsk = nil
		hostedMu.Unlock()
		restoreStdout()
		printed.flush()
		err = errors.Join(err, capture.failure())
		restoreConsole()
		restoreDefault()
		restoreOverrides()
		restoreAWS()
		flowLineInputFn, pickerLineInputFn, authCmdLineInputFn = saved.lineIn, saved.pickerLine, saved.authLine
		flowSecretInputFn, authCmdMaskedSecretPromptFn, secretGetpass = saved.secretIn, saved.authSecret, saved.getpass
		flowRawInputFn, pickerInputFn, authCmdInputFn = saved.rawIn, saved.pickerIn, saved.authIn
		flowCursesChoiceFn, flowRadiolistFn, pickerRadiolistFn = saved.curses, saved.radio, saved.pickerRadio
		setupRadiolist, setupChecklist, nousSubChecklistFn = saved.setupRadio, saved.setupCheck, saved.nousCheck
		secretFieldFn, oauthPromptFn, flowStdinIsTTYFn, pickerConsoleFn = saved.field, saved.oauthPrompt, saved.stdinTTY, saved.console
		anthRunOAuthSetupTokenFn, openBrowserFn = saved.setupToken, openBrowser
		pkceOut, pluginOAuthOut, assignmentOut = saved.pkce, saved.pluginOAuth, saved.assignment
	}()
	return fn()
}

// Sorted indices of a preselection set
func setIndices(set map[int]bool) []int {
	out := []int{}
	for i, on := range set {
		if on {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Claude setup token run on the user's terminal through the host, then the credentials it left behind
func hostedSetupToken(env PoolEnv, ask SetupAsker) (string, error) {
	claudePath := findClaudeCodeCLI("claude")
	if claudePath == "" {
		return "", claudeCLIMissingError{}
	}
	ran, err := askHostedCommand(ask, []string{claudePath, "setup-token"})
	if err != nil {
		return "", err
	}
	if !ran && errors.Is(env.ctx().Err(), context.Canceled) {
		return "", nil
	}
	return claudeSetupTokenResult(env), nil
}
