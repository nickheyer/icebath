package storage

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/desktop/nodehttp"
)

// Scheme the renderer streams local and gateway media through
const MediaProtocol = "hermes-media"

var (
	streamableMediaExtensions = []string{".avi", ".flac", ".m4a", ".mkv", ".mov", ".mp3", ".mp4", ".ogg", ".opus", ".wav", ".webm"}
	forwardedMediaHeaders     = []string{"accept", "if-modified-since", "if-none-match", "if-range", "range"}
	mediaMime                 = map[string]string{
		".aac": "audio/aac", ".avi": "video/x-msvideo", ".flac": "audio/flac", ".m4a": "audio/mp4", ".m4v": "video/mp4",
		".mkv": "video/x-matroska", ".mov": "video/quicktime", ".mp3": "audio/mpeg", ".mp4": "video/mp4", ".oga": "audio/ogg",
		".ogg": "audio/ogg", ".ogv": "video/ogg", ".opus": "audio/ogg", ".wav": "audio/wav", ".webm": "video/webm",
	}
	jsSpace         = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*`
	byteRangeHeader = regexp.MustCompile(`(?i)^` + jsSpace + `bytes` + jsSpace + `=` + jsSpace + `([0-9]*)` + jsSpace + `-` + jsSpace + `([0-9]*)` + jsSpace + `$`)
	drivePath       = regexp.MustCompile(`(?i)^/[a-z]:`)
	encodedDotSeg   = regexp.MustCompile(`(?i)^(?:\.|%2e){1,2}$`)
)

// Response a media protocol request answers with, Body nil for an empty body
type MediaResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

func mediaText(status int, text string, header http.Header) *MediaResponse {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "text/plain;charset=UTF-8")
	return &MediaResponse{Status: status, Header: header, Body: io.NopCloser(strings.NewReader(text))}
}

// Gateway a remote media request is proxied to
type MediaRemoteConnection struct {
	AuthMode     string
	BaseURL      string
	Headers      map[string]string
	Mode         string
	Token        *string
	SharedRemote bool
}

// Remote half of the media protocol, the backend and connections facilities a gateway stream needs
type MediaRemote interface {
	// Gateway that owns the scope, joining any dial already in flight for it
	ResolveRemoteConnection(connectionID, profile string) (MediaRemoteConnection, error)
	// Fetch without cookies
	FetchRemote(url string, header http.Header, method string) (*MediaResponse, error)
	// Fetch with the OAuth session cookies of the gateway
	FetchRemoteWithCookies(url string, header http.Header, method string) (*MediaResponse, error)
	// Native bearer first and the cookie session second, as requestWithOauthFallback runs them
	RequestWithOauthFallback(baseURL string, withBearer func(bearer string) (*MediaResponse, error), withCookie func() (*MediaResponse, error)) (*MediaResponse, error)
}

type mediaTarget struct {
	connectionID string
	filePath     string
	mode         string
	profile      string
}

// Mode, decoded path and scope of a hermes-media URL
func parseMediaProtocolTarget(raw string) (mediaTarget, error) {
	u, err := nodehttp.ParseURL(raw)
	if err != nil {
		return mediaTarget{}, err
	}
	mode := u.Hostname()
	if mode != "remote" && mode != "stream" {
		return mediaTarget{}, errors.New("Unsupported media protocol target")
	}
	filePath, err := nodehttp.DecodeURIComponent(strings.TrimLeft(u.Pathname(), "/"))
	if err != nil {
		return mediaTarget{}, err
	}
	if filePath == "" {
		return mediaTarget{}, errors.New("Missing media path")
	}
	q := u.SearchParams()
	return mediaTarget{connectionID: jsTrim(q.Get("connectionId")), filePath: filePath, mode: mode, profile: jsTrim(q.Get("profile"))}, nil
}

// File name a native save of a hermes-media URL offers, empty for any other URL
func MediaProtocolDownloadName(raw string) string {
	u, err := nodehttp.ParseURL(raw)
	if err != nil || u.Protocol() != MediaProtocol+":" {
		return ""
	}
	t, err := parseMediaProtocolTarget(raw)
	if err != nil {
		return ""
	}
	parts := strings.FieldsFunc(t.filePath, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// Native path of a file URL as the shared fileUrlToNativePath reads it, false when it encodes another path
func fileURLToNativePath(value string) (string, bool) {
	u, err := nodehttp.ParseURL(value)
	if err != nil || u.Protocol() != "file:" || strings.Contains(strings.ToLower(u.Pathname()), "%2f") {
		return "", false
	}
	p, err := nodehttp.DecodeURIComponent(u.Pathname())
	if err != nil {
		return "", false
	}
	host := u.Hostname()
	windows := host != "" || drivePath.MatchString(p)
	sep := func(r rune) bool { return r == '/' }
	if windows {
		sep = func(r rune) bool { return r == '/' || r == '\\' }
	}
	lower := strings.ToLower(p)
	if strings.Contains(lower, "%2f") || (windows && strings.Contains(lower, "%5c")) {
		return "", false
	}
	for _, seg := range strings.FieldsFunc(p, sep) {
		if encodedDotSeg.MatchString(seg) {
			return "", false
		}
	}
	if !windows {
		return p, true
	}
	if strings.Contains(strings.ToLower(u.Pathname()), "%5c") {
		return "", false
	}
	out := p[1:]
	if host != "" {
		out = "//" + host + p
	}
	return strings.ReplaceAll(out, "/", `\`), true
}

// Native path of a media file URL, a bare percent sign kept literal as the gateway reads it
func mediaFileURLPath(fileURL string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(fileURL); i++ {
		if fileURL[i] == '%' && !(i+2 < len(fileURL) && isHexByte(fileURL[i+1]) && isHexByte(fileURL[i+2])) {
			b.WriteString("%25")
			continue
		}
		b.WriteByte(fileURL[i])
	}
	return fileURLToNativePath(b.String())
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hasFileScheme(s string) bool { return len(s) >= 5 && strings.EqualFold(s[:5], "file:") }

// True when the path or file URL names one of the streamable media extensions
func IsStreamableMediaPath(filePath string) bool {
	mediaPath := filePath
	if hasFileScheme(filePath) {
		p, ok := mediaFileURLPath(filePath)
		if !ok {
			return false
		}
		mediaPath = p
	}
	lower := jsLower(mediaPath)
	for _, ext := range streamableMediaExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Range and cache negotiation headers of a media request that travel to the source
func mediaRequestHeaders(source http.Header) http.Header {
	out := http.Header{}
	for _, name := range forwardedMediaHeaders {
		if v := strings.Join(source.Values(name), ", "); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

// Gateway stream URL of a media path, POSIX file URLs sent as native paths
func remoteMediaEndpoint(baseURL, filePath, profile string) (string, error) {
	u, err := nodehttp.ParseURL(strings.TrimRight(baseURL, "/") + "/api/files/stream")
	if err != nil {
		return "", err
	}
	if p := u.Protocol(); p != "http:" && p != "https:" {
		return "", fmt.Errorf("Unsupported Icebath backend URL protocol: %s", p)
	}
	if hasFileScheme(filePath) {
		native, ok := mediaFileURLPath(filePath)
		if !ok {
			return "", errors.New("Invalid media file URL")
		}
		if strings.HasPrefix(native, "/") {
			filePath = native
		}
	}
	q := u.SearchParams()
	q.Set("path", filePath)
	if profile != "" {
		q.Set("profile", profile)
	}
	return u.String(), nil
}

// Media type of a path by the media extension table, octet stream otherwise
func mediaMimeFor(filePath string) string {
	if m, ok := mediaMime[jsLower(nodeExtname(filePath))]; ok {
		return m
	}
	return "application/octet-stream"
}

// Byte range of a single range header, ok false without one and unsatisfiable when it cannot be served
func parseByteRange(header string, size int64) (start, end int64, ok, unsatisfiable bool) {
	if header == "" || strings.Contains(header, ",") {
		return 0, 0, false, false
	}
	m := byteRangeHeader.FindStringSubmatch(header)
	if m == nil || (m[1] == "" && m[2] == "") {
		return 0, 0, false, false
	}
	if size <= 0 {
		return 0, 0, false, true
	}
	num := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	fsize := float64(size)
	var fs, fe float64
	if m[1] == "" {
		suffix := min(num(m[2]), fsize)
		if suffix <= 0 {
			return 0, 0, false, true
		}
		fs, fe = fsize-suffix, fsize-1
	} else {
		fs = num(m[1])
		fe = fsize - 1
		if m[2] != "" {
			fe = min(num(m[2]), fsize-1)
		}
	}
	if fs >= fsize || fs > fe {
		return 0, 0, false, true
	}
	return int64(fs), int64(fe), true, false
}

// Range aware response for a local media file, the body streamed from the handle it was sized from
func buildLocalMediaResponse(resolvedPath, method, rangeHeader string) (*MediaResponse, error) {
	f, err := os.Open(resolvedPath)
	if err != nil {
		if code := hardening.NodeCode(err); code == "ENOENT" || code == "ENOTDIR" {
			return mediaText(404, "Media not found", nil), nil
		}
		return nil, hardening.NodeErr(err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, hardening.NodeErr(err)
	}
	size := st.Size()
	head := strings.EqualFold(method, "HEAD")
	h := http.Header{}
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", mediaMimeFor(resolvedPath))
	h.Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	start, end, ok, unsatisfiable := parseByteRange(rangeHeader, size)
	if unsatisfiable {
		f.Close()
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		return &MediaResponse{Status: 416, Header: h}, nil
	}
	if !ok {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		if head || size == 0 {
			f.Close()
			return &MediaResponse{Status: 200, Header: h}, nil
		}
		return &MediaResponse{Status: 200, Header: h, Body: f}, nil
	}
	length := end - start + 1
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	if head {
		f.Close()
		return &MediaResponse{Status: 206, Header: h}, nil
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, hardening.NodeErr(err)
	}
	return &MediaResponse{Status: 206, Header: h, Body: struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, length), f}}, nil
}

// Answers a hermes-media request, local streams from disk and remote ones through the gateway that owns them
func (s *Service) HandleMediaRequest(remote MediaRemote, method, rawURL string, reqHeader http.Header) *MediaResponse {
	if method != "GET" && method != "HEAD" {
		return mediaText(405, "Method not allowed", http.Header{"Allow": {"GET, HEAD"}})
	}
	t, err := parseMediaProtocolTarget(rawURL)
	if err != nil {
		return mediaText(404, "Media not found", nil)
	}
	if !IsStreamableMediaPath(t.filePath) {
		return mediaText(415, "Unsupported media type", nil)
	}
	header := mediaRequestHeaders(reqHeader)
	if t.mode == "stream" {
		rf, err := hardening.ResolveReadableFileForIpc(files.ResolveMediaStreamFile(t.filePath), hardening.ReadableFileOptions{Purpose: "Media stream"})
		if err != nil {
			return mediaText(404, "Media not found", nil)
		}
		if !IsStreamableMediaPath(rf.ResolvedPath) {
			return mediaText(415, "Unsupported media type", nil)
		}
		res, err := buildLocalMediaResponse(rf.ResolvedPath, method, header.Get("range"))
		if err != nil {
			return mediaText(404, "Media not found", nil)
		}
		return res
	}
	res, err := remoteMedia(remote, t, header, method)
	if err != nil {
		status := 502
		var he *backend.HTTPError
		if errors.As(err, &he) && (he.StatusCode == 401 || he.StatusCode == 403) {
			status = he.StatusCode
		}
		return mediaText(status, "Remote media unavailable", nil)
	}
	return res
}

// Proxies a remote media request with the gateway headers and its token or OAuth credentials
func remoteMedia(remote MediaRemote, t mediaTarget, header http.Header, method string) (*MediaResponse, error) {
	conn, err := remote.ResolveRemoteConnection(t.connectionID, t.profile)
	if err != nil {
		return nil, err
	}
	if conn.Mode != "remote" {
		return mediaText(404, "Remote media backend unavailable", nil), nil
	}
	profile := ""
	if conn.SharedRemote {
		profile = t.profile
	}
	endpoint, err := remoteMediaEndpoint(conn.BaseURL, t.filePath, profile)
	if err != nil {
		return nil, err
	}
	for name, value := range conn.Headers {
		if len(header.Values(name)) == 0 {
			header.Set(name, value)
		}
	}
	if conn.AuthMode == "oauth" {
		return remote.RequestWithOauthFallback(conn.BaseURL, func(bearer string) (*MediaResponse, error) {
			header.Set("authorization", "Bearer "+bearer)
			return remote.FetchRemote(endpoint, header, method)
		}, func() (*MediaResponse, error) {
			res, err := remote.FetchRemoteWithCookies(endpoint, header, method)
			if err != nil {
				return nil, err
			}
			if res.Status == 401 || res.Status == 403 {
				if res.Body != nil {
					res.Body.Close()
				}
				return nil, &backend.HTTPError{StatusCode: res.Status, Message: "Remote media authentication unavailable"}
			}
			return res, nil
		})
	}
	if conn.Token == nil || *conn.Token == "" {
		return mediaText(401, "Remote media authentication unavailable", nil), nil
	}
	header.Set("x-icebath-session-token", *conn.Token)
	return remote.FetchRemote(endpoint, header, method)
}
