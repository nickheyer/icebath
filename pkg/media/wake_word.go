//go:build !icebath_core && !icebath_noextras

package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	mathutil "github.com/nickheyer/icebath/pkg/utils/math"
	portaudioutil "github.com/nickheyer/icebath/pkg/utils/portaudio"
)

// WakeSampleRate is the 16 kHz mono int16 rate every wake engine consumes
const WakeSampleRate = 16000

// Loudness under which a captured block counts as silence
const wakeSilencePeak = 10

var (
	wakeFireCooldownSeconds      = 2.0
	wakeStartTimeoutSeconds      = 5.0
	wakeReadPollSeconds          = 0.05
	wakeReadStallFallbackSeconds = 2.0
	wakeHaltJoinSeconds          = 2.0
	wakeSilenceAlertSeconds      = 10.0
)

// WakeProviderEntry names the engine class and the pm extra of one provider alias
type WakeProviderEntry struct {
	Engine string
	Extra  string
}

// WakeProviderNames lists the provider aliases in their declared order
var WakeProviderNames = []string{"porcupine", "sherpa", "sherpa-onnx", "kws", "open", "openwakeword", "oww", "local"}

// WakeProviders maps every provider alias to its engine class and pm extra
var WakeProviders = map[string]WakeProviderEntry{
	"porcupine":    {"_PorcupineEngine", "wake-porcupine"},
	"sherpa":       {"_SherpaKwsEngine", "wake-sherpa"},
	"sherpa-onnx":  {"_SherpaKwsEngine", "wake-sherpa"},
	"kws":          {"_SherpaKwsEngine", "wake-sherpa"},
	"open":         {"_SherpaKwsEngine", "wake-sherpa"},
	"openwakeword": {"_OpenWakeWordEngine", "wake-openwakeword"},
	"oww":          {"_OpenWakeWordEngine", "wake-openwakeword"},
	"local":        {"_OpenWakeWordEngine", "wake-openwakeword"},
}

// WakeProviderPreference is the auto resolution order of the wake engines
var WakeProviderPreference = []string{"openwakeword", "sherpa", "porcupine"}

// WakeWordInUse reports another surface or process owning the wake word microphone
type WakeWordInUse struct {
	Msg   string
	Cause error
}

func (e *WakeWordInUse) Error() string { return e.Msg }
func (e *WakeWordInUse) Unwrap() error { return e.Cause }

// Kinds of wake word failure a WakeWordError wraps, told apart with errors.Is
var (
	errWakeInvalid      = errors.New("invalid wake word setting")
	errWakeNotInstalled = errors.New("wake word engine not installed")
	errWakeUnavailable  = errors.New("wake word engine cannot load")
	errWakeTimeout      = errors.New("wake word microphone timed out")
	errWakeFailed       = errors.New("wake word engine failed")
)

// WakeWordError is a failure of the wake word API: Kind tells what failed, Cause what made it fail
type WakeWordError struct {
	Kind  error
	Msg   string
	Cause error
}

func (e *WakeWordError) Error() string { return e.Msg }

// Kind and Cause, for errors.Is and errors.As
func (e *WakeWordError) Unwrap() []error {
	if e.Cause == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Cause}
}

var (
	wakeExtraSupported  = wakeEngineSupported
	wakePMAvailable     = wakeEngineAvailable
	wakeRequireExtra    = requireWakeExtra
	wakeSysPlatform     = sysPlatform
	wakeAudioAvailable  = func() (bool, error) { return audioAvailable() }
	wakeLocalInputReady = localInputDeviceReady
	wakeSTTReady        = sttReady
	wakeTTSReady        = ttsReady
	wakeCheckTTS        = func(ctx context.Context) (bool, error) { return checkTTSRequirements(ctx, nil), nil }
)

// Wake word settings of one profile, read from the registry
type WakeConfig struct {
	// The listener may run
	Enabled bool
	// Screen allowed to host the listener, auto for any
	Surface string
	// PortAudio input selector: a device number, a device name, nil for the default microphone
	InputDevice any
	// Engine alias as configured, lower case, auto when blank
	Provider string
	// Phrase the open vocabulary engine detects
	Phrase string
	// How easily an engine fires, 0 to 1
	Sensitivity float64
	// Over threshold frames in a row that fire the openWakeWord engine, 1 to 10
	ConfirmationFrames int
	// A detection starts a new session
	StartNewSession bool
	// The phrases other wake enabled profiles chose are heard too
	ProfileRouting bool
	// openWakeWord model file, empty when none is set since the engine ships without one
	OpenWakeWordModel string
	// Folder of the sherpa keyword model, empty for the one fetched on first use
	SherpaModelDir string
	// Porcupine built in keyword or keyword file
	PorcupineKeyword string
}

// Wake word settings in effect for ctx
func LoadWakeWordConfig(ctx context.Context) *WakeConfig {
	cfg := &WakeConfig{
		Enabled:            settings.WakeWord.Get(ctx),
		Surface:            settings.WakeWordSurface.Get(ctx),
		InputDevice:        wakeInputSelector(settings.WakeWordInputDevice.Get(ctx)),
		Provider:           strings.ToLower(strings.TrimSpace(settings.WakeWordProvider.Get(ctx))),
		Phrase:             strings.TrimSpace(settings.WakeWordPhrase.Get(ctx)),
		Sensitivity:        settings.WakeWordSensitivity.Get(ctx),
		ConfirmationFrames: min(10, max(1, settings.WakeWordConfirmationFrames.Get(ctx))),
		StartNewSession:    settings.WakeWordStartNewSession.Get(ctx),
		ProfileRouting:     settings.WakeWordProfileRouting.Get(ctx),
		OpenWakeWordModel:  strings.TrimSpace(settings.WakeWordOpenwakewordModel.Get(ctx)),
		SherpaModelDir:     strings.TrimSpace(settings.WakeWordSherpaModelDir.Get(ctx)),
		PorcupineKeyword:   strings.TrimSpace(settings.WakeWordPorcupineKeyword.Get(ctx)),
	}
	if cfg.Provider == "" {
		cfg.Provider = "auto"
	}
	if cfg.Phrase == "" {
		cfg.Phrase = settings.WakeWordPhrase.Default
	}
	switch {
	case math.IsNaN(cfg.Sensitivity):
		cfg.Sensitivity = settings.WakeWordSensitivity.Default
	case cfg.Sensitivity < 0:
		cfg.Sensitivity = 0
	case cfg.Sensitivity > 1:
		cfg.Sensitivity = 1
	}
	if cfg.PorcupineKeyword == "" {
		cfg.PorcupineKeyword = settings.WakeWordPorcupineKeyword.Default
	}
	return cfg
}

// PortAudio input selector a setting's text names: a whole number is a device number, true, false and blank the default microphone, other text a device name
func wakeInputSelector(text string) any {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "true") || strings.EqualFold(text, "false") {
		return nil
	}
	if index, err := strconv.Atoi(text); err == nil {
		return index
	}
	return text
}

// Configured provider, auto taking sherpa unless an openWakeWord model is configured, else the first supported engine
func wakeProvider(ctx context.Context, cfg *WakeConfig, supported func(string) (bool, error)) (string, error) {
	if cfg.Provider != "" && cfg.Provider != "auto" {
		return cfg.Provider, nil
	}
	if cfg.OpenWakeWordModel == "" {
		return "sherpa", nil
	}
	if supported == nil {
		supported = func(extra string) (bool, error) { return wakeExtraSupported(ctx, extra) }
	}
	for _, name := range WakeProviderPreference {
		ok, err := supported(WakeProviders[name].Extra)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	return "porcupine", nil
}

// Picovoice access key of the profile ctx serves, empty when none is set
func wakeAccessKey(ctx context.Context) string {
	key, _ := config.ProfileEnvLookup(ctx, home.Dir(ctx))("PORCUPINE_ACCESS_KEY")
	return strings.TrimSpace(key)
}

// Input channel count of a PortAudio device record
func inputChannels(info portaudioutil.Device) int { return info.MaxInputChannels }

// True when PortAudio loads and at least one input device or a default input exists
func localInputDeviceReady() bool {
	sd, err := importAudio()
	if err != nil {
		return false
	}
	devices, err := sd.Devices()
	if err != nil {
		return false
	}
	for _, d := range devices {
		if inputChannels(d) > 0 {
			return true
		}
	}
	dev, err := sd.Device(nil, portaudioutil.Input)
	return err == nil && inputChannels(dev) > 0
}

// Whether a cli, tui or gui surface may host the enabled listener
func (c *WakeConfig) SurfaceEnabled(surface string) bool {
	want := strings.ToLower(strings.TrimSpace(c.Surface))
	return c.Enabled && (want == "" || want == "auto" || want == strings.ToLower(strings.TrimSpace(surface)))
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

// EnrolledProfilePhrases maps every wake enabled profile to the phrase its own settings name, hey and the profile name when it names none
func EnrolledProfilePhrases(ctx context.Context) []WakeProfilePhrase {
	var out []WakeProfilePhrase
	names, err := wakeListProfileNames(ctx)
	if err != nil {
		pkgLog().Warn("wake word: profile routing listens for this profile only, the profile list failed: " + err.Error())
		return nil
	}
	for _, name := range names {
		dir, err := profiles.GetProfileDir(name)
		if err != nil {
			pkgLog().Warn("wake word: profile " + name + " left out of routing: " + err.Error())
			continue
		}
		pctx := settings.WithoutSession(home.WithOverride(ctx, dir))
		if !settings.WakeWord.Get(pctx) {
			continue
		}
		phrase := "hey " + name
		if chosen, err := settings.Resolve(pctx, settings.WakeWordPhrase.Key); err == nil && chosen.Source != settings.SourceDefault {
			if own := strings.TrimSpace(settings.WakeWordPhrase.Get(pctx)); own != "" {
				phrase = own
			}
		}
		if phrase = strings.TrimSpace(phrase); phrase != "" {
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
func describeInputDevice(selector any, sd portaudioutil.Audio) WakeInputDeviceDetails {
	details := WakeInputDeviceDetails{Selector: selector}
	var info portaudioutil.Device
	err := func() error {
		if sd == nil {
			var err error
			if sd, err = importAudio(); err != nil {
				return err
			}
		}
		var err error
		info, err = sd.Device(selector, portaudioutil.Input)
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
	channels, rate, index := info.MaxInputChannels, info.DefaultSampleRate, info.HostAPI
	details.MaxInputChannels, details.DefaultSamplerate, details.HostAPIIndex = &channels, &rate, &index
	if hostapi, err := sd.HostAPI(index); err == nil && hostapi.Name != "" {
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
		raw, _ := mathutil.Linspace(0, float64(n), outputLength+1, true)
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
		xs, _ := mathutil.Linspace(0, float64(n-1), outputLength, true)
		for i, x := range xs {
			values[i] = interpLinear(x, frame)
		}
	}
	for i, v := range values {
		out[i] = int16(min(max(math.RoundToEven(v), math.MinInt16), math.MaxInt16))
	}
	return out
}

// Linear interpolation of the samples at position x, clamped to the first and last sample outside 0..len-1
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
		label = strings.TrimSpace(*details.Name)
	}
	if label == "" {
		if details.Selector == nil {
			label = "system default"
		} else {
			label = anyutil.String(details.Selector)
		}
	}
	if details.HostAPI != nil {
		if hostapi := strings.TrimSpace(*details.HostAPI); hostapi != "" {
			label = label + " (" + hostapi + ")"
		}
	}
	return fmt.Sprintf("Microphone delivers only silence from %s. %s, then toggle the wake word.", label, fix)
}

// Wake engine constructors by class name, the table a test or plugin swaps one engine in
var wakeEngineClasses = map[string]func(ctx context.Context, cfg *WakeConfig) (WakeEngine, error){
	"_OpenWakeWordEngine": newOpenWakeWordEngine,
	"_SherpaKwsEngine":    newSherpaKwsEngine,
	"_PorcupineEngine":    newPorcupineEngine,
}

// Builds the engine of the resolved provider, an invalid setting error for an unknown alias
var wakeBuildEngine = func(ctx context.Context, cfg *WakeConfig) (WakeEngine, error) {
	provider, err := wakeProvider(ctx, cfg, nil)
	if err != nil {
		return nil, err
	}
	entry, ok := WakeProviders[provider]
	if !ok {
		return nil, &WakeWordError{Kind: errWakeInvalid, Msg: "Unknown wake_word provider: " + errchain.Show(provider)}
	}
	return wakeEngineClasses[entry.Engine](ctx, cfg)
}

// Whether an enabled speech to text provider is configured
func sttReady(ctx context.Context) bool {
	req := loadSTTRequest(ctx)
	if !req.Enabled {
		return false
	}
	provider, err := getProvider(ctx, req)
	return err == nil && provider != "none"
}

// Whether the configured text to speech provider can run
func ttsReady(ctx context.Context) bool {
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
	AccessKeySet        bool   `json:"access_key_set"`
	STTAvailable        bool   `json:"stt_available"`
	TTSAvailable        bool   `json:"tts_available"`
	Phrase              string `json:"phrase"`
	Hint                string `json:"hint"`
}

// Whether an engine can run on this platform, the compiled in sherpa engine by its library build
func wakeEngineSupported(ctx context.Context, extra string) (bool, error) {
	if extra == "wake-sherpa" {
		return wakeSpeechSupported(), nil
	}
	return pm.ExtraSupported(ctx, extra)
}

// Refusal of an engine whose extra is not provisioned, naming the command that provisions it
func requireWakeExtra(ctx context.Context, extra string) error {
	ok, err := wakePMAvailable(ctx, extra)
	if err != nil || ok {
		return err
	}
	return &WakeWordError{Kind: errWakeNotInstalled, Msg: "the " + extra + " wake engine is not provisioned. Run `" + pm.InstallHint(extra) + "`"}
}

// Whether an engine's dependencies are present, the compiled in sherpa engine needing only its library build
func wakeEngineAvailable(ctx context.Context, extra string) (bool, error) {
	if extra == "wake-sherpa" {
		return wakeSpeechSupported(), nil
	}
	return pm.Available(ctx, extra)
}

// CheckWakeWordRequirements probes engine support, deps, key, mic, STT and TTS without installing anything
func CheckWakeWordRequirements(ctx context.Context, cfg *WakeConfig, supported func(string) (bool, error)) (WakeRequirements, error) {
	if cfg == nil {
		cfg = LoadWakeWordConfig(ctx)
	}
	if supported == nil {
		supported = func(extra string) (bool, error) { return wakeExtraSupported(ctx, extra) }
	}
	provider, err := wakeProvider(ctx, cfg, supported)
	if err != nil {
		return WakeRequirements{}, err
	}
	feature := "wake-openwakeword"
	switch provider {
	case "porcupine":
		feature = "wake-porcupine"
	case "sherpa", "sherpa-onnx", "kws", "open":
		feature = "wake-sherpa"
	}
	depsOK, err := wakePMAvailable(ctx, feature)
	if err != nil {
		return WakeRequirements{}, err
	}
	platformOK := depsOK
	if !platformOK {
		if platformOK, err = supported(feature); err != nil {
			return WakeRequirements{}, err
		}
	}
	audioOK := false
	if depsOK {
		if audioOK, err = wakeAudioAvailable(); err != nil {
			return WakeRequirements{}, err
		}
	}
	keyOK := true
	modelOK := WakeProviders[provider].Engine != "_OpenWakeWordEngine" || cfg.OpenWakeWordModel != ""
	sttOK := wakeSTTReady(ctx)
	ttsOK := wakeTTSReady(ctx)
	hint := ""
	switch {
	case !platformOK && (cfg.Provider == "" || cfg.Provider == "auto") && cfg.OpenWakeWordModel == "":
		hint = fmt.Sprintf("The %s wake engine that detects %s is not supported on this platform.", provider, errchain.Show(cfg.Phrase))
		owwOK, err := supported(WakeProviders["openwakeword"].Extra)
		if err != nil {
			return WakeRequirements{}, err
		}
		if owwOK {
			hint += " Set wake_word.openwakeword.model to an openWakeWord model file trained for your wake phrase."
		}
	case !platformOK:
		var alternatives []string
		for _, name := range []string{"sherpa", "porcupine"} {
			ok, err := supported(WakeProviders[name].Extra)
			if err != nil {
				return WakeRequirements{}, err
			}
			if ok {
				alternatives = append(alternatives, name)
			}
		}
		hint = fmt.Sprintf("The %s wake engine is not supported on this platform.", provider)
		if len(alternatives) > 0 {
			hint += fmt.Sprintf(" Set wake_word.provider to %s.", strings.Join(alternatives, " or "))
		}
	case !modelOK:
		hint = "Set wake_word.openwakeword.model to an openWakeWord model file trained for your wake phrase, " +
			"or set wake_word.provider to auto so the open vocabulary engine detects " + errchain.Show(cfg.Phrase) + "."
	case provider == "porcupine" && wakeAccessKey(ctx) == "":
		keyOK = false
		hint = "Set PORCUPINE_ACCESS_KEY (free key at https://console.picovoice.ai)."
	case !depsOK:
		hint = pm.InstallHint(feature)
	case depsOK && !audioOK:
		hint = "Microphone capture needs the PortAudio library and a working audio device."
	case !sttOK || !ttsOK:
		var missing []string
		if !sttOK {
			missing = append(missing, "speech-to-text")
		}
		if !ttsOK {
			missing = append(missing, "text-to-speech")
		}
		hint = fmt.Sprintf("Wake word needs %s. Set it up in /settings → Voice.", strings.Join(missing, " and "))
	}
	micOK := depsOK && audioOK
	return WakeRequirements{
		Available: platformOK && keyOK && modelOK && sttOK && ttsOK && micOK, Provider: provider,
		DepsAvailable: depsOK, AudioAvailable: audioOK, LocalInputAvailable: depsOK && wakeLocalInputReady(),
		AccessKeySet: keyOK, STTAvailable: sttOK, TTSAvailable: ttsOK,
		Phrase: cfg.Phrase, Hint: hint,
	}, nil
}
