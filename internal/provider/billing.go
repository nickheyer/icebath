package provider

import (
	"cmp"
	"strings"
	"unicode"
)

// Where the account of a route adds credit: its kind's vendor, else the vendor any kind names for the route's host; else the provider's
// name alone, without a page
func (r *Registry) Billing(route Route) Billing {
	host := BaseHost(route.BaseURL)
	matches := func(spec Spec, entry Billing) bool {
		if len(entry.Hosts) == 0 {
			return host != "" && host == BaseHost(spec.BaseURL)
		}
		for _, claimed := range entry.Hosts {
			if BaseHostMatches(route.BaseURL, claimed) {
				return true
			}
		}
		return false
	}
	own, _ := r.Spec(route.Spec)
	for _, entry := range own.Billing {
		if len(entry.Hosts) == 0 || matches(own, entry) {
			return entry
		}
	}
	for _, spec := range r.Specs() {
		for _, entry := range spec.Billing {
			if matches(spec, entry) {
				return entry
			}
		}
	}
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(cmp.Or(route.Provider, route.Spec)))
	for i, word := range words {
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return Billing{Label: cmp.Or(strings.Join(words, " "), "your provider")}
}
