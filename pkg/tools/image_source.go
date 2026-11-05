package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Raw bytes ingest budget, the 50MB download cap rather than the 20MB provider payload cap
const maxImageIngestBytes = 50 * 1024 * 1024

// Failure classes of the media source resolver
const (
	ImageUnsupportedScheme = "unsupported_scheme"
	ImageSourceUnsafe      = "source_unsafe"
	ImageSourceTooLarge    = "source_too_large"
	ImageSourceNotFound    = "source_not_found"
	ImageNotAnImage        = "not_an_image"
)

// ImageResolutionError is one refused media source with its failure class, source, and origin
type ImageResolutionError struct {
	Kind    string
	Message string
	Src     string
	Origin  string
}

// Error returns the user facing refusal text
func (e *ImageResolutionError) Error() string { return e.Message }

// Builds a resolver error of one failure class
func imageResolutionErr(kind, message, src, origin string) *ImageResolutionError {
	return &ImageResolutionError{Kind: kind, Message: message, Src: src, Origin: origin}
}

// ResolvedImage is media bytes with the verified MIME type and where they came from
type ResolvedImage struct {
	Data []byte
	Mime string
	// One of data, http, file, local, container
	Origin string
}

// Explicit URL scheme such as ftp or s3 with a double slash, bare Windows drive paths lack it
var imageSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*://`)

// Host media caches, the only host paths vision may read under a non local backend
var mediaCacheSubdirs = []string{
	"cache", "images", "image_cache", "audio_cache", "video_cache", "temp_vision_images", "temp_video_files",
}

// Lazy sandbox bring up before an in sandbox read, replaced in tests
var imageSourceEnsureEnv = func(ctx context.Context, env *Env, taskID string) {
	if _, err := EnsureTaskEnv(ctx, env, taskID); err != nil {
		envLog(env).Debug("vision: sandbox bring-up failed", "task_id", taskID, "error", err)
	}
}

// Active sandbox environment of a task or nil, replaced in tests
var imageSourceActiveEnv = func(env *Env, taskID string) Environment {
	if taskID == "" {
		return nil
	}
	e, err := GetActiveEnv(env, taskID)
	if err != nil {
		return nil
	}
	return e
}

// Delay between the two in sandbox read attempts that covers Docker exec warm up
var imageSourceRetryDelay = 150 * time.Millisecond

// Reports whether a permitted kind list allows this kind, an empty list meaning images only
func permitsKind(permitted []string, kind string) bool {
	if len(permitted) == 0 {
		return kind == "image"
	}
	for _, p := range permitted {
		if p == kind {
			return true
		}
	}
	return false
}

// ResolveImageSource turns a data URL, http URL, file URI, or path into verified media bytes
func ResolveImageSource(ctx context.Context, env *Env, src, taskID string, permitted ...string) (*ResolvedImage, error) {
	s := strings.TrimSpace(src)
	if s == "" {
		return nil, imageResolutionErr(ImageSourceNotFound, "image_url is required", src, "")
	}
	if strings.HasPrefix(s, "data:") {
		data, err := resolveImageDataURL(s)
		if err != nil {
			return nil, err
		}
		return finalizeImageSource(data, "data", s, permitted)
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		if reason := imageHTTPBlockReason(env, s); reason != "" {
			return nil, imageResolutionErr(ImageSourceUnsafe, reason, s, "")
		}
		data, err := downloadImageToBytes(ctx, env, s)
		if err != nil {
			return nil, err
		}
		return finalizeImageSource(data, "http", s, permitted)
	}
	if imageSchemeRe.MatchString(s) && !strings.HasPrefix(strings.ToLower(s), "file://") {
		return nil, imageResolutionErr(ImageUnsupportedScheme,
			"Unrecognized image source scheme. Use an http(s) URL, a local file path, a file:// URI, or a data: URL.", s, "")
	}
	candidate := s
	if strings.HasPrefix(strings.ToLower(s), "file://") {
		candidate = s[len("file://"):]
	}
	p := pyExpandUser(candidate)
	local, err := isLocalTerminalBackend(env)
	if err != nil {
		return nil, err
	}
	if target := permittedHostReadTarget(env, p, local); target != "" && isRegularFile(target) {
		if blocked := readBlockError(env, target); blocked != "" {
			return nil, imageResolutionErr(ImageSourceUnsafe, blocked, s, "file")
		}
		data, err := readIngestFile(target, s)
		if err != nil {
			return nil, err
		}
		return finalizeImageSource(data, "file", s, permitted)
	}
	if local {
		return nil, imageResolutionErr(ImageSourceNotFound, fmt.Sprintf("media file not found: '%s'", filepath.Clean(p)), s, "file")
	}
	return resolveContainerImageSource(ctx, env, filepath.Clean(p), taskID, s, permitted)
}

// Reads a host file, refusing anything over the ingest cap before loading it
func readIngestFile(path, src string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxImageIngestBytes {
		return nil, imageResolutionErr(ImageSourceTooLarge, "media exceeds size limit", src, "file")
	}
	return os.ReadFile(path)
}

// Expands a leading tilde the way os.path.expanduser does, HOME first then the user database
func pyExpandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	rest := p[1:]
	name, tail := rest, ""
	if i := strings.IndexAny(rest, `/\`); i >= 0 {
		name, tail = rest[:i], rest[i:]
	}
	var h string
	if name == "" {
		h = os.Getenv("HOME")
		if h == "" {
			if u, err := user.Current(); err == nil {
				h = u.HomeDir
			}
		}
	} else if u, err := user.Lookup(name); err == nil {
		h = u.HomeDir
	}
	if h == "" {
		return p
	}
	if tail == "" {
		return h
	}
	return strings.TrimRight(h, `/\`) + tail
}

// Decodes a base64 data URL, the real MIME verified later from magic bytes
func resolveImageDataURL(s string) ([]byte, error) {
	header, payload, _ := strings.Cut(s, ",")
	if !strings.Contains(header, ";base64") {
		return nil, imageResolutionErr(ImageNotAnImage, "data: URL must be base64-encoded", clipRunes(s, 64), "")
	}
	if (len(payload)*3)/4 > maxImageIngestBytes {
		return nil, imageResolutionErr(ImageSourceTooLarge, "data: URL exceeds size limit", clipRunes(s, 64), "")
	}
	data, err := strictB64Decode(payload)
	if err != nil {
		return nil, imageResolutionErr(ImageNotAnImage, "invalid base64 in data: URL: "+err.Error(), clipRunes(s, 64), "")
	}
	return data, nil
}

// Strict base64 decode that rejects any character outside the alphabet, newlines included
func strictB64Decode(s string) ([]byte, error) {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return nil, fmt.Errorf("Only base64 data is allowed (non-alphabet character at offset %d)", i)
	}
	return base64.StdEncoding.DecodeString(s)
}

// First n runes of a string
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Block reason for an http source before any network IO, empty when allowed
func imageHTTPBlockReason(env *Env, rawURL string) string {
	if !isSafeURL(env, rawURL) {
		return "blocked: unsafe or private URL"
	}
	if blocked := checkWebsiteAccess(env, rawURL); blocked != nil {
		if blocked.Message != "" {
			return blocked.Message
		}
		return "blocked by website policy"
	}
	return ""
}

// Downloads an http source through the guarded vision downloader into memory
func downloadImageToBytes(ctx context.Context, env *Env, rawURL string) ([]byte, error) {
	tf, err := os.CreateTemp("", "*.img")
	if err != nil {
		return nil, err
	}
	tmp := tf.Name()
	tf.Close()
	defer os.Remove(tmp)
	if err := downloadImage(ctx, env, rawURL, tmp, 3); err != nil {
		var policy *mediaPolicyError
		if errors.As(err, &policy) {
			return nil, imageResolutionErr(ImageSourceUnsafe, policy.Error(), rawURL, "http")
		}
		return nil, err
	}
	return os.ReadFile(tmp)
}

// Reports whether the terminal backend runs on the host through the per turn terminal scope
func isLocalTerminalBackend(env *Env) (bool, error) {
	policy, err := terminalPolicy(env)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(policy.Get("TERMINAL_ENV", "local"))) {
	case "local", "":
		return true, nil
	}
	return false, nil
}

// Media cache roots under the session home
func mediaCacheRoots(env *Env) []string {
	h := envHome(env)
	out := make([]string, 0, len(mediaCacheSubdirs))
	for _, sub := range mediaCacheSubdirs {
		out = append(out, filepath.Join(h, sub))
	}
	return out
}

// Host path to read, or empty when the caller must exec read inside the sandbox
func permittedHostReadTarget(env *Env, p string, local bool) string {
	if local {
		return resolveAgainstCwd(env, p)
	}
	real := resolveAgainstCwd(env, FromAgentVisibleCachePath(env, p, ""))
	for _, root := range mediaCacheRoots(env) {
		if isUnder(real, resolveNonStrict(root)) {
			return real
		}
	}
	return ""
}

// Resolves a path the way Path.resolve does, relative parts against the launch directory
func resolveAgainstCwd(env *Env, p string) string {
	if !filepath.IsAbs(p) {
		base := ""
		if env != nil && env.Cwd != "" {
			base = env.Cwd
		} else if wd, err := os.Getwd(); err == nil {
			base = wd
		}
		p = filepath.Join(base, p)
	}
	return resolveNonStrict(p)
}

// Reads the bytes inside the sandbox, failing closed when no sandbox session exists
func resolveContainerImageSource(ctx context.Context, env *Env, p, taskID, src string, permitted []string) (*ResolvedImage, error) {
	if taskID != "" {
		imageSourceEnsureEnv(ctx, env, taskID)
	}
	sandbox := imageSourceActiveEnv(env, taskID)
	if sandbox == nil {
		return nil, imageResolutionErr(ImageSourceNotFound,
			fmt.Sprintf("'%s' is not reachable inside the sandbox and no active sandbox session is available to read it", p), src, "container")
	}
	mount := sandbox.HostCwdMount()
	if mount == "" {
		mount = "/workspace"
	}
	if translated := translateMountedHostPath(p, sandbox.HostCwd(), mount); translated != "" {
		p = translated
	}
	cmd := fmt.Sprintf("head -c %d < %s | base64 | tr -d '\\n'", maxImageIngestBytes+1, shQuote(p))
	var last *ExecResult
	for attempt := 0; attempt < 2; attempt++ {
		res, err := sandbox.Execute(ctx, cmd, ExecOptions{})
		if err != nil {
			return nil, err
		}
		last = res
		if res != nil && res.ReturnCode == 0 {
			break
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(imageSourceRetryDelay):
			}
		}
	}
	if last == nil || last.ReturnCode != 0 {
		suffix := ""
		if last != nil {
			for _, ln := range strings.Split(strings.TrimSpace(last.Output), "\n") {
				if first := strings.TrimSpace(ln); first != "" {
					suffix = " (" + clipRunes(first, 200) + ")"
					break
				}
			}
		}
		return nil, imageResolutionErr(ImageSourceNotFound, fmt.Sprintf("could not read '%s' inside the sandbox%s", p, suffix), src, "container")
	}
	data, err := strictB64Decode(last.Output)
	if err != nil {
		return nil, imageResolutionErr(ImageNotAnImage, fmt.Sprintf("sandbox returned non-image data for '%s': %v", p, err), src, "")
	}
	if len(data) > maxImageIngestBytes {
		return nil, imageResolutionErr(ImageSourceTooLarge, "media exceeds size limit", src, "container")
	}
	return finalizeImageSource(data, "container", src, permitted)
}

// Applies the ingest cap and type check, images by magic bytes and video by extension or mp4 sniff
func finalizeImageSource(data []byte, origin, src string, permitted []string) (*ResolvedImage, error) {
	if len(data) > maxImageIngestBytes {
		return nil, imageResolutionErr(ImageSourceTooLarge, "media exceeds size limit", src, origin)
	}
	if sniffed := detectImageMimeTypeFromBytes(data); sniffed != "" {
		if !permitsKind(permitted, "image") {
			return nil, imageResolutionErr(ImageNotAnImage, "source is an image, but this argument takes a video", src, origin)
		}
		return &ResolvedImage{Data: data, Mime: sniffed, Origin: origin}, nil
	}
	if permitsKind(permitted, "image") && bytes.Contains(bytes.ToLower(data[:min(len(data), 4096)]), []byte("<svg")) {
		return &ResolvedImage{Data: data, Mime: "image/svg+xml", Origin: origin}, nil
	}
	if permitsKind(permitted, "video") {
		if videoMime := detectSourceVideoMime(data, src); videoMime != "" {
			return &ResolvedImage{Data: data, Mime: videoMime, Origin: origin}, nil
		}
		return nil, imageResolutionErr(ImageNotAnImage, "source is not a recognized video (mp4 expected)", src, origin)
	}
	return nil, imageResolutionErr(ImageNotAnImage, "source is not a recognized image", src, origin)
}

// Video MIME from the extension table, else the ISO base media ftyp box at offset 4
func detectSourceVideoMime(data []byte, src string) string {
	pathPart := src
	if imageSchemeRe.MatchString(src) {
		if u, err := url.Parse(src); err == nil {
			pathPart = u.Path
		} else {
			pathPart = splitURL(src).path
		}
	}
	if byExt := detectVideoMimeType(pathPart); byExt != "" {
		return byExt
	}
	if len(data) > 12 && bytes.Equal(data[4:8], []byte("ftyp")) {
		return "video/mp4"
	}
	return ""
}

// ResolveLocalSourceToDataURL inlines a path like media source as a data URL, URLs pass through
func ResolveLocalSourceToDataURL(ctx context.Context, env *Env, src, taskID string, permitted ...string) (string, error) {
	s := strings.TrimSpace(src)
	l := strings.ToLower(s)
	if s == "" || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "data:") {
		return src, nil
	}
	resolved, err := ResolveImageSource(ctx, env, s, taskID, permitted...)
	if err != nil {
		return "", err
	}
	mime := resolved.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(resolved.Data), nil
}
