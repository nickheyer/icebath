package voice

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/portaudio"
)

const (
	// Floor of the trigger while speech plays, above the speaker's own sound in the room
	playbackTrigger = 1500.0
	triggerCeiling  = 4000.0
	defaultBarge    = 3.0
	// Length of one listening block
	blockMS = 30
)

// Blocks of 30 ms in a span of milliseconds
func blocks(ms int) int { return ms / blockMS }

// Bounded queue dropping its oldest entry when full
type ring[T any] struct {
	items []T
	limit int
}

func (r *ring[T]) add(v T) {
	r.items = append(r.items, v)
	if len(r.items) > r.limit {
		r.items = r.items[len(r.items)-r.limit:]
	}
}

// Percentile of values with linear interpolation between ranks
func percentile(values []float64, p float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := float64(len(sorted)-1) * p / 100
	lo := int(rank)
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (sorted[lo+1]-sorted[lo])*(rank-float64(lo))
}

// Barge in detector: the room's quiet floor locked at calibration, a grace window as playback starts and a sliding trip window
type barge struct {
	mode        *Mode
	mult        float64
	calibration int
	grace       int
	needed      int
	ambient     *ring[float64]
	recent      *ring[bool]
	floor       float64
	locked      bool
	wasPlaying  bool
	seen        bool
	left        int
	index       int
	since       int
}

func newBarge(m *Mode, mult float64, calibration, trip, grace int) *barge {
	return &barge{mode: m, mult: mult, calibration: calibration, grace: grace, needed: max(1, int(math.RoundToEven(float64(trip)*0.8))),
		ambient: &ring[float64]{limit: 100}, recent: &ring[bool]{limit: trip}, floor: silenceRMS, since: 10_000}
}

func (b *barge) log(text string) {
	slog.Debug(text)
	if b.mode.mode().GetDebug() {
		fmt.Fprintln(os.Stderr, "[voice-vad] "+text)
	}
}

// The 90th percentile of the quiet blocks and the floor it gives, never under the silence level
func (b *barge) measure() (float64, float64) {
	pct := float64(silenceRMS)
	if len(b.ambient.items) > 0 {
		pct = percentile(b.ambient.items, 90)
	}
	return pct, max(pct, float64(silenceRMS))
}

// Collects quiet blocks until the floor locks, early once playback starts
func (b *barge) calibrate(level float64, playing bool) bool {
	if !playing {
		b.ambient.add(level)
	}
	if len(b.ambient.items) >= b.calibration || playing {
		var pct float64
		pct, b.floor = b.measure()
		b.locked = true
		b.log(fmt.Sprintf("calibrated quiet floor=%.0f (pct90=%.0f, %d blocks, mult=%s)", b.floor, pct, len(b.ambient.items),
			strconv.FormatFloat(b.mult, 'g', 6, 64)))
	}
	return b.locked
}

// Opens the grace window as a fresh playback starts
func (b *barge) track(playing bool) {
	if playing && !b.wasPlaying {
		if !b.seen || b.since > 33 {
			b.left = b.grace
			b.log(fmt.Sprintf("playback started (block=%d) — grace %dms", b.index, b.grace*blockMS))
		}
		b.seen = true
	}
	b.wasPlaying = playing
	if playing {
		b.since = 0
	} else {
		b.since++
	}
}

// Feeds one block's level: the phase that tripped, playback or generation, else empty
func (b *barge) feed(level float64, playing bool) string {
	b.index++
	if !b.locked && !b.calibrate(level, playing) {
		return ""
	}
	b.track(playing)
	low := float64(silenceRMS) * 2
	if playing {
		low = playbackTrigger
	}
	trigger := min(max(b.floor*b.mult, low), triggerCeiling)
	if !playing && level < trigger {
		b.ambient.add(level)
		_, b.floor = b.measure()
	}
	above := level >= trigger
	if above && b.left > 0 {
		b.log(fmt.Sprintf("grace suppression: block=%d rms=%.0f trigger=%.0f (%d blocks left)", b.index, level, trigger, b.left))
		above = false
	}
	if b.left > 0 {
		b.left--
	}
	b.recent.add(above)
	phase := "generation"
	if playing {
		phase = "playback"
	}
	window := 0
	for _, v := range b.recent.items {
		if v {
			window++
		}
	}
	if level >= trigger*0.5 {
		b.log(fmt.Sprintf("block=%d rms=%.0f floor=%.0f trigger=%.0f above=%t window=%d/%d phase=%s", b.index, level, b.floor, trigger, above, window,
			b.needed, phase))
	}
	if !above || window < b.needed {
		return ""
	}
	b.log(fmt.Sprintf("TRIPPED (%s): block=%d rms=%.0f floor=%.0f trigger=%.0f window=%d/%d", phase, b.index, level, b.floor, trigger, window,
		len(b.recent.items)))
	return phase
}

// Listens on its own stream while a reply is written or speech is to say or sounds: talking over it cuts the speech and, while the reply is
// still written, stops the turn; what was said goes in as the next message. One listener at a time, there is one microphone, carried on
// across the replies that follow
func (m *Mode) listen(ctx context.Context) {
	m.mu.Lock()
	if m.duplex {
		m.mu.Unlock()
		return
	}
	m.duplex = true
	m.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug(fmt.Sprintf("full-duplex listener failed: %v", r))
			}
			m.mu.Lock()
			m.duplex = false
			m.mu.Unlock()
		}()
		// Talking over a reply is part of voice input, so the listener gives up the microphone once voice input is off
		live := func() bool {
			if !m.mode().GetInput() {
				return false
			}
			if m.speaking() {
				return true
			}
			m.mu.Lock()
			reply := m.reply
			m.mu.Unlock()
			return reply != nil && reply.live()
		}
		mode := m.mode()
		phase, path := m.overheard(live, mode.GetBargeInMultiplier(), max(0, int(mode.GetBargeInGrace()*1000)))
		if path == "" {
			return
		}
		defer os.Remove(path)
		t, err := m.backend.transcribe(context.WithoutCancel(ctx), path)
		if err != nil {
			slog.Debug(fmt.Sprintf("full-duplex listener failed: %s", err))
			return
		}
		if !t.Success {
			return
		}
		text := strings.TrimSpace(t.Text)
		if text == "" || speech.Hallucination(text) && !m.StopPhrase(text) {
			return
		}
		m.mu.Lock()
		said := string(m.spoken)
		m.mu.Unlock()
		if phase == "playback" && speech.Echo(text, said) {
			slog.Debug("full-duplex listener: capture during playback repeats the spoken reply, dropped as speaker bleed")
			return
		}
		if m.StopPhrase(text) {
			m.Off(ctx)
			m.halt(true, text)
			return
		}
		if m.events.Transcript != nil {
			call(func() { m.events.Transcript(text) })
		}
	}()
}

// Listens in 30 ms blocks while live holds; a trip cuts every speech, stops a reply still written, and records until quiet
func (m *Mode) overheard(live func() bool, mult float64, graceMS int) (string, string) {
	pa, err := audio()
	if err != nil {
		return "", ""
	}
	block := sampleRate * blockMS / 1000
	stream, err := pa.OpenInput(portaudio.StreamParams{SampleRate: sampleRate, Channels: 1, BlockSize: block}, nil)
	if err != nil {
		slog.Debug(fmt.Sprintf("Full-duplex listener failed: %s", err))
		return "", ""
	}
	if err := stream.Start(); err != nil {
		stream.Close()
		slog.Debug(fmt.Sprintf("Full-duplex listener failed: %s", err))
		return "", ""
	}
	defer func() {
		stream.Stop()
		stream.Close()
	}()
	if mult == 0 {
		mult = defaultBarge
	}
	detector := newBarge(m, mult, max(1, blocks(450)), max(1, blocks(300)), max(0, blocks(graceMS)))
	preRoll := &ring[[]int16]{limit: max(1, blocks(1200))}
	for live() {
		data, _, err := stream.Read(block)
		if err != nil {
			slog.Debug(fmt.Sprintf("Full-duplex listener failed: %s", err))
			return "", ""
		}
		preRoll.add(data)
		phase := detector.feed(rms(data), m.player.sounding())
		if phase == "" {
			continue
		}
		m.tripped(phase)
		path, err := m.untilQuiet(stream, block, preRoll.items, max(1, blocks(1250)), max(1, blocks(30_000)))
		if err != nil {
			slog.Debug(fmt.Sprintf("Full-duplex listener failed: %s", err))
			return "", ""
		}
		return phase, path
	}
	return "", ""
}

// Latches the interruption and cuts every speech; while the reply was still written the app stops it
func (m *Mode) tripped(phase string) {
	if phase == "playback" {
		slog.Debug("TTS CUT: full-duplex listener tripped during playback")
	} else {
		slog.Debug("full-duplex listener tripped during generation — interrupting running turn")
	}
	m.mu.Lock()
	m.interrupted = true
	m.mu.Unlock()
	m.drop()
	if m.events.Interrupted != nil {
		call(m.events.Interrupted)
	}
}

// Reads blocks after the pre roll until the closing silence or the cap, then writes the WAV
func (m *Mode) untilQuiet(stream *portaudio.Stream, block int, preRoll [][]int16, closing, limit int) (string, error) {
	frames := slices.Clone(preRoll)
	quiet := 0
	for range limit {
		data, _, err := stream.Read(block)
		if err != nil {
			return "", err
		}
		frames = append(frames, data)
		if rms(data) < silenceRMS {
			quiet++
		} else {
			quiet = 0
		}
		if quiet >= closing {
			break
		}
	}
	var data []int16
	for _, f := range frames {
		data = append(data, f...)
	}
	path, err := recordingPath("wav")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, speech.WAV(speech.PCMBytes(data), sampleRate, channels, sampleBits), 0o600); err != nil {
		return "", err
	}
	slog.Info(fmt.Sprintf("WAV written: %s (%d bytes)", path, 44+2*len(data)))
	return path, nil
}
