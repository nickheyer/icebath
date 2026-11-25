package jsonschemautil

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/utils/netpref"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Source of documents a schema refers to by URI: the JSON document at the URI, decoded; an error wrapping ErrUnsupportedURI hands the URI to the next loader
type Loader func(ctx context.Context, uri string) (any, error)

// Largest document a loader reads
const maxDocumentSize = 16 << 20

// Time the shared client of HTTPLoader gives one request
const loaderTimeout = 30 * time.Second

// Loader fetching http and https URIs with a GET through client, or when client is nil through a shared client that honours the proxy settings and the address family preference
func HTTPLoader(client *http.Client) Loader {
	return func(ctx context.Context, uri string) (any, error) {
		u, err := url.Parse(uri)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedURI, uri)
		}
		c := client
		if c == nil {
			c = sharedClient()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/schema+json, application/json;q=0.9, */*;q=0.1")
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("get %s: %s", uri, resp.Status)
		}
		return readDocument(resp.Body, uri)
	}
}

// Client of HTTPLoader when it is given none, built on first use
var sharedClient = sync.OnceValue(func() *http.Client {
	t := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
	}
	t.Proxy = httputil.Proxy
	return &http.Client{Transport: netpref.Follow(t), Timeout: loaderTimeout}
})

// Loader reading file URIs from fsys, the path of the URI without its leading slash naming the file there; a URI naming another host is not served
func FileLoader(fsys fs.FS) Loader {
	return func(_ context.Context, uri string) (any, error) {
		u, err := url.Parse(uri)
		if err != nil || u.Scheme != "file" || u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedURI, uri)
		}
		p := u.Path
		if p == "" {
			p = u.Opaque
		}
		name := strings.TrimPrefix(path.Clean("/"+p), "/")
		if name == "" || !fs.ValidPath(name) {
			return nil, fmt.Errorf("%s names no file", uri)
		}
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readDocument(f, uri)
	}
}

// Loader decoding data URIs, which carry the document itself, percent-encoded or in base64
func DataLoader() Loader {
	return func(_ context.Context, uri string) (any, error) {
		if len(uri) < 5 || !strings.EqualFold(uri[:5], "data:") {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedURI, uri)
		}
		meta, payload, ok := strings.Cut(uri[5:], ",")
		if !ok {
			return nil, errors.New("data URI has no comma before its data")
		}
		data := []byte(urlutil.Unescape(payload))
		if strings.HasSuffix(strings.ToLower(meta), ";base64") {
			text := strings.TrimRight(string(data), "=")
			decoded, err := base64.RawStdEncoding.DecodeString(text)
			if err != nil {
				if decoded, err = base64.RawURLEncoding.DecodeString(text); err != nil {
					return nil, fmt.Errorf("data URI: %w", err)
				}
			}
			data = decoded
		}
		if len(data) > maxDocumentSize {
			return nil, fmt.Errorf("data URI document is larger than %d bytes", maxDocumentSize)
		}
		return jsonutil.Decode(data)
	}
}

// Document read whole from r and decoded as JSON, refused past maxDocumentSize
func readDocument(r io.Reader, uri string) (any, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", uri, err)
	}
	if len(data) > maxDocumentSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", uri, maxDocumentSize)
	}
	doc, err := jsonutil.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", uri, err)
	}
	return doc, nil
}
