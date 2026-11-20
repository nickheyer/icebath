package pyhttpx

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Failure decoding a Content-Encoding the way httpx raises DecodingError
type DecodingError struct{ Err error }

func (e *DecodingError) Error() string { return e.Err.Error() }
func (e *DecodingError) Unwrap() error { return e.Err }

// Body with every Content-Encoding undone in reverse order as httpx decodes it, unknown codings left as is
func DecodeContent(h http.Header, body []byte) ([]byte, error) {
	var codings []string
	for _, v := range h.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
				codings = append(codings, c)
			}
		}
	}
	for i := len(codings) - 1; i >= 0; i-- {
		var out []byte
		var err error
		switch codings[i] {
		case "gzip", "x-gzip":
			var r *gzip.Reader
			if r, err = gzip.NewReader(bytes.NewReader(body)); err == nil {
				out, err = io.ReadAll(r)
			}
		case "deflate":
			if r, zerr := zlib.NewReader(bytes.NewReader(body)); zerr == nil {
				out, err = io.ReadAll(r)
			} else {
				out, err = io.ReadAll(flate.NewReader(bytes.NewReader(body)))
			}
		case "br":
			out, err = io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
		case "zstd":
			var d *zstd.Decoder
			if d, err = zstd.NewReader(bytes.NewReader(body)); err == nil {
				out, err = io.ReadAll(d)
				d.Close()
			}
		default:
			continue
		}
		if err != nil {
			return nil, &DecodingError{err}
		}
		body = out
	}
	return body, nil
}
