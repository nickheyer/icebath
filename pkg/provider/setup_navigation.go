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
	fallback := ""
	if pyrepr.Truthy(def) {
		fallback = pyrepr.Str(def)
		display = question + " [" + fallback + "]: "
	}
	display = c.Color(display, cliui.Yellow)
	var value string
	var err error
	if password {
		value, err = c.MaskedSecret(display)
	} else {
		value, err = c.LineInputDefault(display, question, fallback)
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

// One answered setup prompt, the prompt title that identifies it and the value it resolved to
type setupAnswer struct {
	key   string
	value any
}

// Per invocation navigation state of the synchronous setup wizard
type setupNavigationState struct {
	sectionIndex, promptIndex, activePromptIndex int
	activeKey                                    string
	guardCancel                                  bool
	resolvedChoices, replayChoices               []setupAnswer
}

// Fresh state reset to the section with the prompt counter set afterwards
func newSetupNavigationState(sectionIndex, promptIndex int) *setupNavigationState {
	s := &setupNavigationState{}
	s.reset(sectionIndex, nil)
	s.promptIndex = promptIndex
	return s
}

// Rewinds the per section counters when entering a section or leaving the wizard
func (s *setupNavigationState) reset(sectionIndex int, replay []setupAnswer) {
	s.sectionIndex, s.promptIndex, s.activePromptIndex, s.activeKey = sectionIndex, 0, -1, ""
	s.resolvedChoices = []setupAnswer{}
	s.replayChoices = deepCopyChoices(replay)
}

func deepCopyChoices(values []setupAnswer) []setupAnswer {
	out := make([]setupAnswer, 0, len(values))
	for _, v := range values {
		out = append(out, setupAnswer{key: v.key, value: pyjson.Clone(v.value)})
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

// Control flow of a cancelled setup wizard, raised as a panic so provider error handlers never swallow it
type setupCancelled struct{}

func (setupCancelled) Error() string { return "" }

// Control flow returning to an earlier setup choice, raised as a panic like the cancel
type setupGoBack struct{ promptIndex int }

func (e setupGoBack) Error() string { return strconv.Itoa(e.promptIndex) }

// Translates the shared prompt events into setup control flow, replaying an answer only to the prompt that gave it
func handleSetupMenuNavigation(event cliui.MenuNavigationEvent, value ...any) (*cliui.MenuNavigationStart, error) {
	state := currentSetupNavigation()
	if state == nil {
		return nil, nil
	}
	switch event {
	case cliui.NavEventBegin:
		if state.sectionIndex < 0 {
			state.activePromptIndex = -1
			return &cliui.MenuNavigationStart{GuardCancel: state.guardCancel}, nil
		}
		key := ""
		if len(value) > 0 {
			key, _ = value[0].(string)
		}
		idx := state.promptIndex
		state.activePromptIndex, state.activeKey = idx, key
		state.promptIndex++
		allowBack := state.sectionIndex > 0 || idx > 0
		if idx < len(state.replayChoices) {
			if recorded := state.replayChoices[idx]; recorded.key == key {
				return &cliui.MenuNavigationStart{AllowBack: allowBack, Replay: true, ReplayValue: pyjson.Clone(recorded.value)}, nil
			}
			state.replayChoices = state.replayChoices[:idx]
		}
		return &cliui.MenuNavigationStart{AllowBack: allowBack, GuardCancel: state.guardCancel}, nil
	case cliui.NavEventResolve:
		if idx := state.activePromptIndex; idx >= 0 {
			var resolved any
			if len(value) > 0 {
				resolved = pyjson.Clone(value[0])
			}
			if idx < len(state.resolvedChoices) {
				state.resolvedChoices = state.resolvedChoices[:idx]
			}
			state.resolvedChoices = append(state.resolvedChoices, setupAnswer{key: state.activeKey, value: resolved})
		}
		return nil, nil
	case cliui.NavEventCancel:
		panic(setupCancelled{})
	case cliui.NavEventBack:
		panic(setupGoBack{promptIndex: state.activePromptIndex})
	}
	return nil, nil
}

// Navigation scope whose prompts ask for a second Escape before cancelling when guard is set
func guardedNavigationScope(guard bool, fn func()) {
	setupNavMu.Lock()
	prev := setupNavigation
	setupNavigation = newSetupNavigationState(-1, 0)
	setupNavigation.guardCancel = guard
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
	answersBySection := map[int][]setupAnswer{}
	replayBySection := map[int][]setupAnswer{}
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
func RunWithSetupNavigation(fn func() error, cancelledMessage string) error {
	return runWithNavigation(false, fn, cancelledMessage)
}

// RunWizardWithSetupNavigation is the navigation scope of the whole wizard, where leaving takes a second Escape
func RunWizardWithSetupNavigation(fn func() error, cancelledMessage string) error {
	return runWithNavigation(true, fn, cancelledMessage)
}

// Runs fn under a navigation scope, a cancel printing the message instead of unwinding
func runWithNavigation(guard bool, fn func() error, cancelledMessage string) (err error) {
	guardedNavigationScope(guard, func() {
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

// Yes or no answer, headless runs taking the default, inside setup and the TUI through the menu
func PromptYesNo(question string, def bool, description string) bool {
	if IsNoninteractive() && hostedAsker() == nil {
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

// SwapSetupRadiolist replaces the setup menu, the returned func restoring the previous one
func SwapSetupRadiolist(fn func(question string, choices []string, def int, description string) int) func() {
	prev := setupRadiolist
	setupRadiolist = fn
	return func() { setupRadiolist = prev }
}

// SetupOutput is the writer the setup console prints on
func SetupOutput() io.Writer { return setupUI().Out }
