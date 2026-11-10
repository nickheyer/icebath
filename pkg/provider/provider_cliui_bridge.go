package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/i18n"
)

// Routes the language env read through the profile secret scope and password prompts through the masked reader
func init() {
	i18n.SetSecretReader(func(ctx context.Context, name string) (string, bool, error) {
		v, ok, err := GetSecret(ctx, name)
		var unscoped *UnscopedSecretError
		if errors.As(err, &unscoped) {
			return "", false, fmt.Errorf("%w: %v", i18n.ErrUnscopedSecret, err)
		}
		return v, ok, err
	})
	cliui.SetMaskedSecretPrompt(func(prompt string) (string, error) {
		v, err := maskedSecretPrompt(prompt)
		switch {
		case errors.Is(err, ErrSecretPromptInterrupted):
			return "", cliui.ErrInterrupt
		case errors.Is(err, io.EOF):
			return "", cliui.ErrEOF
		}
		return v, err
	})
}

// Translates a catalog key in the explicit or active language with str.format kwargs
func i18nT(key, lang string, kwargs map[string]any) string {
	return i18n.T(context.Background(), key, lang, kwargs)
}

// Wraps text in ANSI codes when the process console takes color
func cliColor(text string, codes ...string) string { return cliui.Color(text, codes...) }

// Colors usage gauges in a header or legend line
func colorizeGaugeBarsANSI(line string) string { return cliui.Default().ColorizeGaugeBarsANSI(line) }

func pickerRadioItem(segs []PickerSegment) cliui.RadioItem {
	item := make(cliui.RadioItem, len(segs))
	for i, s := range segs {
		item[i] = cliui.Segment{Text: s.Text, Style: s.Style}
	}
	return item
}

// Applies ANSI colors to a styled picker row
func formatRadioItemANSI(segs []PickerSegment) string {
	return cliui.Default().FormatRadioItemANSI(pickerRadioItem(segs))
}

// Console the full screen menus draw on, the process console unless a test swapped it
var pickerConsoleFn = cliui.Default

// Runs the full screen radio list on the process console and returns the chosen original index
func cursesRadiolist(title string, choices [][]PickerSegment, opts PickerRadiolistOptions) (int, error) {
	items := make([]cliui.RadioItem, len(choices))
	for i, c := range choices {
		items[i] = pickerRadioItem(c)
	}
	cancel := opts.CancelReturns
	return pickerConsoleFn().CursesRadiolist(context.Background(), title, items, opts.Selected, cliui.RadiolistOptions{
		CancelReturns: &cancel, Description: opts.Description, Searchable: opts.Searchable,
		SearchLabels: opts.SearchLabels, Headings: opts.Headings,
	})
}

// Reads non secret text with cursor editing on a terminal, an error being EOF or interrupt
func lineInput(prompt string) (string, error) { return cliui.Default().LineInput(prompt) }

// Multi select checklist returning sorted indices, cancel keeping the preselection
func promptChecklist(title string, items []string, preSelected []int) ([]int, error) {
	pre := map[int]bool{}
	for _, i := range preSelected {
		pre[i] = true
	}
	chosen, err := cliui.Default().CursesChecklist(context.Background(), title, items, pre, pre, nil)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(chosen))
	for i := range chosen {
		out = append(out, i)
	}
	sort.Ints(out)
	return out, nil
}
