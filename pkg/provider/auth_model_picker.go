package provider

import (
	"context"
	"fmt"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

const (
	pickerCustomLabel   = "Enter custom model name"
	pickerSkipLabel     = "Skip (keep current)"
	pickerCurrentSuffix = "  ← currently in use"
	pickerDefaultTitle  = "Select default model:"
	pickerCostCol       = len("Large")
	pickerGaugeGap      = "  "
	pickerANSIDim       = "\033[2m"
	pickerANSIReset     = "\033[0m"
	pickerANSIYellow    = "\033[33m"
)

// PickerSegment is one styled run of a picker row, an empty style drawing plain
type PickerSegment struct {
	Text  string
	Style string
}

// PickerRadiolistOptions carries the cursor start, cancel index, description, search labels and section headings
type PickerRadiolistOptions struct {
	Selected      int
	CancelReturns int
	Description   string
	Searchable    bool
	SearchLabels  []string
	Headings      map[int]string
}

// ModelSelectionPrompt is everything the post login model picker shows
type ModelSelectionPrompt struct {
	ModelIDs        []string
	Pricing         PricingTable
	ConfirmProvider string
	ConfirmBaseURL  string
	ConfirmAPIKey   string
	CurrentModel    string
	Notes           map[string]string
	Title           string
}

// Terminal seams the picker draws and reads through, swappable so tests script the menu and the answers
var (
	pickerRadiolistFn         = cursesRadiolist
	pickerInputFn             = pickerStdinInput
	pickerLineInputFn         = lineInput
	pickerSelectionWarningsFn = modelSelectionWarnings
)

var modelChoiceSave atomic.Int64

// Reads one answer line, in an inline field on a terminal and after the printed prompt otherwise
func pickerStdinInput(out io.Writer, prompt string) (string, error) {
	c := pickerConsoleFn()
	if c.Interactive() {
		return c.LineInput(prompt)
	}
	fmt.Fprint(out, prompt)
	return c.ReadLine()
}

// Asks once before a model that trips any selection guard, true to proceed
func confirmSelectionGuards(env PoolEnv, out io.Writer, modelID, provider, baseURL, apiKey string, includeKinds []string) bool {
	warnings, err := pickerSelectionWarningsFn(env, modelID, provider, baseURL, apiKey, includeKinds)
	if err != nil || len(warnings) == 0 {
		return true
	}
	rule := strings.Repeat("=", 72)
	fmt.Fprintln(out)
	fmt.Fprintln(out, rule)
	fmt.Fprintln(out, combinedSelectionMessage(warnings))
	fmt.Fprintln(out, rule)
	if answer, ran, cancelled := flowMenuAnswer("Switch anyway?", []string{"No", "Yes"}, []string{"n", "y"}, 0); ran {
		return !cancelled && answer == "y"
	}
	response, err := pickerInputFn(out, "Switch anyway? [y/N]: ")
	if err != nil {
		fmt.Fprintln(out)
		return false
	}
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes"
}

type pickerPrice struct {
	in, out, cache string
}

// Column aligned picker rows with dollars per Mtok when the catalog prices them
type modelPickerRows struct {
	currentModel string
	notes        map[string]string
	hasPricing   bool
	hasCache     bool
	nameCol      int
	priceCol     int
	cacheCol     int
	priceCache   map[string]pickerPrice
}

func newModelPickerRows(allModels []string, pricing PricingTable, currentModel string, notes map[string]string) *modelPickerRows {
	r := &modelPickerRows{currentModel: currentModel, notes: notes, priceCache: map[string]pickerPrice{}}
	if r.notes == nil {
		r.notes = map[string]string{}
	}
	longest := 0
	for _, m := range allModels {
		longest = max(longest, len([]rune(m)))
	}
	for _, m := range allModels {
		r.hasPricing = r.hasPricing || len(pricing[m]) > 0
	}
	if r.hasPricing {
		r.nameCol = longest + 2
	}
	r.priceCol = 3
	if !r.hasPricing {
		return r
	}
	for _, mid := range allModels {
		var e pickerPrice
		if p := pricing[mid]; len(p) > 0 {
			e.in = FormatPricePerMtok(rawPriceString(pricingField(p, "prompt")))
			e.out = FormatPricePerMtok(rawPriceString(pricingField(p, "completion")))
			if cacheRead := pricingField(p, "input_cache_read"); truthy(cacheRead) {
				e.cache = FormatPricePerMtok(rawPriceString(cacheRead))
			}
			r.hasCache = r.hasCache || e.cache != ""
		}
		r.priceCache[mid] = e
		r.priceCol = max(r.priceCol, len([]rune(e.in)), len([]rune(e.out)))
		r.cacheCol = max(r.cacheCol, len([]rune(e.cache)))
	}
	if r.hasCache {
		r.cacheCol = max(r.cacheCol, 5)
	}
	return r
}

// Row field with the empty string standing in for a missing key
func pricingField(row map[string]any, key string) any {
	if v, ok := row[key]; ok {
		return v
	}
	return ""
}

// Styled row with prices, note and current marker
func (r *modelPickerRows) segments(mid string) []PickerSegment {
	var current, note []PickerSegment
	if mid == r.currentModel {
		current = []PickerSegment{{pickerCurrentSuffix, ""}}
	}
	if r.notes[mid] != "" {
		note = []PickerSegment{{"  · " + r.notes[mid], "dim"}}
	}
	if !r.hasPricing {
		return append(append([]PickerSegment{{mid, ""}}, note...), current...)
	}
	e := r.priceCache[mid]
	segs := []PickerSegment{{textutil.PadRight(mid, r.nameCol), ""}}
	pricePart := " " + textutil.PadLeft(e.in, r.priceCol) + "  " + textutil.PadLeft(e.out, r.priceCol)
	if r.hasCache {
		pricePart += "  " + textutil.PadLeft(e.cache, r.cacheCol)
	}
	segs = append(segs, PickerSegment{pricePart, ""})
	return append(append(segs, note...), current...)
}

func (r *modelPickerRows) label(mid string) string {
	var b strings.Builder
	for _, s := range r.segments(mid) {
		b.WriteString(s.Text)
	}
	return b.String()
}

// Price column headers between the title and the list
func (r *modelPickerRows) descriptionLines() []string {
	pad := strings.Repeat(" ", 5)
	var lines []string
	if r.hasPricing {
		header := pad + strings.Repeat(" ", r.nameCol) + " " + textutil.PadLeft("In", r.priceCol) + "  " + textutil.PadLeft("Out", r.priceCol)
		if r.hasCache {
			header += "  " + textutil.PadLeft("Cache", r.cacheCol)
		}
		header += "  $/Mtok"
		lines = append(lines, header)
	}
	return lines
}

func (r *modelPickerRows) menuTitle(title string) string {
	if lines := r.descriptionLines(); len(lines) > 0 {
		return title + "\n" + strings.Join(lines, "\n")
	}
	return title
}

func dedupeStrings(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range ids {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// Ordered ids with the current model first and the cursor start
func pickerOrder(modelIDs []string, currentModel string) ([]string, int) {
	var lead []string
	if currentModel != "" && containsString(modelIDs, currentModel) {
		lead = []string{currentModel}
	}
	ordered := dedupeStrings(append(lead, modelIDs...))
	start := 0
	for i, m := range ordered {
		if m == currentModel {
			start = i
			break
		}
	}
	return ordered, start
}

// Prints the numbered fallback title, rows, custom and skip entries
func printNumberedRows(out io.Writer, rows *modelPickerRows, menuTitle string, ordered []string) {
	n := len(ordered)
	for _, line := range splitLines(menuTitle) {
		fmt.Fprintln(out, line)
	}
	numWidth := len(strconv.Itoa(n + 2))
	for i, mid := range ordered {
		fmt.Fprintf(out, "  %*d. %s\n", numWidth, i+1, formatRadioItemANSI(rows.segments(mid)))
	}
	fmt.Fprintf(out, "  %*d. %s\n", numWidth, n+1, pickerCustomLabel)
	fmt.Fprintf(out, "  %*d. %s\n", numWidth, n+2, pickerSkipLabel)
}

// Interactive model picker returning the confirmed choice, false on skip, cancel or a declined guard
func promptModelSelection(env PoolEnv, out io.Writer, p ModelSelectionPrompt) (string, bool) {
	title := p.Title
	if title == "" {
		title = pickerDefaultTitle
	}
	confirmed := func(mid string) (string, bool) {
		if mid == "" {
			return "", false
		}
		var kinds []string
		if p.ConfirmProvider == "" {
			kinds = []string{"data_policy"}
		}
		if !confirmSelectionGuards(env, out, mid, p.ConfirmProvider, p.ConfirmBaseURL, p.ConfirmAPIKey, kinds) {
			return "", false
		}
		return mid, true
	}
	custom := func() (string, bool) {
		text, err := pickerLineInputFn("Enter model name: ")
		if err != nil {
			return "", false
		}
		return confirmed(strings.TrimSpace(text))
	}
	ordered, start := pickerOrder(p.ModelIDs, p.CurrentModel)
	rows := newModelPickerRows(ordered, p.Pricing, p.CurrentModel, p.Notes)
	menuTitle := rows.menuTitle(title)
	n := len(ordered)

	choices := make([][]PickerSegment, 0, n+2)
	for _, mid := range ordered {
		choices = append(choices, rows.segments(mid))
	}
	choices = append(choices, []PickerSegment{{pickerCustomLabel, ""}}, []PickerSegment{{pickerSkipLabel, ""}})
	desc := rows.descriptionLines()
	searchLabels := make([]string, 0, n+2)
	for _, mid := range ordered {
		label, haystack := rows.label(mid), ModelSearchText(mid)
		if haystack != mid {
			label += " " + haystack
		}
		searchLabels = append(searchLabels, label)
	}
	searchLabels = append(searchLabels, pickerCustomLabel, pickerSkipLabel)
	opts := PickerRadiolistOptions{Selected: start, CancelReturns: -1, Description: strings.Join(desc, "\n"),
		Searchable: true, SearchLabels: searchLabels}
	if idx, err := pickerRadiolistFn(title, choices, opts); err == nil {
		if idx < 0 {
			return "", false
		}
		fmt.Fprintln(out)
		if idx < n {
			return confirmed(ordered[idx])
		}
		if idx == n {
			return custom()
		}
		return "", false
	}

	printNumberedRows(out, rows, menuTitle, ordered)
	fmt.Fprintln(out)
	for {
		choice, err := pickerInputFn(out, fmt.Sprintf("Choice [1-%d] (default: skip): ", n+2))
		if err != nil {
			return "", false
		}
		choice = strings.TrimSpace(choice)
		if choice == "" {
			return "", false
		}
		idx, ok := textutil.ParseIntAnyScript(choice)
		if !ok {
			fmt.Fprintln(out, "Please enter a number")
			continue
		}
		switch {
		case 1 <= idx && idx <= int64(n):
			return confirmed(ordered[idx-1])
		case idx == int64(n)+1:
			return custom()
		case idx == int64(n)+2:
			return "", false
		}
		fmt.Fprintf(out, "Please enter 1-%d\n", n+2)
	}
}

// ModelChoiceSaveCount is how many picks were saved in this process
func ModelChoiceSaveCount() int { return int(modelChoiceSave.Load()) }

// Saves the chosen model as model.default of the env's profile through the settings writer, never the env file
func saveModelChoice(env PoolEnv, modelID string) error {
	modelChoiceSave.Add(1)
	_, err := settings.Set(home.WithOverride(context.Background(), envHome(env)), settings.Model.Key, modelID, settings.Global)
	return err
}
