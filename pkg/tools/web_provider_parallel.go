package tools

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const parallelMissingKey = "PARALLEL_API_KEY environment variable not set. Get your API key at https://parallel.ai"

// Parallel API origin when PARALLEL_BASE_URL is unset
const parallelDefaultBaseURL = "https://api.parallel.ai"

// Beta the search and extract endpoints require
const parallelSearchExtractBeta = "search-extract-2025-10-10"

// Retry budget and backoff bounds of the Parallel SDK
const (
	parallelMaxRetries        = 2
	parallelInitialRetryDelay = 0.5
	parallelMaxRetryDelay     = 8.0
)

// Parallel SDK limits, five seconds to connect and ten minutes for each wait on the server
var parallelTimeouts = sdkTimeouts{connect: 5 * time.Second, idle: 600 * time.Second}

// Backoff sleep between Parallel retries, swapped by tests
var parallelSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parallelAPIError is the SDK error a failed Parallel call raises
type parallelAPIError struct{ message string }

func (e *parallelAPIError) Error() string { return e.message }

// Native Parallel REST client speaking the request and retry semantics of the parallel-web SDK
type parallelClient struct {
	apiKey  string
	baseURL string
	async   bool
	custom  [][2]string
}

// Client for the key reading PARALLEL_BASE_URL and PARALLEL_CUSTOM_HEADERS like the SDK constructor
func newParallelClient(env *Env, apiKey string, async bool) *parallelClient {
	base, ok := sdkEnvLookup(env, "PARALLEL_BASE_URL")
	if !ok {
		base = parallelDefaultBaseURL
	}
	c := &parallelClient{apiKey: apiKey, baseURL: base, async: async}
	if raw, ok := sdkEnvLookup(env, "PARALLEL_CUSTOM_HEADERS"); ok {
		for _, line := range strings.Split(raw, "\n") {
			if colon := strings.Index(line, ":"); colon >= 0 {
				c.custom = append(c.custom, [2]string{pystr.Strip(line[:colon]), pystr.Strip(line[colon+1:])})
			}
		}
	}
	return c
}

// Client for the given SDK class cached in its slot for the current key
func parallelClientFor(ctx context.Context, env *Env, slot string, async bool) (*parallelClient, error) {
	return cachedSDKClient(ctx, env, slot, "PARALLEL_API_KEY", parallelMissingKey, func(apiKey string) *parallelClient {
		return newParallelClient(env, apiKey, async)
	})
}

// Synchronous Parallel client the search path uses
func getParallelSyncClient(ctx context.Context, env *Env) (*parallelClient, error) {
	return parallelClientFor(ctx, env, "_parallel_client", false)
}

// Asynchronous Parallel client the extract path uses
func getParallelAsyncClient(ctx context.Context, env *Env) (*parallelClient, error) {
	return parallelClientFor(ctx, env, "_async_parallel_client", true)
}

// PARALLEL_SEARCH_MODE lowered and stripped, agentic for an unset or unknown mode
func resolveParallelSearchMode(env *Env) string {
	mode, ok := sdkEnvLookup(env, "PARALLEL_SEARCH_MODE")
	if !ok {
		mode = "agentic"
	}
	mode = pystr.Strip(pystr.Lower(mode))
	if mode == "fast" || mode == "one-shot" || mode == "agentic" {
		return mode
	}
	return "agentic"
}

// Endpoint URL joined under the base the way the SDK enforces a trailing slash
func (c *parallelClient) endpoint(path string) string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + path
	}
	if !strings.HasSuffix(u.EscapedPath(), "/") {
		u.Path += "/"
		u.RawPath = ""
	}
	return strings.TrimSuffix(u.String(), "?") + strings.TrimLeft(path, "/")
}

// Stainless OS name of the host
func stainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "android":
		return "Android"
	case "ios":
		return "iOS"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "linux":
		return "Linux"
	}
	return "Other:" + runtime.GOOS
}

// Stainless architecture name of the host
func stainlessArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	case "amd64":
		return "x64"
	case "386":
		return "x32"
	}
	return "other:" + runtime.GOARCH
}

// Request headers of one attempt, defaults then platform, auth, custom and per call headers
func (c *parallelClient) headers(retries int) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", icebathUserAgent())
	h.Set("X-Stainless-Lang", "go")
	h.Set("X-Stainless-Package-Version", icebathVersion())
	h.Set("X-Stainless-OS", stainlessOS())
	h.Set("X-Stainless-Arch", stainlessArch())
	h.Set("X-Stainless-Runtime", "go")
	h.Set("X-Stainless-Runtime-Version", runtime.Version())
	h.Set("x-api-key", c.apiKey)
	if c.async {
		h.Set("X-Stainless-Async", "async:goroutine")
	} else {
		h.Set("X-Stainless-Async", "false")
	}
	for _, kv := range c.custom {
		h.Set(kv[0], kv[1])
	}
	h.Set("parallel-beta", parallelSearchExtractBeta)
	h.Set("x-stainless-retry-count", strconv.Itoa(retries))
	h.Set("x-stainless-read-timeout", strconv.Itoa(int(parallelTimeouts.idle/time.Second)))
	return h
}

// Seconds from retry-after-ms, retry-after seconds or an HTTP date, false when absent or unparseable
func parallelRetryAfter(h http.Header) (float64, bool) {
	if h == nil {
		return 0, false
	}
	if v, err := strconv.ParseFloat(pystr.Strip(h.Get("retry-after-ms")), 64); err == nil {
		return v / 1000, true
	}
	raw := h.Get("retry-after")
	if v, err := strconv.ParseFloat(pystr.Strip(raw), 64); err == nil {
		return v, true
	}
	if t, err := mail.ParseDate(raw); err == nil {
		return time.Until(t).Seconds(), true
	}
	return 0, false
}

// Backoff before the next attempt, a short server retry after winning over jittered exponential delay
func parallelRetryDelay(retries int, h http.Header) time.Duration {
	if after, ok := parallelRetryAfter(h); ok && after > 0 && after <= 60 {
		return time.Duration(after * float64(time.Second))
	}
	sleep := math.Min(parallelInitialRetryDelay*math.Pow(2, float64(min(retries, 1000))), parallelMaxRetryDelay)
	return time.Duration(math.Max(sleep*(1-0.25*rand.Float64()), 0) * float64(time.Second))
}

// Whether a failed status is retried, x-should-retry overriding the status rule
func parallelShouldRetry(resp *sdkResponse) bool {
	switch resp.header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return resp.status == 408 || resp.status == 409 || resp.status == 429 || resp.status >= 500
}

// Status error message, the parsed JSON body in Python form when the body parses
func parallelStatusError(resp *sdkResponse) error {
	text := pystr.Strip(resp.text())
	if parsed, err := pyjson.Loads(text); err == nil {
		return &parallelAPIError{message: fmt.Sprintf("Error code: %d - %s", resp.status, pyrepr.Str(parsed))}
	}
	if text != "" {
		return &parallelAPIError{message: text}
	}
	return &parallelAPIError{message: fmt.Sprintf("Error code: %d", resp.status)}
}

// Answer data as the SDK hands it back, the decoded JSON or the raw text of a non JSON reply
func parallelResponseData(resp *sdkResponse) (any, error) {
	contentType := resp.header.Get("content-type")
	if contentType == "" {
		contentType = "*"
	}
	contentType = strings.SplitN(contentType, ";", 2)[0]
	if !strings.HasSuffix(contentType, "json") {
		if data, err := pyjson.LoadsBytes(resp.body); err == nil {
			return data, nil
		}
		return resp.text(), nil
	}
	data, err := pyjson.LoadsBytes(resp.body)
	if err != nil {
		return nil, verbatim(err.Error())
	}
	return data, nil
}

// Posts with the SDK retry policy, connection failures and retryable statuses retried twice
func (c *parallelClient) post(ctx context.Context, path string, body *pyjson.Object) (any, error) {
	payload, err := pyjson.Dumps(body, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	target := c.endpoint(path)
	for retries := 0; ; retries++ {
		remaining := parallelMaxRetries - retries
		resp, err := sdkSend(ctx, parallelTimeouts, http.MethodPost, target, []byte(payload), c.headers(retries))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if remaining > 0 {
				if serr := parallelSleep(ctx, parallelRetryDelay(retries, nil)); serr != nil {
					return nil, serr
				}
				continue
			}
			if isSDKTimeout(err) {
				return nil, &parallelAPIError{message: "Request timed out."}
			}
			return nil, &parallelAPIError{message: "Connection error."}
		}
		if resp.status >= 400 {
			if remaining > 0 && parallelShouldRetry(resp) {
				if serr := parallelSleep(ctx, parallelRetryDelay(retries, resp.header)); serr != nil {
					return nil, serr
				}
				continue
			}
			return nil, parallelStatusError(resp)
		}
		return parallelResponseData(resp)
	}
}

// Field of a constructed SDK model, the AttributeError of a missing required field or a non model value
func parallelAttr(v any, class, field string, required bool) (any, error) {
	d, ok := pyDict(v)
	if !ok {
		return nil, pyAttributeError(v, field)
	}
	val, present := d.Get(field)
	if !present && required {
		return nil, fmt.Errorf("'%s' object has no attribute '%s'", class, field)
	}
	return val, nil
}

// Items of a model list field read with the or [] guard
func parallelList(v any) ([]any, error) {
	return pyIter(pyOr(v, []any{}))
}

type parallelProvider struct{ baseWebProvider }

// Search through the keyless ring or the keyed SDK search, every failure a search error result
func (p *parallelProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	return runSearch(ctx, env, "Parallel", func() (*WebSearchResponse, error) {
		key, err := providerEnv(ctx, env, "PARALLEL_API_KEY")
		if err != nil {
			return nil, err
		}
		if useKeyless(env, "parallel", key) {
			envLog(env).Info("Parallel keyless search", "query", query, "limit", limit)
			return keylessSearchWithFailover(ctx, env, "parallel", query, limit)
		}
		mode := resolveParallelSearchMode(env)
		envLog(env).Info("Parallel search", "query", query, "mode", mode, "limit", limit)
		client, err := getParallelSyncClient(ctx, env)
		if err != nil {
			return nil, err
		}
		response, err := client.post(ctx, "/v1beta/search", pyObject("max_results", min(limit, searchLimitCap), "mode", mode, "objective", query, "search_queries", []string{query}))
		if err != nil {
			return nil, err
		}
		results, err := parallelAttr(response, "SearchResult", "results", true)
		if err != nil {
			return nil, err
		}
		rows, err := parallelList(results)
		if err != nil {
			return nil, err
		}
		hits := make([]WebSearchHit, 0, len(rows))
		for i, r := range rows {
			u, err := parallelAttr(r, "WebSearchResult", "url", true)
			if err != nil {
				return nil, err
			}
			title, err := parallelAttr(r, "WebSearchResult", "title", false)
			if err != nil {
				return nil, err
			}
			excerpts, err := parallelAttr(r, "WebSearchResult", "excerpts", false)
			if err != nil {
				return nil, err
			}
			description, err := pyJoin(" ", pyOr(excerpts, []any{}))
			if err != nil {
				return nil, err
			}
			hits = append(hits, webHit(pyStrField(pyOr(u, "")), pyStrField(pyOr(title, "")), description, i+1))
		}
		return searchOK(hits), nil
	}, true), nil
}

// Extract full page content, URLs Parallel could not fetch reported from its errors list
func (p *parallelProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	return runExtract(ctx, env, "Parallel", urls, func() ([]*WebPage, error) {
		key, err := providerEnv(ctx, env, "PARALLEL_API_KEY")
		if err != nil {
			return nil, err
		}
		if useKeyless(env, "parallel", key) {
			envLog(env).Info("Parallel keyless extract", "urls", len(urls))
			return keylessExtractWithFailover(ctx, env, "parallel", urls)
		}
		envLog(env).Info("Parallel extract", "urls", len(urls))
		client, err := getParallelAsyncClient(ctx, env)
		if err != nil {
			return nil, err
		}
		response, err := client.post(ctx, "/v1beta/extract", pyObject("urls", urls, "full_content", true))
		if err != nil {
			return nil, err
		}
		results, err := parallelAttr(response, "ExtractResponse", "results", true)
		if err != nil {
			return nil, err
		}
		rows, err := parallelList(results)
		if err != nil {
			return nil, err
		}
		out := make([]*WebPage, 0, len(rows))
		for _, r := range rows {
			u, err := parallelAttr(r, "ExtractResult", "url", true)
			if err != nil {
				return nil, err
			}
			title, err := parallelAttr(r, "ExtractResult", "title", false)
			if err != nil {
				return nil, err
			}
			content, err := parallelAttr(r, "ExtractResult", "full_content", false)
			if err != nil {
				return nil, err
			}
			if !pyTruthy(content) {
				excerpts, err := parallelAttr(r, "ExtractResult", "excerpts", false)
				if err != nil {
					return nil, err
				}
				if content, err = pyJoin("\n\n", pyOr(excerpts, []any{})); err != nil {
					return nil, err
				}
			}
			out = append(out, webDocument(pyStrField(pyOr(u, "")), pyStrField(pyOr(title, "")), pyStrField(content), ""))
		}
		errorsField, err := parallelAttr(response, "ExtractResponse", "errors", true)
		if err != nil {
			return nil, err
		}
		failures, err := parallelList(errorsField)
		if err != nil {
			return nil, err
		}
		for _, e := range failures {
			u, err := parallelAttr(e, "ExtractError", "url", true)
			if err != nil {
				return nil, err
			}
			status, _ := parallelAttr(e, "ExtractError", "http_status_code", false)
			reason, err := parallelAttr(e, "ExtractError", "error_type", true)
			if err != nil {
				return nil, err
			}
			body, err := parallelAttr(e, "ExtractError", "content", false)
			if err != nil {
				return nil, err
			}
			entry, err := vendorErrorEntry(pyStrField(pyOr(u, "")), status, reason, body)
			if err != nil {
				return nil, err
			}
			out = append(out, entry)
		}
		return out, nil
	}, true), nil
}

// SetupSchema is the icebath tools picker entry
func (p *parallelProvider) SetupSchema() map[string]any {
	return keylessVariantSchema("Parallel", "PARALLEL_API_KEY", "https://parallel.ai",
		"Objective-tuned search + page extraction on Parallel's anonymous free tier. Rate-limited under burst load.",
		"Objective-tuned search + parallel page extraction via the Parallel SDK. Unthrottled, guaranteed service.")
}

func init() {
	RegisterWebProvider(&parallelProvider{baseWebProvider{name: "parallel", display: "Parallel", keyEnv: "PARALLEL_API_KEY", extract: true, keyless: true}})
}
