package spotify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Marks the Spotify tool errors whose text is shown to the model as is
type spotifyErrorKind interface {
	error
	spotifyError()
}

// Base Spotify tool error
type SpotifyError struct{ Msg string }

func (e *SpotifyError) Error() string     { return e.Msg }
func (e *SpotifyError) ErrorKind() string { return "SpotifyError" }
func (*SpotifyError) spotifyError()       {}

// Raised when the user needs to authenticate with Spotify first
type SpotifyAuthRequiredError struct{ Msg string }

func (e *SpotifyAuthRequiredError) Error() string     { return e.Msg }
func (e *SpotifyAuthRequiredError) ErrorKind() string { return "SpotifyAuthRequiredError" }
func (*SpotifyAuthRequiredError) spotifyError()       {}

// Structured Spotify API failure with its status, raw body and request path
type SpotifyAPIError struct {
	Msg          string
	StatusCode   int
	ResponseBody string
	Path         string
}

func (e *SpotifyAPIError) Error() string     { return e.Msg }
func (e *SpotifyAPIError) ErrorKind() string { return "SpotifyAPIError" }
func (*SpotifyAPIError) spotifyError()       {}

// Python built in exception of a failed conversion or lookup, ValueError, KeyError or TypeError
type kindError struct {
	kind string
	msg  string
}

func (e *kindError) Error() string     { return e.msg }
func (e *kindError) ErrorKind() string { return e.kind }

// Python class name of an error raised inside a tool
func exceptionName(err error) string {
	var pe *errorutil.Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return errorutil.Kind(err)
}

// Resolves the stored Spotify login for a home, swapped by callers that serve a fixed runtime
var resolveRuntimeCredentials = provider.SpotifyResolveRuntimeCredentials

// Web API client of one home holding the resolved runtime credentials
type spotifyClient struct {
	ctx     context.Context
	home    string
	runtime map[string]any
}

// Builds a client, an auth failure becoming SpotifyAuthRequiredError
var newSpotifyClient = func(ctx context.Context, h string) (apiClient, error) {
	c := &spotifyClient{ctx: ctx, home: h}
	runtime, err := c.resolveRuntime(false, true)
	if err != nil {
		return nil, err
	}
	c.runtime = runtime
	return c, nil
}

// Requests the tool handlers issue against the Spotify Web API
type apiClient interface {
	Request(method, path string, params, jsonBody *jsonutil.Object, opts ...requestOption) (any, error)
	GetPlaybackState(market any) (any, error)
	GetCurrentlyPlaying(market any) (any, error)
}

type requestOptions struct {
	allowRetryOn401 bool
	emptyResponse   *jsonutil.Object
}

type requestOption func(*requestOptions)

// Payload a 204 or empty answer returns instead of the default empty marker
func withEmptyResponse(o *jsonutil.Object) requestOption {
	return func(r *requestOptions) { r.emptyResponse = o }
}

func withoutRetryOn401() requestOption { return func(r *requestOptions) { r.allowRetryOn401 = false } }

func (c *spotifyClient) resolveRuntime(forceRefresh, refreshIfExpiring bool) (map[string]any, error) {
	runtime, err := resolveRuntimeCredentials(c.ctx, c.home, forceRefresh, refreshIfExpiring)
	if err != nil {
		var authErr *provider.AuthError
		if errors.As(err, &authErr) {
			return nil, &SpotifyAuthRequiredError{Msg: authErr.Error()}
		}
		return nil, err
	}
	return runtime, nil
}

// API base URL of the runtime without a trailing slash
func (c *spotifyClient) baseURL() string {
	v := c.runtime["base_url"]
	if !anyutil.IsTrue(v) {
		return ""
	}
	return strings.TrimRight(anyutil.String(v), "/")
}

// Copy of an ordered mapping without its None values
func stripNone(o *jsonutil.Object) *jsonutil.Object {
	out := jsonutil.NewObject()
	if o == nil {
		return out
	}
	for _, k := range o.Keys() {
		if v := o.Value(k); v != nil {
			out.Set(k, v)
		}
	}
	return out
}

// httpx query text of one primitive value
func queryValue(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	}
	return anyutil.String(v)
}

// Query values of a mapping, a list value repeating its key
func queryParams(o *jsonutil.Object) url.Values {
	out := make(url.Values, o.Len())
	for _, k := range o.Keys() {
		switch t := o.Value(k).(type) {
		case []any:
			for _, item := range t {
				out.Add(k, queryValue(item))
			}
		case []string:
			out[k] = append(out[k], t...)
		default:
			out.Add(k, queryValue(t))
		}
	}
	return out
}

// Calls the Web API with one forced refresh and retry on a 401, mapping errors to friendly messages
func (c *spotifyClient) Request(method, path string, params, jsonBody *jsonutil.Object, opts ...requestOption) (any, error) {
	o := requestOptions{allowRetryOn401: true}
	for _, opt := range opts {
		opt(&o)
	}
	target := httputil.MergeQuery(c.baseURL()+path, queryParams(stripNone(params)))
	var body io.Reader
	headers := map[string]string{"Authorization": "Bearer " + anyutil.String(c.runtime["access_token"]), "Content-Type": "application/json"}
	if jsonBody != nil {
		encoded, err := jsonutil.Encode(stripNone(jsonBody), jsonutil.Spaced(), jsonutil.ASCII())
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader([]byte(encoded))
	}
	resp, err := provider.SpotifyHTTPDo(c.ctx, method, target, body, headers, 30.0)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && o.allowRetryOn401 {
		runtime, err := c.resolveRuntime(true, true)
		if err != nil {
			return nil, err
		}
		c.runtime = runtime
		return c.Request(method, path, params, jsonBody, withoutRetryOn401())
	}
	if resp.StatusCode >= 400 {
		detail := strings.TrimSpace(resp.Text())
		message := friendlySpotifyErrorMessage(resp.StatusCode, extractSpotifyErrorDetail(resp, detail), path, resp.Header.Get("Retry-After"))
		return nil, &SpotifyAPIError{Msg: message, StatusCode: resp.StatusCode, ResponseBody: detail, Path: path}
	}
	if resp.StatusCode == http.StatusNoContent || len(resp.Content) == 0 {
		if o.emptyResponse != nil && o.emptyResponse.Len() > 0 {
			return o.emptyResponse, nil
		}
		return jsonutil.ObjectOf("success", true, "status_code", int64(resp.StatusCode), "empty", true), nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return resp.JSON()
	}
	return jsonutil.ObjectOf("success", true, "text", resp.Text()), nil
}

// Explanatory payload a player read returns for a bare 204
func empty204(message string) *jsonutil.Object {
	return jsonutil.ObjectOf("status_code", int64(204), "empty", true, "message", message)
}

// Playback state, an explanatory payload when no session is active
func (c *spotifyClient) GetPlaybackState(market any) (any, error) {
	return c.Request("GET", "/me/player", jsonutil.ObjectOf("market", market), nil, withEmptyResponse(empty204(
		"No active Spotify playback session was found. Open Spotify on a device and start playback, or transfer playback to an available device.")))
}

// Currently playing item, an explanatory payload when nothing plays
func (c *spotifyClient) GetCurrentlyPlaying(market any) (any, error) {
	return c.Request("GET", "/me/player/currently-playing", jsonutil.ObjectOf("market", market), nil, withEmptyResponse(empty204(
		"Spotify is not currently playing anything. Start playback in Spotify and try again.")))
}

// Spotify's error message from an error or error.message body, the raw text otherwise
func extractSpotifyErrorDetail(resp *httputil.Response, fallback string) string {
	detail := fallback
	if payload, err := resp.JSON(); err == nil {
		if obj, ok := payload.(*jsonutil.Object); ok {
			switch errObj := obj.ValueOr("error", nil).(type) {
			case *jsonutil.Object:
				if msg := errObj.ValueOr("message", nil); anyutil.IsTrue(msg) {
					detail = anyutil.String(msg)
				}
			case string:
				detail = errObj
			}
		}
	}
	return strings.TrimSpace(detail)
}

// Friendly text of a failed Web API call by status and path
func friendlySpotifyErrorMessage(statusCode int, detail, path, retryAfter string) string {
	isPlaybackPath := strings.HasPrefix(path, "/me/player")
	switch statusCode {
	case 401:
		return "Spotify authentication failed or expired. Run `icebath spotify login` again."
	case 403:
		if isPlaybackPath {
			return "Spotify rejected this playback request. Playback control usually requires a Spotify Premium account " +
				"and an active Spotify Connect device."
		}
		lower := strings.ToLower(detail)
		if strings.Contains(lower, "scope") || strings.Contains(lower, "permission") {
			return "Spotify rejected the request because the current auth scope is insufficient. Re-run `icebath spotify login` to refresh permissions."
		}
		return "Spotify rejected the request. The account may not have permission for this action."
	case 404:
		if isPlaybackPath {
			return "Spotify could not find an active playback device or player session for this request."
		}
		return "Spotify resource not found."
	case 429:
		message := "Spotify rate limit exceeded."
		if retryAfter != "" {
			message += fmt.Sprintf(" Retry after %s seconds.", retryAfter)
		}
		return message
	}
	if detail != "" {
		return detail
	}
	return fmt.Sprintf("Spotify API request failed with status %d.", statusCode)
}

func checkType(itemType, expectedType string) error {
	if expectedType != "" && itemType != expectedType {
		return &SpotifyError{Msg: fmt.Sprintf("Expected a Spotify %s, got %s.", expectedType, itemType)}
	}
	return nil
}

// Path segments of an open.spotify.com URL as urlparse reads them
func urlPathSegments(cleaned string) ([]string, error) {
	u, err := urlutil.Split(cleaned)
	if err != nil {
		return nil, &kindError{kind: "ValueError", msg: err.Error()}
	}
	path := u.Path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		if j := strings.Index(path[i:], ";"); j >= 0 {
			path = path[:i+j]
		}
	} else if j := strings.Index(path, ";"); j >= 0 {
		path = path[:j]
	}
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts, nil
}

// Bare id of a bare id, spotify type id URI or open.spotify.com URL, the type checked when expected
func normalizeSpotifyID(value, expectedType string) (string, error) {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return "", &SpotifyError{Msg: "Spotify id/uri/url is required."}
	}
	var parts []string
	if strings.HasPrefix(cleaned, "spotify:") {
		parts = strings.Split(cleaned, ":")[1:]
	}
	if len(parts) < 2 && strings.Contains(cleaned, "open.spotify.com") {
		segments, err := urlPathSegments(cleaned)
		if err != nil {
			return "", err
		}
		parts = segments
		if len(parts) > 0 && strings.HasPrefix(parts[0], "intl-") {
			parts = parts[1:]
		}
	}
	if len(parts) >= 2 {
		if err := checkType(parts[0], expectedType); err != nil {
			return "", err
		}
		return parts[1], nil
	}
	return cleaned, nil
}

// URI of a reference, a bare id needing the expected type to become one
func normalizeSpotifyURI(value, expectedType string) (string, error) {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return "", &SpotifyError{Msg: "Spotify URI/url/id is required."}
	}
	if strings.HasPrefix(cleaned, "spotify:") {
		parts := strings.Split(cleaned, ":")
		if expectedType != "" && len(parts) >= 3 {
			if err := checkType(parts[1], expectedType); err != nil {
				return "", err
			}
		}
		return cleaned, nil
	}
	itemID, err := normalizeSpotifyID(cleaned, expectedType)
	if err != nil {
		return "", err
	}
	if expectedType != "" {
		return "spotify:" + expectedType + ":" + itemID, nil
	}
	return cleaned, nil
}

// Every reference normalized to a URI, duplicates dropped in first seen order
func normalizeSpotifyURIs(values []string, expectedType string) ([]string, error) {
	seen := map[string]bool{}
	var uris []string
	for _, v := range values {
		uri, err := normalizeSpotifyURI(v, expectedType)
		if err != nil {
			return nil, err
		}
		if !seen[uri] {
			seen[uri] = true
			uris = append(uris, uri)
		}
	}
	if len(uris) == 0 {
		return nil, &SpotifyError{Msg: "At least one Spotify item is required."}
	}
	return uris, nil
}
