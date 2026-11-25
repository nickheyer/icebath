package pvporcupine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func cpuInfoWith(part string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return []byte("processor\t: 0\nBogoMIPS\t: 108.00\nCPU implementer\t: 0x41\nCPU part\t: " + part + "\nCPU revision\t: 4\n"), nil
	}
}

func wantError(t *testing.T, err error, kind Kind, msg string) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T), want %v %q", err, err, kind, msg)
	}
	if pe.Kind != kind || pe.Msg != msg {
		t.Fatalf("error %v %q, want %v %q", pe.Kind, pe.Msg, kind, msg)
	}
}

func TestPvLinuxMachineTable(t *testing.T) {
	if got, err := pvLinuxMachine("x86_64", true, nil); err != nil || got != "x86_64" {
		t.Fatalf("x86_64 gave %q %v", got, err)
	}
	parts := map[string]string{"0xb76": "arm11", "0xd03": "cortex-a53", "0xd08": "cortex-a72", "0xd0b": "cortex-a76", "0xD0B": "cortex-a76"}
	for _, machine := range []string{"aarch64", "armv7l", "armv6l"} {
		for part, core := range parts {
			for _, is64 := range []bool{true, false} {
				want := core
				if is64 {
					want += "-" + machine
				}
				got, err := pvLinuxMachine(machine, is64, cpuInfoWith(part))
				if err != nil || got != want {
					t.Errorf("%s %s 64=%v gave %q %v, want %q", machine, part, is64, got, err, want)
				}
			}
		}
	}
	if got, err := pvLinuxMachine("armv7l", true, cpuInfoWith("0xc07")); err != nil || got != "arm11" {
		t.Fatalf("armv7l fallback gave %q %v", got, err)
	}
	_, err := pvLinuxMachine("aarch64", true, cpuInfoWith("0xc07"))
	wantError(t, err, Unsupported, `Unsupported CPU "0xc07".`)
	_, err = pvLinuxMachine("armv6l", false, cpuInfoWith("0xc07"))
	wantError(t, err, Unsupported, `Unsupported CPU "0xc07".`)
	_, err = pvLinuxMachine("ppc64le", true, cpuInfoWith("0xd03"))
	wantError(t, err, Unsupported, `Unsupported CPU architecture "ppc64le"`)
	_, err = pvLinuxMachine("aarch64", true, func() ([]byte, error) { return []byte("model name\t: x\n"), nil })
	wantError(t, err, CPUUnidentified, "Failed to identify the CPU: the CPU description has no CPU part line\nCPU info: model name\t: x\n")
	unreadable := &fs.PathError{Op: "open", Path: "/proc/cpuinfo", Err: fs.ErrNotExist}
	_, err = pvLinuxMachine("aarch64", true, func() ([]byte, error) { return nil, unreadable })
	wantError(t, err, CPUUnidentified, "Failed to identify the CPU: file does not exist: /proc/cpuinfo")
	_, err = pvLinuxMachine("aarch64", true, func() ([]byte, error) { return []byte("CPU part\t: \xff\n"), nil })
	wantError(t, err, Unsupported, "Unsupported CPU \"\ufffd\".")
}

func TestPvPlatform(t *testing.T) {
	_, _, err := pvPlatform("FreeBSD", "amd64", true, nil)
	wantError(t, err, Unsupported, `Unsupported system "FreeBSD".`)
	_, _, err = pvPlatform("", "", true, nil)
	wantError(t, err, Unsupported, `Unsupported system "".`)
	for _, c := range [][4]string{{"Darwin", "arm64", "Darwin", "arm64"}, {"Darwin", "x86_64", "Darwin", "x86_64"},
		{"Windows", "AMD64", "Windows", "AMD64"}, {"Windows", "ARM64", "Windows", "ARM64"}, {"Linux", "x86_64", "Linux", "x86_64"},
		{"Linux", "aarch64", "Linux", "cortex-a72-aarch64"}} {
		system, machine, err := pvPlatform(c[0], c[1], true, cpuInfoWith("0xd08"))
		if err != nil || system != c[2] || machine != c[3] {
			t.Errorf("%v gave %q %q %v", c, system, machine, err)
		}
	}
	_, _, err = pvPlatform("Linux", "riscv64", true, cpuInfoWith("0xd08"))
	wantError(t, err, Unsupported, `Unsupported CPU architecture "riscv64"`)
}

func TestPvPathTables(t *testing.T) {
	sep := string(os.PathSeparator)
	cases := []struct{ system, machine, lib, subdir string }{
		{"Darwin", "x86_64", "lib/mac/x86_64/libpv_porcupine.dylib", "mac"},
		{"Darwin", "arm64", "lib/mac/arm64/libpv_porcupine.dylib", "mac"},
		{"Darwin", "ppc", "", "mac"},
		{"Linux", "x86_64", "lib/linux/x86_64/libpv_porcupine.so", "linux"},
		{"Linux", "arm11", "lib/raspberry-pi/arm11/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "cortex-a53", "lib/raspberry-pi/cortex-a53/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "cortex-a72", "lib/raspberry-pi/cortex-a72/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "cortex-a76", "lib/raspberry-pi/cortex-a76/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "cortex-a53-aarch64", "lib/raspberry-pi/cortex-a53-aarch64/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "cortex-a72-aarch64", "lib/raspberry-pi/cortex-a72-aarch64/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "cortex-a76-aarch64", "lib/raspberry-pi/cortex-a76-aarch64/libpv_porcupine.so", "raspberry-pi"},
		{"Linux", "arm11-aarch64", "", ""},
		{"Linux", "cortex-a53-armv7l", "", ""},
		{"Windows", "AMD64", "lib/windows/amd64/libpv_porcupine.dll", "windows"},
		{"Windows", "amd64", "lib/windows/amd64/libpv_porcupine.dll", "windows"},
		{"Windows", "ARM64", "lib/windows/arm64/libpv_porcupine.dll", "windows"},
		{"Windows", "x86", "", "windows"},
	}
	for _, c := range cases {
		m := &Module{Dir: "pkgdir", System: c.system, Machine: c.machine}
		unsupported := fmt.Sprintf("Unsupported platform (%q, %q).", c.system, c.machine)
		lib, err := m.PvLibraryPath("")
		if c.lib == "" {
			wantError(t, err, Unsupported, unsupported)
		} else if err != nil || lib != "pkgdir"+sep+c.lib {
			t.Errorf("%s %s library %q %v", c.system, c.machine, lib, err)
		}
		if c.lib != "" {
			if rel, _ := m.PvLibraryPath("rel"); rel != "pkgdir"+sep+"rel"+sep+c.lib {
				t.Errorf("%s %s relative library %q", c.system, c.machine, rel)
			}
		}
		subdir, err := m.PvKeywordFilesSubdir()
		platform, perr := m.PvGetPlatform()
		if c.subdir == "" {
			wantError(t, err, Unsupported, unsupported)
			wantError(t, perr, Unsupported, unsupported)
		} else if err != nil || perr != nil || subdir != c.subdir || platform != c.subdir {
			t.Errorf("%s %s subdir %q %v platform %q %v", c.system, c.machine, subdir, err, platform, perr)
		}
	}
	m := &Module{Dir: "pkgdir"}
	if got := m.PvModelPath(""); got != "pkgdir"+sep+"lib/common/porcupine_params.pv" {
		t.Errorf("model path %q", got)
	}
	if got := m.PvModelPath("x"); got != "pkgdir"+sep+"x"+sep+"lib/common/porcupine_params.pv" {
		t.Errorf("relative model path %q", got)
	}
}

func fakeTree(t *testing.T, subdir string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	kw := filepath.Join(dir, "resources", "keyword_files", subdir)
	if err := os.MkdirAll(kw, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(kw, name), []byte("ppn"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestImportKeywordPaths(t *testing.T) {
	dir := fakeTree(t, "linux", "hey google_linux.ppn", "jarvis_linux.ppn", "pico clock_linux.ppn")
	m, err := importFor(dir, "Linux", "x86_64", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	kwDir := dir + string(os.PathSeparator) + "resources/keyword_files" + string(os.PathSeparator) + "linux"
	want := map[string]string{"hey google": kwDir + string(os.PathSeparator) + "hey google_linux.ppn",
		"jarvis": kwDir + string(os.PathSeparator) + "jarvis_linux.ppn", "pico clock": kwDir + string(os.PathSeparator) + "pico clock_linux.ppn"}
	if !reflect.DeepEqual(m.KeywordPaths, want) {
		t.Fatalf("keyword paths %v, want %v", m.KeywordPaths, want)
	}
	if !reflect.DeepEqual(m.Keywords(), []string{"hey google", "jarvis", "pico clock"}) {
		t.Fatalf("keywords %v", m.Keywords())
	}
	if m.APIURL != "https://rest.picovoice.ai/" {
		t.Fatalf("api url %q", m.APIURL)
	}
	_, err = importFor(dir, "Darwin", "arm64", true, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != FileUnavailable || !errors.Is(err, fs.ErrNotExist) ||
		!strings.HasPrefix(pe.Msg, "Couldn't read the keyword files directory: ") || !strings.HasSuffix(pe.Msg, "mac") {
		t.Fatalf("missing keyword dir error %v", err)
	}
	_, err = importFor(dir, "Linux", "arm11-aarch64", true, nil)
	wantError(t, err, Unsupported, `Unsupported CPU architecture "arm11-aarch64"`)
	_, err = importFor(dir, "SunOS", "x86_64", true, nil)
	wantError(t, err, Unsupported, `Unsupported system "SunOS".`)
}

func TestCreateArgumentErrors(t *testing.T) {
	dir := fakeTree(t, "linux", "jarvis_linux.ppn", "alexa_linux.ppn")
	m, err := importFor(dir, "Linux", "x86_64", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Create("key", CreateOptions{})
	wantError(t, err, InvalidArgument, "Either keywords or keyword paths must be set.")
	_, err = m.Create("key", CreateOptions{Keywords: []string{"jarvis", "nope"}})
	wantError(t, err, InvalidArgument, `Keyword "nope" is not available by default. Available default keywords are: alexa, jarvis`)
	_, err = m.Create("key", CreateOptions{Keywords: []string{"jarvis"}, Sensitivities: []float64{0.5, 0.5}})
	wantError(t, err, InvalidArgument, "Number of keywords does not match the number of sensitivities.")
	_, err = m.Create("", CreateOptions{Keywords: []string{"jarvis"}})
	wantError(t, err, InvalidArgument, "Access key should be a non-empty string.")
	_, err = m.Create("key", CreateOptions{Keywords: []string{"jarvis"}})
	wantError(t, err, FileUnavailable, fmt.Sprintf("Couldn't find Porcupine's dynamic library at %q.", dir+string(os.PathSeparator)+"lib/linux/x86_64/libpv_porcupine.so"))
	other := &Module{Dir: dir, System: "Linux", Machine: "sparc", KeywordPaths: m.KeywordPaths}
	_, err = other.Create("key", CreateOptions{Keywords: []string{"jarvis"}})
	wantError(t, err, Unsupported, `Unsupported platform ("Linux", "sparc").`)
	_, err = other.AvailableDevices(nil)
	wantError(t, err, Unsupported, `Unsupported platform ("Linux", "sparc").`)
}

func TestPorcupineErrorStr(t *testing.T) {
	e := &PorcupineError{Message: "Initialization failed"}
	if e.Error() != "Initialization failed" {
		t.Fatalf("plain %q", e.Error())
	}
	e.MessageStack = []string{"first cause", "second cause"}
	if e.Error() != "Initialization failed:\n  [0] first cause\n  [1] second cause" {
		t.Fatalf("stacked %q", e.Error())
	}
}

func TestStatusErrorKeepsAndNamesTheStatus(t *testing.T) {
	want := []string{"", "out of memory", "I/O error", "invalid argument", "stop iteration", "key error", "invalid state", "runtime error",
		"activation error", "activation limit reached", "activation throttled", "activation refused"}
	for status := int32(1); status < int32(len(want)); status++ {
		var pe *PorcupineError
		if err := statusError(status, "Processing failed", []string{"x"}); !errors.As(err, &pe) || pe.Status != status ||
			pe.Message != "Processing failed" || !reflect.DeepEqual(pe.MessageStack, []string{"x"}) || err.Error() != "Processing failed ("+want[status]+"):\n  [0] x" {
			t.Errorf("status %d gave %v", status, err)
		}
	}
	var pe *PorcupineError
	if err := statusError(12, "m", nil); !errors.As(err, &pe) || pe.Status != 12 || err.Error() != "m (status 12)" {
		t.Errorf("unknown status gave %v", err)
	}
}
