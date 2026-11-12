package pvporcupine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func wantPyError(t *testing.T, err error, class, msg string) {
	t.Helper()
	var pe *PyError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T), want %s %q", err, err, class, msg)
	}
	if pe.Class != class || pe.Msg != msg {
		t.Fatalf("error %s %q, want %s %q", pe.Class, pe.Msg, class, msg)
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
	wantPyError(t, err, "NotImplementedError", "Unsupported CPU: '0xc07'.")
	_, err = pvLinuxMachine("armv6l", false, cpuInfoWith("0xc07"))
	wantPyError(t, err, "NotImplementedError", "Unsupported CPU: '0xc07'.")
	_, err = pvLinuxMachine("ppc64le", true, cpuInfoWith("0xd03"))
	wantPyError(t, err, "NotImplementedError", "Unsupported CPU architecture: `ppc64le`")
	_, err = pvLinuxMachine("aarch64", true, func() ([]byte, error) { return []byte("model name\t: x\n"), nil })
	wantPyError(t, err, "RuntimeError", "Failed to identify the CPU with 'list index out of range'\nCPU info: model name\t: x\n")
	_, err = pvLinuxMachine("aarch64", true, func() ([]byte, error) {
		return nil, &PyError{Class: "FileNotFoundError", Msg: "[Errno 2] No such file or directory: 'cat'"}
	})
	wantPyError(t, err, "RuntimeError", "Failed to identify the CPU with '[Errno 2] No such file or directory: 'cat''\nCPU info: ")
	_, err = pvLinuxMachine("aarch64", true, func() ([]byte, error) { return []byte("CPU part\t: \xff\n"), nil })
	wantPyError(t, err, "RuntimeError", "Failed to identify the CPU with ''utf-8' codec can't decode byte 0xff in position 11: invalid start byte'\nCPU info: ")
}

func TestPvPlatform(t *testing.T) {
	_, _, err := pvPlatform("FreeBSD", "amd64", true, nil)
	wantPyError(t, err, "ValueError", "Unsupported system 'FreeBSD'.")
	_, _, err = pvPlatform("", "", true, nil)
	wantPyError(t, err, "ValueError", "Unsupported system ''.")
	for _, c := range [][4]string{{"Darwin", "arm64", "Darwin", "arm64"}, {"Darwin", "x86_64", "Darwin", "x86_64"},
		{"Windows", "AMD64", "Windows", "AMD64"}, {"Windows", "ARM64", "Windows", "ARM64"}, {"Linux", "x86_64", "Linux", "x86_64"},
		{"Linux", "aarch64", "Linux", "cortex-a72-aarch64"}} {
		system, machine, err := pvPlatform(c[0], c[1], true, cpuInfoWith("0xd08"))
		if err != nil || system != c[2] || machine != c[3] {
			t.Errorf("%v gave %q %q %v", c, system, machine, err)
		}
	}
	_, _, err = pvPlatform("Linux", "riscv64", true, cpuInfoWith("0xd08"))
	wantPyError(t, err, "NotImplementedError", "Unsupported CPU architecture: `riscv64`")
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
		lib, err := m.PvLibraryPath("")
		if c.lib == "" {
			wantPyError(t, err, "NotImplementedError", "Unsupported platform.")
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
			wantPyError(t, err, "NotImplementedError", "Unsupported platform")
			wantPyError(t, perr, "NotImplementedError", `("Unsupported platform ('%s', '%s').", '`+c.system+`', '`+c.machine+`')`)
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
	var pe *PyError
	if !errors.As(err, &pe) || pe.Class != "FileNotFoundError" || !strings.HasPrefix(pe.Msg, "[Errno 2] No such file or directory: '") ||
		!strings.HasSuffix(pe.Msg, "mac'") {
		t.Fatalf("missing keyword dir error %v", err)
	}
	_, err = importFor(dir, "Linux", "arm11-aarch64", true, nil)
	wantPyError(t, err, "NotImplementedError", "Unsupported CPU architecture: `arm11-aarch64`")
	_, err = importFor(dir, "SunOS", "x86_64", true, nil)
	wantPyError(t, err, "ValueError", "Unsupported system 'SunOS'.")
}

func TestCreateArgumentErrors(t *testing.T) {
	dir := fakeTree(t, "linux", "jarvis_linux.ppn", "alexa_linux.ppn")
	m, err := importFor(dir, "Linux", "x86_64", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Create("key", CreateOptions{})
	wantPyError(t, err, "ValueError", "Either `keywords` or `keyword_paths` must be set.")
	_, err = m.Create("key", CreateOptions{Keywords: []string{"jarvis", "nope"}})
	wantPyError(t, err, "ValueError", `One or more keywords are not available by default. Available default keywords are:\nalexa, jarvis`)
	_, err = m.Create("key", CreateOptions{Keywords: []string{"jarvis"}, Sensitivities: []float64{0.5, 0.5}})
	wantPyError(t, err, "ValueError", "Number of keywords does not match the number of sensitivities.")
	_, err = m.Create("", CreateOptions{Keywords: []string{"jarvis"}})
	wantPyError(t, err, "ValueError", "access_key should be a non-empty string.")
	_, err = m.Create("key", CreateOptions{Keywords: []string{"jarvis"}})
	wantPyError(t, err, "OSError", "Couldn't find Porcupine's dynamic library at '"+dir+string(os.PathSeparator)+"lib/linux/x86_64/libpv_porcupine.so'.")
	other := &Module{Dir: dir, System: "Linux", Machine: "sparc", KeywordPaths: m.KeywordPaths}
	_, err = other.Create("key", CreateOptions{Keywords: []string{"jarvis"}})
	wantPyError(t, err, "NotImplementedError", "Unsupported platform.")
	_, err = other.AvailableDevices(nil)
	wantPyError(t, err, "NotImplementedError", "Unsupported platform.")
}

func TestPorcupineErrorStr(t *testing.T) {
	e := &PorcupineError{Class: "PorcupineError", Message: "Initialization failed"}
	if e.Error() != "Initialization failed" {
		t.Fatalf("plain %q", e.Error())
	}
	e.MessageStack = []string{"first cause", "second cause"}
	if e.Error() != "Initialization failed:\n  [0] first cause\n  [1] second cause" {
		t.Fatalf("stacked %q", e.Error())
	}
}

func TestStatusToException(t *testing.T) {
	want := []string{"", "PorcupineMemoryError", "PorcupineIOError", "PorcupineInvalidArgumentError", "PorcupineStopIterationError",
		"PorcupineKeyError", "PorcupineInvalidStateError", "PorcupineRuntimeError", "PorcupineActivationError",
		"PorcupineActivationLimitError", "PorcupineActivationThrottledError", "PorcupineActivationRefusedError"}
	for status := int32(1); status < int32(len(want)); status++ {
		var pe *PorcupineError
		if err := statusError(status, "Processing failed", []string{"x"}); !errors.As(err, &pe) || pe.Class != want[status] ||
			pe.Message != "Processing failed" || !reflect.DeepEqual(pe.MessageStack, []string{"x"}) {
			t.Errorf("status %d gave %v", status, err)
		}
	}
	wantPyError(t, statusError(12, "m", nil), "ValueError", "12 is not a valid Porcupine.PicovoiceStatuses")
}

func TestPvTrainModel(t *testing.T) {
	m := &Module{System: "Linux", Machine: "x86_64"}
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "model.ppn")
	wantPyError(t, m.PvTrainModel(ctx, "k", out, "xx", "hey there", nil), "ValueError", "Invalid language 'xx'")
	bad := "amiga"
	wantPyError(t, m.PvTrainModel(ctx, "k", out, "en", "hey there", &bad), "ValueError", "Invalid platform 'amiga'")
	wantPyError(t, m.PvTrainModel(ctx, "k", out, "en", "", nil), "ValueError", "Phrase must not be empty")
	wantPyError(t, m.PvTrainModel(ctx, "k", out, "en", strings.Repeat("é", 65), nil), "ValueError", "Phrase must not exceed 64 characters")
	unsupported := &Module{System: "Linux", Machine: "sparc"}
	wantPyError(t, unsupported.PvTrainModel(ctx, "k", out, "en", "x", nil), "NotImplementedError", `("Unsupported platform ('%s', '%s').", 'Linux', 'sparc')`)

	var gotBody, gotPath, gotKey, gotAgent, gotType string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody, gotPath, gotKey, gotAgent, gotType = string(body), r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("model-bytes"))
	}))
	defer srv.Close()
	m.APIURL, m.Transport = srv.URL+"/", http.DefaultTransport.(*http.Transport)
	if err := m.PvTrainModel(ctx, "secret", out, "fr", strings.Repeat("é", 64), nil); err != nil {
		t.Fatal(err)
	}
	wantBody := `{"platform": "linux", "phrase": "` + strings.Repeat(string(rune(92))+"u00e9", 64) + `"}`
	if gotBody != wantBody || gotPath != "/fr/api/ppn" || gotKey != "secret" ||
		gotAgent != "Mozilla/5.0 (Windows NT 10.0; Win64; x64)" || gotType != "application/json" {
		t.Fatalf("request body %q want %q path %q key %q agent %q type %q", gotBody, wantBody, gotPath, gotKey, gotAgent, gotType)
	}
	if data, err := os.ReadFile(out); err != nil || string(data) != "model-bytes" {
		t.Fatalf("written model %q %v", data, err)
	}
	pi := "raspberry-pi"
	if err := m.TrainWakeWordFromPhrase(ctx, "secret", out, "en", "hey there", &pi); err != nil || gotBody != `{"platform": "raspberry-pi", "phrase": "hey there"}` {
		t.Fatalf("explicit platform body %q %v", gotBody, err)
	}
	status = 401
	wantPyError(t, m.PvTrainModel(ctx, "secret", out, "en", "hey there", nil), "RuntimeError", "HTTP 401: model-bytes")
	srv.Close()
	err := m.PvTrainModel(ctx, "secret", out, "en", "hey there", nil)
	var pe *PyError
	if !errors.As(err, &pe) || pe.Class != "RuntimeError" || !strings.HasPrefix(pe.Msg, "Request failed: HTTPConnectionPool(host='127.0.0.1', port=") {
		t.Fatalf("connection failure %v", err)
	}
}
