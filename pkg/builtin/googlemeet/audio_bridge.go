package googlemeet

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const blackholeDevice = "BlackHole 2ch"

// Platform name like platform.system, swapped by tests
var bridgePlatform = pyplatform.System

// Runs argv with stdin closed and returns stdout, stderr and the exit code, err set when it cannot start
var bridgeRun = func(argv []string) (string, string, int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return toText(stdout.String()), toText(stderr.String()), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", "", 0, err
	}
	return toText(stdout.String()), toText(stderr.String()), 0, nil
}

// Decodes child output as UTF-8 with replacement like text mode with errors replace
func toText(s string) string { return strings.ToValidUTF8(s, "�") }

// Non zero exit of a checked command, carrying its captured output
type calledProcessError struct {
	argv       []string
	code       int
	stdout     string
	stderr     string
	combined   string
	hasCombine bool
}

func (e *calledProcessError) Error() string {
	items := make([]any, len(e.argv))
	for i, a := range e.argv {
		items[i] = a
	}
	return fmt.Sprintf("Command %s returned non-zero exit status %d.", pyrepr.Repr(items), e.code)
}

// Runs pactl with the arguments, a non zero exit being an error only when check is set
func pactl(check bool, args ...string) (string, error) {
	argv := append([]string{"pactl"}, args...)
	stdout, stderr, code, err := bridgeRun(argv)
	if err != nil {
		return "", err
	}
	if check && code != 0 {
		return "", &calledProcessError{argv: argv, code: code, stdout: stdout, stderr: stderr}
	}
	return stdout, nil
}

// Virtual audio device Chrome's fake mic reads, set up before launch and torn down after
type audioBridge struct {
	namePrefix  string
	platform    string
	deviceName  string
	writeTarget string
	moduleIDs   []int
}

// Bridge whose Linux sink and source names start with prefix
func newAudioBridge(prefix string) *audioBridge { return &audioBridge{namePrefix: prefix} }

// Value of a property that exists only after setup
func ready(value string) (string, error) {
	if value == "" {
		return "", errors.New("AudioBridge not set up yet")
	}
	return value, nil
}

func (b *audioBridge) device() (string, error) { return ready(b.deviceName) }

func (b *audioBridge) target() (string, error) { return ready(b.writeTarget) }

// Provisions the device, refusing unsupported platforms and missing tools
func (b *audioBridge) setup() (map[string]any, error) {
	switch system := bridgePlatform(); system {
	case "Linux":
		return b.setupLinux()
	case "Darwin":
		return b.setupDarwin()
	case "Windows":
		return nil, errors.New("windows not supported in v2")
	default:
		return nil, fmt.Errorf("unsupported platform: %s", system)
	}
}

// Unloads the pactl modules newest first, idempotent and never failing
func (b *audioBridge) teardown() {
	for i := len(b.moduleIDs) - 1; i >= 0; i-- {
		_, _ = pactl(false, "unload-module", fmt.Sprint(b.moduleIDs[i]))
	}
	b.moduleIDs = nil
}

// Records the provisioned device and returns its description
func (b *audioBridge) finish(tag, device, writeTarget string, moduleIDs []int) map[string]any {
	b.platform, b.deviceName, b.writeTarget, b.moduleIDs = tag, device, writeTarget, moduleIDs
	ids := make([]any, len(moduleIDs))
	for i, id := range moduleIDs {
		ids[i] = id
	}
	return map[string]any{"platform": tag, "device_name": device, "sample_rate": 48000, "channels": 2,
		"module_ids": ids, "write_target": writeTarget}
}

// Detail of a failed pactl call, its stderr or the error itself
func stderrOr(err error) string {
	var cpe *calledProcessError
	if errors.As(err, &cpe) && cpe.stderr != "" {
		return cpe.stderr
	}
	return err.Error()
}

// Loads a null sink and a virtual source on its monitor, rolling back the sink when the source fails
func (b *audioBridge) setupLinux() (map[string]any, error) {
	sinkName, srcName := b.namePrefix+"_sink", b.namePrefix+"_src"
	sinkOut, err := pactl(true, "load-module", "module-null-sink", "sink_name="+sinkName, "sink_properties=device.description=IcebathMeetSink")
	if err != nil {
		var cpe *calledProcessError
		if !errors.As(err, &cpe) {
			return nil, fmt.Errorf("pactl not found — install PulseAudio/pipewire-pulse")
		}
		return nil, fmt.Errorf("pactl load-module null-sink failed: %s", stderrOr(err))
	}
	sinkModID, err := parseModuleID(sinkOut)
	if err != nil {
		return nil, err
	}
	srcOut, err := pactl(true, "load-module", "module-virtual-source", "source_name="+srcName, "master="+sinkName+".monitor")
	if err != nil {
		var cpe *calledProcessError
		if !errors.As(err, &cpe) {
			return nil, err
		}
		_, _ = pactl(false, "unload-module", fmt.Sprint(sinkModID))
		return nil, fmt.Errorf("pactl load-module virtual-source failed: %s", stderrOr(err))
	}
	srcModID, err := parseModuleID(srcOut)
	if err != nil {
		return nil, err
	}
	return b.finish("linux", srcName, sinkName, []int{sinkModID, srcModID}), nil
}

// Verifies BlackHole 2ch is installed through system_profiler
func (b *audioBridge) setupDarwin() (map[string]any, error) {
	stdout, stderr, code, err := bridgeRun([]string{"system_profiler", "SPAudioDataType"})
	if err != nil {
		return nil, errors.New("system_profiler not found (macOS-only command)")
	}
	if code != 0 {
		return nil, fmt.Errorf("system_profiler failed: %s", stdout+stderr)
	}
	if !strings.Contains(stdout+stderr, "BlackHole") {
		return nil, errors.New("BlackHole virtual audio device not installed. Install via: brew install blackhole-2ch")
	}
	return b.finish("darwin", blackholeDevice, blackholeDevice, nil), nil
}

// Module id pactl load-module prints as the last token of its first line
func parseModuleID(stdout string) (int, error) {
	text := pystr.Strip(stdout)
	if text == "" {
		return 0, errors.New("pactl load-module returned empty stdout")
	}
	fields := strings.Fields(pystr.Strip(pystr.Splitlines(text, false)[0]))
	token := fields[len(fields)-1]
	n, ok := pyint.Parse(token)
	if !ok || !n.IsInt64() {
		return 0, fmt.Errorf("could not parse pactl module id from: %s", pyrepr.StrRepr(stdout))
	}
	return int(n.Int64()), nil
}
