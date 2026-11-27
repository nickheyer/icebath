package generation

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Refusal of a source a service reads, worded for the model
type Refusal struct{ Text string }

func (e *Refusal) Error() string { return e.Text }

// Refusal of a URL whose host is private or internal
var ErrUnsafe = errors.New("URL failed the SSRF safety check")

// Whether the source is a URL or a data URL rather than a path
func Remote(source string) bool {
	lower := strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:")
}

// Data URL of the bytes
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// Bytes and declared type of a base64 data URL
func Inline(source string) ([]byte, string, error) {
	header, payload, ok := strings.Cut(strings.TrimSpace(source), ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") {
		return nil, "", &Refusal{"Image data URL is missing a comma separator"}
	}
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return nil, "", &Refusal{"data: URL must be base64-encoded"}
	}
	declared, _, _ := strings.Cut(header[len("data:"):], ";")
	data, err := Base64(payload)
	if err != nil {
		return nil, "", &Refusal{"invalid base64 in data: URL: " + err.Error()}
	}
	return data, strings.ToLower(strings.TrimSpace(declared)), nil
}

// Type of an image from its first bytes, empty when it is none of png, jpeg, gif, webp, bmp, tiff, avif, heic or svg
func Sniff(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(data, []byte("BM")):
		return "image/bmp"
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return "image/tiff"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		switch string(data[8:12]) {
		case "avif", "avis":
			return "image/avif"
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return "image/heic"
		}
	case bytes.Contains(bytes.ToLower(data[:min(len(data), 4096)]), []byte("<svg")):
		return "image/svg+xml"
	}
	return ""
}

// Type a file's name gives, empty when it gives none
func TypeOf(name string) string {
	kind, _, _ := strings.Cut(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))), ";")
	return strings.TrimSpace(kind)
}

// Bytes of a file of this machine a call names, ~ expanded, refused when it is one of icebath's credential stores, token folders or an env
// file, or no regular file
func (j *Job) Local(path string) ([]byte, error) {
	path = fsx.ExpandHome(strings.TrimSpace(path))
	if refusal := Unreadable(j.Config, path); refusal != "" {
		return nil, &Refusal{refusal}
	}
	if !fsx.IsFile(path) {
		return nil, &Refusal{"Image input path does not exist or is not a file: " + path}
	}
	return os.ReadFile(path)
}

// Bytes at an http(s) URL whose host passes the private address rules, redirects checked the same way, at most max bytes
func (j *Job) Fetch(ctx context.Context, address string, timeout time.Duration, max int64) ([]byte, error) {
	if !web.SafeURL(j.Config, address) {
		return nil, ErrUnsafe
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := web.Client(j.Config, timeout, true).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		return nil, Status(response.StatusCode, response.Request.URL.String())
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("Image URL exceeds %dMB cap: %s", max>>20, address)
	}
	return data, nil
}

// Bytes and file name of a source of any form: a URL fetched under the private address rules, a data URL, or a file of this machine
func (j *Job) Read(ctx context.Context, source string) ([]byte, string, error) {
	source = strings.TrimSpace(source)
	lower := strings.ToLower(source)
	switch {
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		data, err := j.Fetch(ctx, source, 60*time.Second, maxSource)
		if errors.Is(err, ErrUnsafe) {
			return nil, "", &Refusal{"Image reference URL failed the SSRF safety check: " + source}
		}
		if err != nil {
			return nil, "", err
		}
		at, _, _ := strings.Cut(source, "?")
		return data, cmp.Or(at[strings.LastIndex(at, "/")+1:], "image.png"), nil
	case strings.HasPrefix(lower, "data:"):
		data, declared, err := Inline(source)
		if err != nil {
			return nil, "", err
		}
		_, extension, _ := strings.Cut(declared, "/")
		return data, "image." + cmp.Or(extension, "png"), nil
	}
	data, err := j.Local(source)
	if err != nil {
		return nil, "", err
	}
	name := filepath.Base(source)
	if name == "." || name == string(filepath.Separator) {
		name = "image.png"
	}
	return data, name, nil
}

// Most bytes of a source read whole
const maxSource = 50 << 20

const defenseInDepth = " (Defense-in-depth — not a security boundary; the terminal tool can still bypass.)"

// Folders of icebath no source is read from, with what a read of the folder and of a file in it is told
var unreadable = []struct{ dir, folder, file string }{
	{"mcp-tokens", "is the icebath MCP token directory and cannot be read directly.", "is an icebath MCP token file and cannot be read directly."},
	{"browser-profile", "is the icebath real-profile browser snapshot directory (copied cookies/logins) and cannot be read directly.",
		"is inside the icebath real-profile browser snapshot (copied cookies/logins) and cannot be read directly."},
	{"vault", "is the icebath credential vault directory and cannot be read directly (secrets are filled server-side by browser_vault).",
		"is inside the icebath credential vault (encrypted secrets + local key) and cannot be read directly (browser_vault resolves them server-side)."},
	{"webapp", "is the icebath Webapp sign-in directory and cannot be read directly.",
		"is an icebath Webapp sign-in file (the browser credential or its sign-in records) and cannot be read directly."},
}

// Why a source file of this machine may not be read, empty when it may: icebath's credential stores, token folders and skills hub
// cache, and env files
func Unreadable(cfg *config.Config, raw string) string {
	p := canonical(raw)
	dirs := []string{canonical(cfg.ProfileDir(cfg.Profile()))}
	if root := canonical(cfg.Dir()); root != dirs[0] {
		dirs = append(dirs, root)
	}
	reason := ""
	for _, dir := range dirs {
		if fsx.Within(p, filepath.Join(dir, "skills", ".hub")) {
			reason = "is an internal icebath cache file and cannot be read directly to prevent prompt injection. Use the skills_list or skill_view tools instead."
		}
		for _, name := range terminal.CredentialStores {
			if reason == "" && p == canonical(filepath.Join(dir, filepath.FromSlash(name))) {
				reason = "is an icebath credential store and cannot be read directly. Provider tools consume these credentials through internal channels." +
					defenseInDepth
			}
		}
		for _, denied := range unreadable {
			blocked := canonical(filepath.Join(dir, denied.dir))
			if reason == "" && fsx.Within(p, blocked) {
				reason = denied.file + defenseInDepth
				if p == blocked {
					reason = denied.folder + defenseInDepth
				}
			}
		}
	}
	if reason == "" && redact.EnvFiles[strings.ToLower(filepath.Base(p))] {
		reason = "is a secret-bearing environment file and cannot be read to prevent credential leakage. If you need to check the file structure, read " +
			".env.example instead." + defenseInDepth
	}
	if reason == "" {
		return ""
	}
	return "Access denied: " + raw + " " + reason
}

// Path made absolute with its links resolved as far as they exist
func canonical(p string) string {
	abs, err := filepath.Abs(fsx.ExpandHome(p))
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir, rest := abs, ""
	for dir != filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		dir, rest = filepath.Dir(dir), filepath.Join(filepath.Base(dir), rest)
	}
	return abs
}
