package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyemail"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func mediaLog() *slog.Logger { return slog.Default().With("logger", "gateway.relay.media") }

// RelayMediaMaxBytes mirrors the connector media store cap so oversize files fail before the round trip
const RelayMediaMaxBytes = 25 * 1024 * 1024

const relayMediaRequestTimeout = 30 * time.Second

// Delays in seconds before each extra upload attempt
var relayUploadRetryDelays = []float64{2.0, 5.0}

var relayUploadRetryStatuses = map[int]bool{403: true, 408: true, 429: true, 500: true, 502: true, 503: true, 504: true}

const relayMediaUserAgent = "IcebathAgent-Relay/1.0 (+https://github.com/NousResearch/hermes-agent)"

// HTTP connector base of a ws or wss relay dial URL
func RelayMediaBaseURL(relayDialURL string) string {
	raw := pystr.RstripChars(pystr.Strip(relayDialURL), "/")
	if strings.HasPrefix(raw, "ws://") {
		raw = "http://" + raw[len("ws://"):]
	} else if strings.HasPrefix(raw, "wss://") {
		raw = "https://" + raw[len("wss://"):]
	}
	return strings.TrimSuffix(raw, "/relay")
}

// RelayMediaClient talks to the connector media routes with the per gateway signed bearer
type RelayMediaClient struct {
	baseURL   string
	gatewayID string
	secret    string
}

// Client for the connector base with the gateway credentials, absent ones empty
func NewRelayMediaClient(baseURL, gatewayID, secret string) *RelayMediaClient {
	return &RelayMediaClient{baseURL: pystr.RstripChars(baseURL, "/"), gatewayID: gatewayID, secret: secret}
}

// True when the base URL and both per gateway credentials are present
func (c *RelayMediaClient) Enabled() bool {
	return c.baseURL != "" && c.gatewayID != "" && c.secret != ""
}

func (c *RelayMediaClient) bearer() string {
	return RelayMakeUpgradeToken(c.gatewayID, c.secret, relayDefaultUpgradeTTLSeconds)
}

// True when the URL is a connector re host reference needing the bearer
func (c *RelayMediaClient) IsRelayMediaURL(url string) bool {
	return strings.Contains(url, "/relay/media/")
}

// Reference URL of the uploaded file, empty on any failure
func (c *RelayMediaClient) Upload(ctx context.Context, filePath string, mime, filename *string) (string, error) {
	ref, _, err := c.UploadWithError(ctx, filePath, mime, filename)
	return ref, err
}

// Reference URL or the failure reason, transient failures retried after the configured delays
func (c *RelayMediaClient) UploadWithError(ctx context.Context, filePath string, mime, filename *string) (string, string, error) {
	if !c.Enabled() {
		return "", "relay media client has no credentials", nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		mediaLog().Warn(fmt.Sprintf("relay media upload: cannot read %s", filePath))
		return "", "attachment file is unreadable", nil
	}
	if len(data) == 0 || len(data) > RelayMediaMaxBytes {
		mediaLog().Warn(fmt.Sprintf("relay media upload: %s size %d outside (0, %d]", filePath, len(data), RelayMediaMaxBytes))
		return "", fmt.Sprintf("attachment size %d outside (0, %d]", len(data), RelayMediaMaxBytes), nil
	}
	baseName := pypath.Name(filePath)
	if filename != nil && *filename != "" {
		baseName = *filename
	}
	contentType := ""
	if mime != nil && *mime != "" {
		contentType = *mime
	} else if guessed, _, gerr := pymimetypes.GuessType(baseName, true); gerr == nil {
		contentType = guessed
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	name := pystr.Slice(baseName, 0, 255)
	url := c.baseURL + "/relay/media"
	attempts := len(relayUploadRetryDelays) + 1
	attempt, reason := 0, ""
	for attempt < attempts {
		attempt++
		ref, why, retryable, err := c.postMedia(url, data, contentType, name)
		if err != nil {
			return "", "", err
		}
		reason = why
		if ref != "" {
			if attempt > 1 {
				mediaLog().Info(fmt.Sprintf("relay media upload of %s succeeded on attempt %d", name, attempt))
			}
			return ref, "", nil
		}
		if !retryable || attempt == attempts {
			break
		}
		delay := relayUploadRetryDelays[attempt-1]
		mediaLog().Warn(fmt.Sprintf("relay media upload of %s failed (attempt %d/%d): %s; retrying in %.0fs", name, attempt, attempts, reason, delay))
		if err := sleepSeconds(ctx, delay); err != nil {
			return "", "", err
		}
	}
	mediaLog().Warn(fmt.Sprintf("relay media upload of %s failed (attempt %d/%d): %s", name, attempt, attempts, reason))
	return "", reason, nil
}

// One upload attempt as reference, failure reason and whether another attempt is worth it
func (c *RelayMediaClient) postMedia(url string, data []byte, contentType, name string) (string, string, bool, error) {
	bearer := c.bearer()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", "", false, err
	}
	req.Header.Set("User-Agent", relayMediaUserAgent)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Media-Filename", name)
	body, err := RelayURLOpen(req, relayMediaRequestTimeout)
	if err != nil {
		var httpErr *RelayHTTPError
		if errors.As(err, &httpErr) {
			described := RelayDescribeHTTPError(httpErr, RelayReadErrorBody(httpErr), bearer, c.secret)
			return "", described, relayUploadRetryStatuses[httpErr.Code], nil
		}
		var urlErr *RelayURLError
		if errors.As(err, &urlErr) {
			return "", "transport error: " + urlErr.Reason, true, nil
		}
		return "", "transport error: " + relayURLErrorReason(err), true, nil
	}
	text, err := pystr.DecodeUTF8(body)
	if err != nil {
		return "", "unreadable connector response: " + err.Error(), false, nil
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return "", "unreadable connector response: " + err.Error(), false, nil
	}
	obj, ok := pyjson.AsObject(parsed)
	if !ok {
		return "", "", false, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	if mediaID := obj.Vals["id"]; pyrepr.Truthy(mediaID) {
		return c.baseURL + "/relay/media/" + pyrepr.Str(mediaID), "", false, nil
	}
	return "", "connector answered without a media id", false, nil
}

// Local temp path of a downloaded attachment, empty on any failure
func (c *RelayMediaClient) Download(ctx context.Context, url string, suggestedName *string) (string, error) {
	path, _, ok, err := c.DownloadWithName(ctx, url, suggestedName)
	if !ok {
		return "", err
	}
	return path, err
}

// Temp path and original display name of a downloaded attachment, ok false on any failure
func (c *RelayMediaClient) DownloadWithName(ctx context.Context, url string, suggestedName *string) (string, string, bool, error) {
	if url == "" {
		return "", "", false, nil
	}
	needsAuth := c.IsRelayMediaURL(url)
	if needsAuth && !c.Enabled() {
		return "", "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	headers := http.Header{"User-Agent": {relayMediaUserAgent}}
	if needsAuth {
		headers.Set("Authorization", "Bearer "+c.bearer())
	}
	path, name, err := c.fetchMedia(url, headers, suggestedName)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", false, ctxErr
	}
	if err != nil {
		mediaLog().Warn(fmt.Sprintf("relay media download failed for %s: %s", url, err))
		return "", "", false, nil
	}
	if path == "" {
		return "", "", false, nil
	}
	return path, name, true, nil
}

// GET into a relay_media temp file named by the suggested, disposition or mime extension
func (c *RelayMediaClient) fetchMedia(url string, headers http.Header, suggestedName *string) (string, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header = headers
	resp, err := relayURLOpenResponse(req, relayMediaRequestTimeout)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if raw := resp.Header.Get("Content-Length"); raw != "" {
		n, ok := pyint.Parse(raw)
		if !ok {
			return "", "", fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(raw))
		}
		if n.Int64() > RelayMediaMaxBytes || !n.IsInt64() {
			mediaLog().Warn(fmt.Sprintf("relay media download too large: %s", url))
			return "", "", nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, RelayMediaMaxBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(data) == 0 || len(data) > RelayMediaMaxBytes {
		return "", "", nil
	}
	var suggested any
	if suggestedName != nil {
		suggested = *suggestedName
	}
	name := CleanAttachmentFilename(suggested)
	if name == "" {
		name = relayContentDispositionFilename(resp.Header.Get("Content-Disposition"))
	}
	ext := ""
	if name != "" {
		ext = pypath.Suffix(name)
	}
	if !relayASCIIAlnumExt(ext) || len(pystr.Runes(ext)) > 16 {
		ext = ""
	}
	if ext == "" {
		mime, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
		ext = pymimetypes.GuessExtension(mime, true)
		if ext == "" {
			ext = ".bin"
		}
	}
	tmpDir, err := pypath.Gettempdir()
	if err != nil {
		return "", "", err
	}
	fh, err := os.CreateTemp(tmpDir, "relay_media_*"+ext)
	if err != nil {
		return "", "", err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return "", "", err
	}
	if err := fh.Close(); err != nil {
		return "", "", err
	}
	return fh.Name(), name, nil
}

// True when the extension is ASCII and the text after its dot is a non empty alphanumeric run
func relayASCIIAlnumExt(ext string) bool {
	if ext == "" {
		return false
	}
	for i := 0; i < len(ext); i++ {
		if ext[i] >= 0x80 {
			return false
		}
	}
	rest := ext[1:]
	if rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		ch := rest[i]
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z') {
			return false
		}
	}
	return true
}

// Display cleaned filename a Content-Disposition header names, the RFC 5987 form winning, empty when none
func relayContentDispositionFilename(header string) string {
	if header == "" {
		return ""
	}
	params, err := pyemail.GetParams(header)
	if err != nil {
		return ""
	}
	plain, extended := "", ""
	for _, p := range params[1:] {
		if pystr.Lower(p.Name) != "filename" {
			continue
		}
		if p.RFC2231 {
			if extended == "" {
				extended = pyemail.CollapseRFC2231Value(p)
			}
		} else if plain == "" {
			plain = pyemail.Unquote(p.Value)
		}
	}
	if extended != "" {
		return CleanAttachmentFilename(extended)
	}
	return CleanAttachmentFilename(plain)
}
