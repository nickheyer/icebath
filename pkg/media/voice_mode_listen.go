package media

import (
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pynumpy"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var voiceWriteWAV = writeRecordingWAV

const (
	playbackMinTrigger     = 1500.0
	triggerCeiling         = 4000.0
	defaultBargeMultiplier = 3.0
)

// Fixed length FIFO dropping its oldest entry when full, the collections.deque maxlen behaviour
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

// ListenOptions are the listen_for_speech keywords
type ListenOptions struct {
	Threshold         int
	SustainedMS       int
	CalibrationMS     int
	Capture           bool
	OnTrigger         func()
	PreRollMS         int
	EndpointSilenceMS int
	MaxUtteranceMS    int
}

// DefaultListenOptions are the listen_for_speech defaults
func DefaultListenOptions() ListenOptions {
	return ListenOptions{SustainedMS: 300, CalibrationMS: 400, PreRollMS: 1200, EndpointSilenceMS: 1250, MaxUtteranceMS: 30_000}
}

func pyFloorDivMS(ms int) int { return pyFloorDiv(ms, 30) }

// Blocks until sustained speech rises over an adaptive floor, capturing the utterance to a WAV in capture mode
func ListenForSpeech(shouldStop func() bool, o ListenOptions) (bool, string) {
	sd, err := importAudio()
	if err != nil {
		return false, ""
	}
	tripped, path, err := listenForSpeech(sd, shouldStop, o)
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("Barge-in listener failed: %s", err))
		return false, ""
	}
	return tripped, path
}

// Opens a 30 ms block input stream, stopping and closing it on the way out once it started
func withListenStream(sd pysounddevice.Module, body func(pysounddevice.InputStream) error) error {
	block := int(float64(voiceSampleRate) * 0.03)
	stream, err := sd.InputStream(pysounddevice.StreamParams{Samplerate: voiceSampleRate, Channels: 1, Blocksize: block}, nil)
	if err != nil {
		return err
	}
	if err := stream.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stream.Stop(true)
		_ = stream.Close(true)
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

func listenForSpeech(sd pysounddevice.Module, shouldStop func() bool, o ListenOptions) (tripped bool, path string, err error) {
	block := int(float64(voiceSampleRate) * 0.03)
	calibBlocks := max(1, pyFloorDivMS(o.CalibrationMS))
	tripBlocks := max(1, pyFloorDivMS(o.SustainedMS))
	endpointBlocks := max(1, pyFloorDivMS(o.EndpointSilenceMS))
	maxBlocks := max(1, pyFloorDivMS(o.MaxUtteranceMS))
	floorWindow := &boundedDeque[float64]{maxlen: max(calibBlocks, 100)}
	preRoll := &boundedDeque[[]int16]{maxlen: max(1, pyFloorDivMS(o.PreRollMS))}
	consecutive, minFloor, blockIdx := 0, 0.0, 0
	err = withListenStream(sd, func(stream pysounddevice.InputStream) error {
		for !shouldStop() {
			data, _, err := stream.Read(block)
			if err != nil {
				return err
			}
			rms := pynumpy.RMSInt16(data)
			if o.Capture {
				preRoll.append(data)
			}
			blockIdx++
			if len(floorWindow.items) < calibBlocks {
				floorWindow.append(rms)
				continue
			}
			pct90 := pynumpy.Percentile(floorWindow.items, 90)
			if minFloor == 0.0 && len(floorWindow.items) >= calibBlocks {
				minFloor = max(pct90, float64(silenceRMSThreshold*2))
			}
			floor := max(pct90, minFloor)
			base := float64(o.Threshold)
			if o.Threshold == 0 {
				base = float64(silenceRMSThreshold * 2)
			}
			trigger := min(max(base, floor*8.0), 4000.0)
			if rms < trigger {
				floorWindow.append(rms)
			}
			if rms >= trigger {
				consecutive++
			} else {
				consecutive = 0
			}
			if consecutive > 0 {
				voiceLog().Debug(fmt.Sprintf("VAD above-trigger: block=%d rms=%.0f floor=%.0f trigger=%.0f consec=%d/%d min_floor=%.0f window_len=%d",
					blockIdx, rms, floor, trigger, consecutive, tripBlocks, minFloor, len(floorWindow.items)))
			}
			if consecutive < tripBlocks {
				continue
			}
			voiceLog().Info(fmt.Sprintf("VAD TRIPPED: block=%d rms=%.0f floor=%.0f trigger=%.0f consec=%d min_floor=%.0f — cutting TTS playback",
				blockIdx, rms, floor, trigger, consecutive, minFloor))
			if o.OnTrigger != nil {
				if err := callQuietly(o.OnTrigger); err != nil {
					voiceLog().Debug(fmt.Sprintf("Barge-in trigger callback failed: %s", err))
				}
			}
			tripped = true
			if !o.Capture {
				return nil
			}
			path, err = captureUntilQuiet(stream, block, preRoll.items, endpointBlocks, maxBlocks)
			return err
		}
		return nil
	})
	if err != nil {
		return false, "", err
	}
	if o.Capture && path == "" {
		return false, "", nil
	}
	return tripped, path, nil
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

// Debug logs a VAD line, echoed to stderr when ICEBATH_VOICE_DEBUG is 1
func vadLog(msg string) {
	voiceLog().Debug(msg)
	if pystr.Strip(os.Getenv("ICEBATH_VOICE_DEBUG")) == "1" {
		fmt.Fprintln(os.Stderr, "[voice-vad] "+msg)
	}
}

// Reads blocks after the pre roll until the endpoint silence or the block cap, then writes the WAV
func captureUntilQuiet(stream pysounddevice.InputStream, block int, preRoll [][]int16, endpointBlocks, maxBlocks int) (string, error) {
	frames := append([][]int16(nil), preRoll...)
	quiet := 0
	for range maxBlocks {
		data, _, err := stream.Read(block)
		if err != nil {
			return "", err
		}
		frames = append(frames, data)
		if pynumpy.RMSInt16(data) < silenceRMSThreshold {
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
		pct90 = pynumpy.Percentile(d.ambient.items, 90)
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
		vadLog(fmt.Sprintf("block=%d rms=%.0f floor=%.0f trigger=%.0f above=%s window=%d/%d phase=%s",
			d.blockIdx, rms, d.quietFloor, trigger, pyBool(above), window, d.tripNeeded, phase))
	}
	if !(above && window >= d.tripNeeded) {
		return ""
	}
	vadLog(fmt.Sprintf("TRIPPED (%s): block=%d rms=%.0f floor=%.0f trigger=%.0f window=%d/%d",
		phase, d.blockIdx, rms, d.quietFloor, trigger, window, len(d.recentAbove.items)))
	return phase
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
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
	endpointBlocks := max(1, pyFloorDivMS(o.EndpointSilenceMS))
	mult := defaultBargeMultiplier
	if o.Multiplier != 0 {
		mult = o.Multiplier
	}
	detector := newBargeDetector(mult, max(1, pyFloorDivMS(o.CalibrationMS)), max(1, pyFloorDivMS(o.SustainedMS)), max(0, pyFloorDivMS(o.GraceMS)))
	preRoll := &boundedDeque[[]int16]{maxlen: max(1, pyFloorDivMS(o.PreRollMS))}
	path := ""
	err = withListenStream(sd, func(stream pysounddevice.InputStream) error {
		for !shouldStop() {
			data, _, err := stream.Read(block)
			if err != nil {
				return err
			}
			preRoll.append(data)
			playing := o.IsPlaying != nil && o.IsPlaying()
			phase := detector.feed(pynumpy.RMSInt16(data), playing)
			if phase == "" {
				continue
			}
			if o.OnTrigger != nil {
				if err := callQuietly(func() { o.OnTrigger(phase) }); err != nil {
					voiceLog().Debug(fmt.Sprintf("full-duplex trigger callback failed: %s", err))
				}
			}
			path, err = captureUntilQuiet(stream, block, preRoll.items, endpointBlocks, max(1, pyFloorDivMS(o.MaxUtteranceMS)))
			return err
		}
		return nil
	})
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("Full-duplex listener failed: %s", err))
		return ""
	}
	return path
}
