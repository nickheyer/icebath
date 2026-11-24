package cron

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Reports slot values that fail validation
type BlueprintFillError struct{ Msg string }

func (e *BlueprintFillError) Error() string { return e.Msg }

var blueprintSlotTypes = map[string]bool{"time": true, "enum": true, "text": true, "weekdays": true}

// BlueprintSlot is one fillable field on a blueprint, Options a closed set only when Strict
type BlueprintSlot struct {
	Name     string
	Type     string
	Label    string
	Default  any
	Options  []any
	Optional bool
	Help     string
	Strict   bool
}

// Slot with its type checked against the renderer slot types
func NewBlueprintSlot(s BlueprintSlot) (BlueprintSlot, error) {
	if !blueprintSlotTypes[s.Type] {
		return s, fmt.Errorf("unknown slot type %s (slot %s)", strconv.Quote(s.Type), s.Name)
	}
	return s, nil
}

// AutomationBlueprint is a parameterized automation, Plugin naming its registering plugin
type AutomationBlueprint struct {
	Key              string
	Title            string
	Description      string
	Category         string
	ScheduleTemplate string
	PromptTemplate   string
	Slots            []BlueprintSlot
	DeliverDefault   string
	Skills           []string
	Tags             []string
	Plugin           string
}

// Built-ins then the active profile plugin blueprints, resolved per call
func listBlueprints(ctx context.Context) []AutomationBlueprint {
	return append(append([]AutomationBlueprint(nil), blueprintCatalog...), pluginBlueprints(ctx)...)
}

// First slot named one of names or of the given type
func blueprintSlot(b *AutomationBlueprint, typ string, names ...string) *BlueprintSlot {
	for i := range b.Slots {
		s := &b.Slots[i]
		for _, n := range names {
			if s.Name == n {
				return s
			}
		}
		if typ != "" && s.Type == typ {
			return s
		}
	}
	return nil
}

func blueprintSlotDefault(b *AutomationBlueprint, names ...string) any {
	if s := blueprintSlot(b, "", names...); s != nil {
		return s.Default
	}
	return nil
}

func blueprintValueBlank(v any) bool { return v == nil || v == "" }

func blueprintValue(values map[string]any, s *BlueprintSlot) any {
	if v, ok := values[s.Name]; ok {
		return v
	}
	return s.Default
}

// Short readable description of when the blueprint runs with its defaults
func humanizeBlueprintSchedule(b *AutomationBlueprint) string {
	sched := b.ScheduleTemplate
	if strings.HasPrefix(sched, "*/") {
		every := blueprintSlotDefault(b, "interval_min")
		if !anyutil.IsTrue(every) {
			every = strings.Fields(strings.SplitN(sched, "/", 3)[1])[0]
		}
		return fmt.Sprintf("every %s minutes", anyutil.String(every))
	}
	if strings.Contains(sched, "{interval_hours}") {
		v := blueprintSlotDefault(b, "interval_hours")
		if !anyutil.IsTrue(v) {
			v = "1"
		}
		every := anyutil.String(v)
		scope := ""
		if strings.Contains(sched, "* * 1-5") {
			scope = "weekdays, "
		}
		if every == "1" {
			return scope + "every hour"
		}
		return fmt.Sprintf("%severy %s hours", scope, every)
	}
	var when any
	if ts := blueprintSlot(b, "time"); ts != nil {
		when = ts.Default
	}
	if strings.Contains(sched, "* * 1-5") {
		if anyutil.IsTrue(when) {
			return "weekdays at " + anyutil.String(when)
		}
		return "every weekday"
	}
	if strings.Contains(sched, "{dow}") {
		scope := blueprintSlotDefault(b, "day", "recurrence")
		if !anyutil.IsTrue(scope) {
			scope = ""
		}
		if anyutil.IsTrue(scope) && anyutil.IsTrue(when) {
			return anyutil.String(scope) + " at " + anyutil.String(when)
		}
		if anyutil.IsTrue(when) {
			return "at " + anyutil.String(when)
		}
		return "on a schedule"
	}
	if anyutil.IsTrue(when) {
		return "daily at " + anyutil.String(when)
	}
	return "on a schedule"
}

var (
	blueprintTimeRe        = regexutil.Lazy(regexp.MustCompile, `^([01]?\d|2[0-3]):([0-5]\d)$`)
	blueprintPlaceholderRe = regexp.MustCompile(`\{([\p{L}\p{N}_]+)\}`)
	blueprintDayToDow      = map[string]string{"sunday": "0", "monday": "1", "tuesday": "2", "wednesday": "3", "thursday": "4", "friday": "5", "saturday": "6"}
)

// Error of a template fill, a missing value becoming the fill error with the given prefix
func blueprintFormatErr(err error, prefix string) error {
	var me *templateutil.MissingError
	if errors.As(err, &me) {
		return &BlueprintFillError{Msg: prefix + me.Name}
	}
	return err
}

// Decimal text of the whole number s spells, an error when it is not one
func wholeNumberText(s string) (string, error) {
	n, err := textutil.ParseInt(s)
	if err != nil {
		return "", fmt.Errorf("%s is not a whole number", s)
	}
	return strconv.FormatInt(n, 10), nil
}

// Schedule template filled from resolved slot values
func resolveBlueprintSchedule(b *AutomationBlueprint, values map[string]any) (string, error) {
	sched := b.ScheduleTemplate
	if v := values["schedule"]; anyutil.IsTrue(v) {
		return anyutil.String(v), nil
	}
	repl := map[string]any{}
	if strings.Contains(sched, "{minute}") || strings.Contains(sched, "{hour}") {
		timeVal := values["time"]
		if !anyutil.IsTrue(timeVal) {
			return "", &BlueprintFillError{Msg: "a time is required"}
		}
		m := blueprintTimeRe().FindStringSubmatch(strings.TrimSpace(anyutil.String(timeVal)))
		if m == nil {
			return "", &BlueprintFillError{Msg: fmt.Sprintf("invalid time %s — use HH:MM (24h)", anyutil.Quote(timeVal))}
		}
		hour, err := wholeNumberText(m[1])
		if err != nil {
			return "", err
		}
		minute, err := wholeNumberText(m[2])
		if err != nil {
			return "", err
		}
		repl["hour"], repl["minute"] = hour, minute
	}
	if strings.Contains(sched, "{dow}") {
		if rv, ok := values["recurrence"]; ok {
			preset := strings.ToLower(anyutil.String(rv))
			dow, known := weekdayPresets[preset]
			if !known {
				return "", &BlueprintFillError{Msg: fmt.Sprintf("unknown recurrence %s — one of %s", strconv.Quote(preset), strings.Join(weekdayPresetOrder, ", "))}
			}
			repl["dow"] = dow
		} else if dv, ok := values["day"]; ok {
			day := strings.ToLower(anyutil.String(dv))
			dow, known := blueprintDayToDow[day]
			if !known {
				return "", &BlueprintFillError{Msg: fmt.Sprintf("unknown day %s", strconv.Quote(day))}
			}
			repl["dow"] = dow
		} else {
			repl["dow"] = "*"
		}
	}
	if strings.Contains(sched, "{interval_min}") {
		iv := ""
		if v, ok := values["interval_min"]; ok {
			iv = anyutil.String(v)
		}
		iv = strings.TrimSpace(iv)
		if !textutil.IsDigits(iv) {
			return "", &BlueprintFillError{Msg: fmt.Sprintf("invalid interval %s — minutes as a positive integer", strconv.Quote(iv))}
		}
		n, err := wholeNumberText(iv)
		if err != nil {
			return "", err
		}
		if strings.TrimLeft(n, "0") == "" {
			return "", &BlueprintFillError{Msg: fmt.Sprintf("invalid interval %s — minutes as a positive integer", strconv.Quote(iv))}
		}
		repl["interval_min"] = iv
	}
	for _, m := range blueprintPlaceholderRe.FindAllStringSubmatch(sched, -1) {
		if _, done := repl[m[1]]; done {
			continue
		}
		if v, ok := values[m[1]]; ok {
			repl[m[1]] = anyutil.String(v)
		}
	}
	out, err := templateutil.Format(sched, repl)
	if err != nil {
		return "", blueprintFormatErr(err, "schedule template missing value for ")
	}
	return out, nil
}

// Validated values as the create_job keyword arguments, naming any missing or unknown slot
func fillBlueprint(b *AutomationBlueprint, values map[string]any, origin any) (*jsonutil.Object, error) {
	known := map[string]bool{}
	names := make([]string, 0, len(b.Slots))
	for _, s := range b.Slots {
		known[s.Name] = true
		names = append(names, s.Name)
	}
	var unknown []string
	for k := range values {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		plural := ""
		if len(unknown) > 1 {
			plural = "s"
		}
		return nil, &BlueprintFillError{Msg: fmt.Sprintf("unknown slot%s: %s — valid: %s", plural, strings.Join(unknown, ", "), strings.Join(names, ", "))}
	}
	resolved := map[string]any{}
	for i := range b.Slots {
		s := &b.Slots[i]
		raw := blueprintValue(values, s)
		if blueprintValueBlank(raw) {
			if s.Optional {
				continue
			}
			return nil, &BlueprintFillError{Msg: fmt.Sprintf("missing required value: %s (%s)", s.Name, s.Label)}
		}
		if s.Type == "enum" && s.Strict && len(s.Options) > 0 {
			allowed, opts := false, make([]string, 0, len(s.Options))
			for _, o := range s.Options {
				opts = append(opts, anyutil.String(o))
				allowed = allowed || anyutil.String(o) == anyutil.String(raw)
			}
			if !allowed {
				return nil, &BlueprintFillError{Msg: fmt.Sprintf("%s=%s not allowed — one of %s", s.Name, anyutil.Quote(raw), strings.Join(opts, ", "))}
			}
		}
		resolved[s.Name] = raw
	}
	schedule, err := resolveBlueprintSchedule(b, resolved)
	if err != nil {
		return nil, err
	}
	prompt, err := templateutil.Format(b.PromptTemplate, resolved)
	if err != nil {
		return nil, blueprintFormatErr(err, "blueprint prompt missing value for ")
	}
	spec := jsonutil.NewObject()
	spec.Set("prompt", prompt)
	spec.Set("schedule", schedule)
	spec.Set("name", b.Title)
	if d, ok := resolved["deliver"]; ok {
		spec.Set("deliver", d)
	} else {
		spec.Set("deliver", b.DeliverDefault)
	}
	if len(b.Skills) > 0 {
		spec.Set("skills", stringsToAny(b.Skills))
	}
	if origin != nil {
		spec.Set("origin", origin)
	}
	return spec, nil
}
