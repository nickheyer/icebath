package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

var (
	pngSignature      = []byte("\x89PNG\r\n\x1a\n")
	psFlags           = []string{"-NoProfile", "-NonInteractive"}
	fileImageExts     = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".tiff": true, ".tif": true}
	waylandMimePrefs  = []string{"image/png", "image/jpeg", "image/bmp", "image/gif", "image/webp"}
	wlListTypes       = []string{"wl-paste", "--list-types"}
	xclipTargets      = []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"}
	clipboardPlatform = runtime.GOOS
)

// Subprocess runner of the clipboard tools, replaced by tests the way the hermes cases patch subprocess.run
var clipRun = pysubprocess.RunWith

const wlMissing = "wl-paste not installed — Wayland clipboard unavailable"

// Text mode run result of a clipboard tool, stdout decoded with replacement
type clipResult struct {
	code   int
	stdout string
}

// Runs a clipboard tool in text mode with a timeout, stdin closed
func runClipText(argv []string, timeoutSeconds int) (clipResult, error) {
	res, err := clipRun(nil, argv, pysubprocess.Options{Timeout: time.Duration(timeoutSeconds) * time.Second})
	if err != nil {
		return clipResult{}, err
	}
	return clipResult{code: res.Returncode, stdout: strings.ToValidUTF8(string(res.Stdout), "�")}, nil
}

// Reports a missing executable the way Python raises FileNotFoundError
func isMissingExe(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// Reports an existing non empty file
func nonEmptyFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// Reports a probe that ran and satisfied ok, logging missing when the executable is absent
func clipProbe(argv []string, timeoutSeconds int, ok func(clipResult) bool, missing string) bool {
	r, err := runClipText(argv, timeoutSeconds)
	if err != nil {
		if isMissingExe(err) && missing != "" {
			cliLogger.Debug(missing)
		}
		return false
	}
	return ok(r)
}

// Runs argv with stdout written into dest, true when a non empty file resulted
func pipeToFile(argv []string, dest string) (bool, error) {
	f, err := os.Create(dest)
	if err != nil {
		return false, err
	}
	res, runErr := clipRun(nil, argv, pysubprocess.Options{Timeout: 5 * time.Second, Check: true})
	if res != nil {
		_, _ = f.Write(res.Stdout)
	}
	_ = f.Close()
	if runErr != nil {
		return false, runErr
	}
	return nonEmptyFile(dest), nil
}

// clipBackend is one Linux clipboard backend in fallthrough order
type clipBackend struct {
	enabled bool
	has     func() bool
	save    func(string) bool
}

// Host probes and backends the dispatch reads, swapped by tests as hermes patches the clipboard module functions
var (
	clipIsWSL           = hostinfo.IsWSL
	clipWSLHasImage     = wslHasImage
	clipWSLSave         = wslSave
	clipWaylandHasImage = waylandHasImage
	clipWaylandSave     = waylandSave
	clipXclipSave       = xclipSave
	clipMacosHasImage   = macosHasImage
	clipSaveImage       = saveClipboardImage
)

// Linux backends in fallthrough order of WSL, Wayland then X11
func linuxClipBackends() []clipBackend {
	return []clipBackend{
		{clipIsWSL(), clipWSLHasImage, clipWSLSave},
		{os.Getenv("WAYLAND_DISPLAY") != "", clipWaylandHasImage, clipWaylandSave},
		{true, xclipHasImage, clipXclipSave},
	}
}

// Saves the clipboard image to dest as PNG, true when an image was found and written
func SaveClipboardImage(dest string) bool { return saveClipboardImage(dest) }

// Reports whether the clipboard currently holds an image
func HasClipboardImage() bool { return hasClipboardImage() }

func saveClipboardImage(dest string) bool {
	_ = os.MkdirAll(filepath.Dir(dest), 0o777)
	switch clipboardPlatform {
	case "darwin":
		return macosPngpaste(dest) || macosOsascript(dest)
	case "windows":
		return windowsSave(dest)
	}
	return linuxSave(dest)
}

// Reports whether the clipboard currently holds an image
func hasClipboardImage() bool {
	switch clipboardPlatform {
	case "darwin":
		return clipMacosHasImage()
	case "windows":
		return windowsHasImage()
	}
	for _, b := range linuxClipBackends() {
		if b.enabled && b.has() {
			return true
		}
	}
	return false
}

// clipWriteCommand is one text write candidate, stdin carrying the bytes when pipe is set
type clipWriteCommand struct {
	argv []string
	pipe bool
}

// Text write candidates in platform fallback order, PowerShell decoding base64 of the UTF8 bytes
func writeClipboardCommands(data []byte) []clipWriteCommand {
	b64 := base64.StdEncoding.EncodeToString(data)
	psArgv := append(append([]string{}, psFlags...), "-Command",
		"Set-Clipboard -Value ([System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('"+b64+"')))")
	linux := clipboardPlatform != "darwin" && clipboardPlatform != "windows"
	candidates := []struct {
		enabled bool
		cmd     clipWriteCommand
	}{
		{clipboardPlatform == "darwin", clipWriteCommand{[]string{"pbcopy"}, true}},
		{clipboardPlatform == "windows", clipWriteCommand{append([]string{"powershell"}, psArgv...), false}},
		{linux && hostinfo.IsWSL(), clipWriteCommand{append([]string{"powershell.exe"}, psArgv...), false}},
		{linux && os.Getenv("WAYLAND_DISPLAY") != "", clipWriteCommand{[]string{"wl-copy", "--type", "text/plain"}, true}},
		{linux, clipWriteCommand{[]string{"xclip", "-selection", "clipboard", "-in"}, true}},
		{linux, clipWriteCommand{[]string{"xsel", "--clipboard", "--input"}, true}},
	}
	var out []clipWriteCommand
	for _, c := range candidates {
		if c.enabled {
			out = append(out, c.cmd)
		}
	}
	return out
}

// Reports an SSH session, where native tools would write the remote clipboard
func isRemoteShellSession(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	return getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" || getenv("SSH_CLIENT") != ""
}

// Writes text to the clipboard through the first native tool that succeeds
func writeClipboardText(text string) bool {
	data := []byte(text)
	for _, cmd := range writeClipboardCommands(data) {
		o := pysubprocess.Options{Timeout: 10 * time.Second}
		if cmd.pipe {
			o.Input = data
		}
		res, err := clipRun(nil, cmd.argv, o)
		if err == nil && res.Returncode == 0 {
			return true
		}
	}
	return false
}

// Stdout of an osascript expression, empty on any failure
func osascript(expr string, timeoutSeconds int) string {
	r, err := runClipText([]string{"osascript", "-e", expr}, timeoutSeconds)
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("osascript probe failed: %v", err))
		return ""
	}
	if r.code != 0 {
		return ""
	}
	return r.stdout
}

// Reports a PNG or TIFF bitmap in the clipboard info listing
func macosHasBitmap(info string) bool {
	return strings.Contains(info, "«class PNGf»") || strings.Contains(info, "«class TIFF»")
}

// Local image path of a Finder file url on the clipboard, empty when it is not an image file
func macosClipboardFileImage() string {
	path := pystr.Strip(osascript("POSIX path of (the clipboard as «class furl»)", 3))
	if !fileImageExts[strings.ToLower(filepath.Ext(path))] {
		return ""
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return path
}

// Reports a bitmap or an image file url on the macOS clipboard
func macosHasImage() bool {
	info := osascript("clipboard info", 3)
	return macosHasBitmap(info) || (strings.Contains(info, "«class furl»") && macosClipboardFileImage() != "")
}

// Copies a Finder image file from the clipboard into dest as PNG
func macosSaveFileImage(dest string) bool {
	src := macosClipboardFileImage()
	if src == "" {
		return false
	}
	data, err := os.ReadFile(src)
	if err == nil {
		err = os.WriteFile(dest, data, 0o666)
	}
	if err == nil && (isPNGFile(dest) || (convertToPNG(dest) && isPNGFile(dest))) {
		return true
	}
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("clipboard file-url extract failed: %v", err))
	}
	_ = os.Remove(dest)
	return false
}

// Saves through pngpaste when it is installed
func macosPngpaste(dest string) bool {
	res, err := clipRun(nil, []string{"pngpaste", dest}, pysubprocess.Options{Timeout: 3 * time.Second})
	if err != nil {
		if !isMissingExe(err) {
			cliLogger.Debug(fmt.Sprintf("pngpaste failed: %v", err))
		}
		return false
	}
	return res.Returncode == 0 && nonEmptyFile(dest)
}

// Saves the PNG bitmap through osascript, else a Finder image file url
func macosOsascript(dest string) bool {
	info := osascript("clipboard info", 3)
	if !macosHasBitmap(info) {
		return strings.Contains(info, "«class furl»") && macosSaveFileImage(dest)
	}
	script := "try\n" +
		"  set imgData to the clipboard as «class PNGf»\n" +
		"  set f to open for access POSIX file \"" + dest + "\" with write permission\n" +
		"  write imgData to f\n" +
		"  close access f\n" +
		"on error\n" +
		"  return \"fail\"\n" +
		"end try\n"
	r, err := runClipText([]string{"osascript", "-e", script}, 5)
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("osascript clipboard extract failed: %v", err))
		return false
	}
	return r.code == 0 && !strings.Contains(r.stdout, "fail") && nonEmptyFile(dest)
}

// Quoted sorted image extensions of the PowerShell file drop filter
func fileDropImageExts() string {
	exts := make([]string, 0, len(fileImageExts))
	for e := range fileImageExts {
		exts = append(exts, "'"+e+"'")
	}
	sort.Strings(exts)
	return strings.Join(exts, ",")
}

// Has image and extract as base64 PNG PowerShell scripts tried in order
func psImageStrategies() [][2]string {
	fileDropHit := "try { " +
		"$files = Get-Clipboard -Format FileDropList -ErrorAction Stop;" +
		"$exts = @(" + fileDropImageExts() + ");" +
		"$hit = $files | Where-Object { $exts -contains ([System.IO.Path]::GetExtension($_).ToLowerInvariant()) } | Select-Object -First 1;"
	return [][2]string{
		{"Add-Type -AssemblyName System.Windows.Forms;" +
			"[System.Windows.Forms.Clipboard]::ContainsImage()",
			"Add-Type -AssemblyName System.Windows.Forms;" +
				"Add-Type -AssemblyName System.Drawing;" +
				"$img = [System.Windows.Forms.Clipboard]::GetImage();" +
				"if ($null -eq $img) { exit 1 }" +
				"$ms = New-Object System.IO.MemoryStream;" +
				"$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png);" +
				"[System.Convert]::ToBase64String($ms.ToArray())"},
		{"try { " +
			"$img = Get-Clipboard -Format Image -ErrorAction Stop;" +
			"if ($null -ne $img) { 'True' } else { 'False' }" +
			"} catch { 'False' }",
			"try { " +
				"Add-Type -AssemblyName System.Drawing;" +
				"Add-Type -AssemblyName PresentationCore;" +
				"Add-Type -AssemblyName WindowsBase;" +
				"$img = Get-Clipboard -Format Image -ErrorAction Stop;" +
				"if ($null -eq $img) { exit 1 }" +
				"$ms = New-Object System.IO.MemoryStream;" +
				"if ($img -is [System.Drawing.Image]) {" +
				"$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)" +
				"} elseif ($img -is [System.Windows.Media.Imaging.BitmapSource]) {" +
				"$enc = New-Object System.Windows.Media.Imaging.PngBitmapEncoder;" +
				"$enc.Frames.Add([System.Windows.Media.Imaging.BitmapFrame]::Create($img));" +
				"$enc.Save($ms)" +
				"} else { exit 2 }" +
				"[System.Convert]::ToBase64String($ms.ToArray())" +
				"} catch { exit 1 }"},
		{fileDropHit + "if ($null -ne $hit) { 'True' } else { 'False' }" + "} catch { 'False' }",
			fileDropHit + "if ($null -eq $hit) { exit 1 }" +
				"[System.Convert]::ToBase64String([System.IO.File]::ReadAllBytes($hit))" +
				"} catch { exit 1 }"},
	}
}

// Probes, or with a dest extracts, the Windows clipboard image through a PowerShell executable
func psClipboard(exe string, timeoutSeconds int, label, dest string) bool {
	for _, strategy := range psImageStrategies() {
		script := strategy[0]
		if dest != "" {
			script = strategy[1]
		}
		argv := append(append([]string{exe}, psFlags...), "-Command", script)
		err := func() error {
			r, err := runClipText(argv, timeoutSeconds)
			if err != nil {
				return err
			}
			if dest == "" {
				if r.code == 0 && strings.Contains(r.stdout, "True") {
					return errClipFound
				}
				return nil
			}
			if out := pystr.Strip(r.stdout); r.code == 0 && out != "" {
				data, err := base64.StdEncoding.Strict().DecodeString(out)
				if err != nil {
					return err
				}
				if err := os.WriteFile(dest, data, 0o666); err != nil {
					return err
				}
				if nonEmptyFile(dest) {
					return errClipFound
				}
			}
			return nil
		}()
		switch {
		case errors.Is(err, errClipFound):
			return true
		case err == nil:
		case isMissingExe(err):
			cliLogger.Debug(exe + " not found — clipboard unavailable")
			return false
		default:
			stage := "extraction"
			if dest == "" {
				stage = "check"
			}
			cliLogger.Debug(fmt.Sprintf("%s clipboard image %s failed: %v", label, stage, err))
			if dest != "" {
				_ = os.Remove(dest)
			}
		}
	}
	return false
}

// Sentinel of a strategy that found the image
var errClipFound = errors.New("clipboard image found")

// First working PowerShell executable, resolved once per process
var (
	psExeMu       sync.Mutex
	psExeResolved bool
	psExe         string
)

// First working PowerShell executable cached per process, empty when neither runs
func getPSExe() string {
	psExeMu.Lock()
	defer psExeMu.Unlock()
	if !psExeResolved {
		psExeResolved = true
		for _, name := range []string{"powershell", "pwsh"} {
			argv := append(append([]string{name}, psFlags...), "-Command", "echo ok")
			if clipProbe(argv, 5, func(r clipResult) bool { return r.code == 0 && strings.Contains(r.stdout, "ok") }, "") {
				psExe = name
				break
			}
		}
	}
	return psExe
}

// Reports an image on the Windows clipboard
func windowsHasImage() bool {
	ps := getPSExe()
	return ps != "" && psClipboard(ps, 5, "Windows", "")
}

// Saves the Windows clipboard image into dest
func windowsSave(dest string) bool {
	ps := getPSExe()
	if ps == "" {
		cliLogger.Debug("No PowerShell found — Windows clipboard image paste unavailable")
		return false
	}
	return psClipboard(ps, 15, "Windows", dest)
}

// Saves through the first enabled Linux backend that succeeds
func linuxSave(dest string) bool {
	for _, b := range linuxClipBackends() {
		if b.enabled && b.save(dest) {
			return true
		}
	}
	return false
}

func wslHasImage() bool        { return psClipboard("powershell.exe", 8, "WSL", "") }
func wslSave(dest string) bool { return psClipboard("powershell.exe", 15, "WSL", dest) }

// Reports an image MIME type among the Wayland clipboard offers
func waylandHasImage() bool {
	return clipProbe(wlListTypes, 3, func(r clipResult) bool {
		if r.code != 0 {
			return false
		}
		for _, t := range pystr.Splitlines(r.stdout, false) {
			if strings.HasPrefix(t, "image/") {
				return true
			}
		}
		return false
	}, wlMissing)
}

// Saves the preferred Wayland image offer into dest, normalizing non PNG payloads to PNG
func waylandSave(dest string) bool {
	r, err := runClipText(wlListTypes, 3)
	if err != nil {
		if isMissingExe(err) {
			cliLogger.Debug(wlMissing)
		} else {
			cliLogger.Debug(fmt.Sprintf("wl-paste clipboard extraction failed: %v", err))
			_ = os.Remove(dest)
		}
		return false
	}
	var types []string
	if r.code == 0 {
		types = pystr.Splitlines(r.stdout, false)
	}
	mime := ""
	for _, m := range waylandMimePrefs {
		for _, t := range types {
			if t == m {
				mime = m
				break
			}
		}
		if mime != "" {
			break
		}
	}
	if mime == "" {
		return false
	}
	ok, err := pipeToFile([]string{"wl-paste", "--type", mime}, dest)
	if err != nil {
		if isMissingExe(err) {
			cliLogger.Debug(wlMissing)
		} else {
			cliLogger.Debug(fmt.Sprintf("wl-paste clipboard extraction failed: %v", err))
			_ = os.Remove(dest)
		}
		return false
	}
	if ok && (mime == "image/png" || (convertToPNG(dest) && isPNGFile(dest))) {
		return true
	}
	_ = os.Remove(dest)
	return false
}

// Converts an image to PNG in place through the decoders then ImageMagick, untouched when neither works
func convertToPNG(path string) bool {
	if err := decodeToPNG(path); err == nil {
		return true
	} else {
		cliLogger.Debug(fmt.Sprintf("Pillow BMP→PNG conversion failed: %v", err))
	}
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".bmp"
	err := func() error {
		if err := os.Rename(path, tmp); err != nil {
			return err
		}
		res, err := clipRun(nil, []string{"convert", tmp, "png:" + path}, pysubprocess.Options{Timeout: 5 * time.Second})
		if err != nil {
			return err
		}
		if res.Returncode == 0 && nonEmptyFile(path) {
			_ = os.Remove(tmp)
			return errClipFound
		}
		return os.Rename(tmp, path)
	}()
	if errors.Is(err, errClipFound) {
		return true
	}
	if err != nil {
		if isMissingExe(err) {
			cliLogger.Debug("ImageMagick not installed — cannot convert BMP to PNG")
		} else {
			cliLogger.Debug(fmt.Sprintf("ImageMagick BMP→PNG conversion failed: %v", err))
		}
		if _, tmpErr := os.Stat(tmp); tmpErr == nil {
			if _, pathErr := os.Stat(path); pathErr != nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	return nonEmptyFile(path)
}

// Decodes an image file and rewrites it as PNG
func decodeToPNG(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o666)
}

// Reports a file that starts with the PNG signature
func isPNGFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(pngSignature))
	n, _ := f.Read(head)
	return bytes.Equal(head[:n], pngSignature)
}

// Reports image/png among the X11 clipboard targets
func xclipHasImage() bool {
	return clipProbe(xclipTargets, 3, func(r clipResult) bool { return r.code == 0 && strings.Contains(r.stdout, "image/png") }, "")
}

// Saves the X11 clipboard PNG into dest through xclip
func xclipSave(dest string) bool {
	if !clipProbe(xclipTargets, 3, func(r clipResult) bool { return strings.Contains(r.stdout, "image/png") },
		"xclip not installed — X11 clipboard image paste unavailable") {
		return false
	}
	ok, err := pipeToFile([]string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}, dest)
	if err != nil {
		cliLogger.Debug(fmt.Sprintf("xclip image extraction failed: %v", err))
		_ = os.Remove(dest)
		return false
	}
	return ok
}
