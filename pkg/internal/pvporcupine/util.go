package pvporcupine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
	hostutil "github.com/nickheyer/icebath/pkg/utils/host"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// A builtin Python exception the package raises, named by its class
type PyError struct{ Class, Msg string }

func (e *PyError) Error() string { return e.Msg }

// Whether the running process is a 64 bit one
func is64bit() bool { return strconv.IntSize == 64 }

// Output of cat on the kernel CPU description with subprocess style failures
func catCPUInfo() ([]byte, error) {
	argv := []string{"cat", "/proc/cpuinfo"}
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, &executil.StatusError{Args: argv, ExitCode: executil.ExitCode(exitErr.ProcessState), Stdout: out}
	}
	if err != nil {
		return out, &PyError{Class: errorutil.Kind(err), Msg: err.Error()}
	}
	return out, nil
}

// Picovoice machine name of a Linux host, the Raspberry Pi core for ARM machines
func pvLinuxMachine(machine string, is64 bool, cpuInfo func() ([]byte, error)) (string, error) {
	archInfo := ""
	switch machine {
	case "x86_64":
		return machine, nil
	case "aarch64", "armv7l", "armv6l":
		if is64 {
			archInfo = "-" + machine
		}
	default:
		return "", &PyError{Class: "NotImplementedError", Msg: fmt.Sprintf("Unsupported CPU architecture: `%s`", machine)}
	}
	info := ""
	cpuPart, err := func() (string, error) {
		raw, err := cpuInfo()
		if err != nil {
			return "", err
		}
		if info, err = charsetutil.Decode(raw, "utf-8"); err != nil {
			info = ""
			return "", err
		}
		for _, line := range strings.Split(info, "\n") {
			if strings.Contains(line, "CPU part") {
				fields := strings.Split(line, " ")
				return strings.ToLower(fields[len(fields)-1]), nil
			}
		}
		return "", &PyError{Class: "IndexError", Msg: "list index out of range"}
	}()
	if err != nil {
		return "", &PyError{Class: "RuntimeError", Msg: fmt.Sprintf("Failed to identify the CPU with '%s'\nCPU info: %s", err, info)}
	}
	switch {
	case cpuPart == "0xb76":
		return "arm11" + archInfo, nil
	case cpuPart == "0xd03":
		return "cortex-a53" + archInfo, nil
	case cpuPart == "0xd08":
		return "cortex-a72" + archInfo, nil
	case cpuPart == "0xd0b":
		return "cortex-a76" + archInfo, nil
	case machine == "armv7l":
		pkgLog().Warn(fmt.Sprintf("WARNING: Please be advised that this device (CPU part = %s) is not officially supported by Picovoice. "+
			"Falling back to the armv6-based (Raspberry Pi Zero) library. This is not tested nor optimal.", cpuPart))
		return "arm11", nil
	}
	return "", &PyError{Class: "NotImplementedError", Msg: fmt.Sprintf("Unsupported CPU: '%s'.", cpuPart)}
}

// Picovoice system and machine pair of a host, refusing systems other than Darwin, Linux and Windows
func pvPlatform(system, machine string, is64 bool, cpuInfo func() ([]byte, error)) (string, string, error) {
	if system != "Darwin" && system != "Linux" && system != "Windows" {
		return "", "", &PyError{Class: "ValueError", Msg: fmt.Sprintf("Unsupported system '%s'.", system)}
	}
	if system == "Linux" {
		pvMachine, err := pvLinuxMachine(machine, is64, cpuInfo)
		return system, pvMachine, err
	}
	return system, machine, nil
}

var raspberryPiMachines = map[string]bool{"arm11": true, "cortex-a53": true, "cortex-a72": true, "cortex-a76": true,
	"cortex-a53-aarch64": true, "cortex-a72-aarch64": true, "cortex-a76-aarch64": true}

// The imported pvporcupine package, its directory with the platform and keywords resolved at import
type Module struct {
	Dir          string
	System       string
	Machine      string
	KeywordPaths map[string]string
	APIURL       string
	Transport    *http.Transport
}

// Imports the package unpacked at dir for this host
func Import(dir string) (*Module, error) {
	return importFor(dir, hostutil.OSName(), hostutil.Machine(), is64bit(), catCPUInfo)
}

// Imports the package unpacked at dir for a given host description
func importFor(dir, system, machine string, is64 bool, cpuInfo func() ([]byte, error)) (*Module, error) {
	m := &Module{Dir: dir, APIURL: PvAPIURL, Transport: http.DefaultTransport.(*http.Transport)}
	var err error
	if m.System, m.Machine, err = pvPlatform(system, machine, is64, cpuInfo); err != nil {
		return nil, err
	}
	if m.KeywordPaths, err = m.PvKeywordPaths(""); err != nil {
		return nil, err
	}
	return m, nil
}

// Path of the Porcupine dynamic library for the host platform
func (m *Module) PvLibraryPath(relative string) (string, error) {
	switch m.System {
	case "Darwin":
		switch m.Machine {
		case "x86_64":
			return pathutil.Join(m.Dir, relative, "lib/mac/x86_64/libpv_porcupine.dylib"), nil
		case "arm64":
			return pathutil.Join(m.Dir, relative, "lib/mac/arm64/libpv_porcupine.dylib"), nil
		}
	case "Linux":
		if m.Machine == "x86_64" {
			return pathutil.Join(m.Dir, relative, "lib/linux/x86_64/libpv_porcupine.so"), nil
		} else if raspberryPiMachines[m.Machine] {
			return pathutil.Join(m.Dir, relative, fmt.Sprintf("lib/raspberry-pi/%s/libpv_porcupine.so", m.Machine)), nil
		}
	case "Windows":
		switch strings.ToLower(m.Machine) {
		case "amd64":
			return pathutil.Join(m.Dir, relative, "lib/windows/amd64/libpv_porcupine.dll"), nil
		case "arm64":
			return pathutil.Join(m.Dir, relative, "lib/windows/arm64/libpv_porcupine.dll"), nil
		}
	}
	return "", &PyError{Class: "NotImplementedError", Msg: "Unsupported platform."}
}

// Path of the file holding the model parameters
func (m *Module) PvModelPath(relative string) string {
	return pathutil.Join(m.Dir, relative, "lib/common/porcupine_params.pv")
}

// Keyword files directory name of the host platform
func (m *Module) PvKeywordFilesSubdir() (string, error) {
	switch m.System {
	case "Darwin":
		return "mac", nil
	case "Linux":
		if m.Machine == "x86_64" {
			return "linux", nil
		} else if raspberryPiMachines[m.Machine] {
			return "raspberry-pi", nil
		}
	case "Windows":
		return "windows", nil
	}
	return "", &PyError{Class: "NotImplementedError", Msg: "Unsupported platform"}
}

// Built in keyword names mapped to their keyword file paths for the host platform
func (m *Module) PvKeywordPaths(relative string) (map[string]string, error) {
	subdir, err := m.PvKeywordFilesSubdir()
	if err != nil {
		return nil, err
	}
	keywordFilesDir := pathutil.Join(m.Dir, relative, "resources/keyword_files", subdir)
	entries, err := os.ReadDir(keywordFilesDir)
	if err != nil {
		return nil, &PyError{Class: errorutil.Kind(err), Msg: err.Error()}
	}
	res := map[string]string{}
	for _, entry := range entries {
		x := entry.Name()
		res[strings.Split(x, "_")[0]] = pathutil.Join(keywordFilesDir, x)
	}
	return res, nil
}

var (
	// The language codes the training endpoint accepts
	ValidLanguages = []string{"de", "en", "es", "fr", "it", "ja", "ko", "pt"}
	// The platform names the training endpoint accepts
	ValidPlatforms = []string{"linux", "mac", "windows", "raspberry-pi", "wasm", "android", "ios"}
)

const (
	// The longest phrase the training endpoint accepts
	PorcupinePhraseMaxLength = 64
	// The Picovoice REST endpoint
	PvAPIURL = "https://rest.picovoice.ai/"
)

// Training platform name of the host
func (m *Module) PvGetPlatform() (string, error) {
	switch m.System {
	case "Darwin":
		return "mac", nil
	case "Linux":
		if m.Machine == "x86_64" {
			return "linux", nil
		} else if raspberryPiMachines[m.Machine] {
			return "raspberry-pi", nil
		}
	case "Windows":
		return "windows", nil
	}
	return "", &PyError{Class: "NotImplementedError", Msg: "(" + strconv.Quote("Unsupported platform ('%s', '%s').") + ", " +
		strconv.Quote(m.System) + ", " + strconv.Quote(m.Machine) + ")"}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Trains a keyword model for a phrase over the Picovoice REST endpoint and writes it to outputPath
func (m *Module) PvTrainModel(ctx context.Context, accessKey, outputPath, language, phrase string, platform *string) error {
	if !contains(ValidLanguages, language) {
		return &PyError{Class: "ValueError", Msg: fmt.Sprintf("Invalid language '%s'", language)}
	}
	target := ""
	if platform == nil {
		got, err := m.PvGetPlatform()
		if err != nil {
			return err
		}
		target = got
	} else {
		target = *platform
	}
	if !contains(ValidPlatforms, target) {
		return &PyError{Class: "ValueError", Msg: fmt.Sprintf("Invalid platform '%s'", target)}
	}
	if len(phrase) == 0 {
		return &PyError{Class: "ValueError", Msg: "Phrase must not be empty"}
	}
	if utf8.RuneCountInString(phrase) > PorcupinePhraseMaxLength {
		return &PyError{Class: "ValueError", Msg: fmt.Sprintf("Phrase must not exceed %d characters", PorcupinePhraseMaxLength)}
	}
	payload := jsonutil.NewObject()
	payload.Set("platform", target)
	payload.Set("phrase", phrase)
	body, err := jsonutil.Encode(payload, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return err
	}
	rawURL := m.APIURL + language + "/api/ppn"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return &PyError{Class: "RuntimeError", Msg: fmt.Sprintf("Request failed: %s", err)}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	req.Header.Set("x-api-key", accessKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: httputil.TrustEnvCA(m.Transport), CheckRedirect: httputil.RedirectPolicy(true)}
	resp, err := httputil.Do(client, req.WithContext(ctx), httputil.Timeout{})
	if err != nil {
		return &PyError{Class: "RuntimeError", Msg: fmt.Sprintf("Request failed: %s", err)}
	}
	if resp.StatusCode >= 400 {
		return &PyError{Class: "RuntimeError", Msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Content)}
	}
	return os.WriteFile(outputPath, resp.Content, 0o666)
}
