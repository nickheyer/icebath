package tools

import (
	"maps"
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Registrant whose tools keep what sessions read and wrote, such as the file tools: what tells an agent that a subagent changed files it
// had read
type Tracker interface {
	// Paths the session read whole or in part, sorted
	KnownReads(cfg *config.Config, session string) []string
	// Paths among those given, every path for nil, that a session other than the one left out wrote since the time, by the session that
	// wrote each last, sorted
	WritesSince(cfg *config.Config, exclude string, since time.Time, paths []string) map[string][]string
}

// Paths the session read through the registrants that keep track, sorted, each once
func (r *Registry) KnownReads(cfg *config.Config, session string) []string {
	seen := map[string]bool{}
	for _, registrant := range r.registrants {
		if tracker, ok := registrant.(Tracker); ok {
			for _, path := range tracker.KnownReads(cfg, session) {
				seen[path] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// Paths among those given, every path for nil, that sessions besides the one left out wrote since the time, by session, as the
// registrants that keep track saw them
func (r *Registry) WritesSince(cfg *config.Config, exclude string, since time.Time, paths []string) map[string][]string {
	out := map[string][]string{}
	for _, registrant := range r.registrants {
		if tracker, ok := registrant.(Tracker); ok {
			for session, written := range tracker.WritesSince(cfg, exclude, since, paths) {
				for _, path := range written {
					if !slices.Contains(out[session], path) {
						out[session] = append(out[session], path)
					}
				}
			}
		}
	}
	for session := range out {
		slices.Sort(out[session])
	}
	return out
}
