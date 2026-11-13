package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
)

const maxImageBytes = 20 * 1024 * 1024

var (
	escapedPathChar = regexp.MustCompile(`\\([ ()'"\[\]])`)
	whitespaceRun   = regexp.MustCompile(`[` + jsWS + `]+`)
	gifSignature    = regexp.MustCompile(`^GIF8[79]a$`)
)

// Image file the turn attaches, its path and mime type
type imageAttachment struct {
	Path string
	Mime string
}

// Staged image with its file name and the text typed after its path
type stagedImage struct {
	imageAttachment
	Name      string
	Remainder string
}

// Mime type and extension of PNG, JPEG, GIF or WebP bytes
func imageFormat(data []byte) (string, string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{137, 80, 78, 71, 13, 10, 26, 10}):
		return "image/png", ".png", nil
	case len(data) >= 3 && data[0] == 255 && data[1] == 216 && data[2] == 255:
		return "image/jpeg", ".jpg", nil
	case len(data) >= 6 && gifSignature.Match(data[:6]):
		return "image/gif", ".gif", nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp", ".webp", nil
	}
	return "", "", errors.New(tr("canonical.images.unsupported"))
}

// Whether a rune ends a line for the dot of a JavaScript regex
func isJSLineTerminator(r rune) bool { return r == '\n' || r == '\r' || r == ' ' || r == ' ' }

// Path and remainder of a quoted path, the shortest quoted run followed by the end or whitespace
func quotedPath(value string) (string, string, bool) {
	if value == "" || (value[0] != '"' && value[0] != '\'') {
		return "", "", false
	}
	q := rune(value[0])
	for i, r := range value[1:] {
		if isJSLineTerminator(r) {
			return "", "", false
		}
		if r != q {
			continue
		}
		end := 1 + i
		rest := value[end+utf8.RuneLen(r):]
		if rest == "" {
			return value[1:end], "", true
		}
		if loc := whitespaceRun.FindStringIndex(rest); loc != nil && loc[0] == 0 {
			return value[1:end], rest[loc[1]:], true
		}
	}
	return "", "", false
}

// Local path of a file URL, an error for a URL Node would refuse
func fileURLToPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	encoded := strings.ToLower(u.EscapedPath())
	if runtime.GOOS == "windows" {
		if strings.Contains(encoded, "%2f") || strings.Contains(encoded, "%5c") {
			return "", errors.New(`File URL path must not include encoded \ or / characters`)
		}
		if u.Host != "" && u.Host != "localhost" {
			return `\\` + u.Host + filepath.FromSlash(u.Path), nil
		}
		return filepath.FromSlash(strings.TrimPrefix(u.Path, "/")), nil
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", errors.New(`File URL host must be "localhost" or empty on ` + runtime.GOOS)
	}
	if strings.Contains(encoded, "%2f") {
		return "", errors.New("File URL path must not include encoded / characters")
	}
	return u.Path, nil
}

// File an image argument names, its longest existing prefix when unquoted, and the text after it
func resolveImagePath(raw, base string) (string, string, error) {
	value := strings.TrimFunc(raw, isJSSpace)
	type candidate struct{ path, remainder string }
	var candidates []candidate
	if path, remainder, quoted := quotedPath(value); quoted {
		candidates = []candidate{{path, remainder}}
	} else {
		candidates = []candidate{{value, ""}}
		runs := whitespaceRun.FindAllStringIndex(value, -1)
		for i := len(runs) - 1; i >= 0; i-- {
			at := runs[i][0]
			candidates = append(candidates, candidate{value[:at], strings.TrimFunc(value[at:], isJSSpace)})
		}
	}
	for _, c := range candidates {
		path := escapedPathChar.ReplaceAllString(c.path, "$1")
		if strings.HasPrefix(path, "file://") {
			local, err := fileURLToPath(path)
			if err != nil {
				return "", "", err
			}
			path = local
		}
		if strings.HasPrefix(path, "~/") {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return "", "", err
			}
			path = filepath.Join(userHome, path[2:])
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return "", "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				continue
			}
			return "", "", err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > maxImageBytes {
			return "", "", errors.New(tr("canonical.images.tooLarge"))
		}
		return path, c.remainder, nil
	}
	return "", "", errors.New(tr("canonical.images.notFound", value))
}

// Folder a typed path is relative to, the session's, else the one the TUI was launched from
func (s *tuiStore) sessionCwd() string {
	if truthyJS(s.ui.Info["cwd"]) {
		return jsValueString(s.ui.Info["cwd"])
	}
	return os.Getenv("ICEBATH_CWD")
}

// Stages an image for the destination session, uploaded to a remote owner or copied into the profile image cache
func (s *tuiStore) stageImagePath(raw string, gw turnGateway, destination submissionDestination, then func(*stagedImage, error) tea.Cmd) tea.Cmd {
	remote := strings.TrimFunc(os.Getenv("ICEBATH_TUI_GATEWAY_URL"), isJSSpace) != ""
	base := s.sessionCwd()
	if remote {
		base = os.Getenv("ICEBATH_CWD")
	}
	return func() tea.Msg {
		staged, err := stageImageFile(raw, base, remote, gw, destination)
		return asyncDoneMsg{run: func() tea.Cmd { return then(staged, err) }}
	}
}

// Staging work of an image path, run off the update loop
func stageImageFile(raw, base string, remote bool, gw turnGateway, destination submissionDestination) (*stagedImage, error) {
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		base = cwd
	}
	path, remainder, err := resolveImagePath(raw, base)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBytes {
		return nil, errors.New(tr("canonical.images.tooLarge"))
	}
	mime, ext, err := imageFormat(data)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if remote {
		result, err := gw.request(context.Background(), "image.attach_bytes", map[string]any{"session_id": destination.Sid, "filename": uuid.NewString() + ext, "content_base64": base64.StdEncoding.EncodeToString(data)}, 0)
		if err != nil {
			return nil, err
		}
		owned, _ := asRPCResult(result)["path"].(string)
		if owned == "" {
			return nil, errors.New(tr("canonical.images.noOwnerPath"))
		}
		return &stagedImage{imageAttachment: imageAttachment{Path: owned, Mime: mime}, Name: name, Remainder: remainder}, nil
	}
	directory := filepath.Join(destination.ProfileHome, "cache", "images")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	target := filepath.Join(directory, uuid.NewString()+ext)
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, werr := file.Write(data)
	if werr == nil {
		werr = file.Sync()
	}
	if cerr := file.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, werr
	}
	return &stagedImage{imageAttachment: imageAttachment{Path: target, Mime: mime}, Name: name, Remainder: remainder}, nil
}
