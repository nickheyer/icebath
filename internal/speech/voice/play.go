package voice

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/portaudio"
)

// Speakers of this machine: what plays, how many playbacks sound and the player program of the one running
type player struct {
	speech *speech.Speech
	mu     sync.Mutex
	active int
	proc   *program
	volume func() float64
	// Stop of the thinking blips while they play
	thinking chan struct{}
}

// A player program while it runs: closed done once it ended, stopped once Stop ended it
type program struct {
	cmd     *exec.Cmd
	done    chan struct{}
	stopped atomic.Bool
}

// Counts one playback in or out, never below zero
func (p *player) mark(active bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if active {
		p.active++
	} else {
		p.active = max(0, p.active-1)
	}
}

// Whether any playback sounds
func (p *player) sounding() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active > 0
}

// macOS asks for microphone permission when PortAudio opens output, so speech plays through afplay there
func deviceOutput() bool { return runtime.GOOS != "darwin" }

func samplesOf(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return out
}

// Plays through PortAudio until the stream goes idle or the timeout passes
func playAndWait(pa portaudio.Audio, data []int16, rate int, timeout time.Duration, block int) error {
	if err := pa.Play(data, float64(rate), block); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stream, err := pa.Playback()
		if err != nil {
			return err
		}
		active, err := stream.Active()
		if err != nil {
			return err
		}
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pa.Stop()
	return nil
}

// Evenly spaced values from start to stop, the end included when asked, each start plus a whole number of steps
func linspace(start, stop float64, n int, end bool) []float64 {
	out := make([]float64, max(n, 0))
	div := n
	if end {
		div = n - 1
	}
	if div <= 0 {
		if n > 0 {
			out[0] = start
		}
		return out
	}
	delta := stop - start
	step := delta / float64(div)
	for i := range out {
		if step == 0 && delta != 0 {
			out[i] = start + float64(i)/float64(div)*delta
		} else {
			out[i] = start + float64(i)*step
		}
	}
	if end {
		out[n-1] = stop
	}
	return out
}

// Sine beeps faded at both ends with 60 ms gaps, at the volume
func beeps(frequency int, duration float64, count int, volume float64) []int16 {
	rate := float64(sampleRate)
	n := int(rate * duration)
	gap := make([]int16, int(rate*0.06))
	var out []int16
	for i := range count {
		t := linspace(0, duration, n, false)
		tone := make([]float64, len(t))
		for j, v := range t {
			tone[j] = math.Sin(2 * math.Pi * float64(frequency) * v)
		}
		fade := min(int(rate*0.01), n/4)
		up, down := linspace(0, 1, fade, true), linspace(1, 0, fade, true)
		for j := range min(len(tone), len(up)) {
			tone[j] *= up[j]
		}
		tail := tone[len(tone)-fade:]
		for j := range min(len(tail), len(down)) {
			tail[j] *= down[j]
		}
		for _, v := range tone {
			out = append(out, int16(v*volume*32767))
		}
		if i < count-1 {
			out = append(out, gap...)
		}
	}
	return out
}

// Plays mono PCM through a temp WAV and the players, the macOS route that avoids its permission prompt
func (p *player) throughFile(data []int16, rate int) {
	file, err := os.CreateTemp("", "icebath-tone-*.wav")
	if err != nil {
		slog.Debug(fmt.Sprintf("Tone tempfile playback failed: %s", err))
		return
	}
	defer os.Remove(file.Name())
	_, err = file.Write(speech.WAV(speech.PCMBytes(data), rate, channels, sampleBits))
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Tone tempfile playback failed: %s", err))
		return
	}
	p.Play(file.Name())
}

// Plays count beeps at the beep volume
func (p *player) beep(frequency int, duration float64, count int) {
	if count <= 0 {
		return
	}
	data := beeps(frequency, duration, count, p.volume())
	if !deviceOutput() {
		p.throughFile(data, sampleRate)
		return
	}
	pa, err := audio()
	if err != nil {
		if !unloadable(err) && !missingLibrary(err) {
			slog.Debug(fmt.Sprintf("Beep playback failed: %s", err))
		}
		return
	}
	if err := playAndWait(pa, data, sampleRate, 2*time.Second, 0); err != nil {
		slog.Debug(fmt.Sprintf("Beep playback failed: %s", err))
	}
}

// A soft falling water drop blip, quieter than the beeps
func (p *player) blip(frequency float64) []int16 {
	rate := float64(sampleRate)
	duration := 0.16
	n := int(rate * duration)
	t := linspace(0, duration, n, false)
	glide := linspace(1.0, 0.72, n, true)
	cumulative := 0.0
	tone := make([]float64, n)
	for i := range tone {
		cumulative += frequency * glide[i]
		phase := 2 * math.Pi * cumulative / rate
		tone[i] = 0.8*math.Sin(phase) + 0.2*math.Sin(phase/2.0)
	}
	attack := int(0.02 * rate)
	ramp := linspace(0, 1, attack, true)
	volume := p.volume() * 0.5
	out := make([]int16, n)
	for i := range out {
		env := 1.0
		if i < len(ramp) {
			env = ramp[i]
		}
		env *= math.Exp(-t[i] * 14.0)
		out[i] = int16(tone[i] * env * volume * 32767)
	}
	return out
}

// Starts the thinking blips once, each one played only when allowed; silent on macOS
func (p *player) think(allowed func() bool) {
	p.mu.Lock()
	if p.thinking != nil {
		p.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	p.thinking = stop
	p.mu.Unlock()
	if !deviceOutput() {
		return
	}
	go func() {
		pa, err := audio()
		if err != nil {
			if !unloadable(err) && !missingLibrary(err) {
				slog.Error(fmt.Sprintf("Thinking sound could not open audio output: %s", err))
			}
			return
		}
		blips := [][]int16{p.blip(392.0), p.blip(329.6)}
		played := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			if allowed == nil || allowed() {
				blip := blips[played%len(blips)]
				played++
				if err := pa.Play(blip, sampleRate, 0); err != nil {
					slog.Debug(fmt.Sprintf("Thinking sound blip failed: %s", err))
					return
				}
				select {
				case <-stop:
					pa.Stop()
					return
				case <-time.After(time.Duration(len(blip))*time.Second/sampleRate + 20*time.Millisecond):
				}
				pa.Stop()
			}
			select {
			case <-stop:
				return
			case <-time.After(800*time.Millisecond + mrand.N(400*time.Millisecond)):
			}
		}
	}()
}

// Stops the thinking blips at once
func (p *player) quiet() {
	p.mu.Lock()
	stop := p.thinking
	p.thinking = nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// Stops the player program that runs and PortAudio playback
func (p *player) Stop() {
	p.mu.Lock()
	proc := p.proc
	p.proc = nil
	p.mu.Unlock()
	if proc != nil {
		select {
		case <-proc.done:
		default:
			proc.stopped.Store(true)
			var err error
			if runtime.GOOS == "windows" {
				err = proc.cmd.Process.Kill()
			} else {
				err = proc.cmd.Process.Signal(syscall.SIGTERM)
			}
			if err == nil {
				slog.Info("Audio playback interrupted")
			}
		}
	}
	if pa, err := audio(); err == nil {
		pa.Stop()
	}
}

// Plays a file through PortAudio for WAV, else each player of this machine in turn until one plays it or Stop ends it, counted as a
// playback throughout
func (p *player) Play(path string) bool {
	p.mark(true)
	defer p.mark(false)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		slog.Warn(fmt.Sprintf("Audio file not found: %s", path))
		return false
	}
	if strings.HasSuffix(strings.ToLower(path), ".wav") && deviceOutput() && p.wave(path) {
		return true
	}
	for _, argv := range p.players(path) {
		if _, ok := found(argv[0]); !ok {
			continue
		}
		played, stopped := p.run(argv)
		if played {
			return true
		}
		if stopped {
			return false
		}
	}
	slog.Warn(fmt.Sprintf("No audio player available for %s", path))
	return false
}

// Plays a WAV through PortAudio, faded in with silence around it and 4096 frame blocks under WSL
func (p *player) wave(path string) bool {
	pa, err := audio()
	if err != nil {
		if !unloadable(err) && !missingLibrary(err) {
			slog.Debug(fmt.Sprintf("PortAudio playback failed: %s", err))
		}
		return false
	}
	w, err := speech.ReadWave(path)
	switch {
	case err != nil:
		if _, missing := errors.AsType[*fs.PathError](err); !missing {
			slog.Debug(fmt.Sprintf("PortAudio playback failed: %s", err))
		}
		return false
	case len(w.Frames)%2 != 0:
		slog.Debug("PortAudio playback failed: PCM data ends in a partial 16-bit sample")
		return false
	case w.Rate <= 0:
		slog.Debug(fmt.Sprintf("PortAudio playback failed: WAV sample rate %d is not positive", w.Rate))
		return false
	}
	data, rate, block := samplesOf(w.Frames), w.Rate, 0
	if wsl() {
		fade := linspace(0, 1, int(0.1*float64(rate)), true)
		for i := range min(len(data), len(fade)) {
			data[i] = int16(float64(data[i]) * fade[i])
		}
		padded := make([]int16, int(0.1*float64(rate)), int(0.15*float64(rate))+len(data))
		data = append(append(padded, data...), make([]int16, int(0.05*float64(rate)))...)
		block = 4096
	}
	if err := playAndWait(pa, data, rate, time.Duration(len(data))*time.Second/time.Duration(rate)+2*time.Second, block); err != nil {
		slog.Debug(fmt.Sprintf("PortAudio playback failed: %s", err))
		return false
	}
	return true
}

// The shell pipeline that converts to a Windows temp WAV and plays it through Media.SoundPlayer, nil off WSL
func wslPlayer(path string) []string {
	if !wslPowershell() {
		return nil
	}
	text := func(argv ...string) string {
		out, err := output(argv, 3*time.Second)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(strings.ToValidUTF8(string(out), "�"))
	}
	winTemp := text("cmd.exe", "/c", "echo %TEMP%")
	if winTemp == "" {
		return nil
	}
	linuxTemp := text("wslpath", "-u", winTemp)
	if linuxTemp == "" {
		return nil
	}
	raw := make([]byte, 4)
	rand.Read(raw)
	wav := filepath.Join(linuxTemp, "icebath-tts-"+hex.EncodeToString(raw)+".wav")
	winWAV := text("wslpath", "-w", wav)
	if winWAV == "" {
		return nil
	}
	script := fmt.Sprintf("(New-Object Media.SoundPlayer '%s').PlaySync()", strings.ReplaceAll(winWAV, "'", "''"))
	play := quote("ffmpeg", "-i", path, "-f", "wav", wav, "-loglevel", "quiet", "-y") + " && " + quote("powershell.exe", "-NoProfile", "-Command", script)
	return []string{"/bin/sh", "-c", fmt.Sprintf("( %s ); rc=$?; %s; exit $rc", play, quote("rm", "-f", wav))}
}

// POSIX shell quoting of each word
func quote(words ...string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_") == "" {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'"'"'`) + "'"
	}
	return strings.Join(out, " ")
}

// Players of this machine in the order tried
func (p *player) players(path string) [][]string {
	var out [][]string
	if runtime.GOOS == "darwin" {
		out = append(out, []string{"afplay", path})
	}
	if runtime.GOOS == "linux" {
		if ps := wslPlayer(path); ps != nil {
			out = append(out, ps)
		}
	}
	out = append(out, []string{"ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", path})
	if runtime.GOOS == "linux" {
		out = append(out, []string{"aplay", "-q", path})
	}
	return out
}

// Runs one player to its end as the playback Stop interrupts: whether it exited cleanly, and whether Stop ended it
func (p *player) run(argv []string) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = p.speech.Local().Environ(nil)
	if err := cmd.Start(); err != nil {
		slog.Debug(fmt.Sprintf("System player %s failed: %s", argv[0], err))
		return false, false
	}
	proc := &program{cmd: cmd, done: make(chan struct{})}
	p.mu.Lock()
	p.proc = proc
	p.mu.Unlock()
	err := cmd.Wait()
	close(proc.done)
	p.mu.Lock()
	if p.proc == proc {
		p.proc = nil
	}
	p.mu.Unlock()
	exit, exited := errors.AsType[*exec.ExitError](err)
	switch {
	case ctx.Err() != nil:
		slog.Warn(fmt.Sprintf("System player %s timed out, killing process", argv[0]))
	case err == nil:
		return true, false
	case proc.stopped.Load():
	case exited:
		slog.Debug(fmt.Sprintf("System player %s exited with code %d, trying next", argv[0], exitCode(exit.ProcessState)))
	default:
		slog.Debug(fmt.Sprintf("System player %s failed: %s", argv[0], err))
	}
	return false, proc.stopped.Load()
}

// Exit code of a process, the signal negated for one a signal ended
func exitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return state.ExitCode()
}
