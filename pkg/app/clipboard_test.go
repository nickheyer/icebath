package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

var (
	fakePNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
)

// clipCall is one recorded clipboard tool run
type clipCall struct {
	argv []string
	opts pysubprocess.Options
}

// Routes every clipboard tool run to fake and records the calls
func fakeClipRun(t *testing.T, fake func(argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error)) *[]clipCall {
	t.Helper()
	saved := clipRun
	t.Cleanup(func() { clipRun = saved })
	calls := &[]clipCall{}
	clipRun = func(_ context.Context, argv []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		*calls = append(*calls, clipCall{argv, o})
		return fake(argv, o)
	}
	return calls
}

// Completed run with a return code and stdout text
func clipDone(code int, stdout string) (*pysubprocess.Completed, error) {
	return &pysubprocess.Completed{Returncode: code, Stdout: []byte(stdout)}, nil
}

func TestProbeUsesDevnullStdin(t *testing.T) {
	calls := fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) { return clipDone(0, "") })
	if !clipProbe([]string{"clipboard-tool"}, 3, func(r clipResult) bool { return r.code == 0 }, "") {
		t.Fatal("probe failed")
	}
	if (*calls)[0].opts.Input != nil {
		t.Fatal("probe fed stdin")
	}
}

func TestPipeToFileUsesDevnullStdin(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func(_ []string, o pysubprocess.Options) (*pysubprocess.Completed, error) {
		if o.Input != nil {
			t.Fatal("pipe fed stdin")
		}
		return &pysubprocess.Completed{Stdout: fakePNG}, nil
	})
	if ok, err := pipeToFile([]string{"clipboard-tool"}, dest); !ok || err != nil {
		t.Fatalf("pipe %v %v", ok, err)
	}
	if data, _ := os.ReadFile(dest); !bytes.Equal(data, fakePNG) {
		t.Fatalf("data %q", data)
	}
}

func TestSuccessWritesFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		_ = os.WriteFile(dest, fakePNG, 0o644)
		return clipDone(0, "")
	})
	if !macosPngpaste(dest) {
		t.Fatal("pngpaste failed")
	}
	if st, _ := os.Stat(dest); st.Size() != int64(len(fakePNG)) {
		t.Fatalf("size %d", st.Size())
	}
}

func TestEmptyFileRejected(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		_ = os.WriteFile(dest, nil, 0o644)
		return clipDone(0, "")
	})
	if macosPngpaste(dest) {
		t.Fatal("empty file accepted")
	}
}

func TestImageClassDetection(t *testing.T) {
	for stdout, want := range map[string]bool{"«class PNGf», «class ut16»": true, "«class ut16», «class utf8»": false} {
		fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) { return clipDone(0, stdout) })
		if got := macosHasImage(); got != want {
			t.Fatalf("%q gave %v", stdout, got)
		}
	}
}

func TestSuccessWithPng(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	calls := 0
	fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		calls++
		if calls == 1 {
			return clipDone(0, "«class PNGf», «class ut16»")
		}
		_ = os.WriteFile(dest, fakePNG, 0o644)
		return clipDone(0, "")
	})
	if !macosOsascript(dest) || !nonEmptyFile(dest) {
		t.Fatal("osascript extraction failed")
	}
}

func TestExtractionReturnsFail(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	calls := 0
	fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		calls++
		if calls == 1 {
			return clipDone(0, "«class PNGf»")
		}
		return clipDone(0, "fail")
	})
	if macosOsascript(dest) {
		t.Fatal("fail reply accepted")
	}
}

// Fake osascript serving a Finder file url of src
func furlRun(src string) func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
	return func(argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "clipboard info"):
			return clipDone(0, "«class furl», 28")
		case strings.Contains(joined, "«class furl»"):
			return clipDone(0, src+"\n")
		}
		return clipDone(1, "")
	}
}

func TestOnlyCopiedImageFilesAreClipboardImages(t *testing.T) {
	for name, want := range map[string]bool{"shot.png": true, "notes.txt": false} {
		src := filepath.Join(t.TempDir(), name)
		_ = os.WriteFile(src, fakePNG, 0o644)
		fakeClipRun(t, furlRun(src))
		if got := macosHasImage(); got != want {
			t.Fatalf("%s gave %v", name, got)
		}
	}
}

func TestCopiedImageFileSavesAsPng(t *testing.T) {
	dir := t.TempDir()
	src, dest := filepath.Join(dir, "shot.png"), filepath.Join(dir, "out.png")
	_ = os.WriteFile(src, fakePNG, 0o644)
	fakeClipRun(t, furlRun(src))
	if !macosOsascript(dest) {
		t.Fatal("file url save failed")
	}
	if data, _ := os.ReadFile(dest); !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Fatalf("data %q", data)
	}
}

func TestClipboardImageProbe(t *testing.T) {
	for stdout, want := range map[string]bool{"True\n": true, "False\n": false} {
		fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) { return clipDone(0, stdout) })
		if got := wslHasImage(); got != want {
			t.Fatalf("%q gave %v", stdout, got)
		}
	}
}

func TestWslHasImageFallsBackToGetClipboardImage(t *testing.T) {
	replies := []string{"False\n", "True\n"}
	calls := fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		reply := replies[0]
		replies = replies[1:]
		return clipDone(0, reply)
	})
	if !wslHasImage() || len(*calls) != 2 {
		t.Fatalf("calls %d", len(*calls))
	}
}

func TestWslSaveSuccessfulExtraction(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		return clipDone(0, base64.StdEncoding.EncodeToString(fakePNG)+"\n")
	})
	if !wslSave(dest) {
		t.Fatal("extraction failed")
	}
	if data, _ := os.ReadFile(dest); !bytes.Equal(data, fakePNG) {
		t.Fatalf("data %q", data)
	}
}

func TestInvalidBase64(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		return clipDone(0, "not-valid-base64!!!")
	})
	if wslSave(dest) {
		t.Fatal("invalid base64 accepted")
	}
}

func TestTypeListDetection(t *testing.T) {
	for types, want := range map[string]bool{"image/png\ntext/plain\n": true, "text/html\nimage/bmp\n": true, "text/plain\ntext/html\n": false} {
		fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) { return clipDone(0, types) })
		if got := waylandHasImage(); got != want {
			t.Fatalf("%q gave %v", types, got)
		}
	}
}

// Fake wl-paste listing types and serving the PNG on extraction
func waylandRun(types string) func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
	return func(argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		if slices.Contains(argv, "--list-types") {
			return clipDone(0, types)
		}
		return &pysubprocess.Completed{Stdout: fakePNG}, nil
	}
}

func TestPngExtraction(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	calls := fakeClipRun(t, waylandRun("image/png\ntext/plain\n"))
	if !waylandSave(dest) || !nonEmptyFile(dest) {
		t.Fatal("extraction failed")
	}
	for _, c := range *calls {
		if c.opts.Input != nil {
			t.Fatal("stdin fed")
		}
	}
}

func TestPrefersPngOverBmp(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	calls := fakeClipRun(t, waylandRun("image/bmp\nimage/png\ntext/plain\n"))
	if !waylandSave(dest) || !slices.Contains((*calls)[1].argv, "image/png") {
		t.Fatalf("calls %v", *calls)
	}
}

func TestTargetsDetection(t *testing.T) {
	for targets, want := range map[string]bool{"image/png\ntext/plain\n": true, "text/plain\n": false} {
		fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) { return clipDone(0, targets) })
		if got := xclipHasImage(); got != want {
			t.Fatalf("%q gave %v", targets, got)
		}
	}
}

func TestImageExtractionSuccess(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func(argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		if slices.Contains(argv, "TARGETS") {
			return clipDone(0, "image/png\ntext/plain\n")
		}
		return &pysubprocess.Completed{Stdout: fakePNG}, nil
	})
	if !xclipSave(dest) || !nonEmptyFile(dest) {
		t.Fatal("extraction failed")
	}
}

func TestExtractionFailsCleansUp(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.png")
	fakeClipRun(t, func(argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		if slices.Contains(argv, "TARGETS") {
			return clipDone(0, "image/png\n")
		}
		return nil, errors.New("pipe broke")
	})
	if xclipSave(dest) {
		t.Fatal("broken pipe accepted")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("partial file kept")
	}
}

// Pins the resolved PowerShell executable the way the hermes cases patch _get_ps_exe
func pinPSExe(t *testing.T, exe string) {
	t.Helper()
	psExeMu.Lock()
	savedResolved, savedExe := psExeResolved, psExe
	psExeResolved, psExe = true, exe
	psExeMu.Unlock()
	t.Cleanup(func() {
		psExeMu.Lock()
		psExeResolved, psExe = savedResolved, savedExe
		psExeMu.Unlock()
	})
}

// Fake runner answering successive runs with the given replies
func clipReplies(t *testing.T, replies ...[2]any) *[]clipCall {
	t.Helper()
	return fakeClipRun(t, func([]string, pysubprocess.Options) (*pysubprocess.Completed, error) {
		reply := replies[0]
		if len(replies) > 1 {
			replies = replies[1:]
		}
		return clipDone(reply[0].(int), reply[1].(string))
	})
}

func TestClipboardHasImage(t *testing.T) {
	pinPSExe(t, "powershell")
	clipReplies(t, [2]any{0, "True\n"})
	if !windowsHasImage() {
		t.Fatal("image missed")
	}
}

func TestWindowsHasImageFallsBackToGetClipboardImage(t *testing.T) {
	pinPSExe(t, "powershell")
	calls := clipReplies(t, [2]any{0, "False\n"}, [2]any{0, "True\n"})
	if !windowsHasImage() || len(*calls) != 2 {
		t.Fatalf("calls %d", len(*calls))
	}
}

func TestWindowsSaveSuccessfulExtraction(t *testing.T) {
	pinPSExe(t, "powershell")
	dest := filepath.Join(t.TempDir(), "out.png")
	clipReplies(t, [2]any{0, base64.StdEncoding.EncodeToString(fakePNG) + "\n"})
	if !windowsSave(dest) {
		t.Fatal("extraction failed")
	}
	if data, _ := os.ReadFile(dest); !bytes.Equal(data, fakePNG) {
		t.Fatalf("data %q", data)
	}
}

func TestFallsBackToFiledropImage(t *testing.T) {
	pinPSExe(t, "powershell")
	dest := filepath.Join(t.TempDir(), "out.png")
	calls := clipReplies(t, [2]any{1, ""}, [2]any{1, ""}, [2]any{0, base64.StdEncoding.EncodeToString(fakePNG) + "\n"})
	if !windowsSave(dest) || len(*calls) != 3 {
		t.Fatalf("calls %d", len(*calls))
	}
	if data, _ := os.ReadFile(dest); !bytes.Equal(data, fakePNG) {
		t.Fatalf("data %q", data)
	}
}

func TestImagemagickFailurePreservesOriginal(t *testing.T) {
	fakeBMP := append([]byte("BM"), make([]byte, 100)...)
	for _, failure := range []string{"nonzero-exit", "timeout"} {
		dest := filepath.Join(t.TempDir(), "img.png")
		if err := os.WriteFile(dest, fakeBMP, 0o644); err != nil {
			t.Fatal(err)
		}
		fakeClipRun(t, func(argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
			if failure == "timeout" {
				return nil, &pysubprocess.TimeoutExpired{Cmd: argv, Timeout: 5}
			}
			return clipDone(1, "")
		})
		convertToPNG(dest)
		if data, err := os.ReadFile(dest); err != nil || !bytes.Equal(data, fakeBMP) {
			t.Fatalf("%s lost the original: %v", failure, err)
		}
	}
}
