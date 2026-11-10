package provider

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
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

// Style per gateway level, Free green and Large yellow, every other level plain
var pickerLevelStyle = map[string]string{"free": "green", "top_tier": "yellow"}

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
	ModelIDs           []string
	Pricing            PricingTable
	UnavailableModels  []string
	PortalURL          string
	UnavailableMessage string
	ConfirmProvider    string
	ConfirmBaseURL     string
	ConfirmAPIKey      string
	Usage              any
	CurrentModel       string
	Notes              map[string]string
	Title              string
}

// Terminal seams the picker draws and reads through, swappable so tests script the menu and the answers
var (
	pickerRadiolistFn         = cursesRadiolist
	pickerInputFn             = pickerStdinInput
	pickerLineInputFn         = lineInput
	pickerSelectionWarningsFn = modelSelectionWarnings
)

var (
	pickerStdinOnce sync.Once
	pickerStdin     *bufio.Reader
	modelChoiceSave atomic.Int64
)

// Writes the prompt and reads one line of standard input without its newline
func pickerStdinInput(out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	pickerStdinOnce.Do(func() { pickerStdin = bufio.NewReader(os.Stdin) })
	line, err := pickerStdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
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
	response, err := pickerInputFn(out, "Switch anyway? [y/N]: ")
	if err != nil {
		fmt.Fprintln(out)
		return false
	}
	response = strings.ToLower(pyStripSpace(response))
	return response == "y" || response == "yes"
}

type pickerPrice struct {
	in, out, cache string
	pct            *int
	wasIn, wasOut  string
}

// Column aligned picker rows, Large or Free tags on a window plan and dollars per Mtok otherwise
type modelPickerRows struct {
	currentModel string
	notes        map[string]string
	windowMode   bool
	usage        *PickerUsage
	levels       map[string]string
	hasTopTier   bool
	hasPricing   bool
	hasCache     bool
	anyOnSale    bool
	nameCol      int
	priceCol     int
	cacheCol     int
	priceCache   map[string]pickerPrice
}

func newModelPickerRows(allModels []string, pricing PricingTable, currentModel string, saleChrome bool, usage *PickerUsage, notes map[string]string) *modelPickerRows {
	r := &modelPickerRows{currentModel: currentModel, notes: notes, windowMode: usage != nil, levels: map[string]string{}, priceCache: map[string]pickerPrice{}}
	if r.notes == nil {
		r.notes = map[string]string{}
	}
	longest := 0
	for _, m := range allModels {
		longest = max(longest, len([]rune(m)))
	}
	if r.windowMode {
		pricingAny := PricingTableAsAny(pricing)
		r.usage = usage.WithPricing(pricingAny)
		r.levels = usageTagLevels(allModels, pricingAny)
		for _, level := range r.levels {
			r.hasTopTier = r.hasTopTier || level == "top_tier"
		}
		r.nameCol = longest + 2
		return r
	}
	for _, m := range allModels {
		r.hasPricing = r.hasPricing || len(pricing[m]) > 0
	}
	namePad := 2
	if saleChrome {
		namePad = 3
	}
	if r.hasPricing {
		r.nameCol = longest + namePad
	}
	r.priceCol = 3
	if !r.hasPricing {
		return r
	}
	was := func(raw string) string {
		if raw != "" {
			return FormatPricePerMtok(raw)
		}
		return "?"
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
			if saleChrome {
				if sale, ok := ComputeSaleDiscount(rawPriceString(pricingField(p, "prompt")), rawPriceString(pricingField(p, "completion")), p["original"]); ok {
					r.anyOnSale = true
					pct := sale.Percent
					e.pct = &pct
					if sale.WasPrompt != "" || sale.WasCompletion != "" {
						e.wasIn, e.wasOut = was(sale.WasPrompt), was(sale.WasCompletion)
					}
				}
			}
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

// Window row as the padded name, dim when a full allowance stops it, then the Large or Free tag
func (r *modelPickerRows) costSegments(mid string) []PickerSegment {
	nameStyle := ""
	if r.usage.Blocks(mid) {
		nameStyle = "dim"
	}
	level := r.levels[mid]
	return []PickerSegment{{pickerLjust(mid, r.nameCol), nameStyle}, {pickerGaugeGap, ""},
		{pickerLjust(UsageLevelLabel(level, "en"), pickerCostCol), pickerLevelStyle[level]}}
}

// Styled row with sale star, prices, was prices, note and current marker, or the usage tag on a window plan
func (r *modelPickerRows) segments(mid string) []PickerSegment {
	var current, note []PickerSegment
	if mid == r.currentModel {
		current = []PickerSegment{{pickerCurrentSuffix, ""}}
	}
	if r.notes[mid] != "" {
		note = []PickerSegment{{"  · " + r.notes[mid], "dim"}}
	}
	if r.windowMode {
		return append(r.costSegments(mid), current...)
	}
	if !r.hasPricing {
		return append(append([]PickerSegment{{mid, ""}}, note...), current...)
	}
	e := r.priceCache[mid]
	var segs []PickerSegment
	if e.pct != nil {
		segs = []PickerSegment{{"★ ", "yellow"}, {pickerLjust(mid, r.nameCol-2), ""}}
	} else {
		segs = []PickerSegment{{pickerLjust(mid, r.nameCol), ""}}
	}
	pricePart := " " + pickerRjust(e.in, r.priceCol) + "  " + pickerRjust(e.out, r.priceCol)
	if r.hasCache {
		pricePart += "  " + pickerRjust(e.cache, r.cacheCol)
	}
	segs = append(segs, PickerSegment{pricePart, ""})
	if e.pct != nil {
		segs = append(segs, PickerSegment{fmt.Sprintf("  -%d%%", *e.pct), "yellow"})
		if e.wasIn != "" || e.wasOut != "" {
			segs = append(segs, PickerSegment{fmt.Sprintf("  was %s/%s", e.wasIn, e.wasOut), "dim"})
		}
	}
	return append(append(segs, note...), current...)
}

func (r *modelPickerRows) label(mid string) string {
	var b strings.Builder
	for _, s := range r.segments(mid) {
		b.WriteString(s.Text)
	}
	return b.String()
}

// Lines between the title and the list, plan gauges on a window plan or the price column headers
func (r *modelPickerRows) descriptionLines() []string {
	pad := strings.Repeat(" ", 5)
	var lines []string
	if r.windowMode {
		for i, line := range PlanGaugeLines(r.usage) {
			suffix := ""
			if i == 0 {
				suffix = " — " + r.usage.IncludedSummary()
			}
			lines = append(lines, pad+line+suffix)
		}
		return append(lines, pad+strings.Repeat(" ", r.nameCol)+pickerGaugeGap+"Usage")
	}
	if r.hasPricing {
		header := pad + strings.Repeat(" ", r.nameCol) + " " + pickerRjust("In", r.priceCol) + "  " + pickerRjust("Out", r.priceCol)
		if r.hasCache {
			header += "  " + pickerRjust("Cache", r.cacheCol)
		}
		header += "  $/Mtok"
		if r.anyOnSale {
			header += "  ★ = on sale"
		}
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

// The one line saying what Large means, only on a window plan listing a Large row
func (r *modelPickerRows) footnote() string {
	if !r.hasTopTier {
		return ""
	}
	return TopTierFootnote
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

// Ordered ids, section headings by row index and the cursor start, Recommended first on a window plan
func pickerOrder(modelIDs []string, currentModel string, windowUsage *PickerUsage) ([]string, map[int]string, int) {
	sections := map[int]string{}
	var ordered []string
	if windowUsage != nil {
		head, rest := splitRecommended(dedupeStrings(modelIDs), windowUsage.FeaturedModelIDs)
		ordered = append(head, rest...)
		if len(head) > 0 {
			sections[0] = SectionRecommended
			if len(rest) > 0 {
				sections[len(head)] = SectionOther
			}
		}
	} else {
		var lead []string
		if currentModel != "" && containsString(modelIDs, currentModel) {
			lead = []string{currentModel}
		}
		ordered = dedupeStrings(append(lead, modelIDs...))
	}
	start := 0
	for i, m := range ordered {
		if m == currentModel {
			start = i
			break
		}
	}
	return ordered, sections, start
}

// Prints the numbered fallback title, rows, custom and skip entries and footnote, returning the number width
func printNumberedRows(out io.Writer, rows *modelPickerRows, menuTitle string, ordered []string, sections map[int]string) int {
	n := len(ordered)
	for _, line := range pySplitLines(menuTitle) {
		if rows.windowMode {
			line = colorizeGaugeBarsANSI(line)
		}
		if strings.Contains(line, "★") {
			line = strings.Replace(line, "★", cliColor("★", pickerANSIYellow), 1)
		}
		fmt.Fprintln(out, line)
	}
	numWidth := len(strconv.Itoa(n + 2))
	for i, mid := range ordered {
		if heading, ok := sections[i]; ok {
			fmt.Fprintf(out, "  %s%s%s\n", pickerANSIDim, heading, pickerANSIReset)
		}
		fmt.Fprintf(out, "  %*d. %s\n", numWidth, i+1, formatRadioItemANSI(rows.segments(mid)))
	}
	fmt.Fprintf(out, "  %*d. %s\n", numWidth, n+1, pickerCustomLabel)
	fmt.Fprintf(out, "  %*d. %s\n", numWidth, n+2, pickerSkipLabel)
	if fn := rows.footnote(); fn != "" {
		fmt.Fprintf(out, "  %s%s%s\n", pickerANSIDim, fn, pickerANSIReset)
	}
	return numWidth
}

// Interactive model picker returning the confirmed choice, false on skip, cancel or a declined guard
func promptModelSelection(env PoolEnv, out io.Writer, p ModelSelectionPrompt) (string, bool) {
	title := p.Title
	if title == "" {
		title = pickerDefaultTitle
	}
	saleChrome := strings.ToLower(pyStripSpace(p.ConfirmProvider)) == "nous"
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
		return confirmed(pyStripSpace(text))
	}
	var windowUsage *PickerUsage
	if u, ok := p.Usage.(*PickerUsage); ok && u != nil && saleChrome {
		windowUsage = u
	}
	ordered, sections, start := pickerOrder(p.ModelIDs, p.CurrentModel, windowUsage)
	all := append(append([]string{}, ordered...), p.UnavailableModels...)
	rows := newModelPickerRows(all, p.Pricing, p.CurrentModel, saleChrome, windowUsage, p.Notes)
	menuTitle := rows.menuTitle(title)
	upgradeURL := strings.TrimRight(firstNonEmpty(p.PortalURL, DefaultNousPortalURL), "/")
	n := len(ordered)

	choices := make([][]PickerSegment, 0, n+2)
	for _, mid := range ordered {
		choices = append(choices, rows.segments(mid))
	}
	choices = append(choices, []PickerSegment{{pickerCustomLabel, ""}}, []PickerSegment{{pickerSkipLabel, ""}})
	footer := pyStripSpace(p.UnavailableMessage)
	if footer == "" && len(p.UnavailableModels) > 0 {
		footer = fmt.Sprintf("Upgrade at %s for paid models", upgradeURL)
	}
	desc := rows.descriptionLines()
	if len(p.UnavailableModels) > 0 {
		for _, mid := range p.UnavailableModels {
			desc = append(desc, "   "+rows.label(mid))
		}
		desc = append(desc, "  ── "+footer+" ──")
	}
	if fn := rows.footnote(); fn != "" {
		desc = append(desc, "     "+fn)
	}
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
	if len(sections) > 0 {
		opts.Headings = sections
	}
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

	numWidth := printNumberedRows(out, rows, menuTitle, ordered, sections)
	if len(p.UnavailableModels) > 0 {
		footer := pyStripSpace(p.UnavailableMessage)
		if footer == "" {
			footer = fmt.Sprintf("Unavailable models (requires paid tier — upgrade at %s)", upgradeURL)
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  %s── %s ──%s\n", pickerANSIDim, footer, pickerANSIReset)
		for _, mid := range p.UnavailableModels {
			fmt.Fprintf(out, "  %s  %s%s%s\n", strings.Repeat(" ", numWidth), pickerANSIDim, rows.label(mid), pickerANSIReset)
		}
	}
	fmt.Fprintln(out)
	for {
		choice, err := pickerInputFn(out, fmt.Sprintf("Choice [1-%d] (default: skip): ", n+2))
		if err != nil {
			return "", false
		}
		choice = pyStripSpace(choice)
		if choice == "" {
			return "", false
		}
		idx, ok := pyIntText(choice)
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

// Saves the chosen model as model.default in config.yaml only, never the env file
func saveModelChoice(env PoolEnv, modelID string) error {
	h := envHome(env)
	modelChoiceSave.Add(1)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	if model, ok := tree["model"].(map[string]any); ok {
		pyDictSet(model, "default", modelID)
	} else {
		pyDictSet(tree, "model", map[string]any{"default": modelID})
	}
	if err := config.Save(h, tree); err != nil {
		return err
	}
	noteProviderSetupSaved()
	return nil
}
