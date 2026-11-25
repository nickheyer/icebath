package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Test seam for the client every httpx call sends through
var httpxClient = func() *http.Client { return httputil.Client(false) }

// Test seam for the website policy check the extract gate runs
var extractWebsiteAccess = CheckWebsiteAccess

// Test seam for the private address check the extract gate runs on final urls
var extractURLSafe = isSafeURL

// One GET or POST with redirects unfollowed and every phase bounded by the timeout; a failure to parse the target or send the request is a *url.Error
func httpxSend(ctx context.Context, method, target string, params url.Values, headers [][2]string, payload any, timeout time.Duration) (*httputil.Response, error) {
	target = httputil.MergeQuery(target, params)
	var body io.Reader
	if payload != nil {
		text, err := jsonutil.Encode(payload)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(text)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", icebathUserAgent())
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	return httputil.Do(httpxClient(), req, httputil.EachPhase(timeout))
}

// Response body text for a failing reply, or HTTP code when the body is empty
func httpxStatusDetail(resp *httputil.Response) string {
	return httpStatusDetail(resp.StatusCode, resp.Text())
}

// GET and parse JSON, any non 2xx answer a failure
func httpGetJSON(ctx context.Context, env *Env, label, target string, params url.Values, headers [][2]string, timeout time.Duration, reachTarget string) (any, *WebSearchResponse) {
	resp, err := httpxSend(ctx, http.MethodGet, target, params, headers, nil, timeout)
	if err != nil {
		envLog(env).Warn(label+" request error", "error", err.Error())
		if reachTarget == "" {
			reachTarget = label
		}
		return nil, searchFail(fmt.Sprintf("Could not reach %s: %s", reachTarget, err.Error()))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		envLog(env).Warn(label+" HTTP error", "status", resp.StatusCode, "url", resp.URL)
		return nil, searchFail(fmt.Sprintf("%s returned HTTP %d", label, resp.StatusCode))
	}
	data, err := resp.JSON()
	if err != nil {
		envLog(env).Warn(label+" response parse error", "error", err.Error())
		return nil, searchFail(fmt.Sprintf("Could not parse %s response as JSON", label))
	}
	return data, nil
}

// dict.get with a default over a decoded value, the AttributeError of a non dict
func getDefault(v any, key string, def any) (any, error) {
	d, ok := mappingOf(v)
	if !ok {
		return nil, attributeError(v, "get")
	}
	if val, ok := d.Get(key); ok {
		return val, nil
	}
	return def, nil
}

// Python seq[:n] over a decoded value, lists and strings sliced and every other type raising
func seqHead(v any, n int) (any, error) {
	clamp := func(length int) int {
		if n < 0 {
			n += length
			if n < 0 {
				n = 0
			}
		}
		return min(n, length)
	}
	switch x := v.(type) {
	case []any:
		return x[:clamp(len(x))], nil
	case string:
		r := []rune(x)
		return string(r[:clamp(len(r))]), nil
	}
	if _, ok := mappingOf(v); ok {
		return nil, fmt.Errorf("slice(None, %d, None)", n)
	}
	return nil, fmt.Errorf("expected a list, got %s", errchain.Kind(v))
}

// Brave and SearXNG row normalizer with str of every field and 1 based positions
func titledRows(raw []any, descriptionKey string) ([]WebSearchHit, error) {
	out := make([]WebSearchHit, 0, len(raw))
	for i, item := range raw {
		d, ok := mappingOf(item)
		if !ok {
			return nil, attributeError(item, "get")
		}
		field := func(key string) string {
			if v, ok := d.Get(key); ok {
				return anyutil.String(v)
			}
			return ""
		}
		out = append(out, titleHit(field("title"), field("url"), field(descriptionKey), i+1))
	}
	return out, nil
}
