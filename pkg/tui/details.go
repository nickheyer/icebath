package tui

import (
	"slices"
	"strings"
)

// Details modes in their cycling order
var detailsModes = []string{"hidden", "collapsed", "expanded"}

// Transcript sections whose detail mode can be set on their own
var sectionNames = []string{"thinking", "tools", "subagents", "activity"}

// Built in mode of a section when no override names one
var sectionDefaults = map[string]string{"thinking": "expanded", "tools": "expanded", "activity": "hidden"}

// Details mode implied by the legacy thinking_mode setting
var thinkingFallback = map[string]string{"collapsed": "collapsed", "full": "expanded", "truncated": "collapsed"}

// Config value trimmed and lowercased as its string form, empty for a missing value
func normSetting(v any) string {
	if v == nil {
		return ""
	}
	s, isString := v.(string)
	if !isString {
		s = jsString(v)
	}
	return strings.ToLower(strings.TrimFunc(s, isJSSpace))
}

// Details mode named by a value, empty for anything else
func parseDetailsMode(v any) string {
	if m := normSetting(v); slices.Contains(detailsModes, m) {
		return m
	}
	return ""
}

// One of the four section names, case sensitive
func isSectionName(v any) bool {
	s, isString := v.(string)
	return isString && slices.Contains(sectionNames, s)
}

// Global details mode from details_mode, then the legacy thinking_mode, then collapsed
func resolveDetailsMode(display map[string]any) string {
	if m := parseDetailsMode(display["details_mode"]); m != "" {
		return m
	}
	if m, ok := thinkingFallback[normSetting(display["thinking_mode"])]; ok {
		return m
	}
	return "collapsed"
}

// Per section overrides of a sections setting, unknown sections and modes dropped
func resolveSections(raw any) map[string]string {
	out := map[string]string{}
	obj, isDict := raw.(map[string]any)
	if !isDict {
		return out
	}
	for k, v := range obj {
		if m := parseDetailsMode(v); m != "" && isSectionName(k) {
			out[k] = m
		}
	}
	return out
}

// Mode of a section, its override first, then the global mode under a /details command, else its default or the global mode
func sectionMode(name, global string, sections map[string]string, commandOverride bool) string {
	if m, ok := sections[name]; ok {
		return m
	}
	if commandOverride {
		return global
	}
	if m, ok := sectionDefaults[name]; ok {
		return m
	}
	return global
}

// Details mode after m in the hidden, collapsed, expanded cycle
func nextDetailsMode(m string) string {
	return detailsModes[(slices.Index(detailsModes, m)+1)%len(detailsModes)]
}
