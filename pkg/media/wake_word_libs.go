package media

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pvporcupine"
	"github.com/nickheyer/icebath/pkg/internal/pyopenwakeword"
	"github.com/nickheyer/icebath/pkg/internal/sherpaonnx"
	"github.com/nickheyer/icebath/pkg/media/speech"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Directory of an installed Python package on this process's import path or in the selected environment, else ModuleNotFoundError
func wakeInstalledPackage(ctx context.Context, name string) (string, error) {
	roots := pm.ImportPathEntries()
	venv, envErr := pm.SelectedVenv(ctx, pm.RepoRoot())
	if envErr == nil && venv != "" {
		var site string
		if site, envErr = pm.SitePackages(venv); envErr == nil {
			roots = append(roots, site)
		}
	}
	for _, root := range roots {
		dir := filepath.Join(root, name)
		if info, err := os.Stat(filepath.Join(dir, "__init__.py")); err == nil && info.Mode().IsRegular() {
			return dir, nil
		}
	}
	return "", &WakeWordError{Class: "ModuleNotFoundError", Msg: "No module named '" + name + "'", Cause: envErr}
}

type pyOpenWakeWordLib struct{ module *pyopenwakeword.Module }

func (l pyOpenWakeWordLib) FeaturesFromBuiltin() (owwFeatures, error) {
	features, err := l.module.FeaturesFromBuiltin(nil, nil)
	if err != nil {
		return nil, err
	}
	return features, nil
}

func (l pyOpenWakeWordLib) ModelFromModel(modelPath string) (owwModel, error) {
	model, err := l.module.FromModel(modelPath, nil)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// pyopen_wakeword of the installed distribution over the tensorflowlite_c library it carries
func importPyOpenWakeWord(ctx context.Context) (pyOpenWakeWordModule, error) {
	dir, err := wakeInstalledPackage(ctx, "pyopen_wakeword")
	if err != nil {
		return nil, err
	}
	return pyOpenWakeWordLib{pyopenwakeword.Import(dir)}, nil
}

type pvporcupineLib struct{ module *pvporcupine.Module }

func (l pvporcupineLib) Create(accessKey string, keywordPaths, keywords []string, sensitivities []float64) (pvPorcupine, error) {
	handle, err := l.module.Create(accessKey, pvporcupine.CreateOptions{KeywordPaths: keywordPaths, Keywords: keywords, Sensitivities: sensitivities})
	if err != nil {
		return nil, err
	}
	return handle, nil
}

// pvporcupine of the installed distribution with its platform and built in keywords resolved
func importPvporcupine(ctx context.Context) (pvporcupineModule, error) {
	dir, err := wakeInstalledPackage(ctx, "pvporcupine")
	if err != nil {
		return nil, err
	}
	module, err := pvporcupine.Import(dir)
	if err != nil {
		return nil, err
	}
	return pvporcupineLib{module}, nil
}

type sherpaOnnxLib struct{ module *sherpaonnx.Module }

func (l sherpaOnnxLib) Text2Token(texts []string, tokens, tokensType, bpeModel string) ([][]string, error) {
	encoded, err := l.module.Text2Token(texts, tokens, sherpaonnx.Text2TokenOptions{TokensType: tokensType, BPEModel: &bpeModel})
	if err != nil {
		return nil, err
	}
	out := make([][]string, len(encoded))
	for i, row := range encoded {
		out[i] = make([]string, len(row))
		for j, token := range row {
			out[i][j] = token.(string)
		}
	}
	return out, nil
}

func (l sherpaOnnxLib) KeywordSpotter(cfg sherpaKWSConfig) (sherpaKeywordSpotter, error) {
	options := sherpaonnx.DefaultKeywordSpotterOptions(cfg.Tokens, cfg.Encoder, cfg.Decoder, cfg.Joiner, cfg.KeywordsFile)
	options.KeywordsThreshold, options.NumThreads = cfg.KeywordsThreshold, cfg.NumThreads
	spotter, err := l.module.NewKeywordSpotter(options)
	if err != nil {
		return nil, err
	}
	return sherpaSpotter{spotter}, nil
}

type sherpaSpotter struct{ spotter *sherpaonnx.KeywordSpotter }

type sherpaOnlineStream struct{ stream *sherpaonnx.OnlineStream }

func (s sherpaOnlineStream) AcceptWaveform(sampleRate int, samples []float32) error {
	s.stream.AcceptWaveform(sampleRate, samples)
	return nil
}

func (k sherpaSpotter) CreateStream() (sherpaStream, error) {
	return sherpaOnlineStream{k.spotter.CreateStream(nil)}, nil
}

func (k sherpaSpotter) IsReady(s sherpaStream) bool {
	return k.spotter.IsReady(s.(sherpaOnlineStream).stream)
}

func (k sherpaSpotter) DecodeStream(s sherpaStream) error {
	k.spotter.DecodeStream(s.(sherpaOnlineStream).stream)
	return nil
}

func (k sherpaSpotter) GetResult(s sherpaStream) string {
	return k.spotter.GetResult(s.(sherpaOnlineStream).stream)
}

func (k sherpaSpotter) ResetStream(s sherpaStream) error {
	k.spotter.ResetStream(s.(sherpaOnlineStream).stream)
	return nil
}

// Whether the process runs the speech engine on the GPU, the Piper setting every engine user follows
func speechOnGPU(ctx context.Context) bool {
	return pyTruthy(dictGetDefault(ttsSection(wakeLoadTTSConfig(ctx), "piper"), "use_cuda", false))
}

// Native speech engine of the process, the seam tests replace
var wakeSpeechEngine = func(ctx context.Context) (*sherpaonnx.Module, error) { return speech.Engine(ctx, speechOnGPU(ctx)) }

// Keyword spotting over the native speech engine, its library fetched on first use
func importSherpaOnnx(ctx context.Context) (sherpaOnnxModule, error) {
	module, err := wakeSpeechEngine(ctx)
	if err != nil {
		return nil, &WakeWordError{Class: "ImportError", Msg: err.Error(), Cause: err}
	}
	return sherpaOnnxLib{module}, nil
}

// Loads the Porcupine library from the pvporcupine package in the runtime environment
func LoadPorcupineLibrary(ctx context.Context) error {
	dir, err := wakeInstalledPackage(ctx, "pvporcupine")
	if err != nil {
		return err
	}
	module, err := pvporcupine.Import(dir)
	if err != nil {
		return err
	}
	return module.LoadLibrary()
}

// Whether the speech engine behind the sherpa wake word publishes a library for this platform
func SherpaOnnxAvailable() bool { return wakeSpeechSupported() }

// Loads the tensorflowlite_c library from the pyopen_wakeword package in the runtime environment
func LoadOpenWakeWordLibrary(ctx context.Context) error {
	dir, err := wakeInstalledPackage(ctx, "pyopen_wakeword")
	if err != nil {
		return err
	}
	return pyopenwakeword.Import(dir).LoadLibrary()
}
