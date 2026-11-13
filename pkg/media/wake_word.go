package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pynumpy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/openwakeword"
	"github.com/nickheyer/icebath/pkg/internal/porcupine"
	"github.com/nickheyer/icebath/pkg/media/speech"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// WakeSampleRate is the 16 kHz mono int16 rate every wake engine consumes
const WakeSampleRate = 16000

const (
	wakeDefaultConfirmationFrames = 3
	wakeSilencePeak               = 10
	wakeDefaultPhrase             = "hey icebath"
)

var (
	wakeFireCooldownSeconds      = 2.0
	wakeStartTimeoutSeconds      = 5.0
	wakeReadPollSeconds          = 0.05
	wakeReadStallFallbackSeconds = 2.0
	wakeHaltJoinSeconds          = 2.0
	wakeSilenceAlertSeconds      = 10.0
)

// WakeProviderNames lists the provider aliases in declaration order
var WakeProviderNames = []string{"porcupine", "sherpa", "sherpa-onnx", "kws", "open", "openwakeword", "oww", "local"}

// WakeProviders maps every provider alias to the engine it selects
var WakeProviders = map[string]string{
	"porcupine": "porcupine", "sherpa": "sherpa", "sherpa-onnx": "sherpa", "kws": "sherpa", "open": "sherpa",
	"openwakeword": "openwakeword", "oww": "openwakeword", "local": "openwakeword",
}

// WakeProviderPreference is the auto resolution order of the wake engines, the phrase spotting engine first
var WakeProviderPreference = []string{"sherpa", "openwakeword", "porcupine"}

// WakeWordInUse reports another surface or process owning the wake word microphone
type WakeWordInUse struct {
	Msg   string
	Cause error
}

func (e *WakeWordInUse) Error() string { return e.Msg }
func (e *WakeWordInUse) Unwrap() error { return e.Cause }

// WakeWordError is a Python exception of the wake word API with its class and cause
type WakeWordError struct {
	Class string
	Msg   string
	Cause error
}

func (e *WakeWordError) Error() string { return e.Msg }
func (e *WakeWordError) Unwrap() error { return e.Cause }

var wakeDefaults = map[string]any{
	"enabled": false, "surface": "auto", "input_device": nil, "capture": "auto",
	"provider": "auto", "phrase": wakeDefaultPhrase, "sensitivity": 0.6,
	"confirmation_frames": wakeDefaultConfirmationFrames, "start_new_session": true,
}

func wakeLog() *slog.Logger { return slog.Default().With("logger", "tools.wake_word") }

var (
	wakeLoadConfig = func(ctx context.Context) (config.Tree, error) {
		return config.LoadContext(ctx, home.Dir(ctx), config.Lookup(os.LookupEnv))
	}
	wakeSysPlatform         = sysPlatform
	wakeAudioAvailable      = func() (bool, error) { return audioAvailable() }
	wakeLocalInputReady     = localInputDeviceReady
	wakeSTTReady            = sttReady
	wakeTTSReady            = ttsReady
	wakeLoadTTSConfig       = func(ctx context.Context) map[string]any { return loadTTSConfig(ctx, nil) }
	wakeGetTTSProvider      = func(cfg map[string]any) (string, error) { return getTTSProvider(cfg), nil }
	wakeCheckTTS            = func(ctx context.Context) (bool, error) { return checkTTSRequirements(ctx, nil), nil }
)

// LoadWakeWordConfig returns the wake_word config section shape guarded to a dict
func LoadWakeWordConfig(ctx context.Context) map[string]any {
	tree, err := wakeLoadConfig(ctx)
	if err != nil {
		return map[string]any{}
	}
	if sec, ok := asStrMap(tree["wake_word"]); ok {
		return sec
	}
	return map[string]any{}
}

func wakeGet(cfg map[string]any, key string) any {
	if v := cfg[key]; v != nil {
		return v
	}
	return wakeDefaults[key]
}

// Python max then min clamp keeping the first operand on ties and NaN
func pyClampFloat(n, lo, hi float64) float64 {
	if lo > n {
		n = lo
	}
	if hi < n {
		n = hi
	}
	return n
}

// Numeric config value through cast, the default on junk, clamped to lo..hi as Python min of max
func wakeClamped[T any](cfg map[string]any, key string, cast func(any) (T, error), less func(a, b T) bool, lo, hi T) (T, error) {
	n, err := cast(wakeGet(cfg, key))
	if err != nil {
		var zero T
		if !pyCastFallback(err) {
			return zero, err
		}
		if n, err = cast(wakeDefaults[key]); err != nil {
			return zero, err
		}
	}
	if less(n, lo) {
		n = lo
	}
	if less(hi, n) {
		n = hi
	}
	return n, nil
}

// Wake sensitivity as a float clamped to 0..1, the default on junk
func wakeSensitivity(cfg map[string]any) (float64, error) {
	return wakeClamped(cfg, "sensitivity", pyint.Float, func(a, b float64) bool { return a < b }, 0, 1)
}

// Consecutive over threshold frames required to fire, clamped to 1..10 with the default on junk
func wakeConfirmationFrames(cfg map[string]any) (int, error) {
	n, err := wakeClamped(cfg, "confirmation_frames", pyint.Of, func(a, b *big.Int) bool { return a.Cmp(b) < 0 }, big.NewInt(1), big.NewInt(10))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Whether a cast error is the TypeError or ValueError the clamp falls back on
func pyCastFallback(err error) bool {
	var e *pyint.Error
	return errors.As(err, &e) && (e.Kind == "TypeError" || e.Kind == "ValueError")
}

// Configured provider alias, auto resolving to the first engine the support check and config allow
func wakeProvider(cfg map[string]any, supported func(engine string) (bool, error)) (string, error) {
	provider := pystr.Lower(pystr.Strip(pyrepr.Str(wakeGet(cfg, "provider"))))
	if provider != "" && provider != "auto" {
		return provider, nil
	}
	for _, name := range WakeProviderPreference {
		ok, err := supported(name)
		if err != nil {
			return "", err
		}
		if ok && (name != "openwakeword" || openWakeWordModel(cfg) != "") {
			return name, nil
		}
	}
	return "porcupine", nil
}

// Configured PortAudio input selector, keeping indices and stripped names, nil for blanks and bools
func wakeInputDevice(cfg map[string]any) any {
	raw := wakeGet(cfg, "input_device")
	if _, ok := raw.(bool); ok || raw == nil {
		return nil
	}
	if _, ok := pyrepr.Int(raw); ok {
		return raw
	}
	if s := pystr.Strip(pyrepr.Str(raw)); s != "" {
		return s
	}
	return nil
}

// WakePhrase returns the human facing wake phrase label, the engine keys detection
func WakePhrase(ctx context.Context, cfg map[string]any) string {
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	if s := pyrepr.Str(wakeGet(cfg, "phrase")); s != "" {
		return s
	}
	return wakeDefaultPhrase
}

// ResolveCaptureMode picks local or client capture, a working backend mic winning under auto
func ResolveCaptureMode(ctx context.Context, cfg map[string]any, preferClient, forceLocal bool) string {
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	if forceLocal {
		return "local"
	}
	v := wakeGet(cfg, "capture")
	if !pyTruthy(v) {
		v = "auto"
	}
	raw := pystr.Lower(pystr.Strip(pyrepr.Str(v)))
	if raw == "client" || raw == "remote" || raw == "external" {
		return "client"
	}
	if raw != "local" && preferClient && !wakeLocalInputReady() {
		return "client"
	}
	return "local"
}

// Input channel count of a PortAudio device record
func inputChannels(info pysounddevice.Device) int { return info.MaxInputChannels }

// True when PortAudio loads and at least one input device or a default input exists
func localInputDeviceReady() bool {
	sd, err := importAudio()
	if err != nil {
		return false
	}
	devices, err := sd.QueryDevices()
	if err != nil {
		return false
	}
	for _, d := range devices {
		if inputChannels(d) > 0 {
			return true
		}
	}
	dev, err := sd.QueryDevice(nil, "input")
	return err == nil && inputChannels(dev) > 0
}

// WakeSurfaceEnabled reports whether a cli, tui or gui surface may host the enabled listener
func WakeSurfaceEnabled(ctx context.Context, surface string, cfg map[string]any) bool {
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	want := pystr.Lower(pystr.Strip(pyrepr.Str(wakeGet(cfg, "surface"))))
	if want == "" {
		want = "auto"
	}
	return pyTruthy(cfg["enabled"]) && (want == "auto" || want == pystr.Lower(pystr.Strip(surface)))
}

// Active profile name, default when unset
func wakeActiveProfileName(ctx context.Context) string {
	if name := profiles.GetActiveProfileName(ctx); name != "" {
		return name
	}
	return "default"
}

// WakeProfilePhrase pairs a wake enabled profile with its own wake phrase
type WakeProfilePhrase struct {
	Profile string
	Phrase  string
}

// Every profile list_profiles reports, failing where its per profile summary fails
func wakeListProfileNames(ctx context.Context) ([]string, error) {
	var names []string
	def := profiles.DefaultHome()
	if st, err := os.Stat(def); err == nil && st.IsDir() {
		if _, _, err := profiles.ReadConfigModel(ctx, def); err != nil {
			return nil, err
		}
		names = append(names, "default")
	}
	named, err := profiles.IterNamedProfileDirs(true)
	if err != nil || len(named) == 0 {
		return names, err
	}
	aliases, err := profiles.BuildAliasMap()
	if err != nil {
		return nil, err
	}
	for _, entry := range named {
		canon, err := profiles.NormalizeProfileName(filepath.Base(entry))
		if err != nil {
			return nil, err
		}
		if _, _, err := profiles.ReadConfigModel(ctx, entry); err != nil {
			return nil, err
		}
		if alias := aliases[canon]; alias != "" {
			if _, err := profiles.FindOurWrapper(alias); err != nil {
				return nil, err
			}
		}
		names = append(names, filepath.Base(entry))
	}
	return names, nil
}

// EnrolledProfilePhrases maps every wake enabled profile to its phrase read raw from its own config
func EnrolledProfilePhrases(ctx context.Context) []WakeProfilePhrase {
	var out []WakeProfilePhrase
	names, err := wakeListProfileNames(ctx)
	if err != nil {
		return nil
	}
	for _, name := range names {
		dir, err := profiles.GetProfileDir(name)
		if err != nil {
			continue
		}
		raw, err := config.ReadUserConfigRaw(ctx, filepath.Join(dir, "config.yaml"))
		if err != nil {
			continue
		}
		wc := raw["wake_word"]
		if !pyTruthy(wc) {
			continue
		}
		sec, ok := asStrMap(wc)
		if !ok || !pyTruthy(sec["enabled"]) {
			continue
		}
		phrase := "hey " + name
		if pyTruthy(sec["phrase"]) {
			phrase = pyrepr.Str(sec["phrase"])
		}
		if phrase = pystr.Strip(phrase); phrase != "" {
			out = setPhrase(out, name, phrase)
		}
	}
	return out
}

// Assigns a profile phrase keeping the first insertion position like a dict
func setPhrase(list []WakeProfilePhrase, profile, phrase string) []WakeProfilePhrase {
	for i := range list {
		if list[i].Profile == profile {
			list[i].Phrase = phrase
			return list
		}
	}
	return append(list, WakeProfilePhrase{Profile: profile, Phrase: phrase})
}

// WakeInputDeviceDetails is the JSON safe diagnostic of one PortAudio input selector
type WakeInputDeviceDetails struct {
	Selector          any      `json:"selector"`
	Error             *string  `json:"error,omitempty"`
	Name              *string  `json:"name,omitempty"`
	MaxInputChannels  *int     `json:"max_input_channels,omitempty"`
	DefaultSamplerate *float64 `json:"default_samplerate,omitempty"`
	HostAPIIndex      *int     `json:"hostapi_index,omitempty"`
	HostAPI           *string  `json:"hostapi,omitempty"`
}

// Resolves a PortAudio selector into diagnostics, importing the audio layer unless sd is given
func describeInputDevice(selector any, sd pysounddevice.Module) WakeInputDeviceDetails {
	details := WakeInputDeviceDetails{Selector: selector}
	var info pysounddevice.Device
	err := func() error {
		if sd == nil {
			var err error
			if sd, err = importAudio(); err != nil {
				return err
			}
		}
		var err error
		info, err = sd.QueryDevice(selector, "input")
		return err
	}()
	if err != nil {
		msg := err.Error()
		details.Error = &msg
		return details
	}
	if info.Name != "" {
		name := info.Name
		details.Name = &name
	}
	channels, rate, index := info.MaxInputChannels, info.DefaultSamplerate, info.HostAPI
	details.MaxInputChannels, details.DefaultSamplerate, details.HostAPIIndex = &channels, &rate, &index
	if hostapi, err := sd.QueryHostAPI(index); err == nil && hostapi.Name != "" {
		name := hostapi.Name
		details.HostAPI = &name
	}
	return details
}

// Converts one native rate capture block to an exact engine frame, averaging down and interpolating up
func resampleAudioFrame(frame []int16, outputLength int) []int16 {
	n := len(frame)
	if n == outputLength {
		return append([]int16(nil), frame...)
	}
	out := make([]int16, outputLength)
	if n == 0 {
		return out
	}
	values := make([]float64, outputLength)
	if n > outputLength {
		raw, _ := pynumpy.Linspace(0, float64(n), outputLength+1, true)
		edges := make([]int, len(raw))
		for i, e := range raw {
			edges[i] = int(math.Floor(e))
		}
		for i := 0; i < outputLength; i++ {
			lo, hi := edges[i], n
			if i+1 < outputLength {
				hi = edges[i+1]
			}
			var sum float64
			if lo < hi {
				sum = float64(frame[lo])
				for _, v := range frame[lo+1 : hi] {
					sum += float64(v)
				}
			} else {
				sum = float64(frame[lo])
			}
			values[i] = sum / float64(edges[i+1]-edges[i])
		}
	} else {
		xs, _ := pynumpy.Linspace(0, float64(n-1), outputLength, true)
		for i, x := range xs {
			values[i] = interpLinear(x, frame)
		}
	}
	for i, v := range values {
		out[i] = int16(pyClampFloat(math.RoundToEven(v), -32768, 32767))
	}
	return out
}

// numpy interp of x against integer sample positions 0..len-1
func interpLinear(x float64, fp []int16) float64 {
	last := len(fp) - 1
	switch {
	case math.IsNaN(x):
		return x
	case x < 0:
		return float64(fp[0])
	case x > float64(last):
		return float64(fp[last])
	case x == float64(last):
		return float64(fp[last])
	}
	j := int(math.Floor(x))
	if float64(j) == x {
		return float64(fp[j])
	}
	slope := float64(fp[j+1]) - float64(fp[j])
	return float64(slope*(x-float64(j))) + float64(fp[j])
}

// SilentAudioHint returns the platform remediation for an armed stream delivering silence
func SilentAudioHint(details WakeInputDeviceDetails) string {
	platform := wakeSysPlatform()
	if platform == "darwin" {
		return "Microphone delivers only silence. Grant the Icebath backend " +
			"microphone access in System Settings > Privacy & Security > " +
			"Microphone, then toggle the wake word."
	}
	fix := "Check the selected input device"
	if platform == "win32" {
		fix = "Set wake_word.input_device to a different PortAudio input device"
	}
	label := ""
	if details.Name != nil {
		label = pystr.Strip(*details.Name)
	}
	if label == "" {
		if details.Selector == nil {
			label = "system default"
		} else {
			label = pyrepr.Str(details.Selector)
		}
	}
	if details.HostAPI != nil {
		if hostapi := pystr.Strip(*details.HostAPI); hostapi != "" {
			label = label + " (" + hostapi + ")"
		}
	}
	return fmt.Sprintf("Microphone delivers only silence from %s. %s, then toggle the wake word.", label, fix)
}

// Wake engine constructors by engine name
var wakeEngineConstructors = map[string]func(ctx context.Context, cfg map[string]any) (WakeEngine, error){
	"openwakeword": newOpenWakeWordEngine,
	"sherpa":       newSherpaKwsEngine,
	"porcupine":    newPorcupineEngine,
}

// Builds the engine of the resolved provider, an error for an unknown alias
func wakeBuildEngine(ctx context.Context, cfg map[string]any) (WakeEngine, error) {
	provider, err := wakeProvider(cfg, wakeNativeSupport)
	if err != nil {
		return nil, err
	}
	engine, ok := WakeProviders[provider]
	if !ok {
		return nil, &WakeWordError{Msg: "Unknown wake_word provider: " + pyrepr.StrRepr(provider)}
	}
	return wakeEngineConstructors[engine](ctx, cfg)
}

// Whether an enabled speech to text provider is configured
func sttReady(ctx context.Context) bool {
	sttConfig := loadSTTConfig(ctx)
	if !isSTTEnabled(ctx, sttConfig) {
		return false
	}
	provider, err := getProvider(ctx, sttConfig)
	return err == nil && provider != "none"
}

// Whether the configured text to speech provider can run
func ttsReady(ctx context.Context) bool {
	if _, err := wakeGetTTSProvider(wakeLoadTTSConfig(ctx)); err != nil {
		return false
	}
	ok, err := wakeCheckTTS(ctx)
	return err == nil && ok
}

// WakeRequirements reports whether wake word detection can run with a remediation hint
type WakeRequirements struct {
	Available           bool   `json:"available"`
	Provider            string `json:"provider"`
	DepsAvailable       bool   `json:"deps_available"`
	AudioAvailable      bool   `json:"audio_available"`
	LocalInputAvailable bool   `json:"local_input_available"`
	Capture             string `json:"capture"`
	AccessKeySet        bool   `json:"access_key_set"`
	STTAvailable        bool   `json:"stt_available"`
	TTSAvailable        bool   `json:"tts_available"`
	Phrase              string `json:"phrase"`
	Hint                string `json:"hint"`
}

// Whether this platform has a native build of an engine
func wakeNativeSupport(engine string) (bool, error) {
	switch engine {
	case "sherpa":
		return speech.Supported(), nil
	case "openwakeword":
		return openwakeword.Supported(), nil
	case "porcupine":
		return porcupine.Supported(), nil
	}
	return false, nil
}

// CheckWakeWordRequirements probes engine support, configuration, key, mic, STT and TTS, supported overriding the native build check
func CheckWakeWordRequirements(ctx context.Context, cfg map[string]any, supported func(engine string) (bool, error)) (WakeRequirements, error) {
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	if supported == nil {
		supported = wakeNativeSupport
	}
	provider, err := wakeProvider(cfg, supported)
	if err != nil {
		return WakeRequirements{}, err
	}
	engine := WakeProviders[provider]
	platformOK, err := supported(engine)
	if err != nil {
		return WakeRequirements{}, err
	}
	configOK := engine != "openwakeword" || openWakeWordModel(cfg) != ""
	depsOK := platformOK && configOK
	audioOK := false
	if depsOK {
		if audioOK, err = wakeAudioAvailable(); err != nil {
			return WakeRequirements{}, err
		}
	}
	keyOK := true
	sttOK := wakeSTTReady(ctx)
	ttsOK := wakeTTSReady(ctx)
	captureMode := ResolveCaptureMode(ctx, cfg, false, false)
	hint := ""
	switch {
	case engine == "":
		hint = "Unknown wake_word.provider " + pyrepr.StrRepr(provider) + ", use auto, sherpa, openwakeword or porcupine."
	case !platformOK:
		var alternatives []string
		for _, name := range WakeProviderPreference {
			ok, err := supported(name)
			if err != nil {
				return WakeRequirements{}, err
			}
			if ok && name != engine {
				alternatives = append(alternatives, name)
			}
		}
		hint = fmt.Sprintf("The %s wake engine has no build for this platform.", provider)
		if len(alternatives) > 0 {
			hint += fmt.Sprintf(" Set wake_word.provider to %s.", strings.Join(alternatives, " or "))
		}
	case !configOK:
		hint = "Set wake_word.openwakeword.model to a .tflite model file or a shipped openWakeWord model name."
	case engine == "porcupine" && pystr.Strip(os.Getenv("PORCUPINE_ACCESS_KEY")) == "":
		keyOK = false
		hint = "Set PORCUPINE_ACCESS_KEY (free key at https://console.picovoice.ai)."
	case !audioOK && captureMode == "local":
		hint = "No local microphone on this backend. Remote desktop can stream " +
			"the client mic — set wake_word.capture: client or use a desktop " +
			"build with client-capture wake support."
	case !sttOK || !ttsOK:
		var missing []string
		if !sttOK {
			missing = append(missing, "speech-to-text")
		}
		if !ttsOK {
			missing = append(missing, "text-to-speech")
		}
		hint = fmt.Sprintf("Wake word needs %s configured — run `icebath tools` "+
			"(Voice section) or see the voice-mode docs.", strings.Join(missing, " and "))
	}
	micOK := depsOK && (captureMode == "client" || audioOK)
	return WakeRequirements{
		Available: depsOK && keyOK && sttOK && ttsOK && micOK, Provider: provider,
		DepsAvailable: depsOK, AudioAvailable: audioOK, LocalInputAvailable: depsOK && wakeLocalInputReady(),
		Capture: captureMode, AccessKeySet: keyOK, STTAvailable: sttOK, TTSAvailable: ttsOK,
		Phrase: WakePhrase(ctx, cfg), Hint: hint,
	}, nil
}
