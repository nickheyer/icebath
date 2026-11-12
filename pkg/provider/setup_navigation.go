package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// SetupExit is the sys.exit a setup prompt raises on Ctrl-C, recovered at the command boundary
type SetupExit struct{ Code int }

func (e SetupExit) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// RecoverSetupExit turns a SetupExit panic into the exit code, re-panicking anything else
func RecoverSetupExit(code *int) {
	if r := recover(); r != nil {
		if exit, ok := r.(SetupExit); ok {
			*code = exit.Code
			return
		}
		panic(r)
	}
}

var (
	setupConsoleMu sync.Mutex
	setupConsole   *cliui.Console
)

// Console the setup prompts print on, the process console unless a command swapped its output
func setupUI() *cliui.Console {
	setupConsoleMu.Lock()
	defer setupConsoleMu.Unlock()
	if setupConsole != nil {
		return setupConsole
	}
	return cliui.Default()
}

// WithSetupOutput routes setup prints to w for a command's duration, the returned func restores the previous console
func WithSetupOutput(w io.Writer) func() {
	base := setupUI()
	return WithSetupConsole(&cliui.Console{In: base.In, Out: w, Getenv: base.Getenv, StdinTTY: base.StdinTTY, StdoutTTY: base.StdoutTTY,
		TermSize: base.TermSize, MaskedSecretPrompt: base.MaskedSecretPrompt, OpenScreen: base.OpenScreen})
}

// WithSetupConsole makes c the console setup prompts read and print on, the returned func restores the previous one
func WithSetupConsole(c *cliui.Console) func() {
	setupConsoleMu.Lock()
	prev := setupConsole
	setupConsole = c
	setupConsoleMu.Unlock()
	return func() {
		setupConsoleMu.Lock()
		setupConsole = prev
		setupConsoleMu.Unlock()
	}
}

// SetupConsole is the console setup prompts read and print on
func SetupConsole() *cliui.Console { return setupUI() }

func setupPrintln(text string) { fmt.Fprintln(setupUI().Out, text) }

// PrintSetupHeader prints a cyan section header, gap adding an extra blank line before it
func PrintSetupHeader(title string, gap bool) {
	c := setupUI()
	if gap {
		setupPrintln("")
	}
	setupPrintln("")
	setupPrintln(c.Color("◆ "+title, cliui.Cyan, cliui.Bold))
}

// SetupInfo prints each line as info in order, a nil line printing a bare blank line
func SetupInfo(lines ...any) {
	for _, line := range lines {
		if line == nil {
			setupPrintln("")
			continue
		}
		setupUI().PrintInfo(pyrepr.Str(line))
	}
}

// SetupSectionRule prints the blank padded cyan divider of the key entry screens
func SetupSectionRule(title string) {
	setupPrintln("")
	setupPrintln(setupUI().Color("  ─── "+title+" ───", cliui.Cyan))
	setupPrintln("")
}

var bracketedPastePattern = regexp.MustCompile(`\x1b\[\s*200~|\x1b\[\s*201~`)

// Strips terminal bracketed paste control markers from pasted text
func sanitizePastedInput(value string) string {
	if value == "" {
		return value
	}
	return bracketedPastePattern.ReplaceAllString(value, "")
}

// SetupPrompt reads a line with an optional default, a password through the masked prompt, Ctrl-C or EOF exiting 1
func SetupPrompt(question, def string, password bool) string {
	return setupPromptValue(question, def, password).(string)
}

// Reads a line with a default of any type, the stripped answer, else a truthy default, else empty
func setupPromptValue(question string, def any, password bool) any {
	c := setupUI()
	display := question + ": "
	if pyrepr.Truthy(def) {
		display = question + " [" + pyrepr.Str(def) + "]: "
	}
	display = c.Color(display, cliui.Yellow)
	var value string
	var err error
	if password {
		value, err = c.MaskedSecret(display)
	} else {
		value, err = c.LineInput(display)
	}
	if err != nil {
		if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
			setupPrintln("")
			panic(SetupExit{Code: 1})
		}
		panic(err)
	}
	if trimmed := pystr.Strip(sanitizePastedInput(value)); trimmed != "" {
		return trimmed
	}
	if pyrepr.Truthy(def) {
		return def
	}
	return ""
}

// Per invocation navigation state of the synchronous setup wizard
type setupNavigationState struct {
	sectionIndex, promptIndex, activePromptIndex int
	resolvedChoices, replayChoices               []any
}

// Fresh state reset to the section with the prompt counter set afterwards
func newSetupNavigationState(sectionIndex, promptIndex int) *setupNavigationState {
	s := &setupNavigationState{}
	s.reset(sectionIndex, nil)
	s.promptIndex = promptIndex
	return s
}

// Rewinds the per section counters when entering a section or leaving the wizard
func (s *setupNavigationState) reset(sectionIndex int, replay []any) {
	s.sectionIndex, s.promptIndex, s.activePromptIndex = sectionIndex, 0, -1
	s.resolvedChoices = []any{}
	s.replayChoices = deepCopyChoices(replay)
}

func deepCopyChoices(values []any) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, pyjson.Clone(v))
	}
	return out
}

var (
	setupNavMu      sync.Mutex
	setupNavigation *setupNavigationState
)

func currentSetupNavigation() *setupNavigationState {
	setupNavMu.Lock()
	defer setupNavMu.Unlock()
	return setupNavigation
}

// Setup navigation raised as a panic so provider error handlers never swallow it, marked for the metrics classifiers
type setupControlFlow interface {
	error
	setupControlFlow()
}

// Control flow of a cancelled setup wizard
type setupCancelled struct{}

func (setupCancelled) Error() string     { return "" }
func (setupCancelled) SetupCancelled()   {}
func (setupCancelled) setupControlFlow() {}

// Control flow returning to an earlier setup choice
type setupGoBack struct{ promptIndex int }

func (e setupGoBack) Error() string   { return strconv.Itoa(e.promptIndex) }
func (setupGoBack) SetupGoBack()      {}
func (setupGoBack) setupControlFlow() {}

// Reports whether a recovered panic value is setup navigation that must keep unwinding
func isSetupControlFlow(r any) bool {
	_, ok := r.(setupControlFlow)
	return ok
}

// Translates the shared menu events into setup control flow
func handleSetupMenuNavigation(event cliui.MenuNavigationEvent, value ...any) (*cliui.MenuNavigationStart, error) {
	state := currentSetupNavigation()
	if state == nil {
		return nil, nil
	}
	switch event {
	case cliui.NavEventBegin:
		if state.sectionIndex < 0 {
			state.activePromptIndex = -1
			return &cliui.MenuNavigationStart{}, nil
		}
		idx := state.promptIndex
		state.activePromptIndex = idx
		state.promptIndex++
		allowBack := state.sectionIndex > 0 || idx > 0
		if idx < len(state.replayChoices) {
			return &cliui.MenuNavigationStart{AllowBack: allowBack, Replay: true, ReplayValue: pyjson.Clone(state.replayChoices[idx])}, nil
		}
		return &cliui.MenuNavigationStart{AllowBack: allowBack}, nil
	case cliui.NavEventResolve:
		if idx := state.activePromptIndex; idx >= 0 {
			var resolved any
			if len(value) > 0 {
				resolved = pyjson.Clone(value[0])
			}
			if idx < len(state.resolvedChoices) {
				state.resolvedChoices = state.resolvedChoices[:idx]
			}
			state.resolvedChoices = append(state.resolvedChoices, resolved)
		}
		return nil, nil
	case cliui.NavEventCancel:
		panic(setupCancelled{})
	case cliui.NavEventBack:
		panic(setupGoBack{promptIndex: state.activePromptIndex})
	}
	return nil, nil
}

// HandleSetupMenuNavigation is the menu event handler a setup scope installs, cancel and back raised as setup control flow
func HandleSetupMenuNavigation(event cliui.MenuNavigationEvent, value ...any) (*cliui.MenuNavigationStart, error) {
	return handleSetupMenuNavigation(event, value...)
}

// Installs the setup navigation state and menu handler around fn and restores both after
func setupNavigationScope(fn func()) {
	setupNavMu.Lock()
	prev := setupNavigation
	setupNavigation = newSetupNavigationState(-1, 0)
	setupNavMu.Unlock()
	token := cliui.SetMenuNavigationHandler(handleSetupMenuNavigation)
	defer func() {
		cliui.ResetMenuNavigationHandler(token)
		setupNavMu.Lock()
		setupNavigation = prev
		setupNavMu.Unlock()
	}()
	fn()
}

// SetupStep is one labelled section of the wizard
type SetupStep struct {
	Label  string
	Action func() error
}

// Runs one action, reporting a left arrow return instead of letting it unwind further
func runSetupSection(action func() error) (nav *setupGoBack, err error) {
	defer func() {
		if r := recover(); r != nil {
			if back, ok := r.(setupGoBack); ok {
				nav = &back
				return
			}
			panic(r)
		}
	}()
	return nil, action()
}

// Runs sections with left arrow navigation, back from a first choice reopening the previous section
func runSetupSteps(steps []SetupStep) error {
	state := currentSetupNavigation()
	sectionIndex := 0
	answersBySection := map[int][]any{}
	replayBySection := map[int][]any{}
	record := func() {
		if state != nil {
			answersBySection[sectionIndex] = deepCopyChoices(state.resolvedChoices)
		}
	}
	defer func() {
		if state != nil {
			state.reset(-1, nil)
		}
	}()
	for sectionIndex < len(steps) {
		step := steps[sectionIndex]
		if state != nil {
			replay := replayBySection[sectionIndex]
			delete(replayBySection, sectionIndex)
			state.reset(sectionIndex, replay)
		}
		nav, err := runSetupSection(step.Action)
		if err != nil {
			return err
		}
		record()
		if nav == nil {
			sectionIndex++
			continue
		}
		var previousIndex, targetPrompt int
		if nav.promptIndex > 0 {
			previousIndex, targetPrompt = sectionIndex, nav.promptIndex-1
		} else {
			previousIndex = max(0, sectionIndex-1)
			targetPrompt = max(0, len(answersBySection[previousIndex])-1)
		}
		previous := answersBySection[previousIndex]
		replayBySection[previousIndex] = deepCopyChoices(previous[:min(targetPrompt, len(previous))])
		setupPrintln("")
		if previousIndex == sectionIndex {
			setupUI().PrintInfo(fmt.Sprintf("Returning to the previous choice in %s...", step.Label))
		} else {
			setupUI().PrintInfo(fmt.Sprintf("Returning to %s...", steps[previousIndex].Label))
		}
		sectionIndex = previousIndex
	}
	return nil
}

// RunWithSetupNavigation runs fn under a navigation scope, a cancel printing the message instead of unwinding
func RunWithSetupNavigation(fn func() error, cancelledMessage string) (err error) {
	setupNavigationScope(func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(setupCancelled); ok {
					SetupInfo(nil, cancelledMessage)
					return
				}
				panic(r)
			}
		}()
		err = fn()
	})
	return err
}

// Runs steps under a navigation scope, a cancel printing the message instead of unwinding
func runSetupStepsWithNavigation(steps []SetupStep, cancelledMessage string) error {
	return RunWithSetupNavigation(func() error { return runSetupSteps(steps) }, cancelledMessage)
}

// RunSetupSteps runs labelled sections with left arrow navigation between and inside them
func RunSetupSteps(steps []SetupStep) error { return runSetupSteps(steps) }

// SetupNavigationSectionIndex is the section index of the running setup navigation, false outside a setup scope
func SetupNavigationSectionIndex() (int, bool) {
	state := currentSetupNavigation()
	if state == nil {
		return 0, false
	}
	return state.sectionIndex, true
}

// RunSetupActionWithNavigation runs a setup style menu flow with Escape and nested Left navigation outside the wizard
func RunSetupActionWithNavigation(ctx context.Context, label string, action func(ctx context.Context) error, cancelledMessage string) error {
	return runSetupStepsWithNavigation([]SetupStep{{Label: label, Action: func() error { return action(ctx) }}}, cancelledMessage)
}

// Single select menu through the shared radiolist, a seam the tests replace, cancel answering -1
var setupRadiolist = func(question string, choices []string, def int, description string) int {
	idx, err := CursesPromptChoice(question, choices, def, description)
	if err != nil {
		if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
			setupPrintln("")
			panic(SetupExit{Code: 1})
		}
		panic(err)
	}
	return idx
}

// PromptChoice asks for one of the choices with arrow keys, a cancel outside setup keeping the default
func PromptChoice(question string, choices []string, def int, description string) int {
	idx := setupRadiolist(question, choices, def, description)
	if idx < 0 {
		return def
	}
	if idx == def {
		SetupInfo("  Skipped (keeping current)", nil)
		return def
	}
	setupPrintln("")
	return idx
}

// IsNoninteractive reports a spawn with no human to answer prompts, ICEBATH_NONINTERACTIVE truthy
func IsNoninteractive() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_NONINTERACTIVE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// PromptYesNo asks yes or no, headless and closed stdin answering the default, inside setup through the menu
func PromptYesNo(question string, def bool, description string) bool {
	if IsNoninteractive() {
		return def
	}
	if currentSetupNavigation() != nil {
		selected := 1
		if def {
			selected = 0
		}
		return setupRadiolist(question, []string{"Yes", "No"}, selected, description) == 0
	}
	defaultStr := "y/N"
	if def {
		defaultStr = "Y/n"
	}
	c := setupUI()
	for {
		value, err := c.Input(c.Color(question+" ["+defaultStr+"]: ", cliui.Yellow))
		if err != nil {
			if errors.Is(err, cliui.ErrInterrupt) {
				setupPrintln("")
				panic(SetupExit{Code: 1})
			}
			if errors.Is(err, cliui.ErrEOF) {
				setupPrintln("")
				return def
			}
			panic(err)
		}
		switch pystr.Lower(pystr.Strip(value)) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		c.PrintError("Please enter 'y' or 'n'")
	}
}

// Multi select menu through the shared checklist on the setup console, a seam the tests replace
var setupChecklist = func(title string, items []string, pre map[int]bool) (map[int]bool, error) {
	return setupUI().CursesChecklist(context.Background(), title, items, pre, pre, nil)
}

// SwapSetupChecklist replaces the setup checklist, the returned func restoring the previous one
func SwapSetupChecklist(fn func(title string, items []string, pre map[int]bool) (map[int]bool, error)) func() {
	prev := setupChecklist
	setupChecklist = fn
	return func() { setupChecklist = prev }
}

// PromptChecklist asks for several items, returning sorted picked indices, cancel keeping the preselection
func PromptChecklist(title string, items []string, preSelected []int) []int {
	pre := map[int]bool{}
	for _, i := range preSelected {
		pre[i] = true
	}
	chosen, err := setupChecklist(title, items, pre)
	if err != nil {
		if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
			setupPrintln("")
			panic(SetupExit{Code: 1})
		}
		panic(err)
	}
	out := make([]int, 0, len(chosen))
	for i, on := range chosen {
		if on {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// SetupPrintInfo prints a dim indented line on the setup console
func SetupPrintInfo(text string) { setupUI().PrintInfo(text) }

// SetupPrintSuccess prints a green check line on the setup console
func SetupPrintSuccess(text string) { setupUI().PrintSuccess(text) }

// SetupPrintWarning prints a yellow warning line on the setup console
func SetupPrintWarning(text string) { setupUI().PrintWarning(text) }

// SetupPrintError prints a red cross line on the setup console
func SetupPrintError(text string) { setupUI().PrintError(text) }

// SetupModuleInstalled reports whether the dependency environment finds a Python module, false when the probe fails
func SetupModuleInstalled(ctx context.Context, name string) (found bool) {
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	return pm.AnchorImportable(ctx, name)
}

// SetupNavigationScope runs fn with the setup navigation state and menu handler installed
func SetupNavigationScope(fn func()) { setupNavigationScope(fn) }

// SwapSetupRadiolist replaces the setup menu, the returned func restoring the previous one
func SwapSetupRadiolist(fn func(question string, choices []string, def int, description string) int) func() {
	prev := setupRadiolist
	setupRadiolist = fn
	return func() { setupRadiolist = prev }
}

// SetupOutput is the writer the setup console prints on
func SetupOutput() io.Writer { return setupUI().Out }
