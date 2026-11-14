package toolchain

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/home"
)

// Names of the llama.cpp server builds by the compute backend each one drives
const (
	LlamaCPU    = "llamacpp-cpu"
	LlamaCUDA   = "llamacpp-cuda"
	LlamaVulkan = "llamacpp-vulkan"
	LlamaHIP    = "llamacpp-hip"
	LlamaMetal  = "llamacpp-metal"
)

// Names of the other catalog entries
const (
	Bws             = "bws"
	Chromium        = "chromium"
	CuaDriver       = "cua-driver"
	EspeakData      = "espeak-ng-data"
	Ffmpeg          = "ffmpeg"
	Gh              = "gh"
	Git             = "git"
	IronProxy       = "iron-proxy"
	MsalRuntime     = "msalruntime"
	OpenWakeWord    = "openwakeword"
	Porcupine       = "porcupine"
	PortAudio       = "portaudio"
	Pinyin          = "pypinyin"
	Ripgrep         = "ripgrep"
	SherpaKWSModel  = "sherpa-kws-model"
	SherpaOnnx      = "sherpa-onnx"
	SherpaOnnxCUDA  = "sherpa-onnx-cuda"
	WhisperCpp      = "whispercpp-cpu"
	WhisperCppModel = "whispercpp-models"
)

// Directory the sherpa keyword spotting model unpacks into below the wake word cache
const SherpaKWSModelDir = "sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01"

// Variables a probed proxy daemon may see, never the secrets of the operator
var proxyEnvAllowlist = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "NO_COLOR", "SSL_CERT_DIR", "SSL_CERT_FILE", "SYSTEMROOT", "USERPROFILE"}

// Environment of the process cut down to the proxy allowlist
func allowlistedEnv(context.Context, *Store) []string {
	var env []string
	for _, name := range proxyEnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// Environment of the process with driver telemetry switched off for the smoke run
func quietDriverEnv(ctx context.Context, s *Store) []string {
	if s.telemetry != nil {
		if optedIn, err := s.telemetry(ctx); err == nil && optedIn {
			return os.Environ()
		}
	}
	return append(os.Environ(), "CUA_DRIVER_RS_TELEMETRY_ENABLED=0")
}

// Reports a glibc or macOS or Windows target, the ones most upstreams build for
func notMuslOrBionic(t Target) bool { return !t.musl() && !t.bionic() }

// Recipe of one llama.cpp server build whose release assets exist for the listed targets
func llamaRecipe(name, backend string, stage func(context.Context, *staging) error, targets ...Target) *recipe {
	has := map[Target]bool{}
	for _, t := range targets {
		has[t] = true
	}
	return &recipe{
		name: name, kind: Program,
		entry: map[string]string{"win32": "llama-server.exe", "posix": "llama-server"},
		probe: []string{"--version"}, probeDir: true, stage: stage,
		gap: func(t Target) string {
			if has[t] {
				return ""
			}
			return "llama.cpp publishes no " + backend + " build for " + string(t)
		},
	}
}

// Every entry the product can fetch, in name order
func catalog() []*recipe {
	sherpaEntry := map[string]string{
		"win32": "lib/sherpa-onnx-c-api.dll", "darwin-x64": "lib/libsherpa-onnx-c-api.dylib",
		"darwin-arm64": "lib/libsherpa-onnx-c-api.dylib", "posix": "lib/libsherpa-onnx-c-api.so",
	}
	return []*recipe{
		{
			name: Bws, kind: Program, onPath: true, flatten: true,
			entry: map[string]string{"posix": "bws", "win32": "bws.exe"},
			probe: []string{"--version"}, stage: stageExecutable,
			gap: gapTable(map[Target]string{LinuxARM64Bionic: "upstream does not ship an Android binary"}),
		},
		{
			name: Chromium, kind: Program, standard: true,
			marker: "INSTALLATION_COMPLETE", find: findChromium, stage: stageChromium,
			emulated: map[Target]bool{WindowsARM64: true},
			dirName:  func(version string, _ Target) string { return "chromium-" + cutPlus(version) },
			env: func(dir string, _ Target) map[string][]string {
				return map[string][]string{"PLAYWRIGHT_BROWSERS_PATH": {filepath.Dir(dir)}}
			},
			gap: func(t Target) string {
				switch {
				case t.bionic():
					return "no Chromium build for Android/Termux"
				case t.musl():
					return "Playwright/Chrome-for-Testing publishes no musl build"
				}
				return ""
			},
		},
		{
			name: CuaDriver, kind: Program, onPath: true, standard: true, flatten: true,
			entry: map[string]string{
				"darwin-arm64": "CuaDriver.app/Contents/MacOS/cua-driver", "darwin-x64": "CuaDriver.app/Contents/MacOS/cua-driver",
				"win32": "cua-driver.exe", "posix": "cua-driver",
			},
			probe: []string{"--version"}, probeEnv: quietDriverEnv, stage: stageCuaDriver,
			gap: func(t Target) string {
				switch {
				case t.musl():
					return "cua-driver does not publish a musl build"
				case t.bionic():
					return "cua-driver does not publish an Android build"
				}
				return ""
			},
		},
		{
			name: EspeakData, kind: Data, files: []string{"espeak-ng-data"},
		},
		{
			name: Ffmpeg, kind: Program, onPath: true, required: true, flatten: true,
			entry: map[string]string{
				"win32": "bin/ffmpeg.exe", "linux-x64": "bin/ffmpeg", "linux-arm64": "bin/ffmpeg",
				"linux-arm64-bionic": "data/data/com.termux/files/usr/bin/ffmpeg", "posix": "ffmpeg",
			},
			probe: []string{"-version"},
			gap:   gapUnless("BtbN Linux builds link glibc dynamically", func(t Target) bool { return !t.musl() }),
		},
		{
			name: Gh, kind: Program, onPath: true, flatten: true,
			entry: map[string]string{"win32": "bin/gh.exe", "posix": "bin/gh"},
			probe: []string{"--version"},
		},
		{
			name: Git, kind: Program, onPath: true,
			entry: map[string]string{"win32": "cmd/git.exe"},
			probe: []string{"--version"}, unpack: unpackPortableGit,
			env: func(dir string, _ Target) map[string][]string {
				return map[string][]string{"PATH": {filepath.Join(dir, "cmd"), filepath.Join(dir, "usr", "bin")}}
			},
			gap: gapTable(map[Target]string{
				LinuxX64: "POSIX uses system git by choice", LinuxARM64: "POSIX uses system git by choice",
				LinuxX64Musl: "POSIX uses system git by choice", LinuxARM64Musl: "POSIX uses system git by choice",
				DarwinX64: "POSIX uses system git by choice", DarwinARM64: "POSIX uses system git by choice",
			}),
		},
		{
			name: IronProxy, kind: Program, onPath: true,
			entry: map[string]string{"posix": "iron-proxy"},
			probe: []string{"--version"}, probeEnv: allowlistedEnv,
			unpack: unpackSigned, stage: stageSigned,
			gap: func(t Target) string {
				switch {
				case t.windows():
					return "this integration requires a Linux/macOS host (or WSL)"
				case t.bionic():
					return "upstream does not ship an Android binary"
				}
				return ""
			},
		},
		llamaRecipe(LlamaCPU, "cpu", stageLlama, WindowsX64, WindowsARM64, LinuxX64, LinuxARM64, DarwinX64, DarwinARM64),
		llamaRecipe(LlamaCUDA, "cuda", stageLlamaCUDA, WindowsX64, WindowsARM64, LinuxX64, LinuxARM64),
		llamaRecipe(LlamaHIP, "hip", stageLlama, WindowsX64, LinuxX64),
		llamaRecipe(LlamaMetal, "metal", stageLlama, DarwinX64, DarwinARM64),
		llamaRecipe(LlamaVulkan, "vulkan", stageLlama, WindowsX64, LinuxX64, LinuxARM64),
		{
			name: MsalRuntime, kind: Library,
			entry: map[string]string{
				"darwin-arm64": "pymsalruntime/libmsalruntime_arm64.dylib", "darwin-x64": "pymsalruntime/libmsalruntime_x86_64.dylib",
				"linux-x64": "pymsalruntime/libmsalruntime.so", "win32-x64": "pymsalruntime/msalruntime.dll",
				"win32-arm64": "pymsalruntime/msalruntime_arm64.dll",
			},
			gap: gapUnless("Microsoft publishes no msalruntime library for this target", func(t Target) bool {
				return t.windows() || t.system() == "darwin" || t == LinuxX64
			}),
		},
		{
			name: OpenWakeWord, kind: Library,
			entry: map[string]string{"": "pyopen_wakeword/lib/*tensorflowlite_c.*"},
			files: []string{"pyopen_wakeword/models"},
			gap: gapUnless("openWakeWord publishes no TensorFlow Lite build for this target", func(t Target) bool {
				return notMuslOrBionic(t) && t != WindowsARM64
			}),
		},
		{
			name: Porcupine, kind: Library,
			entry: map[string]string{
				"darwin-x64": "pvporcupine/lib/mac/x86_64/libpv_porcupine.dylib", "darwin-arm64": "pvporcupine/lib/mac/arm64/libpv_porcupine.dylib",
				"linux-x64": "pvporcupine/lib/linux/x86_64/libpv_porcupine.so", "linux-arm64": "pvporcupine/lib/raspberry-pi/*/libpv_porcupine.so",
				"win32-x64": "pvporcupine/lib/windows/amd64/libpv_porcupine.dll", "win32-arm64": "pvporcupine/lib/windows/arm64/libpv_porcupine.dll",
			},
			files: []string{"pvporcupine/lib/common/porcupine_params.pv", "pvporcupine/resources/keyword_files"},
			gap:   gapUnless("Porcupine publishes no library for this target", notMuslOrBionic),
		},
		{
			name: PortAudio, kind: Library,
			entry: map[string]string{"": "_sounddevice_data/portaudio-binaries/libportaudio*"},
			gap: gapUnless("Linux uses the PortAudio library of the system", func(t Target) bool {
				return t.windows() || t.system() == "darwin"
			}),
		},
		{
			name: Pinyin, kind: Data,
			files: []string{"pypinyin/pinyin_dict.json", "pypinyin/phrases_dict.json"},
		},
		{
			name: Ripgrep, kind: Program, onPath: true, required: true, flatten: true,
			entry: map[string]string{"win32": "rg.exe", "posix": "rg"},
			probe: []string{"--version"}, unpack: unpackRipgrepRuntime, stage: stageRipgrep,
		},
		{
			name: SherpaKWSModel, kind: Data, homeDir: home.Wakewords, present: SherpaKWSModelDir + "/tokens.txt",
		},
		{
			name: SherpaOnnx, kind: Library, flatten: true, entry: sherpaEntry,
			gap: gapUnless("sherpa-onnx publishes no shared library for this target", func(t Target) bool { return !t.musl() }),
		},
		{
			name: SherpaOnnxCUDA, kind: Library, flatten: true, entry: sherpaEntry,
			gap: gapUnless("sherpa-onnx publishes no CUDA build for this target", func(t Target) bool {
				return t == LinuxX64 || t == WindowsX64
			}),
		},
		{
			name: WhisperCpp, kind: Program, flatten: true,
			entry: map[string]string{"win32-arm64": "whisper-cli.exe"},
			probe: []string{"--help"},
			gap:   gapUnless("uses the existing faster-whisper provider", func(t Target) bool { return t == WindowsARM64 }),
		},
		{
			name: WhisperCppModel, kind: Data, homeDir: home.WhisperCpp, partials: home.WhisperCppPartials, raw: true,
		},
	}
}

// Part of a pinned version before its plus sign
func cutPlus(version string) string {
	for i := 0; i < len(version); i++ {
		if version[i] == '+' {
			return version[:i]
		}
	}
	return version
}
