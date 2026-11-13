package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/files"
	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	"github.com/nickheyer/icebath/pkg/desktop/nodehttp"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

// Connection and body idle deadline of a gateway download
const gatewayFetchTimeout = 30 * time.Second

// Download pool, isolated from the JSON pool so large bodies never block interactive calls
var downloadAgent = nodehttp.NewAgent(8)

var (
	gatewayDataURLPattern   = regexp.MustCompile(`(?s)^data:([^,]*),(.*)$`)
	dispositionEncoded      = regexp.MustCompile(`(?i)filename\*=(?:UTF-8'')?([^;]+)`)
	dispositionPlain        = regexp.MustCompile(`(?i)filename="?([^";]+)"?`)
	saveFilterExtension     = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	allFilesFilter          = files.FileFilter{Name: "All Files", Extensions: []string{"*"}}
	errGatewayFileMalformed = errors.New("Malformed data URL")
)

// Names a renderer requested gateway file is saved under
type GatewaySaveContext struct {
	FallbackName string
	Suggested    string
}

// Streaming and capped data URL request paths of a gateway file
type GatewayFileRequestPaths struct {
	DataURL  string
	Download string
}

// Request paths of filePath scoped by scopePath, session_id appended when the renderer sent one
func GatewayFileRequestPathsFor(filePath string, scopePath func(string) string, sessionID any, hasSession bool) GatewayFileRequestPaths {
	encoded := encodeURIComponent(filePath)
	session := ""
	if hasSession {
		session = "&session_id=" + encodeURIComponent(jsStringRaw(sessionID))
	}
	return GatewayFileRequestPaths{
		DataURL:  scopePath("/api/fs/read-data-url?path=" + encoded + session),
		Download: scopePath("/api/fs/download?path=" + encoded + session),
	}
}

// Backend route of a gateway file, registered connections never falling through to the legacy profile pool
func ResolveGatewayFileBackend[T any](connectionID, profile any, ensureLegacy func(profile *string) (T, error), ensureRegistry func(connectionID string, profile *string) (T, error)) (T, *string, *string, error) {
	id := trimmedOrNil(connectionID)
	prof := trimmedOrNil(profile)
	var conn T
	var err error
	if id != nil {
		conn, err = ensureRegistry(*id, prof)
	} else {
		conn, err = ensureLegacy(prof)
	}
	return conn, id, prof, err
}

// Trimmed JavaScript String of a value with null read as empty, nil when nothing remains
func trimmedOrNil(v any) *string {
	s := jsTrim(jsString(v))
	if s == "" {
		return nil
	}
	return &s
}

// JavaScript String of a value, null and undefined spelled out as String does
func jsStringRaw(v any) string {
	if v == nil {
		return "null"
	}
	return jsString(v)
}

// Escapes s as encodeURIComponent, every byte outside the unreserved marks percent encoded
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Sibling temp name of an in flight download in the destination directory
func DownloadTempPath(destPath string) string {
	var raw [4]byte
	rand.Read(raw[:])
	return filepath.Join(filepath.Dir(destPath), ".icebath-download-"+hex.EncodeToString(raw[:])+".part")
}

// Streams body into an exclusively created sibling temp file renamed onto destPath once whole
func PumpStreamToFile(body io.Reader, destPath string) error {
	tempPath := DownloadTempPath(destPath)
	f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return hardening.NodeErr(err)
	}
	discard := func(cause error) error {
		f.Close()
		os.Remove(tempPath)
		return cause
	}
	buf := make([]byte, 64*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return discard(hardening.NodeErr(werr))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return discard(rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tempPath)
		return hardening.NodeErr(err)
	}
	if err := os.Rename(tempPath, destPath); err != nil {
		os.Remove(tempPath)
		return hardening.NodeErr(err)
	}
	return nil
}

// Writes an in memory body with the temp file and rename contract of PumpStreamToFile
func WriteBufferToFile(buf []byte, destPath string) error {
	return PumpStreamToFile(bytes.NewReader(buf), destPath)
}

// Bytes of a data URL from the capped read-data-url route
func ParseDataURLToBuffer(dataURL string) ([]byte, error) {
	m := gatewayDataURLPattern.FindStringSubmatch(dataURL)
	if m == nil {
		return nil, errGatewayFileMalformed
	}
	if strings.Contains(strings.ToLower(m[1]), ";base64") {
		return nodehttp.DecodeBase64(m[2]), nil
	}
	text, err := nodehttp.DecodeURIComponent(m[2])
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// Base name a Content-Disposition header offers, the RFC 5987 form first, empty when none
func FilenameFromContentDisposition(value string) string {
	raw := ""
	if m := dispositionEncoded.FindStringSubmatch(value); m != nil && m[1] != "" {
		raw = m[1]
	} else if m := dispositionPlain.FindStringSubmatch(value); m != nil {
		raw = m[1]
	}
	if raw == "" {
		return ""
	}
	if decoded, err := nodehttp.DecodeURIComponent(jsTrim(raw)); err == nil {
		return nodeBasename(decoded)
	}
	return nodeBasename(jsTrim(raw))
}

// Save dialog filters for a download name, its whitelisted extension first and All Files last
func SaveDialogFilters(filename string) []files.FileFilter {
	name := nodeBasename(jsTrim(filename))
	ext := jsLower(strings.TrimPrefix(nodeExtname(name), "."))
	if !saveFilterExtension.MatchString(ext) {
		return []files.FileFilter{allFilesFilter}
	}
	return []files.FileFilter{{Name: strings.ToUpper(ext) + " File", Extensions: []string{ext}}, allFilesFilter}
}

// Gateway file path as the renderer sent it, trimmed
func GatewayFilePath(rawPath any) string {
	if !jsTruthy(rawPath) {
		return ""
	}
	return jsTrim(jsString(rawPath))
}

// Save result of a gateway download
func gatewaySaved(path string) *Object {
	out := NewObject()
	out.Set("path", path)
	out.Set("saved", true)
	return out
}

func gatewayCanceled() *Object {
	out := NewObject()
	out.Set("canceled", true)
	out.Set("saved", false)
	return out
}

// Asks for a destination with the body unread, then streams it there, abort dropping the request
func (s *Service) FinalizeGatewayDownload(res *http.Response, sc GatewaySaveContext, abort func()) (*Object, error) {
	if status := res.StatusCode; status >= 400 {
		msg := readGatewayErrorText(res.Body)
		return nil, &backend.HTTPError{StatusCode: status, Message: fmt.Sprintf("%d: %s", status, msg)}
	}
	filename := FilenameFromContentDisposition(strings.Join(res.Header.Values("Content-Disposition"), ", "))
	if filename == "" {
		filename = sc.Suggested
	}
	if filename == "" {
		filename = sc.FallbackName
	}
	result, err := s.host.ShowSaveDialog(files.SaveDialogOptions{DefaultPath: filename, Filters: SaveDialogFilters(filename), Title: "Save File"})
	if err != nil {
		abort()
		return nil, err
	}
	if result.Canceled || result.FilePath == "" {
		abort()
		return gatewayCanceled(), nil
	}
	if err := PumpStreamToFile(res.Body, result.FilePath); err != nil {
		abort()
		return nil, err
	}
	return gatewaySaved(result.FilePath), nil
}

// First 500 bytes of an error body as UTF-8 text, whatever arrived when the body fails
func readGatewayErrorText(body io.Reader) string {
	var kept []byte
	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if room := 500 - len(kept); room > 0 && n > 0 {
			kept = append(kept, buf[:min(n, room)]...)
		}
		if err != nil {
			break
		}
	}
	text, _ := pycodecs.DecodeReplace(kept, "utf-8")
	return text
}

// Streams through download, falling back to the capped data URL route only when it answers 404
func (s *Service) SaveGatewayDownload(paths GatewayFileRequestPaths, sc GatewaySaveContext, download func(requestPath string, sc GatewaySaveContext) (*Object, error), readDataURL func(requestPath string) (string, error)) (*Object, error) {
	out, err := download(paths.Download, sc)
	if err == nil {
		return out, nil
	}
	if !IsNotFoundError(err) {
		return nil, err
	}
	dataURL, err := readDataURL(paths.DataURL)
	if err != nil {
		return nil, err
	}
	buf, err := ParseDataURLToBuffer(dataURL)
	if err != nil {
		return nil, err
	}
	name := sc.Suggested
	if name == "" {
		name = sc.FallbackName
	}
	result, err := s.host.ShowSaveDialog(files.SaveDialogOptions{DefaultPath: name, Filters: SaveDialogFilters(name), Title: "Save File"})
	if err != nil {
		return nil, err
	}
	if result.Canceled || result.FilePath == "" {
		return gatewayCanceled(), nil
	}
	if err := WriteBufferToFile(buf, result.FilePath); err != nil {
		return nil, err
	}
	return gatewaySaved(result.FilePath), nil
}

// True for a transport error carrying HTTP status 404
func IsNotFoundError(err error) bool {
	var he *backend.HTTPError
	return errors.As(err, &he) && he.StatusCode == 404
}

// Body whose reads fail once no data arrives for idle while a reader waits on it
type stallReader struct {
	r      io.Reader
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func (s *stallReader) Read(p []byte) (int, error) {
	t := time.AfterFunc(gatewayFetchTimeout, func() {
		s.cancel(fmt.Errorf("Icebath backend download stalled: no data for %dms", gatewayFetchTimeout.Milliseconds()))
	})
	n, err := s.r.Read(p)
	t.Stop()
	if err != nil && err != io.EOF {
		if cause := context.Cause(s.ctx); cause != nil {
			return n, cause
		}
	}
	return n, err
}

// Parses a gateway download URL, refusing anything but http and https
func gatewayDownloadURL(raw string) (string, error) {
	u, err := nodehttp.ParseURL(raw)
	if err != nil {
		return "", fmt.Errorf("Invalid URL: %s", err.Error())
	}
	if p := u.Protocol(); p != "http:" && p != "https:" {
		return "", fmt.Errorf("Unsupported Icebath backend URL protocol: %s", p)
	}
	return u.Protocol(), nil
}

// Downloads url with the session token or a bearer and saves it through FinalizeGatewayDownload
func (s *Service) DownloadViaTokenToFile(rawURL string, token *string, sc GatewaySaveContext, bearer string) (*Object, error) {
	module, err := gatewayDownloadURL(rawURL)
	if err != nil {
		return nil, err
	}
	u, _ := nodehttp.ParseURL(rawURL)
	header := http.Header{}
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	} else {
		value := ""
		if token != nil {
			value = *token
		}
		header.Set("X-Icebath-Session-Token", value)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	timeout := fmt.Errorf("Timed out connecting to Icebath backend after %dms", gatewayFetchTimeout.Milliseconds())
	timer := time.AfterFunc(gatewayFetchTimeout, func() { cancel(timeout) })
	res, err := downloadAgent.Get(ctx, u, module, header)
	timer.Stop()
	if err != nil {
		cancel(nil)
		if cause := context.Cause(ctx); errors.Is(cause, timeout) {
			return nil, timeout
		}
		return nil, err
	}
	defer cancel(nil)
	defer res.Body.Close()
	res.Body = io.NopCloser(&stallReader{r: res.Body, ctx: ctx, cancel: cancel})
	return s.FinalizeGatewayDownload(res, sc, func() { cancel(nil) })
}
