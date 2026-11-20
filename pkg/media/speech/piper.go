package speech

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
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/fetch"
	"github.com/nickheyer/icebath/pkg/internal/sherpaonnx"
)

type pinned struct {
	size   int64
	sha256 string
}

var voiceBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/"

// Phonemizer data every converted voice needs beside its model
var espeakData = fetch.Asset{Name: "espeak-ng-data.tar.bz2", URL: voiceBase + "espeak-ng-data.tar.bz2", Size: 7252012,
	SHA256: "4135ccf82e1f40613491c0874d4945ae9e9c7840933d8e25a6f9e003d9ebf533"}

// Native speech engine of this process, on the GPU build when asked
func Engine(ctx context.Context, gpu bool) (*sherpaonnx.Module, error) {
	return sherpaonnx.Load(ctx, gpu)
}

// Whether the speech engine publishes a library for this platform
func Supported() bool { return sherpaonnx.Supported() }

// Model files and synthesis defaults of one Piper voice
type PiperVoice struct {
	Model       string
	Tokens      string
	DataDir     string
	NoiseScale  float64
	NoiseW      float64
	LengthScale float64
}

// Returned for a voice name the catalog does not hold
type UnknownVoiceError struct {
	Name string
}

func (e *UnknownVoiceError) Error() string {
	return fmt.Sprintf("Piper voice %q is not in the voice catalog, pick one like en_US-lessac-medium or give the path of an .onnx voice file", e.Name)
}

// The parts of a Piper voice description this package reads
type piperConfig struct {
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

// Names of every catalog voice in order
func PiperVoices() []string {
	names := make([]string, 0, len(piperArchives))
	for name := range piperArchives {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readPiperConfig(path string) (*piperConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config piperConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%s is not a Piper voice description: %w", path, err)
	}
	return &config, nil
}

// Voice with the scales its description recommends, the engine defaults when it names none
func voiceWithDefaults(v PiperVoice, config *piperConfig) PiperVoice {
	v.NoiseScale, v.NoiseW, v.LengthScale = 0.667, 0.8, 1.0
	if config == nil {
		return v
	}
	if config.Inference.NoiseScale != nil {
		v.NoiseScale = *config.Inference.NoiseScale
	}
	if config.Inference.NoiseW != nil {
		v.NoiseW = *config.Inference.NoiseW
	}
	if config.Inference.LengthScale != nil {
		v.LengthScale = *config.Inference.LengthScale
	}
	return v
}

// Catalog voice by name, fetched into the icebath cache on first use
func FetchPiper(ctx context.Context, name string) (PiperVoice, error) {
	pin, ok := piperArchives[name]
	if !ok {
		return PiperVoice{}, &UnknownVoiceError{Name: name}
	}
	archive := "vits-piper-" + name + ".tar.bz2"
	tree, err := fetch.Tree(ctx, fetch.Asset{Name: archive, URL: voiceBase + archive, Size: pin.size, SHA256: pin.sha256})
	if err != nil {
		return PiperVoice{}, err
	}
	dir := filepath.Join(tree, "vits-piper-"+name)
	models, _ := filepath.Glob(filepath.Join(dir, "*.onnx"))
	if len(models) != 1 {
		return PiperVoice{}, fmt.Errorf("voice archive %s holds %d model files, expected one", archive, len(models))
	}
	voice := PiperVoice{Model: models[0], Tokens: filepath.Join(dir, "tokens.txt"), DataDir: filepath.Join(dir, "espeak-ng-data")}
	config, err := readPiperConfig(models[0] + ".json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return PiperVoice{}, err
	}
	return voiceWithDefaults(voice, config), nil
}

// One metadata entry of an ONNX model in protobuf wire form
func metadataEntry(key, value string) []byte {
	entry := append([]byte{0x0a}, binary.AppendUvarint(nil, uint64(len(key)))...)
	entry = append(entry, key...)
	entry = append(entry, 0x12)
	entry = binary.AppendUvarint(entry, uint64(len(value)))
	entry = append(entry, value...)
	return append(binary.AppendUvarint([]byte{0x72}, uint64(len(entry))), entry...)
}

// Token table of a voice, one symbol and its first id per line in id order
func piperTokens(config *piperConfig) []byte {
	type row struct {
		symbol string
		id     int
	}
	rows := make([]row, 0, len(config.PhonemeIDMap))
	for symbol, ids := range config.PhonemeIDMap {
		if len(ids) > 0 {
			rows = append(rows, row{symbol, ids[0]})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	var out strings.Builder
	for _, r := range rows {
		out.WriteString(r.symbol + " " + strconv.Itoa(r.id) + "\n")
	}
	return []byte(out.String())
}

// Writes the engine's form of a voice Piper published as a model with a description beside it
func convertPiper(model string, config *piperConfig, dir string) error {
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
		for _, kv := range [][2]string{{"model_type", "vits"}, {"comment", "piper"}, {"language", config.Language.NameNative},
			{"voice", config.Espeak.Voice}, {"has_espeak", "1"}, {"n_speakers", strconv.Itoa(max(config.NumSpeakers, 1))},
			{"sample_rate", strconv.Itoa(config.Audio.SampleRate)}} {
			if _, err = dst.Write(metadataEntry(kv[0], kv[1])); err != nil {
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
	return os.WriteFile(filepath.Join(dir, "tokens.txt"), piperTokens(config), 0o644)
}

// Voice from a model file the user already has, as Piper publishes it or already converted for the engine
func AdoptPiper(ctx context.Context, model string) (PiperVoice, error) {
	info, err := os.Stat(model)
	if err != nil {
		return PiperVoice{}, err
	}
	dir := filepath.Dir(model)
	config, err := readPiperConfig(model + ".json")
	if errors.Is(err, os.ErrNotExist) {
		tokens, data := filepath.Join(dir, "tokens.txt"), filepath.Join(dir, "espeak-ng-data")
		if _, terr := os.Stat(tokens); terr != nil {
			return PiperVoice{}, fmt.Errorf("%s has no %s beside it, a Piper voice is an .onnx file with its .onnx.json", model, filepath.Base(model)+".json")
		}
		return voiceWithDefaults(PiperVoice{Model: model, Tokens: tokens, DataDir: data}, nil), nil
	}
	if err != nil {
		return PiperVoice{}, err
	}
	if config.PhonemeType != "" && config.PhonemeType != "espeak" {
		return PiperVoice{}, fmt.Errorf("%s uses %s phonemes, the speech engine runs Piper voices built on espeak phonemes", model, config.PhonemeType)
	}
	if len(config.PhonemeIDMap) == 0 || config.Audio.SampleRate <= 0 || config.Espeak.Voice == "" {
		return PiperVoice{}, fmt.Errorf("%s.json lacks the phoneme table, sample rate or espeak voice of a Piper voice", model)
	}
	espeak, err := fetch.Tree(ctx, espeakData)
	if err != nil {
		return PiperVoice{}, err
	}
	abs, err := filepath.Abs(model)
	if err != nil {
		return PiperVoice{}, err
	}
	key := sha256.Sum256([]byte(abs + "|" + strconv.FormatInt(info.Size(), 10) + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10)))
	converted := filepath.Join(fetch.Root(), "piper-voices", hex.EncodeToString(key[:12]))
	voice := PiperVoice{Model: filepath.Join(converted, "voice.onnx"), Tokens: filepath.Join(converted, "tokens.txt"), DataDir: filepath.Join(espeak, "espeak-ng-data")}
	if _, err := os.Stat(voice.Tokens); err == nil {
		return voiceWithDefaults(voice, config), nil
	}
	if err := os.MkdirAll(filepath.Dir(converted), 0o755); err != nil {
		return PiperVoice{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(converted), "convert.*.part")
	if err != nil {
		return PiperVoice{}, err
	}
	defer os.RemoveAll(staging)
	if err := convertPiper(model, config, staging); err != nil {
		return PiperVoice{}, err
	}
	if err := os.Rename(staging, converted); err != nil {
		if _, serr := os.Stat(voice.Tokens); serr != nil {
			return PiperVoice{}, err
		}
	}
	return voiceWithDefaults(voice, config), nil
}

// Engine options of the voice at its recommended scales
func (v PiperVoice) Options(threads int) sherpaonnx.VoiceOptions {
	return sherpaonnx.VoiceOptions{Model: v.Model, Tokens: v.Tokens, DataDir: v.DataDir, NoiseScale: float32(v.NoiseScale),
		NoiseScaleW: float32(v.NoiseW), LengthScale: float32(v.LengthScale), Threads: threads}
}
