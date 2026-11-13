package discord

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pynumpy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Mixer PCM geometry, 48 kHz stereo signed 16 bit little endian in 20 millisecond frames
const (
	mixerSampleRate      = 48000
	mixerChannels        = 2
	mixerSampleWidth     = 2
	mixerFrameLengthMS   = 20
	mixerSamplesPerFrame = mixerSampleRate * mixerFrameLengthMS / 1000
	mixerFrameSize       = mixerSamplesPerFrame * mixerChannels * mixerSampleWidth
	mixerBytesPerMS      = mixerSampleRate * mixerChannels * mixerSampleWidth / 1000
)

// One PCM stream feeding the mixer, read in frames with optional looping, gain and linear fade in
type mixerChild struct {
	pcm        []byte
	pos        int
	fadeDone   int
	finished   bool
	loop       bool
	gain       float64
	fadeFrames int
}

// Child over pcm padded with silence to a whole number of frames
func newMixerChild(pcm []byte, loop bool, gain float64, fadeInMS int) *mixerChild {
	pad := (mixerFrameSize - len(pcm)%mixerFrameSize) % mixerFrameSize
	padded := make([]byte, len(pcm)+pad)
	copy(padded, pcm)
	return &mixerChild{pcm: padded, loop: loop, gain: gain, fadeFrames: max(0, pyFloorDiv(fadeInMS, mixerFrameLengthMS))}
}

// Python floor division of two ints
func pyFloorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Next frame as float32 samples with gain and fade applied, nil once a non looping child ends
func (c *mixerChild) readFrame() []float32 {
	if c.finished {
		return nil
	}
	if c.pos >= len(c.pcm) {
		if c.loop && len(c.pcm) > 0 {
			c.pos = 0
		} else {
			c.finished = true
			return nil
		}
	}
	chunk := c.pcm[c.pos:min(c.pos+mixerFrameSize, len(c.pcm))]
	c.pos += mixerFrameSize
	samples := make([]float32, mixerFrameSize/mixerSampleWidth)
	for i := 0; i+1 < len(chunk); i += 2 {
		samples[i/2] = float32(int16(binary.LittleEndian.Uint16(chunk[i:])))
	}
	gain := c.gain
	if c.fadeFrames > 0 && c.fadeDone < c.fadeFrames {
		c.fadeDone++
		gain *= float64(c.fadeDone) / float64(c.fadeFrames)
	}
	if gain != 1.0 {
		g := float32(gain)
		for i := range samples {
			samples[i] *= g
		}
	}
	return samples
}

// Continuous PCM source summing a looping ambient bed and one shot speech that ducks the bed
type voiceMixer struct {
	mu                sync.Mutex
	ambient           *mixerChild
	speech            []*mixerChild
	ambientGain       float64
	duckGain          float64
	speechGain        float64
	duckReleaseFrames int
	duckReleaseLeft   int
	closed            bool
	speechActive      bool
}

func newVoiceMixer(ambientGain, duckGain, speechGain float64, duckReleaseMS int) *voiceMixer {
	return &voiceMixer{ambientGain: ambientGain, duckGain: duckGain, speechGain: speechGain,
		duckReleaseFrames: max(1, pyFloorDiv(duckReleaseMS, mixerFrameLengthMS))}
}

// The mixer hands raw PCM to the player
func (m *voiceMixer) isOpus() bool { return false }

// Installs the looping ambient bed, or clears it for empty pcm, optionally replacing the ambient gain
func (m *voiceMixer) setAmbient(pcm []byte, gain *float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gain != nil {
		m.ambientGain = *gain
	}
	if len(pcm) == 0 {
		m.ambient = nil
		return
	}
	gainNow := m.ambientGain
	if m.speechActive {
		gainNow = m.duckGain
	}
	m.ambient = newMixerChild(pcm, true, gainNow, 200)
}

// Layers a one shot speech clip over the bed and ducks it, the mixer speech gain when gain is nil
func (m *voiceMixer) playSpeech(pcm []byte, gain *float64, fadeInMS int) {
	if len(pcm) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.speechGain
	if gain != nil {
		g = *gain
	}
	m.speech = append(m.speech, newMixerChild(pcm, false, g, fadeInMS))
	m.speechActive = true
	m.duckReleaseLeft = 0
	if m.ambient != nil {
		m.ambient.gain = m.duckGain
	}
}

// Whether speech is still playing
func (m *voiceMixer) speechIsActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.speechActive
}

// Drops in flight speech at once and starts releasing the duck
func (m *voiceMixer) stopSpeech() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.speech = nil
	m.beginDuckReleaseLocked()
}

func (m *voiceMixer) beginDuckReleaseLocked() {
	m.speechActive = false
	m.duckReleaseLeft = m.duckReleaseFrames
}

// One mixed frame, always a full frame so the player never stops for the life of the connection
func (m *voiceMixer) read() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return make([]byte, mixerFrameSize), nil
	}
	var acc []float32
	add := func(frame []float32) {
		if acc == nil {
			acc = frame
			return
		}
		for i := range acc {
			acc[i] += frame[i]
		}
	}
	if len(m.speech) > 0 {
		var live []*mixerChild
		for _, child := range m.speech {
			frame := child.readFrame()
			if frame == nil {
				continue
			}
			add(frame)
			live = append(live, child)
		}
		m.speech = live
		if len(m.speech) == 0 && m.speechActive {
			m.beginDuckReleaseLocked()
		}
	}
	if m.ambient != nil {
		if m.duckReleaseLeft > 0 && !m.speechActive {
			m.duckReleaseLeft--
			frac := 1.0 - float64(m.duckReleaseLeft)/float64(m.duckReleaseFrames)
			m.ambient.gain = m.duckGain + (m.ambientGain-m.duckGain)*frac
		} else if !m.speechActive && m.duckReleaseLeft == 0 {
			m.ambient.gain = m.ambientGain
		}
		if amb := m.ambient.readFrame(); amb != nil {
			add(amb)
		}
	}
	out := make([]byte, mixerFrameSize)
	if acc == nil {
		return out, nil
	}
	for i, v := range acc {
		v = min(max(v, -32768), 32767)
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(v)))
	}
	return out, nil
}

// Closes the mixer when the player stops, leaving only silence
func (m *voiceMixer) cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.ambient = nil
	m.speech = nil
}

// Python returncode of an exited process, the negated signal number when a signal ended it
func pyReturnCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return ps.ExitCode()
}

// Audio file decoded through ffmpeg to mixer PCM, nil on any failure
func decodeToPCM(ctx context.Context, path string, timeout time.Duration) []byte {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exe := resolveFFmpegExecutable()
	args := []string{"-y", "-loglevel", "error", "-i", path, "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1"}
	cmd := ffmpegCommand(cctx, exe, args...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		argv := make([]any, 0, len(args)+1)
		for _, a := range append([]string{exe}, args...) {
			argv = append(argv, a)
		}
		slog.Warn(fmt.Sprintf("decode_to_pcm failed for %s: Command '%s' timed out after %s seconds", path, pyrepr.Repr(argv),
			pyrepr.Repr(timeout.Seconds())))
		return nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		slog.Warn(fmt.Sprintf("decode_to_pcm failed for %s: %s", path, err))
		return nil
	}
	if rc := pyReturnCode(cmd.ProcessState); rc != 0 {
		slog.Warn(fmt.Sprintf("ffmpeg decode failed for %s (rc=%d): %s", path, rc, cut(pystr.DecodeUTF8Replace(stderr.Bytes()), 200)))
		return nil
	}
	if stdout.Len() == 0 {
		return nil
	}
	return stdout.Bytes()
}

// Synthesised looping ambient bed of two detuned partials under a slow tremolo plus default_rng(7) smoothed noise
func synthAmbientPCM(seconds float64) ([]byte, error) {
	count, err := pyint.Of(float64(mixerSampleRate * seconds))
	if err != nil {
		return nil, err
	}
	if !count.IsInt64() {
		return nil, &pynumpy.ValueError{Msg: "Maximum allowed size exceeded"}
	}
	n := int(count.Int64())
	if seconds == 0 {
		return nil, &pyint.Error{Kind: "ZeroDivisionError", Msg: "division by zero"}
	}
	wholeCycle := func(target float64) float64 {
		cycles := max(1, math.RoundToEven(target*seconds))
		return cycles / seconds
	}
	f1, f2, trem := wholeCycle(110.0), wholeCycle(110.5), wholeCycle(0.5)
	w1, w2, w3 := 2*math.Pi*f1, 2*math.Pi*f2, 2*math.Pi*trem
	noise, err := pynumpy.DefaultRNG(7).StandardNormal(n)
	if err != nil {
		return nil, err
	}
	if noise, err = pynumpy.ConvolveSameBox64(noise); err != nil {
		return nil, err
	}
	if n != len(noise) && n != 1 {
		return nil, &pynumpy.ValueError{Msg: fmt.Sprintf("operands could not be broadcast together with shapes (%d,) (%d,) ", n, len(noise))}
	}
	signal := make([]float64, len(noise))
	peak := 0.0
	for i := range signal {
		t := float64(min(i, n-1)) / mixerSampleRate
		pad := float64(0.55*math.Sin(float64(w1*t))) + float64(0.45*math.Sin(float64(w2*t)))
		tremolo := 0.6 + float64(0.4*float64(0.5*float64(1+math.Sin(float64(w3*t)))))
		signal[i] = float64(pad*tremolo) + float64(0.08*noise[i])
		peak = max(peak, math.Abs(signal[i]))
	}
	if peak == 0 {
		peak = 1.0
	}
	out := make([]byte, len(signal)*mixerChannels*mixerSampleWidth)
	for i, v := range signal {
		s := uint16(int16(float64(float64(v/peak)*0.5) * 32767.0))
		for ch := range mixerChannels {
			binary.LittleEndian.PutUint16(out[(i*mixerChannels+ch)*2:], s)
		}
	}
	return out, nil
}
