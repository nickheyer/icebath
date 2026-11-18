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

// Details mode named by a word in any case, empty for anything else
func parseDetailsMode(word string) string {
	if m := strings.ToLower(strings.TrimSpace(word)); slices.Contains(detailsModes, m) {
		return m
	}
	return ""
}

// One of the four section names, case sensitive
func isSectionName(name string) bool { return slices.Contains(sectionNames, name) }

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
