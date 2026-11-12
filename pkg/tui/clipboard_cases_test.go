package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// One recorded clipboard command call
type clipCall struct {
	cmd   string
	args  []string
	stdin *string
}

// Read runner answering each call from a list of outputs or errors
func fakeRun(calls *[]clipCall, outs ...any) clipboardRun {
	return func(_ context.Context, cmd string, args []string) (string, error) {
		i := len(*calls)
		*calls = append(*calls, clipCall{cmd: cmd, args: args})
		if i >= len(outs) {
			i = len(outs) - 1
		}
		if err, isErr := outs[i].(error); isErr {
			return "", err
		}
		return outs[i].(string), nil
	}
}

// Write starter answering each call from a list of exit results
func fakeStart(calls *[]clipCall, oks ...bool) clipboardStart {
	return func(_ context.Context, cmd string, args []string, stdin *string) bool {
		i := len(*calls)
		*calls = append(*calls, clipCall{cmd: cmd, args: args, stdin: stdin})
		if i >= len(oks) {
			i = len(oks) - 1
		}
		return oks[i]
	}
}

func TestReadClipboardText(t *testing.T) {
	ctx := context.Background()
	failed := errors.New("clipboard failed")
	psArgs := strings.Join(powershellReadArgs, " ")
	t.Run("reads text from pbpaste on macOS", func(t *testing.T) {
		var calls []clipCall
		got, ok := readClipboardTextWith(ctx, "darwin", fakeRun(&calls, "hello world\n"), envMap(nil))
		if !ok || got != "hello world\n" || calls[0].cmd != "pbpaste" || len(calls[0].args) != 0 {
			t.Fatalf("got %q %v %+v", got, ok, calls)
		}
	})
	t.Run("reads text from PowerShell on Windows", func(t *testing.T) {
		var calls []clipCall
		b64 := base64.StdEncoding.EncodeToString([]byte("from windows\r\n"))
		got, _ := readClipboardTextWith(ctx, "windows", fakeRun(&calls, b64), envMap(nil))
		if got != "from windows\r\n" || calls[0].cmd != "powershell" || strings.Join(calls[0].args, " ") != psArgs {
			t.Fatalf("got %q %+v", got, calls)
		}
	})
	t.Run("tries powershell.exe first on WSL", func(t *testing.T) {
		var calls []clipCall
		b64 := base64.StdEncoding.EncodeToString([]byte("from wsl\n"))
		got, _ := readClipboardTextWith(ctx, "linux", fakeRun(&calls, b64), envMap(map[string]string{"WSL_INTEROP": "/tmp/socket"}))
		if got != "from wsl\n" || calls[0].cmd != "powershell.exe" || strings.Join(calls[0].args, " ") != psArgs {
			t.Fatalf("got %q %+v", got, calls)
		}
	})
	t.Run("uses wl-paste on Wayland Linux", func(t *testing.T) {
		var calls []clipCall
		got, _ := readClipboardTextWith(ctx, "linux", fakeRun(&calls, "from wayland\n"), envMap(map[string]string{"WAYLAND_DISPLAY": "wayland-1"}))
		if got != "from wayland\n" || calls[0].cmd != "wl-paste" || strings.Join(calls[0].args, " ") != "--type text" {
			t.Fatalf("got %q %+v", got, calls)
		}
	})
	t.Run("falls back to xclip on Linux when wl-paste fails", func(t *testing.T) {
		var calls []clipCall
		got, _ := readClipboardTextWith(ctx, "linux", fakeRun(&calls, failed, "from x11\n"), envMap(map[string]string{"WAYLAND_DISPLAY": "wayland-1"}))
		if got != "from x11\n" || len(calls) != 2 || calls[0].cmd != "wl-paste" || calls[1].cmd != "xclip" || strings.Join(calls[1].args, " ") != "-selection clipboard -out" {
			t.Fatalf("got %q %+v", got, calls)
		}
	})
	t.Run("returns null when every clipboard backend fails", func(t *testing.T) {
		var calls []clipCall
		if _, ok := readClipboardTextWith(ctx, "linux", fakeRun(&calls, failed), envMap(map[string]string{"WAYLAND_DISPLAY": "wayland-1"})); ok {
			t.Fatal("want no text")
		}
	})
	t.Run("preserves CJK text via base64 decoding from PowerShell on WSL", func(t *testing.T) {
		var calls []clipCall
		text := "\u4f60\u597d\uff0c\u4e16\u754c\n"
		got, _ := readClipboardTextWith(ctx, "linux", fakeRun(&calls, base64.StdEncoding.EncodeToString([]byte(text))), envMap(map[string]string{"WSL_INTEROP": "/tmp/socket"}))
		if got != text {
			t.Fatalf("got %q", got)
		}
	})
}

func TestIsUsableClipboardText(t *testing.T) {
	t.Run("accepts normal text", func(t *testing.T) {
		if !isUsableClipboardText("hello world\n") {
			t.Fatal("want true")
		}
	})
	t.Run("rejects empty or whitespace-only content", func(t *testing.T) {
		if isUsableClipboardText("") || isUsableClipboardText("  \n\t") {
			t.Fatal("want false")
		}
	})
	t.Run("rejects binary-looking clipboard payloads", func(t *testing.T) {
		if isUsableClipboardText("PNG\x00\x01\x02\x03IHDR") || isUsableClipboardText("TIFF\ufffd\ufffd\ufffdmetadata") {
			t.Fatal("want false")
		}
	})
}

func TestWriteClipboardText(t *testing.T) {
	ctx := context.Background()
	stdinOf := func(c clipCall) string {
		if c.stdin == nil {
			return "<none>"
		}
		return *c.stdin
	}
	t.Run("does nothing off macOS when no tools are available", func(t *testing.T) {
		var calls []clipCall
		if writeClipboardTextWith(ctx, "hello", "linux", fakeStart(&calls, false), envMap(nil)) {
			t.Fatal("want false")
		}
	})
	t.Run("writes text to pbcopy on macOS", func(t *testing.T) {
		var calls []clipCall
		if !writeClipboardTextWith(ctx, "hello world", "darwin", fakeStart(&calls, true), envMap(nil)) || calls[0].cmd != "pbcopy" || len(calls[0].args) != 0 || stdinOf(calls[0]) != "hello world" {
			t.Fatalf("%+v", calls)
		}
	})
	t.Run("returns false when pbcopy fails", func(t *testing.T) {
		var calls []clipCall
		if writeClipboardTextWith(ctx, "hello world", "darwin", fakeStart(&calls, false), envMap(nil)) {
			t.Fatal("want false")
		}
	})
	wayland := envMap(map[string]string{"WAYLAND_DISPLAY": "wayland-1"})
	t.Run("uses wl-copy on Wayland Linux", func(t *testing.T) {
		var calls []clipCall
		if !writeClipboardTextWith(ctx, "wayland text", "linux", fakeStart(&calls, true), wayland) || calls[0].cmd != "wl-copy" || strings.Join(calls[0].args, " ") != "--type text/plain" || stdinOf(calls[0]) != "wayland text" {
			t.Fatalf("%+v", calls)
		}
	})
	t.Run("falls back to xclip when wl-copy fails on Wayland", func(t *testing.T) {
		var calls []clipCall
		if !writeClipboardTextWith(ctx, "x11 text", "linux", fakeStart(&calls, false, true), wayland) || calls[0].cmd != "wl-copy" || calls[1].cmd != "xclip" || strings.Join(calls[1].args, " ") != "-selection clipboard -in" {
			t.Fatalf("%+v", calls)
		}
	})
	t.Run("falls back to xsel when both wl-copy and xclip fail", func(t *testing.T) {
		var calls []clipCall
		if !writeClipboardTextWith(ctx, "xsel text", "linux", fakeStart(&calls, false, false, true), wayland) || len(calls) != 3 || calls[2].cmd != "xsel" || strings.Join(calls[2].args, " ") != "--clipboard --input" {
			t.Fatalf("%+v", calls)
		}
	})
	psScript := func(text string) string {
		return powershellWriteScript(base64.StdEncoding.EncodeToString([]byte(text)))
	}
	t.Run("uses PowerShell on WSL2 when WSL_DISTRO_NAME is set", func(t *testing.T) {
		var calls []clipCall
		ok := writeClipboardTextWith(ctx, "wsl text", "linux", fakeStart(&calls, true), envMap(map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}))
		if !ok || calls[0].cmd != "powershell.exe" || strings.Join(calls[0].args, "|") != "-NoProfile|-NonInteractive|-Command|"+psScript("wsl text") || calls[0].stdin != nil {
			t.Fatalf("%+v", calls)
		}
	})
	t.Run("prefers the Windows clipboard path over wl-copy inside WSLg", func(t *testing.T) {
		var calls []clipCall
		ok := writeClipboardTextWith(ctx, "wslg text", "linux", fakeStart(&calls, true), envMap(map[string]string{"WSL_DISTRO_NAME": "Ubuntu", "WAYLAND_DISPLAY": "wayland-0"}))
		if !ok || len(calls) != 1 || calls[0].cmd != "powershell.exe" {
			t.Fatalf("%+v", calls)
		}
	})
	t.Run("uses PowerShell on Windows", func(t *testing.T) {
		var calls []clipCall
		ok := writeClipboardTextWith(ctx, "win text", "windows", fakeStart(&calls, true), envMap(nil))
		if !ok || calls[0].cmd != "powershell" || strings.Join(calls[0].args, "|") != "-NoProfile|-NonInteractive|-Command|"+psScript("win text") {
			t.Fatalf("%+v", calls)
		}
	})
	t.Run("preserves CJK text via base64 encoding in PowerShell on WSL", func(t *testing.T) {
		var calls []clipCall
		text := "\u4f60\u597d\u4e16\u754c"
		writeClipboardTextWith(ctx, text, "linux", fakeStart(&calls, true), envMap(map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}))
		script := calls[0].args[len(calls[0].args)-1]
		if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(text))) {
			t.Fatalf("script %q", script)
		}
	})
}
