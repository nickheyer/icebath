package cron

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// BlueprintFillError reports slot values that fail validation, a ValueError in hermes
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
		return s, fmt.Errorf("unknown slot type %s (slot %s)", pyrepr.StrRepr(s.Type), s.Name)
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

// Built-in blueprint or a namespaced plugin blueprint of the active profile, nil when unknown
func getBlueprint(ctx context.Context, key string) *AutomationBlueprint {
	for i := range blueprintCatalog {
		if blueprintCatalog[i].Key == key {
			b := blueprintCatalog[i]
			return &b
		}
	}
	if strings.Contains(key, ":") {
		for _, b := range pluginBlueprints(ctx) {
			if b.Key == key {
				return &b
			}
		}
	}
	return nil
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

// JSON a dashboard or GUI form renderer needs for the blueprint
func blueprintFormSchema(b *AutomationBlueprint) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("key", b.Key)
	o.Set("title", b.Title)
	o.Set("description", b.Description)
	o.Set("category", b.Category)
	o.Set("tags", stringsToAny(b.Tags))
	fields := make([]any, 0, len(b.Slots))
	for _, s := range b.Slots {
		f := pyjson.NewObject()
		f.Set("name", s.Name)
		f.Set("type", s.Type)
		f.Set("label", s.Label)
		f.Set("default", s.Default)
		f.Set("options", append([]any{}, s.Options...))
		f.Set("optional", s.Optional)
		f.Set("strict", s.Strict)
		f.Set("help", s.Help)
		fields = append(fields, f)
	}
	o.Set("fields", fields)
	return o
}

func blueprintValueBlank(v any) bool { return v == nil || v == "" }

func blueprintValue(values map[string]any, s *BlueprintSlot) any {
	if v, ok := values[s.Name]; ok {
		return v
	}
	return s.Default
}

// Ready to paste slash command with each slot value or default, text slots quoted
func blueprintSlashCommand(b *AutomationBlueprint, values map[string]any) string {
	parts := []string{"/blueprint " + b.Key}
	for i := range b.Slots {
		s := &b.Slots[i]
		val := blueprintValue(values, s)
		if blueprintValueBlank(val) {
			if s.Optional {
				continue
			}
			val = ""
		}
		sval := pyrepr.Str(val)
		if s.Type == "text" || strings.Contains(sval, " ") {
			sval = `"` + strings.ReplaceAll(sval, `"`, `\"`) + `"`
		}
		parts = append(parts, s.Name+"="+sval)
	}
	return strings.Join(parts, " ")
}

// App deep link URL for the blueprint with its non blank slot values as the query
func blueprintDeeplink(b *AutomationBlueprint, values map[string]any) string {
	var query [][2]string
	seen := map[string]int{}
	for i := range b.Slots {
		s := &b.Slots[i]
		val := blueprintValue(values, s)
		if blueprintValueBlank(val) {
			continue
		}
		if at, ok := seen[s.Name]; ok {
			query[at][1] = pyrepr.Str(val)
			continue
		}
		seen[s.Name] = len(query)
		query = append(query, [2]string{s.Name, pyrepr.Str(val)})
	}
	qs := ""
	if len(query) > 0 {
		qs = "?" + pyurl.URLEncode(query)
	}
	return "icebath://blueprint/" + pyurl.Quote(b.Key, "/") + qs
}

// Short readable description of when the blueprint runs with its defaults
func humanizeBlueprintSchedule(b *AutomationBlueprint) string {
	sched := b.ScheduleTemplate
	if strings.HasPrefix(sched, "*/") {
		every := blueprintSlotDefault(b, "interval_min")
		if !pyrepr.Truthy(every) {
			every = strings.Fields(strings.SplitN(sched, "/", 3)[1])[0]
		}
		return fmt.Sprintf("every %s minutes", pyrepr.Str(every))
	}
	if strings.Contains(sched, "{interval_hours}") {
		v := blueprintSlotDefault(b, "interval_hours")
		if !pyrepr.Truthy(v) {
			v = "1"
		}
		every := pyrepr.Str(v)
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
		if pyrepr.Truthy(when) {
			return "weekdays at " + pyrepr.Str(when)
		}
		return "every weekday"
	}
	if strings.Contains(sched, "{dow}") {
		scope := blueprintSlotDefault(b, "day", "recurrence")
		if !pyrepr.Truthy(scope) {
			scope = ""
		}
		if pyrepr.Truthy(scope) && pyrepr.Truthy(when) {
			return pyrepr.Str(scope) + " at " + pyrepr.Str(when)
		}
		if pyrepr.Truthy(when) {
			return "at " + pyrepr.Str(when)
		}
		return "on a schedule"
	}
	if pyrepr.Truthy(when) {
		return "daily at " + pyrepr.Str(when)
	}
	return "on a schedule"
}

// Unified serializable shape for docs and the dashboard API
func blueprintCatalogEntry(b *AutomationBlueprint) *pyjson.Object {
	o := blueprintFormSchema(b)
	if b.Plugin != "" {
		o.Set("source", "plugin")
		o.Set("plugin", b.Plugin)
	} else {
		o.Set("source", "builtin")
		o.Set("plugin", nil)
	}
	o.Set("schedule", b.ScheduleTemplate)
	o.Set("scheduleHuman", humanizeBlueprintSchedule(b))
	o.Set("command", blueprintSlashCommand(b, nil))
	o.Set("appUrl", blueprintDeeplink(b, nil))
	return o
}

var (
	blueprintTimeRe        = mustPyRe(`^([01]?\d|2[0-3]):([0-5]\d)$`)
	blueprintPlaceholderRe = regexp.MustCompile(`\{([\p{L}\p{N}_]+)\}`)
	blueprintDayToDow      = map[string]string{"sunday": "0", "monday": "1", "tuesday": "2", "wednesday": "3", "thursday": "4", "friday": "5", "saturday": "6"}
)

// Error of a str.format call, a missing key becoming the fill error with the given prefix
func blueprintFormatErr(err error, prefix string) error {
	var fe *pyrepr.FormatError
	if errors.As(err, &fe) && fe.Class == "KeyError" {
		return &BlueprintFillError{Msg: prefix + fe.Msg}
	}
	return err
}

func pyIntText(s string) (string, error) {
	n, ok := pystr.IntText(s)
	if !ok {
		return "", fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(s))
	}
	return n.String(), nil
}

// Schedule template filled from resolved slot values
func resolveBlueprintSchedule(b *AutomationBlueprint, values map[string]any) (string, error) {
	sched := b.ScheduleTemplate
	if v := values["schedule"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v), nil
	}
	repl := map[string]any{}
	if strings.Contains(sched, "{minute}") || strings.Contains(sched, "{hour}") {
		timeVal := values["time"]
		if !pyrepr.Truthy(timeVal) {
			return "", &BlueprintFillError{Msg: "a time is required"}
		}
		m, _ := blueprintTimeRe.FindStringMatch(pystr.Strip(pyrepr.Str(timeVal)))
		if m == nil {
			return "", &BlueprintFillError{Msg: fmt.Sprintf("invalid time %s — use HH:MM (24h)", pyrepr.Repr(timeVal))}
		}
		hour, err := pyIntText(m.GroupByNumber(1).String())
		if err != nil {
			return "", err
		}
		minute, err := pyIntText(m.GroupByNumber(2).String())
		if err != nil {
			return "", err
		}
		repl["hour"], repl["minute"] = hour, minute
	}
	if strings.Contains(sched, "{dow}") {
		if rv, ok := values["recurrence"]; ok {
			preset := pystr.Lower(pyrepr.Str(rv))
			dow, known := weekdayPresets[preset]
			if !known {
				return "", &BlueprintFillError{Msg: fmt.Sprintf("unknown recurrence %s — one of %s", pyrepr.StrRepr(preset), strings.Join(weekdayPresetOrder, ", "))}
			}
			repl["dow"] = dow
		} else if dv, ok := values["day"]; ok {
			day := pystr.Lower(pyrepr.Str(dv))
			dow, known := blueprintDayToDow[day]
			if !known {
				return "", &BlueprintFillError{Msg: fmt.Sprintf("unknown day %s", pyrepr.StrRepr(day))}
			}
			repl["dow"] = dow
		} else {
			repl["dow"] = "*"
		}
	}
	if strings.Contains(sched, "{interval_min}") {
		iv := ""
		if v, ok := values["interval_min"]; ok {
			iv = pyrepr.Str(v)
		}
		iv = pystr.Strip(iv)
		if !pystr.StrIsDigit(iv) {
			return "", &BlueprintFillError{Msg: fmt.Sprintf("invalid interval %s — minutes as a positive integer", pyrepr.StrRepr(iv))}
		}
		n, err := pyIntText(iv)
		if err != nil {
			return "", err
		}
		if strings.TrimLeft(n, "0") == "" {
			return "", &BlueprintFillError{Msg: fmt.Sprintf("invalid interval %s — minutes as a positive integer", pyrepr.StrRepr(iv))}
		}
		repl["interval_min"] = iv
	}
	for _, m := range blueprintPlaceholderRe.FindAllStringSubmatch(sched, -1) {
		if _, done := repl[m[1]]; done {
			continue
		}
		if v, ok := values[m[1]]; ok {
			repl[m[1]] = pyrepr.Str(v)
		}
	}
	out, err := pyrepr.FormatMap(sched, repl)
	if err != nil {
		return "", blueprintFormatErr(err, "schedule template missing value for ")
	}
	return out, nil
}

// Validated values as the create_job keyword arguments, naming any missing or unknown slot
func fillBlueprint(b *AutomationBlueprint, values map[string]any, origin any) (*pyjson.Object, error) {
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
				opts = append(opts, pyrepr.Str(o))
				allowed = allowed || pyrepr.Str(o) == pyrepr.Str(raw)
			}
			if !allowed {
				return nil, &BlueprintFillError{Msg: fmt.Sprintf("%s=%s not allowed — one of %s", s.Name, pyrepr.Repr(raw), strings.Join(opts, ", "))}
			}
		}
		resolved[s.Name] = raw
	}
	schedule, err := resolveBlueprintSchedule(b, resolved)
	if err != nil {
		return nil, err
	}
	prompt, err := pyrepr.FormatMap(b.PromptTemplate, resolved)
	if err != nil {
		return nil, blueprintFormatErr(err, "blueprint prompt missing value for ")
	}
	spec := pyjson.NewObject()
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
