package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/speech"
	"github.com/nickheyer/icebath/internal/speech/sherpa"
)

// Size and sha256 of a voice archive
type pin struct {
	size   int64
	sha256 string
}

// Where the speech engine project publishes its voices
const voiceBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/"

// Phonemizer data every converted voice needs beside its model
var espeakData = speech.Asset{Name: "espeak-ng-data.tar.bz2", URL: voiceBase + "espeak-ng-data.tar.bz2", Size: 7252012,
	SHA256: "4135ccf82e1f40613491c0874d4945ae9e9c7840933d8e25a6f9e003d9ebf533"}

// Model files and synthesis scales of one voice
type voice struct {
	model, tokens, data             string
	noiseScale, noiseW, lengthScale float64
}

// What this plugin reads of a Piper voice description
type description struct {
	Audio struct {
		SampleRate int `json:"sample_rate"`
	} `json:"audio"`
	Espeak struct {
		Voice string `json:"voice"`
	} `json:"espeak"`
	Inference struct {
		NoiseScale  *float64 `json:"noise_scale"`
		LengthScale *float64 `json:"length_scale"`
		NoiseW      *float64 `json:"noise_w"`
	} `json:"inference"`
	PhonemeType  string           `json:"phoneme_type"`
	PhonemeIDMap map[string][]int `json:"phoneme_id_map"`
	NumSpeakers  int              `json:"num_speakers"`
	Language     struct {
		NameNative string `json:"name_native"`
	} `json:"language"`
}

// Names of every catalog voice, sorted
func Voices() []string {
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func describe(path string) (*description, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d description
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("%s is not a Piper voice description: %w", path, err)
	}
	return &d, nil
}

// Voice with the scales its description recommends, the engine's defaults where it names none
func (v voice) scaled(d *description) voice {
	v.noiseScale, v.noiseW, v.lengthScale = 0.667, 0.8, 1.0
	if d == nil {
		return v
	}
	if d.Inference.NoiseScale != nil {
		v.noiseScale = *d.Inference.NoiseScale
	}
	if d.Inference.NoiseW != nil {
		v.noiseW = *d.Inference.NoiseW
	}
	if d.Inference.LengthScale != nil {
		v.lengthScale = *d.Inference.LengthScale
	}
	return v
}

// Catalog voice of the name, fetched on first use
func fetch(ctx context.Context, sp *speech.Speech, name string) (voice, error) {
	p, ok := catalog[name]
	if !ok {
		return voice{}, fmt.Errorf("Piper voice %q is not in the voice catalog, pick one like en_US-lessac-medium or give the path of an .onnx voice file", name)
	}
	archive := "vits-piper-" + name + ".tar.bz2"
	tree, err := sp.Tree(ctx, speech.Asset{Name: archive, URL: voiceBase + archive, Size: p.size, SHA256: p.sha256})
	if err != nil {
		return voice{}, err
	}
	dir := filepath.Join(tree, "vits-piper-"+name)
	models, _ := filepath.Glob(filepath.Join(dir, "*.onnx"))
	if len(models) != 1 {
		return voice{}, fmt.Errorf("voice archive %s holds %d model files, expected one", archive, len(models))
	}
	v := voice{model: models[0], tokens: filepath.Join(dir, "tokens.txt"), data: filepath.Join(dir, "espeak-ng-data")}
	d, err := describe(models[0] + ".json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return voice{}, err
	}
	return v.scaled(d), nil
}

// One metadata entry of an ONNX model in protobuf wire form
func metadata(key, value string) []byte {
	entry := append([]byte{0x0a}, binary.AppendUvarint(nil, uint64(len(key)))...)
	entry = append(entry, key...)
	entry = append(entry, 0x12)
	entry = binary.AppendUvarint(entry, uint64(len(value)))
	entry = append(entry, value...)
	return append(binary.AppendUvarint([]byte{0x72}, uint64(len(entry))), entry...)
}

// Token table of a voice, one symbol and its first id per line in id order
func tokens(d *description) []byte {
	type row struct {
		symbol string
		id     int
	}
	rows := make([]row, 0, len(d.PhonemeIDMap))
	for symbol, ids := range d.PhonemeIDMap {
		if len(ids) > 0 {
			rows = append(rows, row{symbol, ids[0]})
		}
	}
	slices.SortFunc(rows, func(a, b row) int { return a.id - b.id })
	var out strings.Builder
	for _, r := range rows {
		out.WriteString(r.symbol + " " + strconv.Itoa(r.id) + "\n")
	}
	return []byte(out.String())
}

// Writes the engine's form of a voice Piper published as a model with a description beside it
func convert(model string, d *description, dir string) error {
	src, err := os.Open(model)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(dir, "voice.onnx"))
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if err == nil {
		for _, kv := range [][2]string{{"model_type", "vits"}, {"comment", "piper"}, {"language", d.Language.NameNative}, {"voice", d.Espeak.Voice},
			{"has_espeak", "1"}, {"n_speakers", strconv.Itoa(max(d.NumSpeakers, 1))}, {"sample_rate", strconv.Itoa(d.Audio.SampleRate)}} {
			if _, err = dst.Write(metadata(kv[0], kv[1])); err != nil {
				break
			}
		}
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tokens.txt"), tokens(d), 0o644)
}

// Voice of a model file you already have, as Piper publishes it with its .onnx.json, or already converted for the engine
func adopt(ctx context.Context, sp *speech.Speech, model string) (voice, error) {
	info, err := os.Stat(model)
	if err != nil {
		return voice{}, err
	}
	dir := filepath.Dir(model)
	d, err := describe(model + ".json")
	if errors.Is(err, os.ErrNotExist) {
		table, data := filepath.Join(dir, "tokens.txt"), filepath.Join(dir, "espeak-ng-data")
		if _, terr := os.Stat(table); terr != nil {
			return voice{}, fmt.Errorf("%s has no %s beside it, a Piper voice is an .onnx file with its .onnx.json", model, filepath.Base(model)+".json")
		}
		return voice{model: model, tokens: table, data: data}.scaled(nil), nil
	}
	if err != nil {
		return voice{}, err
	}
	if d.PhonemeType != "" && d.PhonemeType != "espeak" {
		return voice{}, fmt.Errorf("%s uses %s phonemes, the speech engine runs Piper voices built on espeak phonemes", model, d.PhonemeType)
	}
	if len(d.PhonemeIDMap) == 0 || d.Audio.SampleRate <= 0 || d.Espeak.Voice == "" {
		return voice{}, fmt.Errorf("%s.json lacks the phoneme table, sample rate or espeak voice of a Piper voice", model)
	}
	espeak, err := sp.Tree(ctx, espeakData)
	if err != nil {
		return voice{}, err
	}
	abs, err := filepath.Abs(model)
	if err != nil {
		return voice{}, err
	}
	sum := sha256.Sum256([]byte(abs + "|" + strconv.FormatInt(info.Size(), 10) + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	converted := filepath.Join(sp.Assets(), "piper-voices", hex.EncodeToString(sum[:12]))
	v := voice{model: filepath.Join(converted, "voice.onnx"), tokens: filepath.Join(converted, "tokens.txt"), data: filepath.Join(espeak, "espeak-ng-data")}
	if _, err := os.Stat(v.tokens); err == nil {
		return v.scaled(d), nil
	}
	if err := os.MkdirAll(filepath.Dir(converted), 0o755); err != nil {
		return voice{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(converted), "convert.*.part")
	if err != nil {
		return voice{}, err
	}
	defer os.RemoveAll(staging)
	if err := convert(model, d, staging); err != nil {
		return voice{}, err
	}
	if err := os.Rename(staging, converted); err != nil {
		if _, serr := os.Stat(v.tokens); serr != nil {
			return voice{}, err
		}
	}
	return v.scaled(d), nil
}

// Engine options of the voice at its scales
func (v voice) options(threads int) sherpa.VoiceOptions {
	return sherpa.VoiceOptions{Model: v.model, Tokens: v.tokens, DataDir: v.data, NoiseScale: float32(v.noiseScale), NoiseScaleW: float32(v.noiseW),
		LengthScale: float32(v.lengthScale), Threads: threads}
}
