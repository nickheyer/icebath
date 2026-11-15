package cliui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/i18n"
)

func (c *Console) println(text string) { fmt.Fprintln(c.Out, text) }

// PrintInfo prints a dim indented line
func (c *Console) PrintInfo(text string) { c.println(c.Color("  "+text, Dim)) }

// PrintSuccess prints a green check line
func (c *Console) PrintSuccess(text string) { c.println(c.Color("✓ "+text, Green)) }

// PrintWarning prints a yellow warning line
func (c *Console) PrintWarning(text string) { c.println(c.Color("⚠ "+text, Yellow)) }

// PrintError prints a red cross line
func (c *Console) PrintError(text string) { c.println(c.Color("✗ "+text, Red)) }

// PrintHeader prints a yellow header after a blank line
func (c *Console) PrintHeader(text string) { c.println(c.Color("\n  "+text, Yellow)) }

// PrintTruncated prints the footer of a capped listing, nil more meaning at least one hidden row
func (c *Console) PrintTruncated(ctx context.Context, more *int, hint string) {
	count := i18n.T(ctx, "cli.shared.more_not_shown", "", nil)
	if more != nil {
		count = i18n.T(ctx, "cli.shared.n_more", "", map[string]any{"count": strconv.Itoa(*more)})
	}
	suffix := ""
	if hint != "" {
		suffix = " (" + hint + ")"
	}
	c.println(c.Color("  … "+count+suffix, Dim))
}

// LineInput reads one line in an inline field with editing keys on a terminal and as plain input otherwise
func (c *Console) LineInput(prompt string) (string, error) {
	return c.ask(inputSpec{title: cleanTitle(prompt)}, func() (string, error) { return c.input(prompt) })
}

// LineInputDefault reads one line whose field shows the value a bare Enter keeps
func (c *Console) LineInputDefault(prompt, title, fallback string) (string, error) {
	return c.ask(inputSpec{title: cleanTitle(title), fallback: fallback}, func() (string, error) { return c.input(prompt) })
}

// SecretLine reads one hidden line in an inline masked field, errEditorUnavailable off a capable terminal
func (c *Console) SecretLine(prompt, mask string) (string, error) {
	if mask != "" {
		mask = c.glyphs().mask
	}
	return c.ask(inputSpec{title: cleanTitle(prompt), secret: true, mask: mask}, func() (string, error) { return "", errEditorUnavailable })
}

// ErrNoEditor reports a terminal that cannot draw an inline field
var ErrNoEditor = errEditorUnavailable

// Runs one inline field under scoped navigation, replaying a recorded answer, plain taking over when the terminal cannot draw
func (c *Console) ask(spec inputSpec, plain func() (string, error)) (string, error) {
	if !c.interactive() {
		return plain()
	}
	handler := navHandler(nil)
	var start *MenuNavigationStart
	if handler != nil {
		s, err := handler(NavEventBegin, spec.title)
		if err != nil {
			return "", err
		}
		start = s
	}
	if start != nil && start.Replay {
		if text, ok := start.ReplayValue.(string); ok {
			if _, err := handler(NavEventResolve, text); err != nil {
				return "", err
			}
			replay := &inputPrompt{c: c, spec: spec}
			replay.ed.insert(text)
			region := c.openRegion()
			region.close(replay.answer(inputDone, false))
			return text, nil
		}
	}
	spec.cancel = handler != nil
	spec.back, spec.guardEsc = start != nil && start.AllowBack, start != nil && start.GuardCancel
	field := &inputPrompt{c: c, spec: spec}
	text, outcome, err := field.run()
	if errors.Is(err, errEditorUnavailable) {
		return plain()
	}
	if err != nil {
		return "", err
	}
	switch outcome {
	case inputCancelled:
		if _, err := handler(NavEventCancel); err != nil {
			return "", err
		}
		return "", ErrInterrupt
	case inputBack:
		if _, err := handler(NavEventBack); err != nil {
			return "", err
		}
		return "", ErrInterrupt
	}
	if handler != nil {
		if _, err := handler(NavEventResolve, text); err != nil {
			return "", err
		}
	}
	return text, nil
}

// Prompt reads a stripped answer or the default on plain Enter, empty after printing a newline on Ctrl-C or EOF
func (c *Console) Prompt(question, def string, password bool) string {
	suffix := ""
	if def != "" {
		suffix = " [" + def + "]"
	}
	display := c.Color("  "+question+suffix+": ", Yellow)
	var value string
	var err error
	if password {
		value, err = c.secretPrompt()(display)
	} else {
		value, err = c.LineInput(display)
	}
	if err != nil {
		if errors.Is(err, ErrInterrupt) || errors.Is(err, ErrEOF) {
			c.println("")
			return ""
		}
		panic(err)
	}
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	return def
}

// PromptYesNo asks a localized yes or no question, accepting y or the localized affirmative initial
func (c *Console) PromptYesNo(ctx context.Context, question string, def bool) bool {
	hint := i18n.T(ctx, "cli.shared.yes_no_default_no", "", nil)
	if def {
		hint = i18n.T(ctx, "cli.shared.yes_no_default_yes", "", nil)
	}
	answer := c.Prompt(question+" ("+hint+")", "", false)
	if answer == "" {
		return def
	}
	yes := strings.ToLower(strings.TrimSpace(i18n.T(ctx, "cli.shared.yes_initial", "", nil)))
	low := strings.ToLower(answer)
	return strings.HasPrefix(low, "y") || (yes != "" && strings.HasPrefix(low, yes))
}

var errEditorUnavailable = errors.New("line editor unavailable")

var yesNoPromptRe = regexp.MustCompile(`^(.*?)\s*[\[(]\s*([Yy])\s*/\s*([Nn])\s*[\])]\s*:?\s*$`)

// Input reads one line like Python input, inside a setup presentation as a yes or no list or a field
func (c *Console) Input(prompt string) (string, error) {
	if !c.IsFlow() {
		return c.input(prompt)
	}
	m := yesNoPromptRe.FindStringSubmatch(ansi.Strip(prompt))
	if m == nil {
		return c.ask(inputSpec{title: cleanTitle(prompt)}, func() (string, error) { return c.input(prompt) })
	}
	selected := 0
	if m[3] == "N" {
		selected = 1
	}
	cancel := -1
	idx, err := c.CursesRadiolist(context.Background(), m[1], []RadioItem{PlainItem("Yes"), PlainItem("No")}, selected,
		RadiolistOptions{CancelReturns: &cancel})
	switch {
	case err != nil:
		return "", err
	case idx < 0:
		return "", ErrInterrupt
	}
	return []string{"y", "n"}[idx], nil
}
