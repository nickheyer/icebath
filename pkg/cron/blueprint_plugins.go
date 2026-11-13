package cron

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var (
	blueprintKeyRe       = mustPyRe(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	blueprintNamespaceRe = mustPyRe(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	blueprintSlotNameRe  = mustPyRe(`^[A-Za-z_]\w{0,63}$`)
	blueprintSlotFields  = []string{"name", "type", "label", "default", "options", "optional", "help", "strict"}
	derivedScheduleSlot  = map[string]string{"minute": "time", "hour": "time", "dow": ""}
)

func pyReMatches(re interface {
	MatchString(string) (bool, error)
}, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

func blueprintText(name string, value any) (string, error) {
	s, ok := value.(string)
	if !ok || pystr.Strip(s) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return pystr.Strip(s), nil
}

// Members of an iterable of non empty strings, a string or non iterable value refused
func blueprintStrings(name string, value any) ([]string, error) {
	items, iterable := blueprintIterable(value)
	if _, isStr := value.(string); isStr || !iterable {
		return nil, fmt.Errorf("%s must be a list of strings", name)
	}
	out := make([]string, 0, len(items))
	for _, v := range items {
		s, ok := v.(string)
		if !ok || pystr.Strip(s) == "" {
			return nil, fmt.Errorf("%s must be a list of non-empty strings", name)
		}
		out = append(out, s)
	}
	return out, nil
}

// Members a Python iteration of a wire value yields, mapping keys in order, false for a non iterable
func blueprintIterable(value any) ([]any, bool) {
	switch t := value.(type) {
	case []any:
		return t, true
	case []string:
		return stringsToAny(t), true
	case *pyjson.Object:
		return stringsToAny(t.Keys), true
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return stringsToAny(keys), true
	}
	return nil, false
}

// Tuple of a wire options value, an empty one for a falsy value
func blueprintOptions(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		return append([]any(nil), t...), nil
	case []string:
		return stringsToAny(t), nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		return stringsToAny(t.Keys), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return stringsToAny(keys), nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// Slot from an in process BlueprintSlot or its field mapping, the plugin host wire form
func blueprintSlotFrom(raw any) (BlueprintSlot, error) {
	var slot BlueprintSlot
	var name, labelVal any
	switch t := raw.(type) {
	case BlueprintSlot:
		s, err := NewBlueprintSlot(t)
		if err != nil {
			return s, err
		}
		slot, name, labelVal = s, s.Name, s.Label
	case *BlueprintSlot:
		s, err := NewBlueprintSlot(*t)
		if err != nil {
			return s, err
		}
		slot, name, labelVal = s, s.Name, s.Label
	case map[string]any, *pyjson.Object:
		data := map[string]any{}
		if o, ok := t.(*pyjson.Object); ok {
			for _, k := range o.Keys {
				data[k] = o.Vals[k]
			}
		} else {
			data = t.(map[string]any)
		}
		var unknown []string
		for k := range data {
			if !containsString(blueprintSlotFields, k) {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return slot, fmt.Errorf("unknown slot field(s): %s", strings.Join(unknown, ", "))
		}
		if _, ok := data["options"]; ok {
			opts, err := blueprintOptions(data["options"])
			if err != nil {
				return slot, err
			}
			slot.Options = opts
		}
		var missing []string
		for _, f := range blueprintSlotFields[:3] {
			if _, ok := data[f]; !ok {
				missing = append(missing, "'"+f+"'")
			}
		}
		if len(missing) > 0 {
			return slot, fmt.Errorf("slot %s: BlueprintSlot.__init__() %s", pyrepr.Repr(data["name"]), missingArgsText(missing))
		}
		if typ, ok := data["type"].(string); !ok || !blueprintSlotTypes[typ] {
			return slot, fmt.Errorf("unknown slot type %s (slot %s)", pyrepr.Repr(data["type"]), pyrepr.Str(data["name"]))
		}
		name, labelVal = data["name"], data["label"]
		slot.Name, _ = data["name"].(string)
		slot.Label, _ = data["label"].(string)
		slot.Type = data["type"].(string)
		slot.Default = data["default"]
		slot.Optional = pyrepr.Truthy(data["optional"])
		slot.Strict = true
		if v, ok := data["strict"]; ok {
			slot.Strict = pyrepr.Truthy(v)
		}
		if v, ok := data["help"]; ok {
			slot.Help = pyrepr.Str(v)
		}
	default:
		return slot, fmt.Errorf("slot must be a mapping, got %s", pyrepr.TypeName(raw))
	}
	if s, ok := name.(string); !ok || !pyReMatches(blueprintSlotNameRe, s) {
		return slot, fmt.Errorf("invalid slot name %s", pyrepr.Repr(name))
	}
	if _, err := blueprintText("slot "+slot.Name+" label", labelVal); err != nil {
		return slot, err
	}
	return slot, nil
}

// TypeError text of missing required dataclass arguments
func missingArgsText(names []string) string {
	switch len(names) {
	case 1:
		return "missing 1 required positional argument: " + names[0]
	case 2:
		return "missing 2 required positional arguments: " + names[0] + " and " + names[1]
	}
	return fmt.Sprintf("missing %d required positional arguments: %s, and %s", len(names), strings.Join(names[:len(names)-1], ", "), names[len(names)-1])
}

// Placeholder names of a template, each required to be a plain slot name
func blueprintPlaceholders(template, what string) ([]string, error) {
	names, err := pyrepr.FormatFieldNames(template)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if !pyReMatches(blueprintSlotNameRe, n) {
			return nil, fmt.Errorf("%s placeholder {%s} must be a plain slot name", what, n)
		}
	}
	return names, nil
}

// Rejects duplicate slots and unknown placeholders, filling a fully defaulted blueprint to check its schedule
func validatePluginBlueprint(b *AutomationBlueprint) error {
	byName := map[string]*BlueprintSlot{}
	for i := range b.Slots {
		byName[b.Slots[i].Name] = &b.Slots[i]
	}
	if len(byName) != len(b.Slots) {
		return fmt.Errorf("slot names must be unique")
	}
	names, err := blueprintPlaceholders(b.PromptTemplate, "prompt_template")
	if err != nil {
		return err
	}
	for _, n := range names {
		slot := byName[n]
		if slot == nil {
			return fmt.Errorf("prompt_template uses {%s} but there is no slot named %s", n, pyrepr.StrRepr(n))
		}
		if slot.Optional && blueprintValueBlank(slot.Default) {
			return fmt.Errorf("prompt_template uses optional slot %s that has no default", pyrepr.StrRepr(n))
		}
	}
	if names, err = blueprintPlaceholders(b.ScheduleTemplate, "schedule_template"); err != nil {
		return err
	}
	for _, n := range names {
		if needed, derived := derivedScheduleSlot[n]; derived {
			if needed != "" && byName[needed] == nil {
				return fmt.Errorf("schedule_template uses {%s}, which needs a slot named %s", n, pyrepr.StrRepr(needed))
			}
		} else if byName[n] == nil {
			return fmt.Errorf("schedule_template uses {%s} but there is no slot named %s", n, pyrepr.StrRepr(n))
		}
	}
	for _, s := range b.Slots {
		if !s.Optional && blueprintValueBlank(s.Default) {
			return nil
		}
	}
	spec, err := fillBlueprint(b, map[string]any{}, nil)
	if err != nil {
		return err
	}
	sched, _ := spec.Get("schedule")
	_, err = openCronStore(home.Dir(context.Background()), nil).parseSchedule(sched.(string))
	return err
}

// Validated plugin blueprint keyed plugin colon key, the BuildPluginBlueprint service of the plugin host
func BuildPluginBlueprint(plugin string, key any, spec plugins.BlueprintSpec) (string, any, error) {
	if !pyReMatches(blueprintNamespaceRe, plugin) {
		return "", nil, fmt.Errorf("plugin name %s cannot namespace a blueprint key", pyrepr.StrRepr(plugin))
	}
	keyText, isStr := key.(string)
	if !isStr || !pyReMatches(blueprintKeyRe, keyText) {
		return "", nil, fmt.Errorf("invalid key %s: use lowercase letters, digits, '-' or '_' (the '%s:' namespace is added automatically)", pyrepr.Repr(key), plugin)
	}
	slots, iterable := blueprintIterable(spec.Slots)
	_, isStr = spec.Slots.(string)
	_, isObject := spec.Slots.(*pyjson.Object)
	_, isMap := spec.Slots.(map[string]any)
	if isStr || isObject || isMap || !iterable {
		return "", nil, fmt.Errorf("slots must be a list")
	}
	b := AutomationBlueprint{Key: plugin + ":" + keyText, Plugin: plugin}
	var err error
	for _, f := range []struct {
		name string
		src  any
		dst  *string
	}{{"title", spec.Title, &b.Title}, {"description", spec.Description, &b.Description}, {"category", spec.Category, &b.Category},
		{"schedule_template", spec.ScheduleTemplate, &b.ScheduleTemplate}, {"prompt_template", spec.PromptTemplate, &b.PromptTemplate}} {
		if *f.dst, err = blueprintText(f.name, f.src); err != nil {
			return "", nil, err
		}
	}
	for _, raw := range slots {
		slot, err := blueprintSlotFrom(raw)
		if err != nil {
			return "", nil, err
		}
		b.Slots = append(b.Slots, slot)
	}
	if b.DeliverDefault, err = blueprintText("deliver_default", spec.DeliverDefault); err != nil {
		return "", nil, err
	}
	if b.Skills, err = blueprintStrings("skills", spec.Skills); err != nil {
		return "", nil, err
	}
	if b.Tags, err = blueprintStrings("tags", spec.Tags); err != nil {
		return "", nil, err
	}
	if err := validatePluginBlueprint(&b); err != nil {
		return "", nil, err
	}
	return b.Key, b, nil
}

// Blueprints the active profile enabled plugins registered, empty with a warning when the manager fails
func pluginBlueprints(ctx context.Context) (out []AutomationBlueprint) {
	defer func() {
		if r := recover(); r != nil {
			err := panicTraceback(r)
			slog.Warn("plugin automation blueprints unavailable\n" + tracebackFrames(err) + cronExceptionName(err) + ": " + err.Error())
			out = nil
		}
	}()
	plugins.DiscoverPlugins(ctx, false)
	for _, v := range plugins.GetPluginManager(ctx).ListAutomationBlueprints() {
		out = append(out, v.(AutomationBlueprint))
	}
	return out
}
