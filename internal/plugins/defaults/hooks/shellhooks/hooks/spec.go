package hooks

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/internal/tools/diff"
)

const (
	// Seconds a hook may take when its entry gives none, and the most it may take
	DefaultTimeout = 60
	MaxTimeout     = 300
	// Longest a matcher may take to match a tool name
	matchTimeout = time.Second
)

// Tool names a hook or a webhook is for, every tool for an empty one
type Matcher struct {
	Text string
	// Pattern the whole name must match, nil for one that did not compile and is matched as it is written
	re *regexp2.Regexp
}

// Matcher of the text, trimmed; one that is not a valid pattern is warned about once and matched literally
func (r *Registrant) matcher(profile, kind, text string) Matcher {
	m := Matcher{Text: strings.TrimSpace(text)}
	if m.Text == "" {
		return m
	}
	re, err := regexp2.Compile(`\A(?:`+m.Text+`)\z`, regexp2.None)
	if err != nil {
		r.warn(profile, fmt.Sprintf("%s matcher %s is invalid (%v) — treating as literal equality", kind, show(m.Text), err))
		return m
	}
	re.MatchTimeout = matchTimeout
	m.re = re
	return m
}

// Whether the tool is one the matcher is for
func (m Matcher) Matches(tool string) (bool, error) {
	switch {
	case m.Text == "":
		return true, nil
	case m.re == nil:
		return tool == m.Text, nil
	}
	return m.re.MatchString(tool)
}

// One hook of the events setting
type Spec struct {
	Event   string
	Command string
	Matcher Matcher
	// Seconds it may take
	Timeout int
	// It blocks the call when it fails, for a point that can be blocked
	FailClosed bool
}

// Identity of a hook as it is wired: what it runs on, for which tools, and what it runs
type key struct{ event, matcher, command string }

func (s Spec) key() key { return key{s.Event, s.Matcher.Text, s.Command} }

func (s Spec) pair() pair { return pair{s.Event, s.Command} }

// Hooks the profile's settings set, each malformed entry warned about once and left out, and whether none was left out
func (r *Registrant) specs(cfg *config.Config) ([]Spec, bool) {
	raw := cfg.Get(shellconfig.Events)
	if raw == nil {
		return nil, true
	}
	block, ok := raw.(map[string]any)
	profile := cfg.Profile()
	if !ok {
		r.warn(profile, fmt.Sprintf("%s must be a mapping of event names to lists of hook definitions; got %s", shellconfig.Events, kindOf(raw)))
		return nil, false
	}
	var specs []Spec
	complete := true
	for _, event := range slices.Sorted(maps.Keys(block)) {
		entries := block[event]
		point, known := hooks.PointOf(event)
		switch {
		case known && point.NoShell:
			r.warn(profile, fmt.Sprintf("hook event %s is plugin-only: shell hooks cannot return its directive, so this registration is refused rather "+
				"than silently ignored", show(event)))
			continue
		case !known:
			if close := diff.Close(event, hooks.Points(), 1, 0.6); len(close) > 0 {
				r.warn(profile, fmt.Sprintf("unknown hook event %s in %s — did you mean %s?", show(event), shellconfig.Events, show(close[0])))
			} else {
				r.warn(profile, fmt.Sprintf("unknown hook event %s in %s (valid: %s)", show(event), shellconfig.Events, strings.Join(hooks.Points(), ", ")))
			}
			complete = false
			continue
		case entries == nil:
			continue
		}
		list, ok := entries.([]any)
		if !ok {
			r.warn(profile, fmt.Sprintf("%s.%s must be a list of hook definitions; got %s", shellconfig.Events, event, kindOf(entries)))
			complete = false
			continue
		}
		for i, entry := range list {
			spec, ok := r.entry(profile, point, i, entry)
			if !ok {
				complete = false
				continue
			}
			specs = append(specs, spec)
		}
	}
	return specs, complete
}

// One entry of an event's list, false with a warning when it is malformed; a field it gets wrong is warned about and its default taken
func (r *Registrant) entry(profile string, point hooks.Point, index int, raw any) (Spec, bool) {
	where := fmt.Sprintf("%s.%s[%d]", shellconfig.Events, point.Name, index)
	warn := func(text string) { r.warn(profile, where+text) }
	entry, ok := raw.(map[string]any)
	if !ok {
		warn(fmt.Sprintf(" must be a mapping with a 'command' key; got %s", kindOf(raw)))
		return Spec{}, false
	}
	command, _ := entry["command"].(string)
	if command = strings.TrimSpace(command); command == "" {
		warn(" is missing a non-empty 'command' field")
		return Spec{}, false
	}
	matcher := ""
	if value, given := entry["matcher"]; given && value != nil {
		text, isText := value.(string)
		switch {
		case !isText:
			warn(".matcher must be a string regex; ignoring")
		case !point.Tool:
			warn(fmt.Sprintf(".matcher=%s will be ignored at runtime — the matcher field is only honored for pre_tool_call / post_tool_call.  The hook "+
				"will fire on every %s event.", show(text), point.Name))
		default:
			matcher = text
		}
	}
	timeout := DefaultTimeout
	if value, given := entry["timeout"]; given {
		n, whole := config.Whole(value)
		switch {
		case !whole:
			warn(fmt.Sprintf(".timeout must be an int (got %s); using default %ds", show(value), DefaultTimeout))
		case n < 1:
			warn(fmt.Sprintf(".timeout must be >=1; using default %ds", DefaultTimeout))
		case n > MaxTimeout:
			warn(fmt.Sprintf(".timeout=%ds exceeds max %ds; clamping", n, MaxTimeout))
			timeout = MaxTimeout
		default:
			timeout = n
		}
	}
	closed := false
	if value, given := entry[shellconfig.Block]; given {
		on, isBool := value.(bool)
		switch {
		case !isBool:
			warn(fmt.Sprintf(".fail_closed must be a boolean (got %s); using default false (fail open)", show(value)))
		case on && !point.Blocking:
			warn(fmt.Sprintf(".fail_closed=true will be ignored at runtime — fail_closed only applies to blocking-capable events (%s).  The hook will "+
				"fail open on %s like any other hook.", hooks.PreToolCall, point.Name))
		default:
			closed = on
		}
	}
	return Spec{Event: point.Name, Command: command, Matcher: r.matcher(profile, "shell hook", matcher), Timeout: timeout, FailClosed: closed}, true
}

// Kind of a value read from the settings, as a warning names it
func kindOf(value any) string {
	switch value.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case int, int64:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", value)
}

// Value as a warning shows it: text quoted, true and false and nothing by name
func show(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return "'" + strings.ReplaceAll(v, "'", `\'`) + "'"
	case bool:
		if v {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(value)
}
