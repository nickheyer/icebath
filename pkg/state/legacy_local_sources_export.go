package state

import "sort"

// Sorted local surface labels older builds stored their chats under
func LegacyLocalSources() []string {
	out := make([]string, 0, len(legacyLocalSources))
	for label := range legacyLocalSources {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}
