// Package linktier records which tiers of icebath the link files of this binary named
package linktier

import (
	"strings"
	"sync"
)

// Tiers a binary is linked from, the core always present
const (
	Core    = "core"
	Plugins = "plugins"
	Extras  = "extras"
)

var (
	mu sync.RWMutex
	// Tiers the link files of the binary named, nil until the first one does
	linked map[string]bool
)

// Marks a tier as linked into this binary, called once by the link file of that tier
func Link(tier string) {
	mu.Lock()
	if linked == nil {
		linked = map[string]bool{Core: true}
	}
	linked[tier] = true
	mu.Unlock()
}

// Whether the tier is part of this binary, every tier when no link file named one
func Linked(tier string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return linked == nil || linked[tier]
}

// Name of the build that carries every tier up to and including tier
func buildName(tier string) string {
	switch tier {
	case Plugins:
		return "plugins build"
	case Extras:
		return "full build"
	}
	return "core build"
}

// Name of the build this binary is
func ThisBuild() string {
	switch {
	case Linked(Extras):
		return buildName(Extras)
	case Linked(Plugins):
		return buildName(Plugins)
	}
	return buildName(Core)
}

// Line telling which build carries a feature whose tier this binary leaves out, false for a linked tier
func Refusal(display, tier string) (string, bool) {
	if tier == "" || Linked(tier) {
		return "", false
	}
	return display + " is in the " + buildName(tier) + " of icebath, this binary is the " + ThisBuild(), true
}

// Line for a feature of tier whose code this binary was built without, also in a binary no link file named
func Missing(display, tier string) string {
	if line, refused := Refusal(display, tier); refused {
		return line
	}
	return display + " is in the " + buildName(tier) + " of icebath, this binary was built without it"
}

// Tiers this binary links in build order, every tier when no link file named one
func LinkedTiers() []string {
	var out []string
	for _, tier := range []string{Core, Plugins, Extras} {
		if Linked(tier) {
			out = append(out, tier)
		}
	}
	return out
}

// Line naming the build this binary is and the tiers it links, printed by the version output and the doctor
func BuildLine() string {
	return "Build: " + ThisBuild() + " (" + strings.Join(LinkedTiers(), ", ") + ")"
}

// Slash commands outside the core and the tier that carries each
var SlashTiers = map[string]string{
	"voice": Extras, "wake": Extras, "pet": Extras, "hatch": Extras, "cron": Extras,
	"blueprint": Extras, "suggestions": Extras, "kanban": Extras, "browser": Extras,
	"update": Extras, "arena": Extras,
	"handoff": Plugins, "platforms": Plugins, "egress": Plugins, "topic": Plugins,
}
