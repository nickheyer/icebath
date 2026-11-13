package system

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/desktop/storage"
)

// STA apartment PowerShell that prints the Windows clipboard image as base64 PNG with no trailing newline
const wslClipboardImageScript = "Add-Type -AssemblyName System.Windows.Forms,System.Drawing\n" +
	"$img = [System.Windows.Forms.Clipboard]::GetImage()\n" +
	"if ($null -eq $img) { exit 0 }\n" +
	"$ms = New-Object System.IO.MemoryStream\n" +
	"$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)\n" +
	"[Console]::Out.Write([System.Convert]::ToBase64String($ms.ToArray()))"

const wslClipboardTimeout = 8 * time.Second

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// Base64 of the UTF-16LE script, the -EncodedCommand form
func encodePowerShellCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[2*i:], u)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// PowerShell on the WSL PATH first, then its fixed Windows location
func powershellCandidates() []string {
	return []string{"powershell.exe", "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"}
}

// PNG bytes of the script output, nil for empty output or bytes without the PNG signature
func decodeClipboardImageBase64(stdout string) []byte {
	b64 := jsTrim(stdout)
	if b64 == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) < len(pngSignature) || !bytes.Equal(data[:len(pngSignature)], pngSignature) {
		return nil
	}
	return data
}

// Runs one PowerShell candidate with the 8 second deadline and 64 MiB output cap, an error for any failure
func runPowerShell(name string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wslClipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out cappedBuffer
	out.limit = 64 * 1024 * 1024
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if out.over {
		return "", errMaxBuffer
	}
	return out.String(), nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.over = true
		return 0, errMaxBuffer
	}
	return b.Buffer.Write(p)
}

type maxBufferError struct{}

func (maxBufferError) Error() string { return "stdout maxBuffer length exceeded" }

var errMaxBuffer error = maxBufferError{}

// Reads the Windows host clipboard image through PowerShell from WSL, nil when there is none
func readWslWindowsClipboardImage(run func(name string, args []string) (string, error), candidates []string) []byte {
	encoded := encodePowerShellCommand(wslClipboardImageScript)
	for _, ps := range candidates {
		stdout, err := run(ps, []string{"-NoProfile", "-NonInteractive", "-STA", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded})
		if err != nil {
			continue
		}
		if decoded := decodeClipboardImageBase64(stdout); decoded != nil {
			return decoded
		}
		if strings.TrimSpace(stdout) == "" {
			return nil
		}
	}
	return nil
}

// Writes the clipboard text, the writeClipboard invoke
func (s *Service) WriteClipboard(text any) bool {
	s.o.Host.WriteClipboardText(jsOr(text, ""))
	return true
}

// Reads the clipboard text, the readClipboard invoke
func (s *Service) ReadClipboard() string {
	return s.o.Host.ReadClipboardText()
}

// Saves the clipboard PNG as a composer image, the Windows host clipboard on WSL second, empty without an image
func (s *Service) SaveClipboardImage() (string, error) {
	if png := s.o.Host.ReadClipboardPNG(); len(png) > 0 {
		return storage.WriteComposerImage(s.o.UserData, png, ".png", "", time.Now())
	}
	if s.o.IsWSL {
		if png := readWslWindowsClipboardImage(runPowerShell, powershellCandidates()); png != nil {
			return storage.WriteComposerImage(s.o.UserData, png, ".png", "", time.Now())
		}
	}
	return "", nil
}
