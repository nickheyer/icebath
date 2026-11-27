package web

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/nickheyer/icebath/internal/web"
)

// Longest one request may take, and one round of engines is waited for
const patience = 10 * time.Second

// Most bytes of a page read
const maxBody = 64 << 20

var errSlow = fmt.Errorf("timed out after %s", patience)

// What an engine reaches the web through: a browser primp impersonates, with cookies of its own
type browser struct {
	client *http.Client
	header http.Header
}

func newBrowser(connections http.RoundTripper) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{client: &http.Client{Transport: connections, Jar: jar}, header: impersonate()}
}

// Cookies the site gets from now on
func (b *browser) cookies(site string, pairs [][2]string) {
	address, _ := url.Parse(site)
	cookies := make([]*http.Cookie, len(pairs))
	for i, pair := range pairs {
		cookies[i] = &http.Cookie{Name: pair[0], Value: pair[1]}
	}
	b.client.Jar.SetCookies(address, cookies)
}

// Text of the page when it answers 200, empty for any other status
func (b *browser) page(ctx context.Context, method, address string, params, form [][2]string) (string, error) {
	status, text, err := b.send(ctx, method, address, params, form)
	if err != nil || status != http.StatusOK {
		return "", err
	}
	return text, nil
}

// Status and text of the answer, the params added to the address and the form sent as the body, redirects followed
func (b *browser) send(ctx context.Context, method, address string, params, form [][2]string) (int, string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, patience, errSlow)
	defer cancel()
	if len(params) > 0 {
		separator := "?"
		if strings.Contains(address, "?") {
			separator = "&"
		}
		address += separator + encode(params)
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(encode(form))
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return 0, "", err
	}
	request.Header = b.header.Clone()
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := b.client.Do(request)
	if err != nil {
		return 0, "", failed(ctx, method, address, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody))
	if err != nil {
		return 0, "", failed(ctx, method, address, err)
	}
	data, err := decode(response.Header, raw)
	if err != nil {
		return 0, "", err
	}
	return response.StatusCode, strings.ToValidUTF8(string(web.Decoded(response.Header.Get("Content-Type"), data)), "\uFFFD"), nil
}

// Failure of the request, said as a timeout when it ran out of time
func failed(ctx context.Context, method, address string, err error) error {
	_, wrapped := errors.AsType[*url.Error](err)
	switch {
	case errors.Is(context.Cause(ctx), errSlow):
		err = errSlow
	case wrapped:
		return err
	}
	return &url.Error{Op: method[:1] + strings.ToLower(method[1:]), URL: address, Err: err}
}

// Body with its content codings undone, the last applied first, up to one there is no decoder for
func decode(header http.Header, body []byte) ([]byte, error) {
	var codings []string
	for _, value := range header.Values("Content-Encoding") {
		for coding := range strings.SplitSeq(value, ",") {
			if coding = strings.ToLower(strings.TrimSpace(coding)); coding != "" && coding != "identity" {
				codings = append(codings, coding)
			}
		}
	}
	for _, coding := range slices.Backward(codings) {
		open, ok := decoders[coding]
		if !ok {
			break
		}
		reader, err := open(bytes.NewReader(body))
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(reader, maxBody))
			reader.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("the search reply could not be decoded from %s: %w", coding, err)
		}
	}
	return body, nil
}

// Readers undoing the content codings the browsers accept
var decoders = map[string]func(io.Reader) (io.ReadCloser, error){
	"gzip":    gunzip,
	"x-gzip":  gunzip,
	"deflate": inflate,
	"br": func(r io.Reader) (io.ReadCloser, error) {
		return io.NopCloser(brotli.NewReader(r)), nil
	},
	"zstd": func(r io.Reader) (io.ReadCloser, error) {
		decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		return decoder.IOReadCloser(), nil
	},
}

func gunzip(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}

// Deflate data in a zlib stream when it starts like one, raw otherwise
func inflate(r io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(r)
	if head, err := buffered.Peek(2); err == nil && head[0]&0x0f == 8 && head[0]>>4 <= 7 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
		return zlib.NewReader(buffered)
	}
	return flate.NewReader(buffered), nil
}

// Pairs in their order as a query string or form body, encoded the way browsers do
func encode(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, pair := range pairs {
		parts[i] = escape(pair[0], "*-._", true) + "=" + escape(pair[1], "*-._", true)
	}
	return strings.Join(parts, "&")
}

// Text percent-encoded but for slashes and what Python's quote keeps
func quote(text string) string {
	return escape(text, "_.-~/", false)
}

// Bytes percent-encoded but for ASCII letters, digits and those kept, a space as a plus sign when plus is set
func escape(text, keep string, plus bool) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', strings.IndexByte(keep, c) >= 0:
			b.WriteByte(c)
		case c == ' ' && plus:
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
