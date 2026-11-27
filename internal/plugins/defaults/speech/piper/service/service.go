// Piper's voices on this machine, run by the native speech engine
package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/piper/config"
	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/sherpa"
	"github.com/nickheyer/icebath/internal/terminal"
)

func Register(registry *speech.Registry) {
	p := &piper{}
	registry.Register(speech.Services{Speakers: []speech.Voice{{
		Name: config.Name, Title: "Piper", Label: "Piper (local)", About: "Voices in 44 languages on this computer, no API key needed",
		Local:      true,
		Setup:      speech.Setup{Badge: "free"},
		Ready:      func(*speech.Job) bool { return sherpa.Supported() },
		Limit:      func(*speech.Job) int { return config.DefaultLimit },
		Synthesize: p.synthesize,
		Opus:       speech.TranscodedOpus,
		Warm:       func(ctx context.Context, job *speech.Job) error { _, err := p.load(ctx, job); return err },
		Release: func(context.Context, *speech.Job) error {
			if n := p.voices.Clear(); n > 0 {
				slog.Info(fmt.Sprintf("[TTS] released %d resident local model(s)", n))
			}
			return nil
		},
		Resident: true,
		Choices:  []speech.Choices{speech.Fixed(config.Voice, Voices()...)},
	}}})
}

type piper struct {
	voices speech.Resident[*sherpa.Voice]
}

// Files of the voice setting: a voice file you have, then one of the name in the voices folder, then the catalog's
func resolve(ctx context.Context, job *speech.Job, name string) (voice, error) {
	if candidate := terminal.ExpandUser(name); strings.EqualFold(filepath.Ext(candidate), ".onnx") && exists(candidate) {
		return adopt(ctx, job.Speech, candidate)
	}
	folder := filepath.Join(job.Config.ProfileDir(job.Config.Profile()), "cache", "piper-voices")
	if own := job.Text(config.VoicesDir, ""); own != "" {
		folder = terminal.ExpandUser(own)
	}
	if local := filepath.Join(folder, name+".onnx"); exists(local) {
		return adopt(ctx, job.Speech, local)
	}
	slog.Info(fmt.Sprintf("[Piper] Preparing voice '%s' (first use downloads it)", name))
	return fetch(ctx, job.Speech, name)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The voice of the settings loaded into the engine, the scales that are set over its own; refused where the engine has no build
func (p *piper) load(ctx context.Context, job *speech.Job) (*sherpa.Voice, error) {
	if !sherpa.Supported() {
		return nil, speech.Absent("Piper provider selected but the local speech engine has no build for this platform. Start 'icebath', type " +
			"'/voice setup' and choose another provider.")
	}
	files, err := resolve(ctx, job, job.Text(config.Voice, config.DefaultVoice))
	if err != nil {
		return nil, err
	}
	for key, scale := range map[string]*float64{config.LengthScale: &files.lengthScale, config.NoiseScale: &files.noiseScale, config.NoiseWScale: &files.noiseW} {
		if job.Set(key) {
			*scale = job.Float(key)
		}
	}
	gpu := job.Config.GetBool(config.CUDA)
	key := fmt.Sprintf("%s::cuda=%t::scales=%g,%g,%g", files.model, gpu, files.lengthScale, files.noiseScale, files.noiseW)
	return p.voices.Load(key, func() (*sherpa.Voice, error) {
		slog.Info("[Piper] Loading voice: " + files.model)
		engine, err := sherpa.Load(ctx, job.Speech, gpu)
		if err != nil {
			return nil, err
		}
		v, err := engine.NewVoice(files.options(min(runtime.NumCPU(), 4)))
		if err == nil {
			slog.Info("[Piper] Voice loaded")
		}
		return v, err
	})
}

// Samples scaled to full range when asked, multiplied by the volume and clipped
func shape(samples []float32, normalize bool, volume float64) []float32 {
	scale := float32(volume)
	if normalize {
		var peak float32
		for _, s := range samples {
			peak = max(peak, float32(math.Abs(float64(s))))
		}
		if peak < 1e-8 {
			scale = 0
		} else {
			scale /= peak
		}
	}
	out := make([]float32, len(samples))
	for i, s := range samples {
		out[i] = min(max(s*scale, -1), 1)
	}
	return out
}

// 16 bit little endian PCM of float samples
func pcm16(samples []float32) []byte {
	out := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(math.Round(float64(min(max(s, -1), 1))*32767))))
	}
	return out
}

// Speech of the engine as WAV, then the container the path asks for
func (p *piper) synthesize(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
	v, err := p.load(ctx, job)
	if err != nil {
		return "", err
	}
	speed, _ := job.Speed(request, "")
	samples, rate, err := v.Speak(request.Text, max(0, job.Config.GetInt(config.SpeakerID)), float32(speed))
	if err != nil {
		return "", err
	}
	samples = shape(samples, job.Config.GetBool(config.Normalize), job.Float(config.Volume))
	wav := speech.WAVPath(request.Path)
	if err := os.WriteFile(wav, speech.WAV(pcm16(samples), rate, 1, 2), 0o666); err != nil {
		return "", err
	}
	return job.FromWAV(ctx, wav, request.Path)
}
