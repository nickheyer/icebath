package gateway

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Insertion ordered string set evicting its oldest entries first
type relayOrderedSet struct {
	order []string
	set   map[string]bool
}

func newRelayOrderedSet() *relayOrderedSet { return &relayOrderedSet{set: map[string]bool{}} }

func (s *relayOrderedSet) has(k string) bool { return s.set[k] }

func (s *relayOrderedSet) add(k string) {
	if !s.set[k] {
		s.set[k] = true
		s.order = append(s.order, k)
	}
}

func (s *relayOrderedSet) remove(k string) {
	if s.set[k] {
		delete(s.set, k)
		s.order = slices.DeleteFunc(s.order, func(x string) bool { return x == k })
	}
}

// Drops the oldest entries until at most cap remain
func (s *relayOrderedSet) evict(cap int) {
	for len(s.order) > cap {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
}

// Insertion ordered map of string keys to values evicting its oldest entries first
type relayOrdered[V any] struct {
	order []string
	vals  map[string]V
}

type relayOrderedInts = relayOrdered[int]

type relayOrderedFloats = relayOrdered[float64]

func newRelayOrdered[V any]() *relayOrdered[V] { return &relayOrdered[V]{vals: map[string]V{}} }

func (m *relayOrdered[V]) size() int { return len(m.order) }

func newRelayOrderedInts() *relayOrderedInts { return &relayOrdered[int]{vals: map[string]int{}} }

func newRelayOrderedFloats() *relayOrderedFloats {
	return &relayOrdered[float64]{vals: map[string]float64{}}
}

func (m *relayOrdered[V]) has(k string) bool {
	_, ok := m.vals[k]
	return ok
}

func (m *relayOrdered[V]) get(k string) (V, bool) {
	v, ok := m.vals[k]
	return v, ok
}

// Sets the value, a new key going to the end
func (m *relayOrdered[V]) set(k string, v V) {
	if _, ok := m.vals[k]; !ok {
		m.order = append(m.order, k)
	}
	m.vals[k] = v
}

// Moves an existing key to the end, the OrderedDict move_to_end
func (m *relayOrdered[V]) moveToEnd(k string) {
	if i := slices.Index(m.order, k); i >= 0 {
		m.order = append(append(m.order[:i:i], m.order[i+1:]...), k)
	}
}

func (m *relayOrdered[V]) pop(k string) (V, bool) {
	v, ok := m.vals[k]
	if ok {
		delete(m.vals, k)
		m.order = slices.DeleteFunc(m.order, func(x string) bool { return x == k })
	}
	return v, ok
}

func (m *relayOrdered[V]) keys() []string { return slices.Clone(m.order) }

func (m *relayOrdered[V]) evict(cap int) {
	for len(m.order) > cap {
		delete(m.vals, m.order[0])
		m.order = m.order[1:]
	}
}

// Adopts a renegotiated descriptor into the live capability surface
func (a *RelayAdapter) applyDescriptor(descriptor CapabilityDescriptor) {
	a.mu.Lock()
	a.descriptor = descriptor
	a.mu.Unlock()
	a.Traits.MaxMessageLength = descriptor.MaxMessageLength
	a.Traits.SupportsCodeBlocks = descriptor.MarkdownDialect != "" && descriptor.MarkdownDialect != "plain"
	a.Traits.SupportsInchannelContinuable = descriptor.SupportsInchannelContinuable
	a.Traits.SupportsStatusText = descriptor.Platform == relaySlack
}

// Platform sub block of the config extras, the whole extras when absent or not a mapping
func (a *RelayAdapter) relayPlatformExtra(platform string) map[string]any {
	extra := a.Config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	if sub, ok := extra[platform].(map[string]any); ok {
		return sub
	}
	return extra
}

// Boolean of a config knob, strings read through the truthy spellings and nil giving the default
func relayCoerceFlag(raw any, def bool) bool {
	if raw == nil {
		return def
	}
	if b, ok := raw.(bool); ok {
		return b
	}
	return relayTruthyFlags[pystr.Lower(pystr.Strip(pyrepr.Str(raw)))]
}

func (a *RelayAdapter) slackFlag(knob string, def bool) bool {
	return relayCoerceFlag(a.relayPlatformExtra("slack")[knob], def)
}

func (a *RelayAdapter) effectiveReplyInThread() bool { return a.slackFlag("reply_in_thread", true) }

func (a *RelayAdapter) dmTopLevelThreadsAsSessions() bool {
	return a.slackFlag("dm_top_level_threads_as_sessions", true)
}

// Explicit Slack unfurl link and media hints, empty for other platforms or when unset
func (a *RelayAdapter) slackUnfurlHints(platform string) map[string]bool {
	if pystr.Lower(platform) != relaySlack {
		return nil
	}
	extra := a.relayPlatformExtra("slack")
	hints := map[string]bool{}
	for _, knob := range []string{"unfurl_links", "unfurl_media"} {
		switch v := extra[knob].(type) {
		case bool:
			hints[knob] = v
		case string:
			s := pystr.Lower(pystr.Strip(v))
			if relayTruthyFlags[s] || relayFalsyFlags[s] {
				hints[knob] = relayTruthyFlags[s]
			}
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// Copies the Slack unfurl hints into the metadata
func (a *RelayAdapter) stampSlackUnfurl(platform string, metadata map[string]any) {
	for k, v := range a.slackUnfurlHints(platform) {
		metadata[k] = v
	}
}
