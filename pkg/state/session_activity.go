package state

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/sleepclock"
)

// ActivityDescriptionMax is the code point budget of a session activity description
const ActivityDescriptionMax = 120

// ActivityProvenance names where a durable or in memory activity stamp came from
type ActivityProvenance string

const (
	ActivityProvenanceUnknown                  ActivityProvenance = "unknown"
	ActivityProvenanceAgentCompression         ActivityProvenance = "agent.compression"
	ActivityProvenanceAgentCompressionTimeout  ActivityProvenance = "agent.compression_timeout"
	ActivityProvenanceAgentCompressionCooldown ActivityProvenance = "agent.compression_cooldown"
	ActivityProvenanceAgentCompressionTurnhold ActivityProvenance = "agent.compression_turnhold"
)

var activityProvenances = map[ActivityProvenance]bool{
	ActivityProvenanceUnknown:                  true,
	ActivityProvenanceAgentCompression:         true,
	ActivityProvenanceAgentCompressionTimeout:  true,
	ActivityProvenanceAgentCompressionCooldown: true,
	ActivityProvenanceAgentCompressionTurnhold: true,
}

// Stripped activity text clamped to the description budget with an ellipsis
func BoundActivityDescription(description string) string {
	text := []rune(pystr.Strip(description))
	if len(text) <= ActivityDescriptionMax {
		return string(text)
	}
	return string(text[:ActivityDescriptionMax-1]) + "…"
}

// Known provenance for the stripped value, unknown when unset or unrecognized
func NormalizeActivityProvenance(provenance string) ActivityProvenance {
	p := ActivityProvenance(pystr.Strip(provenance))
	if activityProvenances[p] {
		return p
	}
	return ActivityProvenanceUnknown
}

// AwakeIdleMeter removes host sleep from the idle readings of a polling watchdog
type AwakeIdleMeter struct {
	wallNow, monoNow func() float64
	wall, mono       float64
	asleepS          float64
}

// Meter whose clocks start now, wall time against the clock that stops during sleep
func NewAwakeIdleMeter() *AwakeIdleMeter {
	return newAwakeIdleMeter(func() float64 { return float64(time.Now().UnixNano()) / 1e9 }, sleepclock.AwakeSeconds)
}

func newAwakeIdleMeter(wallNow, monoNow func() float64) *AwakeIdleMeter {
	return &AwakeIdleMeter{wallNow: wallNow, monoNow: monoNow, wall: wallNow(), mono: monoNow()}
}

// Meter reading the given wall and monotonic clocks
func NewAwakeIdleMeterWithClocks(wallNow, monoNow func() float64) *AwakeIdleMeter {
	return newAwakeIdleMeter(wallNow, monoNow)
}

// Wall clock idle seconds minus the host sleep observed since the last activity
func (m *AwakeIdleMeter) Measure(idleS float64) float64 {
	wall, mono := m.wallNow(), m.monoNow()
	sleptS := max(0.0, (wall-m.wall)-(mono-m.mono))
	if idleS < wall-m.wall {
		m.asleepS = 0.0
	}
	m.asleepS += sleptS
	m.wall, m.mono = wall, mono
	m.asleepS = max(0.0, min(m.asleepS, idleS+sleptS))
	return max(0.0, idleS-m.asleepS)
}
