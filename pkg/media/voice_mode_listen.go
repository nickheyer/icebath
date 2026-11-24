//go:build !icebath_core && !icebath_noextras

package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/nickheyer/icebath/pkg/settings"
	mathutil "github.com/nickheyer/icebath/pkg/utils/math"
	portaudioutil "github.com/nickheyer/icebath/pkg/utils/portaudio"
)

var voiceWriteWAV = writeRecordingWAV

const (
	playbackMinTrigger     = 1500.0
	triggerCeiling         = 4000.0
	defaultBargeMultiplier = 3.0
)

// Fixed length FIFO dropping its oldest entry when full
type boundedDeque[T any] struct {
	items  []T
	maxlen int
}

func (d *boundedDeque[T]) append(v T) {
	d.items = append(d.items, v)
	if len(d.items) > d.maxlen {
		d.items = d.items[len(d.items)-d.maxlen:]
	}
}

// Whole 30 ms blocks in a duration given in milliseconds
func msBlocks(ms int) int { return ms / 30 }

// Opens a 30 ms block input stream, closing it on the way out and stopping it first once it started
func withListenStream(sd portaudioutil.Audio, body func(portaudioutil.InputStream) error) error {
	block := int(float64(voiceSampleRate) * 0.03)
	stream, err := sd.OpenInput(portaudioutil.StreamParams{SampleRate: voiceSampleRate, Channels: 1, BlockSize: block}, nil)
	if err != nil {
		return err
	}
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		return err
	}
	defer func() {
		_ = stream.Stop()
		_ = stream.Close()
	}()
	return callQuietlyErr(func() error { return body(stream) })
}

// Runs a body, its panic returned as an error
func callQuietlyErr(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

// Runs a user callback, its panic returned as an error
func callQuietly(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	fn()
	return nil
}

// Debug logs a VAD line, echoed to stderr when voice.debug is on for the process home
func vadLog(msg string) {
	pkgLog().Debug(msg)
	if settings.VoiceDebug.Get(context.Background()) {
		fmt.Fprintln(os.Stderr, "[voice-vad] "+msg)
	}
}

// Reads blocks after the pre roll until the endpoint silence or the block cap, then writes the WAV
func captureUntilQuiet(stream portaudioutil.InputStream, block int, preRoll [][]int16, endpointBlocks, maxBlocks int) (string, error) {
	frames := append([][]int16(nil), preRoll...)
	quiet := 0
	for range maxBlocks {
		data, _, err := stream.Read(block)
		if err != nil {
			return "", err
		}
		frames = append(frames, data)
		if mathutil.RMS(data) < silenceRMSThreshold {
			quiet++
		} else {
			quiet = 0
		}
		if quiet >= endpointBlocks {
			break
		}
	}
	var audio []int16
	for _, f := range frames {
		audio = append(audio, f...)
	}
	return voiceWriteWAV(audio, voiceSampleRate)
}

// Barge in detector with a quiet floor locked at calibration, playback grace and a sliding trip window
type bargeDetector struct {
	mult                float64
	calibBlocks         int
	graceBlocks         int
	tripNeeded          int
	ambient             *boundedDeque[float64]
	recentAbove         *boundedDeque[bool]
	quietFloor          float64
	floorLocked         bool
	playingPrev         bool
	playbackSeen        bool
	graceRemaining      int
	blockIdx            int
	blocksSincePlayback int
}

func newBargeDetector(mult float64, calibBlocks, tripBlocks, graceBlocks int) *bargeDetector {
	return &bargeDetector{mult: mult, calibBlocks: calibBlocks, graceBlocks: graceBlocks,
		tripNeeded: max(1, int(math.RoundToEven(float64(tripBlocks)*0.8))), ambient: &boundedDeque[float64]{maxlen: 100},
		recentAbove: &boundedDeque[bool]{maxlen: tripBlocks}, quietFloor: silenceRMSThreshold, blocksSincePlayback: 10_000}
}

// The 90th percentile of the ambient blocks and the floor it yields, never under the silence threshold
func (d *bargeDetector) floor() (float64, float64) {
	pct90 := float64(silenceRMSThreshold)
	if len(d.ambient.items) > 0 {
		pct90 = mathutil.Percentile(d.ambient.items, 90)
	}
	return pct90, max(pct90, float64(silenceRMSThreshold))
}

// Collects quiet blocks until the floor locks, locking early once playback starts
func (d *bargeDetector) calibrate(rms float64, playing bool) bool {
	if !playing {
		d.ambient.append(rms)
	}
	if len(d.ambient.items) >= d.calibBlocks || playing {
		var pct90 float64
		pct90, d.quietFloor = d.floor()
		d.floorLocked = true
		vadLog(fmt.Sprintf("calibrated quiet floor=%.0f (pct90=%.0f, %d blocks, mult=%s)", d.quietFloor, pct90, len(d.ambient.items),
			strconv.FormatFloat(d.mult, 'g', 6, 64)))
	}
	return d.floorLocked
}

// Opens the grace window on a fresh playback start and counts blocks since playback
func (d *bargeDetector) trackPlayback(playing bool) {
	if playing && !d.playingPrev {
		if !d.playbackSeen || d.blocksSincePlayback > 33 {
			d.graceRemaining = d.graceBlocks
			vadLog(fmt.Sprintf("playback started (block=%d) — grace %dms", d.blockIdx, d.graceBlocks*30))
		}
		d.playbackSeen = true
	}
	d.playingPrev = playing
	if playing {
		d.blocksSincePlayback = 0
	} else {
		d.blocksSincePlayback++
	}
}

func countTrue(values []bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}

// Feeds one block level, returning the phase that tripped or empty
func (d *bargeDetector) feed(rms float64, playing bool) string {
	d.blockIdx++
	if !d.floorLocked && !d.calibrate(rms, playing) {
		return ""
	}
	d.trackPlayback(playing)
	floorTrigger := float64(silenceRMSThreshold) * 2
	if playing {
		floorTrigger = playbackMinTrigger
	}
	trigger := min(max(d.quietFloor*d.mult, floorTrigger), triggerCeiling)
	if !playing && rms < trigger {
		d.ambient.append(rms)
		_, d.quietFloor = d.floor()
	}
	above := rms >= trigger
	if above && d.graceRemaining > 0 {
		vadLog(fmt.Sprintf("grace suppression: block=%d rms=%.0f trigger=%.0f (%d blocks left)", d.blockIdx, rms, trigger, d.graceRemaining))
		above = false
	}
	if d.graceRemaining > 0 {
		d.graceRemaining--
	}
	d.recentAbove.append(above)
	phase := "generation"
	if playing {
		phase = "playback"
	}
	window := countTrue(d.recentAbove.items)
	if rms >= trigger*0.5 {
		vadLog(fmt.Sprintf("block=%d rms=%.0f floor=%.0f trigger=%.0f above=%t window=%d/%d phase=%s",
			d.blockIdx, rms, d.quietFloor, trigger, above, window, d.tripNeeded, phase))
	}
	if !(above && window >= d.tripNeeded) {
		return ""
	}
	vadLog(fmt.Sprintf("TRIPPED (%s): block=%d rms=%.0f floor=%.0f trigger=%.0f window=%d/%d",
		phase, d.blockIdx, rms, d.quietFloor, trigger, window, len(d.recentAbove.items)))
	return phase
}

// DuplexOptions are the full_duplex_listen keywords, a zero Multiplier meaning the default
type DuplexOptions struct {
	IsPlaying         func() bool
	OnTrigger         func(phase string)
	Multiplier        float64
	SustainedMS       int
	CalibrationMS     int
	GraceMS           int
	PreRollMS         int
	EndpointSilenceMS int
	MaxUtteranceMS    int
}

// DefaultDuplexOptions are the full_duplex_listen defaults
func DefaultDuplexOptions() DuplexOptions {
	return DuplexOptions{SustainedMS: 300, CalibrationMS: 450, GraceMS: 500, PreRollMS: 1200, EndpointSilenceMS: 1250, MaxUtteranceMS: 30_000}
}

// FullDuplexListen listens through generation and playback, returning the WAV of the utterance that barged in
func FullDuplexListen(shouldStop func() bool, o DuplexOptions) string {
	sd, err := importAudio()
	if err != nil {
		return ""
	}
	block := int(float64(voiceSampleRate) * 0.03)
	endpointBlocks := max(1, msBlocks(o.EndpointSilenceMS))
	mult := defaultBargeMultiplier
	if o.Multiplier != 0 {
		mult = o.Multiplier
	}
	detector := newBargeDetector(mult, max(1, msBlocks(o.CalibrationMS)), max(1, msBlocks(o.SustainedMS)), max(0, msBlocks(o.GraceMS)))
	preRoll := &boundedDeque[[]int16]{maxlen: max(1, msBlocks(o.PreRollMS))}
	path := ""
	err = withListenStream(sd, func(stream portaudioutil.InputStream) error {
		for !shouldStop() {
			data, _, err := stream.Read(block)
			if err != nil {
				return err
			}
			preRoll.append(data)
			playing := o.IsPlaying != nil && o.IsPlaying()
			phase := detector.feed(mathutil.RMS(data), playing)
			if phase == "" {
				continue
			}
			if o.OnTrigger != nil {
				if err := callQuietly(func() { o.OnTrigger(phase) }); err != nil {
					pkgLog().Debug(fmt.Sprintf("full-duplex trigger callback failed: %s", err))
				}
			}
			path, err = captureUntilQuiet(stream, block, preRoll.items, endpointBlocks, max(1, msBlocks(o.MaxUtteranceMS)))
			return err
		}
		return nil
	})
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Full-duplex listener failed: %s", err))
		return ""
	}
	return path
}
