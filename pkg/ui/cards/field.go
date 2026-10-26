package cards

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"github.com/charmbracelet/x/ansi"
)

// Kind of value a field takes
type fieldKind int

const (
	textField fieldKind = iota
	secretField
	choiceField
	numberField
	areaField
)

// One input of a dialog or a settings row, its value kept as text and empty while unset
type field struct {
	key         string
	label       string
	help        string
	placeholder string
	// Heading the field sits under, fields sharing one forming a section
	group    string
	kind     fieldKind
	choices  []string
	value    string
	cursor   int
	lo, hi   float64
	step     float64
	start    float64
	integer  bool
	optional bool
	unit     func(float64) string
	show     func() bool
	check    func(string) error
	onChange func()
	// Editor of a multi line field, built by the sheet on first use
	area *textarea.Model
}

// Reports whether the field shows right now
func (f *field) visible() bool { return f.show == nil || f.show() }

// Reports whether the field is a switch, a choice between true and false
func (f *field) toggle() bool {
	return f.kind == choiceField && len(f.choices) == 2 && f.choices[0] == "true" && f.choices[1] == "false"
}

// Reports whether the switch is on
func (f *field) on() bool { return f.value == "true" }

// Turns the switch to its other side
func (f *field) flip() { f.set(strconv.FormatBool(!f.on())) }

// Number the text holds, false while unset or unparsable
func (f *field) number() (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(f.value), 64)
	return v, err == nil
}

// Text of a number at the precision of the step
func (f *field) format(v float64) string {
	if f.integer {
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	return strconv.FormatFloat(v, 'f', decimals(f.step), 64)
}

// Places after the point a step needs, at most six
func decimals(step float64) int {
	for d := range 6 {
		scaled := step * math.Pow(10, float64(d))
		if math.Abs(scaled-math.Round(scaled)) < 1e-9 {
			return d
		}
	}
	return 6
}

// Range end as the slider labels it
func (f *field) mark(v float64) string {
	if f.unit != nil {
		return f.unit(v)
	}
	return f.format(v)
}

// Reports whether the number has a ceiling, one below or at the floor meaning none
func (f *field) bounded() bool { return f.hi > f.lo }

// Holds the multiple of the step nearest v, kept at or above the floor and under any ceiling
func (f *field) setNumber(v float64) {
	if f.step > 0 {
		v = math.Round(v/f.step) * f.step
	}
	v = max(v, f.lo)
	if f.bounded() {
		v = min(v, f.hi)
	}
	f.set(f.format(v))
}

// Replaces the value with the cursor at its end and tells the listener
func (f *field) set(v string) {
	f.value = v
	f.cursor = len([]rune(v))
	if f.onChange != nil {
		f.onChange()
	}
}

// Picks the neighbouring choice, steps the number from its start when unset, or moves the cursor
func (f *field) nudge(dir int) {
	switch f.kind {
	case choiceField:
		if len(f.choices) == 0 {
			return
		}
		i := slices.Index(f.choices, f.value)
		j := 0
		if i >= 0 {
			j = min(max(i+dir, 0), len(f.choices)-1)
		}
		if f.choices[j] != f.value {
			f.set(f.choices[j])
		}
	case numberField:
		if v, ok := f.number(); ok {
			f.setNumber(v + float64(dir)*f.step)
			return
		}
		f.setNumber(f.start)
	default:
		f.cursor = min(max(f.cursor+dir, 0), len([]rune(f.value)))
	}
}

// Inserts typed text at the cursor, a space flipping a switch and a choice jumping to the next match
func (f *field) typeText(text string) {
	text = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
	if f.kind == choiceField {
		if f.toggle() && strings.TrimSpace(text) == "" {
			f.flip()
			return
		}
		n := len(f.choices)
		from := slices.Index(f.choices, f.value)
		for k := 1; k <= n; k++ {
			c := f.choices[((from+k)%n+n)%n]
			if strings.HasPrefix(strings.ToLower(c), strings.ToLower(text)) {
				f.set(c)
				return
			}
		}
		return
	}
	if f.kind == numberField {
		text = strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' || r == '.' || r == '-' {
				return r
			}
			return -1
		}, text)
	}
	if text == "" {
		return
	}
	r := []rune(f.value)
	at := min(f.cursor, len(r))
	f.value = string(r[:at]) + text + string(r[at:])
	f.cursor = at + len([]rune(text))
	if f.onChange != nil {
		f.onChange()
	}
}

// Edits the text around the cursor by a form editing action, reporting whether it was one
func (f *field) edit(id string) bool {
	if f.kind == choiceField {
		return false
	}
	r := []rune(f.value)
	at := min(f.cursor, len(r))
	switch id {
	case "delete.back":
		if at == 0 {
			return true
		}
		f.value, f.cursor = string(r[:at-1])+string(r[at:]), at-1
	case "delete.word":
		i := at
		for i > 0 && r[i-1] == ' ' {
			i--
		}
		for i > 0 && r[i-1] != ' ' {
			i--
		}
		f.value, f.cursor = string(r[:i])+string(r[at:]), i
	case "clear":
		f.value, f.cursor = "", 0
	case "home":
		f.cursor = 0
		return true
	case "end":
		f.cursor = len(r)
		return true
	default:
		return false
	}
	if f.onChange != nil {
		f.onChange()
	}
	return true
}

// Leaves an optional field empty, reporting whether it is one
func (f *field) unset() bool {
	if !f.optional {
		return false
	}
	f.set("")
	return true
}

// Problem with the value, a required field left empty or what its check refuses
func (f *field) problem() string {
	v := strings.TrimSpace(f.value)
	if v == "" {
		if f.optional || f.kind == choiceField {
			return ""
		}
		return f.label + " is required"
	}
	if f.kind == numberField {
		n, ok := f.number()
		if !ok {
			return f.label + " must be a number"
		}
		if f.bounded() && (n < f.lo || n > f.hi) {
			return f.label + " must lie between " + f.format(f.lo) + " and " + f.format(f.hi)
		}
		if n < f.lo {
			return f.label + " must be at least " + f.format(f.lo)
		}
		if f.integer && n != math.Trunc(n) {
			return f.label + " must be a whole number"
		}
	}
	if f.check != nil {
		if err := f.check(v); err != nil {
			return err.Error()
		}
	}
	return ""
}

// Columns the number well needs for the floor, the value and any ceiling
func (f *field) boxWidth() int {
	w := max(ansi.StringWidth(f.format(f.lo)), ansi.StringWidth(f.value))
	if f.bounded() {
		w = max(w, ansi.StringWidth(f.format(f.hi)))
	}
	return w
}

// Applies a click on a hit at column x, picking a choice, flipping a switch, placing the slider or the cursor
func (f *field) click(h hit, x int) {
	switch {
	case h.choice >= 0 && h.choice < len(f.choices):
		if f.choices[h.choice] != f.value {
			f.set(f.choices[h.choice])
		}
	case h.choice == hitFlip:
		f.flip()
	case h.choice == hitTrack:
		span := max(h.x1-h.x0-1, 1)
		f.setNumber(f.lo + float64(x-h.x0)/float64(span)*(f.hi-f.lo))
	case h.choice == hitBox && f.kind == areaField:
		f.placeCursor(h.row, x-h.x0-1)
	case h.choice == hitBox && f.kind != numberField:
		f.cursor = min(max(x-h.x0-1, 0), len([]rune(f.value)))
	}
}
