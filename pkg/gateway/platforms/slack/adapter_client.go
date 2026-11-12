package slack

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	slackapi "github.com/slack-go/slack"
)

// Web API methods slack_sdk sends as GET query parameters rather than a JSON body
var getMethods = map[string]bool{
	"conversations.replies": true, "conversations.history": true, "conversations.info": true,
	"users.info": true,
}

// Web API client of one workspace sending hermes's call arguments verbatim, slack-go owning uploads
type webClient struct {
	token     string
	apiURL    string
	http      *http.Client
	sdk       *slackapi.Client
	userAgent string
}

// Client for a bot token with the proxy pinned and the IcebathAgent attribution prefix
func newWebClient(token, proxyURL string) *webClient {
	httpClient := slackHTTPClient(proxyURL)
	apiURL := slackapi.APIURL
	prefix := "IcebathAgent"
	if ua, err := updaterecord.UserAgent("IcebathAgent"); err == nil {
		prefix = ua
	}
	return &webClient{
		token:     token,
		apiURL:    apiURL,
		http:      httpClient,
		sdk:       slackapi.New(token, slackapi.OptionAPIURL(apiURL), slackapi.OptionHTTPClient(httpClient)),
		userAgent: prefix + " Go/" + strings.TrimPrefix(runtime.Version(), "go") + " slack-go/0.35.0 " + runtime.GOOS,
	}
}

// Error a call on a missing workspace client raises, the AttributeError Python raises on None
func noClientError(method string) error {
	return &gateway.PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute '" + method + "'"}
}

// Failed Web API call with the decoded body, headers and HTTP status Slack answered
type slackAPIError struct {
	URL        string
	Response   map[string]any
	Headers    http.Header
	StatusCode int
}

func (e *slackAPIError) Error() string {
	return "The request to the Slack API failed. (url: " + e.URL + ")\nThe server responded with: " + pyrepr.Repr(e.Response)
}

// Web API call result with its decoded body and response headers
type apiResult struct {
	Data    map[string]any
	Headers http.Header
}

// Value of a result field, nil for a missing field or a nil result
func (r *apiResult) get(key string) any {
	if r == nil {
		return nil
	}
	return r.Data[key]
}

// Calls a Web API method with the arguments, an ok false or non 200 answer returned as slackAPIError
func (c *webClient) call(ctx context.Context, method string, args map[string]any) (*apiResult, error) {
	if c == nil {
		return nil, noClientError(strings.ReplaceAll(method, ".", "_"))
	}
	endpoint := c.apiURL + method
	var req *http.Request
	var err error
	if getMethods[method] {
		q := url.Values{}
		for k, v := range args {
			if v == nil {
				continue
			}
			q.Set(k, queryValue(v))
		}
		target := endpoint
		if len(q) > 0 {
			target += "?" + q.Encode()
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	} else {
		clean := map[string]any{}
		for k, v := range args {
			if v != nil {
				clean[k] = v
			}
		}
		body, merr := json.Marshal(clean)
		if merr != nil {
			return nil, merr
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json;charset=utf-8")
		}
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	data := slackResponsePayload(decodeWebBody(raw))
	if resp.StatusCode != http.StatusOK || data["ok"] != true {
		return nil, &slackAPIError{URL: endpoint, Response: data, Headers: resp.Header, StatusCode: resp.StatusCode}
	}
	return &apiResult{Data: data, Headers: resp.Header}, nil
}

// Body decoded as JSON into plain values, kept as raw bytes when it is not JSON
func decodeWebBody(raw []byte) *webResponse {
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return &webResponse{Data: raw}
	}
	return &webResponse{Data: pyjson.Plain(v)}
}

// Query string form of an argument, JSON for containers and Python str for scalars
func queryValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any, map[string]any:
		b, _ := json.Marshal(t)
		return string(b)
	}
	return pyrepr.Str(v)
}

// One file of a files_upload_v2 call, a local path or in memory content
type fileUpload struct {
	File     string
	Content  []byte
	Filename string
}

// Uploads the files through getUploadURLExternal and completes them as one message in the channel
func (c *webClient) filesUploadV2(ctx context.Context, channel string, uploads []fileUpload, initialComment, threadTS string) (*apiResult, error) {
	if c == nil {
		return nil, noClientError("files_upload_v2")
	}
	var files []any
	for _, up := range uploads {
		data := up.Content
		if up.File != "" {
			b, err := os.ReadFile(up.File)
			if err != nil {
				return nil, err
			}
			data = b
		}
		filename := up.Filename
		if filename == "" && up.File != "" {
			filename = filepath.Base(up.File)
		}
		urlResp, err := c.sdk.GetUploadURLExternalContext(ctx, slackapi.GetUploadURLExternalParameters{FileName: filename, FileSize: len(data)})
		if err != nil {
			return nil, err
		}
		if err := c.sdk.UploadToURL(ctx, slackapi.UploadToURLParameters{UploadURL: urlResp.UploadURL, Reader: bytes.NewReader(data), Filename: filename}); err != nil {
			return nil, err
		}
		files = append(files, map[string]any{"id": urlResp.FileID, "title": filename})
	}
	args := map[string]any{"files": files, "channel_id": channel, "initial_comment": initialComment}
	if threadTS != "" {
		args["thread_ts"] = threadTS
	}
	return c.call(ctx, "files.completeUploadExternal", args)
}

// HTTP status an SDK or Web API error carries, zero when it has none
func errorStatusCode(err error) int {
	var apiErr *slackAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	var rate *slackapi.RateLimitedError
	if errors.As(err, &rate) {
		return http.StatusTooManyRequests
	}
	var status slackapi.StatusCodeError
	if errors.As(err, &status) {
		return status.Code
	}
	return 0
}

// Error code field of a Slack API failure, empty when the error carries none
func slackErrorCode(err error) string {
	var apiErr *slackAPIError
	if errors.As(err, &apiErr) {
		return strOrEmpty(apiErr.Response["error"])
	}
	var resp slackapi.SlackErrorResponse
	if errors.As(err, &resp) {
		return resp.Err
	}
	return ""
}

// Response headers of a Slack API failure, nil when the error carries none
func errorHeaders(err error) http.Header {
	var apiErr *slackAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Headers
	}
	var rate *slackapi.RateLimitedError
	if errors.As(err, &rate) {
		return http.Header{"Retry-After": {fmt.Sprintf("%d", int(rate.RetryAfter/time.Second))}}
	}
	return nil
}

// Whether a transport error is a timeout or connection failure other than a permanent TLS failure
func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var certVerify *tls.CertificateVerificationError
	if errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &certInvalid) ||
		errors.As(err, &recordErr) || errors.As(err, &certVerify) {
		return false
	}
	var opErr *net.OpError
	var urlErr *url.Error
	return errors.As(err, &opErr) || (errors.As(err, &urlErr) && !errors.As(err, new(*slackAPIError)))
}
