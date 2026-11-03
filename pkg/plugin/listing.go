package plugin

import (
	"sort"
	"strings"
)

// Share of the context window the skill listing may take unless configured
const DefaultListingShare = 0.01

// Characters the listing may take when the window is unknown
const fallbackListingChars = 8000

// Text the model sees about the skills it may load, every name kept and descriptions dropped under budget pressure
func (l *Library) Listing(window int, share float64, usage map[string]int) string {
	skills := l.ModelSkills()
	if len(skills) == 0 {
		return ""
	}
	budget := fallbackListingChars
	if window > 0 {
		if share <= 0 {
			share = DefaultListingShare
		}
		budget = int(float64(window) * share * 4)
	}
	lines := make([]string, len(skills))
	for i, s := range skills {
		desc := s.Listing()
		if desc != "" {
			lines[i] = "- " + s.Name + ": " + desc
		} else {
			lines[i] = "- " + s.Name
		}
	}
	total := 0
	for _, line := range lines {
		total += len(line) + 1
	}
	if total > budget {
		order := make([]int, len(skills))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return usage[skills[order[a]].Name] < usage[skills[order[b]].Name] })
		for _, i := range order {
			if total <= budget {
				break
			}
			short := "- " + skills[i].Name
			total -= len(lines[i]) - len(short)
			lines[i] = short
		}
	}
	return strings.Join(lines, "\n")
}
