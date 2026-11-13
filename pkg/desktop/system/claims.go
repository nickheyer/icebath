package system

import (
	"math"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/dedupe"
)

// Process start, the zero of process.uptime
var processStart = time.Now()

// Answers the startup latency once, milliseconds since process start, nil for every later claim
type startupLatencyClaim struct {
	mu      sync.Mutex
	claimed bool
	uptime  func() float64
}

func (c *startupLatencyClaim) claim() *int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed {
		return nil
	}
	c.claimed = true
	ms := int(math.Max(0, math.Round(c.uptime()*1000)))
	return &ms
}

// Uptime in seconds since process start
func processUptime() float64 { return time.Since(processStart).Seconds() }

// Hands the first caller the startup latency, the claimStartupLatency invoke
func (s *Service) ClaimStartupLatency() *int { return s.latency.claim() }

// True for the one window that owns an ambient cue, speech claims held ten minutes and instant cues one second
func (s *Service) ClaimAmbientCue(key any) bool {
	return s.ambient(jsStringOrEmpty(key), time.Now())
}

func newAmbientArbiter() dedupe.Arbiter {
	return dedupe.NewAmbientClaimArbiter(dedupe.DefaultInterval, dedupe.SpeechClaimTTL)
}

// JS String of a renderer argument with null and undefined read as empty
func jsStringOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// Asks for microphone access where the OS gates it, the requestMicrophoneAccess invoke
func (s *Service) RequestMicrophoneAccess() bool { return askForMicrophoneAccess() }
